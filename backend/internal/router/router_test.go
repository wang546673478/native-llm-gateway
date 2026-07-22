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
	name    string
	proto   provider.Protocol
	models  []string
}

func (p *fakeProvider) Name() string                          { return p.name }
func (p *fakeProvider) Protocol() provider.Protocol           { return p.proto }
func (p *fakeProvider) Models() []string                      { return p.models }
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

func TestRouter_HealthFilterSkipsOpen(t *testing.T) {
	mgr := newFakeManager(t,
		&fakeProvider{name: "p1", proto: provider.ProtocolOpenAI, models: []string{"m1"}},
		&fakeProvider{name: "p2", proto: provider.ProtocolOpenAI, models: []string{"m2"}},
	)
	r := NewRouter(zap.NewNop(), mgr, nil, Config{
		Aliases: map[string]AliasConfig{
			"x": {
				Strategy: "priority",
				Providers: []ProviderRoute{
					{Name: "p1", Model: "m1", Priority: 1},
					{Name: "p2", Model: "m2", Priority: 2},
				},
			},
		},
	})
	r.SetHealthStatus("p1", true) // 标记 p1 OPEN

	req := &provider.Request{Model: "x", Path: "/v1/chat/completions"}
	it, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "p2" {
		t.Errorf("got %s, want p2 (p1 OPEN should be skipped)", res.ProviderName)
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
