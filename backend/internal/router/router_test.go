package router

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// fakeProvider 最小可用的 Provider(用于测试 Router)
type fakeProvider struct {
	name   string
	proto  provider.Protocol
	models []string
}

func (p *fakeProvider) Name() string                { return p.name }
func (p *fakeProvider) Protocol() provider.Protocol { return p.proto }
func (p *fakeProvider) Models() []string            { return p.models }
func (p *fakeProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (p *fakeProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, nil
}
func (p *fakeProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeProvider) Close() error                          { return nil }

// newFakeManager 构造一个带 fake providers 的 Manager
// 每个测试独立 Registry,避免污染全局
func newFakeManager(t *testing.T, ps ...provider.Provider) *provider.Manager {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range ps {
		p := p
		reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
			return p, nil
		})
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	cfg := &provider.ManagerConfig{
		Providers: make(map[string]provider.ManagerProviderConfig),
	}
	for _, p := range ps {
		cfg.Providers[p.Name()] = provider.ManagerProviderConfig{
			Enabled:  true,
			Endpoint: "http://example.com",
			Protocol: p.Protocol(),
			Models:   p.Models(),
			APIKeys:  []string{"sk-test"},
		}
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return mgr
}

func TestRouter_PriorityStrategyPicksLowestPriority(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"m1"}},
		&fakeProvider{name: "p2", proto: provider.ProtocolOpenAI, models: []string{"m2"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases: map[string]AliasConfig{
			"coding-model": {
				Strategy: "priority",
				Providers: []ProviderRoute{
					{Name: "p1", Model: "m1", Priority: 5},
					{Name: "p2", Model: "m2", Priority: 1},
				},
			},
		},
	})

	req := &provider.Request{Model: "coding-model", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	first, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first.ProviderName != "p2" {
		t.Errorf("first = %s, want p2 (priority=1)", first.ProviderName)
	}
	second, _ := it.Next()
	if second == nil || second.ProviderName != "p1" {
		t.Errorf("second should be p1, got %v", second)
	}
}

func TestRouter_ProtocolFilterRejectsMismatch(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "anthropic-p", proto: provider.ProtocolAnthropic, models: []string{"m1"}},
		&fakeProvider{name: "openai-p", proto: provider.ProtocolOpenAI, models: []string{"m2"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases: map[string]AliasConfig{
			"x": {
				Strategy: "priority",
				Providers: []ProviderRoute{
					{Name: "anthropic-p", Model: "m1", Priority: 1},
					{Name: "openai-p", Model: "m2", Priority: 2},
				},
			},
		},
	})

	// 请求是 OpenAI 协议 → anthropic provider 应被过滤
	req := &provider.Request{Model: "x", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "openai-p" {
		t.Errorf("got %s, want openai-p (anthropic should be filtered by protocol)", res.ProviderName)
	}
}

func TestRouter_UnknownAliasReturnsErrNoRoute(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"known-model"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases: map[string]AliasConfig{},
	})
	req := &provider.Request{Model: "totally-unknown", Path: "/v1/chat/completions"}
	_, err := r.Route(context.Background(), req)
	if err == nil {
		t.Error("expected ErrNoRoute for unknown model")
	}
}

func TestRouter_DirectModelLookup(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"known-model"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{Aliases: map[string]AliasConfig{}})
	req := &provider.Request{Model: "known-model", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil || res == nil {
		t.Fatalf("Next: %v / %v", res, err)
	}
	if res.ProviderName != "p1" {
		t.Errorf("got %s, want p1", res.ProviderName)
	}
}

// === P-catch-all 兜底路由测试 ===

// TestRouter_CatchAllLongForm P-catch-all:
// 未知 model 名 + catch_all(长格式 providers)→ 按 catch_all 路由,
// 协议过滤只留匹配请求路径的面;alias 表命中时 catch_all 不生效
func TestRouter_CatchAllLongForm(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"MiniMax-M3"}},
		&fakeProvider{name: "deepseek", proto: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash"}},
		&fakeProvider{name: "qwen", proto: provider.ProtocolOpenAI, models: []string{"qwen-plus"}},
	)
	catchAll := &AliasConfig{
		Alias:    "*",
		Strategy: "priority",
		Providers: []ProviderRoute{
			{Name: "minimax", Model: "MiniMax-M3", Priority: 1},
			{Name: "deepseek", Model: "deepseek-v4-flash", Priority: 2},
		},
	}
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases: map[string]AliasConfig{
			"claude-opus-4-5": {Alias: "claude-opus-4-5", Strategy: "priority", Providers: []ProviderRoute{
				{Name: "minimax", Model: "MiniMax-M3", Priority: 1},
			}},
		},
		CatchAll: catchAll,
	})

	// 未知 model 名(openai 面)→ catch_all,协议过滤只留 openai 面
	req := &provider.Request{Model: "gpt-5", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route unknown with catch_all: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "deepseek" || res.ModelID != "deepseek-v4-flash" {
		t.Errorf("catch_all(openai 面)= %s/%s, want deepseek/deepseek-v4-flash",
			res.ProviderName, res.ModelID)
	}

	// 未知 model 名(anthropic 面)→ 只留 anthropic 面
	reqA := &provider.Request{Model: "gpt-5", Path: "/v1/messages"}
	itA, err := r.Route(context.Background(), reqA)
	if err != nil {
		t.Fatalf("Route unknown (anthropic) with catch_all: %v", err)
	}
	resA, err := itA.Next()
	if err != nil {
		t.Fatalf("Next (anthropic): %v", err)
	}
	if resA.ProviderName != "minimax" || resA.ModelID != "MiniMax-M3" {
		t.Errorf("catch_all(anthropic 面)= %s/%s, want minimax/MiniMax-M3",
			resA.ProviderName, resA.ModelID)
	}

	// alias 表命中时 catch_all 不生效
	reqAlias := &provider.Request{Model: "claude-opus-4-5", Path: "/v1/messages"}
	itAlias, err := r.Route(context.Background(), reqAlias)
	if err != nil {
		t.Fatalf("Route alias: %v", err)
	}
	resAlias, err := itAlias.Next()
	if err != nil {
		t.Fatalf("Next (alias): %v", err)
	}
	if resAlias.ProviderName != "minimax" {
		t.Errorf("alias = %s, want minimax(catch_all 不应覆盖 alias)", resAlias.ProviderName)
	}

	// 真实 model 名也走 catch_all 链(客户端模型名只是标签,qwen 不在链上被跳过)
	reqReal := &provider.Request{Model: "qwen-plus", Path: "/v1/chat/completions"}
	itReal, err := r.Route(context.Background(), reqReal)
	if err != nil {
		t.Fatalf("Route real model: %v", err)
	}
	resReal, err := itReal.Next()
	if err != nil {
		t.Fatalf("Next (real): %v", err)
	}
	if resReal.ProviderName != "deepseek" {
		t.Errorf("real model = %s, want deepseek(catch_all 链,不直连声明者)", resReal.ProviderName)
	}
}

// TestRouter_CatchAllShortForm P-catch-all:
// catch_all 用短格式 target_model → 自动发现声明该 model 的 provider
func TestRouter_CatchAllShortForm(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases:  map[string]AliasConfig{},
		CatchAll: &AliasConfig{Alias: "*", TargetModel: "deepseek-v4-flash"},
	})
	req := &provider.Request{Model: "o4-mini", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "p1" || res.ModelID != "deepseek-v4-flash" {
		t.Errorf("catch_all(short)= %s/%s, want p1/deepseek-v4-flash", res.ProviderName, res.ModelID)
	}
}

// TestRouter_CatchAllAuto_WhitelistSelect P-whitelist-select:
// 白名单参与候选模型选择 — provider 声明过白名单里的模型就用白名单模型
// (按白名单顺序),声明里没有白名单模型的 provider 不参与
func TestRouter_CatchAllAuto_WhitelistSelect(t *testing.T) {
	now := time.Now()
	mkPool := func() *keypool.Pool {
		return keypool.NewPool("p", []*keypool.Key{{
			ID: "1", ProviderName: "p", Name: "k1", Key: "sk",
			Status: keypool.KeyStatusActive, BillingSource: "api",
			CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}

	mgr := newFakeManager(t,
		&fakeProvider{name: "deepseek", proto: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}},
		&fakeProvider{name: "minimax-openai", proto: provider.ProtocolOpenAI, models: []string{"MiniMax-M3"}},
	)
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{
		"deepseek":       mkPool(),
		"minimax-openai": mkPool(),
	}, Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	// 白名单 [deepseek-v4-pro]:deepseek 候选用 v4-pro(声明过),minimax 不参与
	it, err := r.Route(context.Background(),
		&provider.Request{Model: "claude-opus-5", Path: "/v1/chat/completions"},
		WithAllowedModels([]string{"deepseek-v4-pro"}))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "deepseek" || res.ModelID != "deepseek-v4-pro" {
		t.Errorf("whitelist-select = %s/%s, want deepseek/deepseek-v4-pro",
			res.ProviderName, res.ModelID)
	}

	// 白名单 [MiniMax-M3, deepseek-v4-pro]:两个 provider 都按白名单模型参与
	it2, err := r.Route(context.Background(),
		&provider.Request{Model: "claude-opus-5", Path: "/v1/chat/completions"},
		WithAllowedModels([]string{"MiniMax-M3", "deepseek-v4-pro"}))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res2, err := it2.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res2.ModelID != "MiniMax-M3" && res2.ModelID != "deepseek-v4-pro" {
		t.Errorf("whitelist-select = %s, want MiniMax-M3 或 deepseek-v4-pro", res2.ModelID)
	}

	// 白名单里没有 provider 声明过的模型 → ErrNoRoute
	if _, err := r.Route(context.Background(),
		&provider.Request{Model: "x", Path: "/v1/chat/completions"},
		WithAllowedModels([]string{"does-not-exist"})); err == nil {
		t.Error("expected ErrNoRoute when whitelist matches no declared model")
	}

	// 不带白名单(或通配)→ 用默认模型(第一个声明);
	// 同 tier 内 provider 顺序是 map 迭代随机的,两个默认模型都合法
	it3, err := r.Route(context.Background(),
		&provider.Request{Model: "x", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res3, err := it3.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res3.ModelID != "deepseek-v4-flash" && res3.ModelID != "MiniMax-M3" {
		t.Errorf("no whitelist = %s, want 默认模型(deepseek-v4-flash 或 MiniMax-M3)", res3.ModelID)
	}
}

// TestRouter_CatchAllAbsent P-catch-all: 没配 catch_all 时未知 model 照旧 ErrNoRoute
func TestRouter_CatchAllAbsent(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"known-model"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{Aliases: map[string]AliasConfig{}})
	req := &provider.Request{Model: "gpt-5", Path: "/v1/chat/completions"}
	if _, err := r.Route(context.Background(), req); err == nil {
		t.Error("expected ErrNoRoute without catch_all")
	}
}

// TestRouter_CatchAllAuto P-catch-all 自动模式(catch_all: {}):
// 所有 enabled provider 都参与,协议面按请求路径过滤,默认模型取第一个声明,
// tier 计费 token_plan 优先;无 key 的 provider 自然跳过
func TestRouter_CatchAllAuto(t *testing.T) {
	now := time.Now()
	mkPool := func(bs string) *keypool.Pool {
		return keypool.NewPool("p", []*keypool.Key{{
			ID: "1", ProviderName: "p", Name: "k1", Key: "sk",
			Status: keypool.KeyStatusActive, BillingSource: bs,
			CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}
	tokenPlanPool := mkPool("token_plan")
	apiPool := mkPool("api")
	noKeyPool := keypool.NewPool("p", nil, nil, keypool.Config{})

	mgr := newFakeManager(t,
		&fakeProvider{name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"MiniMax-M3"}},
		&fakeProvider{name: "deepseek-anthropic", proto: provider.ProtocolAnthropic, models: []string{"deepseek-v4-flash"}},
		&fakeProvider{name: "deepseek", proto: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash"}},
		&fakeProvider{name: "qwen", proto: provider.ProtocolOpenAI, models: []string{"qwen-plus"}},
	)
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{
		"minimax":            tokenPlanPool,
		"deepseek-anthropic": apiPool,
		"deepseek":           apiPool,
		"qwen":               noKeyPool, // 无 key → 自然跳过
	}, Config{
		Aliases:  map[string]AliasConfig{},
		CatchAll: &AliasConfig{Alias: "*"}, // 空规则 = 自动模式
	})

	// anthropic 面:只留 anthropic 面 provider,token_plan 桶排最前
	req := &provider.Request{Model: "whatever-model", Path: "/v1/messages"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "minimax" || res.ModelID != "MiniMax-M3" {
		t.Errorf("first = %s/%s, want minimax/MiniMax-M3(token_plan 优先)",
			res.ProviderName, res.ModelID)
	}

	// openai 面:deepseek(有 api key)命中;qwen(无 key)跳过
	req2 := &provider.Request{Model: "gpt-5", Path: "/v1/chat/completions"}
	it2, err := r.Route(context.Background(), req2)
	if err != nil {
		t.Fatalf("Route (openai): %v", err)
	}
	res2, err := it2.Next()
	if err != nil {
		t.Fatalf("Next (openai): %v", err)
	}
	if res2.ProviderName != "deepseek" || res2.ModelID != "deepseek-v4-flash" {
		t.Errorf("openai face = %s/%s, want deepseek/deepseek-v4-flash",
			res2.ProviderName, res2.ModelID)
	}

	// 真实 model 名(qwen 声明过 qwen-plus)也走链,不直连 qwen —
	// 客户端模型名只是标签,路由只按协议面 + tier
	reqReal := &provider.Request{Model: "qwen-plus", Path: "/v1/messages"}
	itReal, err := r.Route(context.Background(), reqReal)
	if err != nil {
		t.Fatalf("Route real model: %v", err)
	}
	resReal, err := itReal.Next()
	if err != nil {
		t.Fatalf("Next (real): %v", err)
	}
	if resReal.ProviderName != "minimax" || resReal.ModelID != "MiniMax-M3" {
		t.Errorf("real model via chain = %s/%s, want minimax/MiniMax-M3(token_plan 优先)",
			resReal.ProviderName, resReal.ModelID)
	}

	// 默认模型缺省 = 第一个声明(M2),无需显式 default_model
	mgr2 := newFakeManager(t,
		&fakeProvider{name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"M2", "M3"}},
	)
	r2 := NewRouter(zap.NewNop(), mgr2, map[string]*keypool.Pool{
		"minimax": tokenPlanPool,
	}, Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})
	it3, err := r2.Route(context.Background(), &provider.Request{Model: "x", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res3, err := it3.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res3.ModelID != "M2" {
		t.Errorf("default model = %q, want M2(第一个声明)", res3.ModelID)
	}
}

// silence unused if scheduler/test funcs trimmed

// === P64 buildKeyCandidates 单元测试 ===

func TestBuildKeyCandidates_GlobalTierFlatten(t *testing.T) {
	// 三个 provider,每个都有 token_plan + api key
	// 期望输出顺序: [tp_a, tp_b, tp_c, api_a, api_b, api_c]
	now := time.Now()
	mkKey := func(id uint, tier string) *keypool.Key {
		return &keypool.Key{
			ID:            fmt.Sprintf("%d", id),
			ProviderName:  "p",
			Name:          fmt.Sprintf("k%d", id),
			Key:           "x",
			Status:        keypool.KeyStatusActive,
			BillingSource: tier,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	pools := map[string]*keypool.Pool{
		"a": keypool.NewPool("a", []*keypool.Key{mkKey(1, "token_plan"), mkKey(2, "api")}, nil, keypool.Config{}),
		"b": keypool.NewPool("b", []*keypool.Key{mkKey(3, "token_plan"), mkKey(4, "api")}, nil, keypool.Config{}),
		"c": keypool.NewPool("c", []*keypool.Key{mkKey(5, "token_plan"), mkKey(6, "api")}, nil, keypool.Config{}),
	}
	routes := []ProviderRoute{
		{Name: "a", Model: "m"},
		{Name: "b", Model: "m"},
		{Name: "c", Model: "m"},
	}
	out := buildKeyCandidates(routes, pools)
	if len(out) != 6 {
		t.Fatalf("expected 6 candidates, got %d", len(out))
	}
	wantTiers := []string{"token_plan", "token_plan", "token_plan", "api", "api", "api"}
	for i, w := range wantTiers {
		if out[i].Tier != w {
			t.Errorf("out[%d].Tier = %q, want %q", i, out[i].Tier, w)
		}
	}
}

func TestBuildKeyCandidates_MissingTier(t *testing.T) {
	// a 只有 api,b 只有 token_plan → 输出 [tp_b, api_a]
	now := time.Now()
	mkKey := func(tier string) *keypool.Key {
		return &keypool.Key{
			ID: "1", ProviderName: "p", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	pools := map[string]*keypool.Pool{
		"a": keypool.NewPool("a", []*keypool.Key{mkKey("api")}, nil, keypool.Config{}),
		"b": keypool.NewPool("b", []*keypool.Key{mkKey("token_plan")}, nil, keypool.Config{}),
	}
	routes := []ProviderRoute{{Name: "a", Model: "m"}, {Name: "b", Model: "m"}}
	out := buildKeyCandidates(routes, pools)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d (%+v)", len(out), out)
	}
	if out[0].Tier != "token_plan" || out[0].Name != "b" {
		t.Errorf("out[0] = %+v, want (b, token_plan)", out[0])
	}
	if out[1].Tier != "api" || out[1].Name != "a" {
		t.Errorf("out[1] = %+v, want (a, api)", out[1])
	}
}

func TestBuildKeyCandidates_NilPool(t *testing.T) {
	// pool nil → 兜底按 api 产一个 KeyCandidate
	routes := []ProviderRoute{{Name: "a", Model: "m"}}
	out := buildKeyCandidates(routes, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1, got %d", len(out))
	}
	if out[0].Tier != "api" {
		t.Errorf("expected api fallback, got %q", out[0].Tier)
	}
}

func TestBuildKeyCandidates_SameTierStableOrder(t *testing.T) {
	// 同 tier 内保留输入顺序
	now := time.Now()
	mkKey := func(id uint, tier string) *keypool.Key {
		return &keypool.Key{
			ID: fmt.Sprintf("%d", id), ProviderName: "p", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	pools := map[string]*keypool.Pool{
		"z": keypool.NewPool("z", []*keypool.Key{mkKey(1, "token_plan")}, nil, keypool.Config{}),
		"a": keypool.NewPool("a", []*keypool.Key{mkKey(2, "token_plan")}, nil, keypool.Config{}),
		"m": keypool.NewPool("m", []*keypool.Key{mkKey(3, "token_plan")}, nil, keypool.Config{}),
	}
	routes := []ProviderRoute{{Name: "z", Model: "m"}, {Name: "a", Model: "m"}, {Name: "m", Model: "m"}}
	out := buildKeyCandidates(routes, pools)
	if len(out) != 3 {
		t.Fatalf("expected 3, got %d", len(out))
	}
	// 全部 token_plan,顺序 = [z, a, m]
	if out[0].Name != "z" || out[1].Name != "a" || out[2].Name != "m" {
		t.Errorf("order broken: %+v", []string{out[0].Name, out[1].Name, out[2].Name})
	}
}

func TestBuildKeyCandidates_EmptyPoolOnlyDefaultTier(t *testing.T) {
	// provider pool 里有 key 但 BillingSource 全空 → 兜底按 api
	now := time.Now()
	mkKey := func() *keypool.Key {
		return &keypool.Key{
			ID: "1", ProviderName: "p", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: "",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	pools := map[string]*keypool.Pool{
		"a": keypool.NewPool("a", []*keypool.Key{mkKey()}, nil, keypool.Config{}),
	}
	out := buildKeyCandidates([]ProviderRoute{{Name: "a", Model: "m"}}, pools)
	if len(out) != 1 || out[0].Tier != "api" {
		t.Errorf("expected (a, api), got %+v", out)
	}
}

// TestRouteIterator_NextTieredWalk P64: 验证 Next() 跨 tier 推进
// 候选: [(a,tp), (b,tp), (a,api)]
// 场景: a.tp 池空 → Next() 应该跳过 (a,tp),试 (b,tp) 成功
func TestRouteIterator_NextTieredWalk(t *testing.T) {
	now := time.Now()
	mkKey := func(id uint, tier string) *keypool.Key {
		return &keypool.Key{
			ID: fmt.Sprintf("%d", id), ProviderName: "p", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	// a 池只有 api(没 token_plan key)
	poolA := keypool.NewPool("a", []*keypool.Key{mkKey(1, "api")}, nil, keypool.Config{})
	// b 池有 token_plan key
	poolB := keypool.NewPool("b", []*keypool.Key{mkKey(2, "token_plan")}, nil, keypool.Config{})

	mgr := newFakeManager(t,
		&fakeProvider{name: "a", proto: provider.ProtocolOpenAI, models: []string{"m"}},
		&fakeProvider{name: "b", proto: provider.ProtocolOpenAI, models: []string{"m"}},
	)

	candidates := buildKeyCandidates(
		[]ProviderRoute{{Name: "a", Model: "m"}, {Name: "b", Model: "m"}},
		map[string]*keypool.Pool{"a": poolA, "b": poolB},
	)
	// 期望顺序: (b,tp) 因为 a 没有 tp,b 有 tp;然后 (a,api)
	if len(candidates) != 2 {
		t.Fatalf("expected 2, got %d (%+v)", len(candidates), candidates)
	}
	if candidates[0].Name != "b" || candidates[0].Tier != "token_plan" {
		t.Errorf("candidates[0] = %+v, want (b, token_plan)", candidates[0])
	}
	if candidates[1].Name != "a" || candidates[1].Tier != "api" {
		t.Errorf("candidates[1] = %+v, want (a, api)", candidates[1])
	}

	it := &RouteIterator{
		alias:      "m",
		candidates: candidates,
		pools:      map[string]*keypool.Pool{"a": poolA, "b": poolB},
		manager:    mgr,
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "b" || res.Key.BillingSource != "token_plan" {
		t.Errorf("got (%s, %s), want (b, token_plan)", res.ProviderName, res.Key.BillingSource)
	}
}

// TestRouteIterator_NextExhaustsTierBeforeRolling P64: 验证 "token_plan 全死才进 api"
// 候选: [(a,tp), (b,tp), (a,api), (b,api)]
// 场景: a.tp 池空,b.tp 池空 → Next() 应跳过这两个,落到 (a,api)
func TestRouteIterator_NextExhaustsTierBeforeRolling(t *testing.T) {
	now := time.Now()
	mkKey := func(id uint, tier string) *keypool.Key {
		return &keypool.Key{
			ID: fmt.Sprintf("%d", id), ProviderName: "p", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	// a 池只有 api;b 池只有 api(token_plan 都是空)
	poolA := keypool.NewPool("a", []*keypool.Key{mkKey(1, "api")}, nil, keypool.Config{})
	poolB := keypool.NewPool("b", []*keypool.Key{mkKey(2, "api")}, nil, keypool.Config{})

	mgr := newFakeManager(t,
		&fakeProvider{name: "a", proto: provider.ProtocolOpenAI, models: []string{"m"}},
		&fakeProvider{name: "b", proto: provider.ProtocolOpenAI, models: []string{"m"}},
	)

	candidates := buildKeyCandidates(
		[]ProviderRoute{{Name: "a", Model: "m"}, {Name: "b", Model: "m"}},
		map[string]*keypool.Pool{"a": poolA, "b": poolB},
	)
	// 期望顺序: a 和 b 都没 tp → 只有 (a,api) 和 (b,api)
	if len(candidates) != 2 {
		t.Fatalf("expected 2, got %d", len(candidates))
	}

	it := &RouteIterator{
		alias:      "m",
		candidates: candidates,
		pools:      map[string]*keypool.Pool{"a": poolA, "b": poolB},
		manager:    mgr,
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.Key.BillingSource != "api" {
		t.Errorf("expected api tier (因为 token_plan 全死), got %s", res.Key.BillingSource)
	}
	if res.ProviderName != "a" {
		t.Errorf("expected a (candidates 第一个), got %s", res.ProviderName)
	}
}

// TestRouteIterator_TierTagged Task 4: RouteResult 带 Tier 字段
// 两个 provider:minimax(token_plan key)、deepseek(api key),同协议面,
// buildKeyCandidates 拉平后顺序 [minimax/token_plan, deepseek/api] —
// Next() 依次返回带正确 Tier 的候选(层切换判定层 Task 5 的输入基础)
func TestRouteIterator_TierTagged(t *testing.T) {
	now := time.Now()
	mkPool := func(bs string) *keypool.Pool {
		return keypool.NewPool("p", []*keypool.Key{{
			ID: "1", ProviderName: "p", Name: "k1", Key: "sk",
			Status: keypool.KeyStatusActive, BillingSource: bs,
			CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}

	mgr := newFakeManager(t,
		&fakeProvider{name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"MiniMax-M3"}},
		&fakeProvider{name: "deepseek", proto: provider.ProtocolAnthropic, models: []string{"deepseek-v4-flash"}},
	)
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{
		"minimax":  mkPool("token_plan"),
		"deepseek": mkPool("api"),
	}, Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "claude-opus-5", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	first, err := it.Next()
	if err != nil {
		t.Fatalf("Next 1st: %v", err)
	}
	if first.ProviderName != "minimax" {
		t.Errorf("1st ProviderName = %s, want minimax(token_plan 桶优先)", first.ProviderName)
	}
	if first.Tier != "token_plan" {
		t.Errorf("1st Tier = %q, want token_plan", first.Tier)
	}

	second, err := it.Next()
	if err != nil {
		t.Fatalf("Next 2nd: %v", err)
	}
	if second.ProviderName != "deepseek" {
		t.Errorf("2nd ProviderName = %s, want deepseek", second.ProviderName)
	}
	if second.Tier != "api" {
		t.Errorf("2nd Tier = %q, want api", second.Tier)
	}
}

// TestRouter_CatchAllAuto_ResponsesFilter P-responses:
// /responses 透传只走原生支持 Responses API 的 provider(manager 标记),
// 不支持的 provider(qwen)不参与 — 避免 404 model_not_found 中断 failover
func TestRouter_CatchAllAuto_ResponsesFilter(t *testing.T) {
	now := time.Now()
	mkPool := func() *keypool.Pool {
		return keypool.NewPool("p", []*keypool.Key{{
			ID: "1", ProviderName: "p", Name: "k1", Key: "sk",
			Status: keypool.KeyStatusActive, BillingSource: "api",
			CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}

	mgr := newFakeManager(t,
		&fakeProvider{name: "deepseek", proto: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash"}},
		&fakeProvider{name: "qwen", proto: provider.ProtocolOpenAI, models: []string{"qwen-plus"}},
	)
	// 标记能力:deepseek 支持,qwen 不支持
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{
		Providers: map[string]provider.ManagerProviderConfig{
			"deepseek": {Enabled: true, Protocol: provider.ProtocolOpenAI, Models: []string{"deepseek-v4-flash"}, APIKeys: []string{"sk"}, ResponsesAPI: true},
			"qwen":     {Enabled: true, Protocol: provider.ProtocolOpenAI, Models: []string{"qwen-plus"}, APIKeys: []string{"sk"}},
		},
	}); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{
		"deepseek": mkPool(),
		"qwen":     mkPool(),
	}, Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	// /responses 请求:只有 deepseek 参与
	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5-codex", Path: "/responses"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "deepseek" {
		t.Errorf("responses chain = %s, want deepseek(qwen 不支持应被过滤)", res.ProviderName)
	}
	// 第二个候选也没有 qwen
	if _, err := it.Next(); err == nil {
		t.Error("expected no more candidates(qwen 被过滤)")
	}

	// chat/completions 请求:两个都参与
	it2, err := r.Route(context.Background(),
		&provider.Request{Model: "x", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route chat: %v", err)
	}
	if _, err := it2.Next(); err != nil {
		t.Errorf("chat chain should have candidates: %v", err)
	}
}

// TestRouter_CatchAllAuto_DeterministicOrder P-catch-all-order:
// catch_all 自动模式的候选顺序必须确定性。manager.GetAll() 是 Go map,迭代
// 顺序随机 — 不排序则同层 provider 谁先被尝试不可复现(2026-08-07 实测:
// Claude Code 相邻两条相同请求一条先打 mimo、一条先打 minimax)。
// 修复后按 name 排序 + tier 分桶:同输入两次 Route 顺序一致。
func TestRouter_CatchAllAuto_DeterministicOrder(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"MiniMax-M3"}},
		&fakeProvider{name: "mimo-token-plan-anthropic", proto: provider.ProtocolAnthropic, models: []string{"mimo-v2.5-pro"}},
		&fakeProvider{name: "deepseek-anthropic", proto: provider.ProtocolAnthropic, models: []string{"deepseek-v4-flash"}},
	)
	now := time.Now()
	mkKey := func(providerName, tier string) *keypool.Key {
		return &keypool.Key{ID: providerName + "-1", ProviderName: providerName, Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier, CreatedAt: now, UpdatedAt: now}
	}
	pools := map[string]*keypool.Pool{
		"minimax":                   keypool.NewPool("minimax", []*keypool.Key{mkKey("minimax", "token_plan")}, nil, keypool.Config{}),
		"mimo-token-plan-anthropic": keypool.NewPool("mimo-token-plan-anthropic", []*keypool.Key{mkKey("mimo-token-plan-anthropic", "token_plan")}, nil, keypool.Config{}),
		"deepseek-anthropic":        keypool.NewPool("deepseek-anthropic", []*keypool.Key{mkKey("deepseek-anthropic", "api")}, nil, keypool.Config{}),
	}
	r := NewRouter(zap.NewNop(), mgr, pools, Config{
		Aliases:  map[string]AliasConfig{},
		CatchAll: &AliasConfig{Alias: "*"}, // 空规则 = 自动模式
	})

	collect := func() []string {
		req := &provider.Request{Model: "claude-opus-5", Path: "/v1/messages"}
		it, err := r.Route(context.Background(), req)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		var names []string
		for {
			res, err := it.Next()
			if err != nil {
				break
			}
			names = append(names, res.ProviderName)
		}
		return names
	}

	first := collect()
	second := collect()
	if len(first) != 3 {
		t.Fatalf("expected 3 candidates, got %v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("order differs between runs: %v vs %v", first, second)
		}
	}
	// 期望:name 排序 + tier 分桶 → token_plan 桶 [mimo-token-plan-anthropic, minimax]
	// (mimo-token-plan-anthropic < minimax 按字典序),再 api 桶 [deepseek-anthropic]
	want := []string{"mimo-token-plan-anthropic", "minimax", "deepseek-anthropic"}
	for i, w := range want {
		if first[i] != w {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, first[i], w, first)
		}
	}
}

// TestBuildKeyCandidates_MixedTierPool_BlockBillingIntersects P-mixed-tier-pool:
// 同一 vendor 共享 pool 混层(mimo sk-/tp- 两套 key 一个池)时,候选 tier =
// 池 tiers ∩ 块声明 billing — api 块不能生成 token_plan 候选(否则 tp- key
// 被发到 api 端点 401,2026-08-07 实测 key 12 被误标 COOLING)
func TestBuildKeyCandidates_MixedTierPool_BlockBillingIntersects(t *testing.T) {
	now := time.Now()
	mkKey := func(tier string) *keypool.Key {
		return &keypool.Key{ID: "1", ProviderName: "mimo", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier, CreatedAt: now, UpdatedAt: now}
	}
	shared := keypool.NewPool("mimo", []*keypool.Key{mkKey("token_plan"), mkKey("api")}, nil, keypool.Config{})
	pools := map[string]*keypool.Pool{
		"mimo":                      shared,
		"mimo-token-plan":           shared,
		"mimo-anthropic":            shared,
		"mimo-token-plan-anthropic": shared,
	}
	routes := []ProviderRoute{
		{Name: "mimo-anthropic", Model: "mimo-v2.5", BillingSource: "api"},
		{Name: "mimo-token-plan-anthropic", Model: "mimo-v2.5-pro", BillingSource: "token_plan"},
	}
	out := buildKeyCandidates(routes, pools)
	if len(out) != 2 {
		t.Fatalf("expected 2 candidates (one per block billing), got %+v", out)
	}
	// 顺序:token_plan 桶在前 → mimo-token-plan-anthropic;api 桶 → mimo-anthropic
	if out[0].Name != "mimo-token-plan-anthropic" || out[0].Tier != "token_plan" {
		t.Errorf("out[0] = %s/%s, want mimo-token-plan-anthropic/token_plan", out[0].Name, out[0].Tier)
	}
	if out[1].Name != "mimo-anthropic" || out[1].Tier != "api" {
		t.Errorf("out[1] = %s/%s, want mimo-anthropic/api", out[1].Name, out[1].Tier)
	}
}

// TestBuildKeyCandidates_BlockBillingNotInPool_FallsBackToPoolTiers:
// 块声明 billing 与池 key 完全无交集(池全 token_plan,块未声明/默认 api)→
// 回退池并集(旧行为,保持兼容)
func TestBuildKeyCandidates_BlockBillingNotInPool_FallsBackToPoolTiers(t *testing.T) {
	now := time.Now()
	mkKey := func(tier string) *keypool.Key {
		return &keypool.Key{ID: "1", ProviderName: "mm", Name: "k", Key: "x",
			Status: keypool.KeyStatusActive, BillingSource: tier, CreatedAt: now, UpdatedAt: now}
	}
	pools := map[string]*keypool.Pool{
		"mm": keypool.NewPool("mm", []*keypool.Key{mkKey("token_plan")}, nil, keypool.Config{}),
	}
	// mm 块声明 api,但池只有 token_plan key → 交集空 → 回退 [token_plan]
	out := buildKeyCandidates([]ProviderRoute{{Name: "mm", Model: "m", BillingSource: "api"}}, pools)
	if len(out) != 1 || out[0].Tier != "token_plan" {
		t.Errorf("expected fallback (mm, token_plan), got %+v", out)
	}
}
