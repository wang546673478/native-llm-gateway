package router

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRelayRoutingUsesRequestedModelNotRoutingModel(t *testing.T) {
	relay := &fakeProvider{name: "relay-requested", proto: provider.ProtocolAnthropic, models: []string{"client-model"}}
	builtin := &fakeProvider{name: "builtin-routing", proto: provider.ProtocolAnthropic, models: []string{"routing-model"}}

	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(relay.name, func(provider.ProviderConfig) (provider.Provider, error) {
		return relay, nil
	}, relay.proto, relay.name, true)
	reg.Register(builtin.name, func(provider.ProviderConfig) (provider.Provider, error) { return builtin, nil })
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		relay.name:   {Enabled: true, Protocol: relay.proto},
		builtin.name: {Enabled: true, Protocol: builtin.proto},
	}}); err != nil {
		t.Fatal(err)
	}
	_ = mgr.LoadModelsFromStore(context.Background(), fakeModelStore{rows: []provider.DBModelRow{
		{Vendor: relay.name, ModelID: "client-model"},
		{Vendor: builtin.name, ModelID: "routing-model"},
	}})

	r := NewRouter(zap.NewNop(), mgr, nil, Config{CatchAll: &AliasConfig{}})
	it, err := r.Route(context.Background(), &provider.Request{
		Path: "/v1/messages", Model: "routing-model", RoutingModel: "routing-model", RequestedModel: "client-model",
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	seen := map[string]string{}
	for {
		result, err := it.Next()
		if err != nil {
			break
		}
		seen[result.ProviderName] = result.ModelID
	}
	if seen[relay.name] != "client-model" {
		t.Fatalf("relay model = %q, want client-model; all=%v", seen[relay.name], seen)
	}
	if seen[builtin.name] != "routing-model" {
		t.Fatalf("builtin model = %q, want routing-model; all=%v", seen[builtin.name], seen)
	}
}

// newFakeRelayManager 构造一个「全部 provider 都注册为中转站」的 Manager,
// 并按 fakeProvider.models 喂 provider_model_faces 等价的面归属行。
// 中转站的 vendor = name(与 relay/loader.go 的 RegisterWithProtocolVendorRelay 一致)。
func newFakeRelayManager(t *testing.T, ps ...*fakeProvider) *provider.Manager {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range ps {
		p := p
		reg.RegisterWithProtocolVendorRelay(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
			return p, nil
		}, p.Protocol(), p.Name(), true) // isRelay = true
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	cfg := &provider.ManagerConfig{Providers: make(map[string]provider.ManagerProviderConfig)}
	for _, p := range ps {
		cfg.Providers[p.Name()] = provider.ManagerProviderConfig{
			Enabled:       true,
			Endpoint:      "http://example.com",
			Protocol:      p.Protocol(),
			BillingSource: "api",
			ResponsesAPI:  true, // 免得 /responses 过滤干扰本组测试
		}
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	var rows []provider.DBModelRow
	var faces []provider.DBFaceRow
	for _, p := range ps {
		for i, m := range p.models {
			rows = append(rows, provider.DBModelRow{Vendor: p.Name(), ModelID: m})
			faces = append(faces, provider.DBFaceRow{
				Vendor: p.Name(), Face: p.Name(), ModelID: m, SortOrder: i,
			})
		}
	}
	if err := mgr.LoadModelsFromStore(context.Background(),
		fakeModelStore{rows: rows, faceRows: faces}); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}
	return mgr
}

// newFakeRelayManagerNoResponsesFlag 与 newFakeRelayManager 同构,但把 ResponsesAPI
// 全设为 false —— 这正是中转站在 responsesAPI map 里的形态:2026-08-25 删掉
// relay 侧写入路径后,中转站压根不进那张表,查出来恒为零值 false。
// 中转站是纯透传,网关无从知道上游支不支持 /responses,所以这个 false 不能当资格判定;
// router 的 isRelay 短路是唯一防线,本测试就是盯它的。
func newFakeRelayManagerNoResponsesFlag(t *testing.T, ps ...*fakeProvider) *provider.Manager {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range ps {
		p := p
		reg.RegisterWithProtocolVendorRelay(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
			return p, nil
		}, p.Protocol(), p.Name(), true)
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	cfg := &provider.ManagerConfig{Providers: make(map[string]provider.ManagerProviderConfig)}
	for _, p := range ps {
		cfg.Providers[p.Name()] = provider.ManagerProviderConfig{
			Enabled:       true,
			Endpoint:      "http://example.com",
			Protocol:      p.Protocol(),
			BillingSource: "api",
			ResponsesAPI:  false, // 关键:线上 11 个 tokenmarket 站就是这个状态
		}
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	var rows []provider.DBModelRow
	var faces []provider.DBFaceRow
	for _, p := range ps {
		for i, m := range p.models {
			rows = append(rows, provider.DBModelRow{Vendor: p.Name(), ModelID: m})
			faces = append(faces, provider.DBFaceRow{
				Vendor: p.Name(), Face: p.Name(), ModelID: m, SortOrder: i,
			})
		}
	}
	if err := mgr.LoadModelsFromStore(context.Background(),
		fakeModelStore{rows: rows, faceRows: faces}); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}
	return mgr
}

// TestRelay_ResponsesFilter_DoesNotApplyToRelays 中转站不受 /responses 能力过滤。
// 起因(2026-08-25 线上):当时靠手填 DB 列 supports_responses_api 判定,只有 codex
// 一个站是 t、其余 8 个 openai 站全 f → /responses 候选名单长度恒为 1 → codex 403
// 之后 failover 无处可切 → 502。而这 8 个站的 /v1/responses 实测都是 200,那个 false
// 是错的。该列已随写入路径一并删除,中转站现在恒为零值 false,只靠 isRelay 短路放行 ——
// 删掉那个短路,本测试必须红。
func TestRelay_ResponsesFilter_DoesNotApplyToRelays(t *testing.T) {
	mgr := newFakeRelayManagerNoResponsesFlag(t,
		&fakeProvider{name: "codex", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol"}},
		&fakeProvider{name: "tm-pro", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol"}},
		&fakeProvider{name: "tm-codex1", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol"}},
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("codex", "tm-pro", "tm-codex1"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-sol", Path: "/v1/responses"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := collectNames(t, it)
	if len(got) != 3 {
		t.Fatalf("/responses 候选 = %v(%d 个), want 3 个 —— "+
			"中转站被 supports_responses_api=false 筛掉了,failover 无处可切", got, len(got))
	}
}

// relayPools 给每个中转站建一个单 key 的 api 层 pool
func relayPools(names ...string) map[string]*keypool.Pool {
	now := time.Now()
	out := make(map[string]*keypool.Pool, len(names))
	for _, n := range names {
		out[n] = keypool.NewPool(n, []*keypool.Key{{
			ID: "1", ProviderName: n, Name: "k1", Key: "sk",
			Status: keypool.KeyStatusActive, BillingSource: "api",
			CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}
	return out
}

// collectNames 把迭代器走完,收集候选 provider 名
func collectNames(t *testing.T, it *RouteIterator) []string {
	t.Helper()
	var got []string
	for {
		res, err := it.Next()
		if err != nil {
			return got
		}
		got = append(got, res.ProviderName)
	}
}

// TestRelay_ModelFirst_FiltersStationsWithoutModel P-relay-model-first:
// 中转站候选必须按「客户端模型名 ∈ 该站声明的模型」筛选。
//
// 不筛不只是浪费一次请求 —— 404 → ErrorTypeModelNotFound(provider.go:307)
// → 非 retryable(provider.go:258)→ tryCandidate 返 outcomeFatal → runCandidateLoop
// 当场收尾。一个不合的站会把整条 failover 链掐断,它后面本来能用的站一个都试不到。
// 2026-08-25 线上实测:claude-opus-5→tokenmarket-kiro4 404×8、
// gpt-5.6-sol→tokenmarket-codex 404×3,而这两个模型分别有 13/10 个站声明。
func TestRelay_ModelFirst_FiltersStationsWithoutModel(t *testing.T) {
	mgr := newFakeRelayManager(t,
		// terra 只有 pro 系声明;codex1 只有 sol
		&fakeProvider{name: "tm-codex1", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol"}},
		&fakeProvider{name: "tm-pro3", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol", "gpt-5.6-terra"}},
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("tm-codex1", "tm-pro3"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	// terra:只有 tm-pro3 声明 → 只有它进候选
	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-terra", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route terra: %v", err)
	}
	got := collectNames(t, it)
	if len(got) != 1 || got[0] != "tm-pro3" {
		t.Errorf("terra 候选 = %v, want [tm-pro3](tm-codex1 没声明 terra 应被筛掉)", got)
	}

	// sol:两家都声明 → 都进候选
	it2, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-sol", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route sol: %v", err)
	}
	if got := collectNames(t, it2); len(got) != 2 {
		t.Errorf("sol 候选 = %v, want 两家都在", got)
	}

	// 谁都没声明的模型 → ErrNoRoute(而不是盲发给某家吃 404 掐断链)
	if _, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-luna", Path: "/v1/chat/completions"}); err != ErrNoRoute {
		t.Errorf("未声明模型 err = %v, want ErrNoRoute", err)
	}
}

// TestRelay_ModelFirst_PassesThroughClientModelName P-relay-model-first:
// 中转站透传客户端原始模型名,不改写成 default_model。
// 这是「模型优先 + 透传」的后半句:筛完之后发给上游的必须是客户端报的那个名字。
func TestRelay_ModelFirst_PassesThroughClientModelName(t *testing.T) {
	mgr := newFakeRelayManager(t,
		// default_model 会是 sort_order 首行 = gpt-5.4
		&fakeProvider{name: "tm-pro", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.4", "gpt-5.6-terra"}},
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("tm-pro"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-terra", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ModelID != "gpt-5.6-terra" {
		t.Errorf("ModelID = %q, want gpt-5.6-terra(中转站透传客户端模型名,不能改写成 default_model gpt-5.4)",
			res.ModelID)
	}
}

// TestRelay_ModelFirst_UnsyncedStationIsWildcard P-relay-model-first 核心不变式:
// 该站无模型归属行(从未同步过上游模型清单)→ 当通配放行,不判死。
// 按空集判死会让新建中转站在首次同步前全部候选被筛空 → 503。
// 与二十轮 provider_model_faces 的「无归属行回退放行」是同一条不变式。
func TestRelay_ModelFirst_UnsyncedStationIsWildcard(t *testing.T) {
	mgr := newFakeRelayManager(t,
		&fakeProvider{name: "tm-new", proto: provider.ProtocolOpenAI, models: nil}, // 未同步
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("tm-new"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "whatever-model", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("未同步中转站应放行(通配),got err %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "tm-new" || res.ModelID != "whatever-model" {
		t.Errorf("got %s/%s, want tm-new/whatever-model", res.ProviderName, res.ModelID)
	}
}

// TestRelay_ModelFirst_CaseInsensitive 模型名大小写不敏感匹配
func TestRelay_ModelFirst_CaseInsensitive(t *testing.T) {
	mgr := newFakeRelayManager(t,
		&fakeProvider{name: "tm-kiro", proto: provider.ProtocolAnthropic,
			models: []string{"claude-opus-5"}},
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("tm-kiro"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "Claude-Opus-5", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("大小写变体应匹配: %v", err)
	}
	if _, err := it.Next(); err != nil {
		t.Errorf("Next: %v", err)
	}
}

// TestRelay_ModelFirst_AppliesToExplicitAliasPath P-relay-model-first:
// 显式 alias / catch_all(带 providers 列表)走 filterCandidates,必须和
// routeCatchAllAuto 用同一条规则(relayServesModel 单源)。
// 两处行为分歧会让同一个中转站在自动模式和显式模式下拿到不同候选 —— 正是
// 「删一个字段要改两处」那类漂移。
func TestRelay_ModelFirst_AppliesToExplicitAliasPath(t *testing.T) {
	mgr := newFakeRelayManager(t,
		&fakeProvider{name: "tm-codex1", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol"}},
		&fakeProvider{name: "tm-pro3", proto: provider.ProtocolOpenAI,
			models: []string{"gpt-5.6-sol", "gpt-5.6-terra"}},
	)
	// 显式 providers 列表 + 写死的 model 字段(对中转站应被客户端模型名覆盖)
	r := NewRouter(zap.NewNop(), mgr, relayPools("tm-codex1", "tm-pro3"), Config{
		Aliases: map[string]AliasConfig{},
		CatchAll: &AliasConfig{
			Alias:    "*",
			Strategy: "priority",
			Providers: []ProviderRoute{
				{Name: "tm-codex1", Model: "gpt-5.4", Priority: 1},
				{Name: "tm-pro3", Model: "gpt-5.4", Priority: 2},
			},
		},
	})

	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-terra", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// tm-codex1 没声明 terra → 应被筛掉,只剩 tm-pro3
	if res.ProviderName != "tm-codex1" && res.ProviderName != "tm-pro3" {
		t.Fatalf("unexpected provider %s", res.ProviderName)
	}
	if res.ProviderName == "tm-codex1" {
		t.Errorf("tm-codex1 没声明 gpt-5.6-terra,不该进候选(显式 alias 路径漏了模型过滤)")
	}
	// 写死的 model=gpt-5.4 必须被客户端模型名覆盖
	if res.ModelID != "gpt-5.6-terra" {
		t.Errorf("ModelID = %q, want gpt-5.6-terra(中转站的 model 字段以客户端为准)", res.ModelID)
	}
}

// TestRelay_ModelFirst_NormalVendorUnchanged 回归守卫:模型优先只对中转站生效,
// 普通厂商的 default_model + 白名单选择逻辑一行不变。
// 普通厂商在 catch_all 下客户端模型名只是标签(不参与路由决策),该厂商没声明
// 客户端要的模型时仍用自己的 default_model 承接 —— 不能被误筛掉。
func TestRelay_ModelFirst_NormalVendorUnchanged(t *testing.T) {
	// 注意用 newFakeManager(非 relay 注册)
	mgr := newFakeManager(t,
		&fakeProvider{name: "deepseek", proto: provider.ProtocolOpenAI,
			models: []string{"deepseek-v4-flash"}},
	)
	r := NewRouter(zap.NewNop(), mgr, relayPools("deepseek"),
		Config{Aliases: map[string]AliasConfig{}, CatchAll: &AliasConfig{Alias: "*"}})

	// 客户端要一个 deepseek 完全没声明的模型
	it, err := r.Route(context.Background(),
		&provider.Request{Model: "gpt-5.6-terra", Path: "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("普通厂商不该被模型优先筛掉: %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.ProviderName != "deepseek" {
		t.Errorf("provider = %s, want deepseek", res.ProviderName)
	}
	// 普通厂商用自己的 default_model,不透传客户端模型名
	if res.ModelID != "deepseek-v4-flash" {
		t.Errorf("ModelID = %q, want deepseek-v4-flash(普通厂商用 default_model,不透传)",
			res.ModelID)
	}
}

// TestRelayServesModel_Unit relayServesModel 的直接单测(单源 helper,两处调用点共用)
func TestRelayServesModel_Unit(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		model  string
		want   bool
	}{
		{"空清单当通配", nil, "anything", true},
		{"空切片当通配", []string{}, "anything", true},
		{"命中", []string{"a", "b"}, "b", true},
		{"未命中", []string{"a", "b"}, "c", false},
		{"大小写不敏感", []string{"GPT-5.6-Sol"}, "gpt-5.6-sol", true},
		{"空模型名未命中非空清单", []string{"a"}, "", false},
	}
	for _, c := range cases {
		if got := relayServesModel(c.models, c.model); got != c.want {
			t.Errorf("%s: relayServesModel(%v, %q) = %v, want %v",
				c.name, c.models, c.model, got, c.want)
		}
	}
}
