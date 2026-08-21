// Package provider — Manager 单元测试
package provider

import (
	"context"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"go.uber.org/zap"
)

// fakeProviderForEndpoint 最小 Provider 实现(EndpointFor 测试用)
// 构造方式照抄 router_test.go 的 fakeProvider:注册到独立 Registry,
// 经 LoadFromConfig 加载
type fakeProviderForEndpoint struct {
	name string
}

func (p *fakeProviderForEndpoint) Name() string { return p.name }
func (p *fakeProviderForEndpoint) Protocol() Protocol {
	return ProtocolOpenAI
}
func (p *fakeProviderForEndpoint) SendRequest(ctx context.Context, req *Request) (*Response, error) {
	return nil, nil
}
func (p *fakeProviderForEndpoint) SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error) {
	return nil, nil, nil
}
func (p *fakeProviderForEndpoint) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeProviderForEndpoint) ListModels(ctx context.Context) ([]string, error) {
	return nil, nil
}
func (p *fakeProviderForEndpoint) SetPool(*keypool.Pool) {}
func (p *fakeProviderForEndpoint) Close() error          { return nil }

// TestManager_EndpointFor 查 provider 的 endpoint(baseURL,给 quotacheck.CheckQuota 用)
// 注册的 provider 返回其配置的 endpoint;未注册返回空串
func TestManager_EndpointFor(t *testing.T) {
	reg := NewRegistry()
	reg.Register("fake", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "fake"}, nil
	})
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"fake": {
				Enabled:  true,
				Endpoint: "https://fake.example/v1",
				Protocol: ProtocolOpenAI,
			},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}

	if got := mgr.EndpointFor("fake"); got != "https://fake.example/v1" {
		t.Errorf("EndpointFor(fake) = %q, want %q", got, "https://fake.example/v1")
	}
	if got := mgr.EndpointFor("nope"); got != "" {
		t.Errorf("EndpointFor(nope) = %q, want empty string", got)
	}
}

// fakeModelStore 最小 ModelStore 实现(LoadModelsFromStore 测试用)
type fakeModelStore struct {
	rows     []DBModelRow
	faceRows []DBFaceRow
}

func (s fakeModelStore) All(ctx context.Context) ([]DBModelRow, error) {
	return s.rows, nil
}
func (s fakeModelStore) ListByVendor(ctx context.Context, vendor string) ([]DBModelRow, error) {
	var out []DBModelRow
	for _, r := range s.rows {
		if r.Vendor == vendor {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s fakeModelStore) AllFaces(ctx context.Context) ([]DBFaceRow, error) {
	return s.faceRows, nil
}

// TestManager_LoadModelsFromStore 从 DB 读模型,CostFor/DefaultModelFor 按 vendor 归位。
// 关键:fake store 喂 vendor=minimax 的行,CostFor 传注册面名 "minimax-openai"
// 必须经 VendorFor 归到 "minimax" 后命中 —— 否则计费全 0(编译过但运行错)。
func TestManager_LoadModelsFromStore(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterWithProtocolVendor("minimax-openai", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "minimax-openai"}, nil
	}, ProtocolOpenAI, "minimax")
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"minimax-openai": {
				Enabled:  true,
				Endpoint: "https://minimax.example/v1",
				Protocol: ProtocolOpenAI,
			},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}

	store := fakeModelStore{rows: []DBModelRow{
		{Vendor: "minimax", ModelID: "MiniMax-M3", CostPerMillionInput: 0.28, CostPerMillionCacheRead: 0, CostPerMillionOutput: 1.10},
	}}
	if err := mgr.LoadModelsFromStore(context.Background(), store); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}

	// CostFor 归位:注册面 "minimax-openai" → vendor "minimax" → 命中
	want := ModelCost{CostPerMillionInput: 0.28, CostPerMillionCacheRead: 0, CostPerMillionOutput: 1.10}
	if got := mgr.CostFor("minimax-openai", "MiniMax-M3"); got != want {
		t.Errorf("CostFor(minimax-openai, MiniMax-M3) = %+v, want %+v", got, want)
	}

	// DefaultModelFor 按 vendor 归位到首个 model
	if got := mgr.DefaultModelFor("minimax-openai"); got != "MiniMax-M3" {
		t.Errorf("DefaultModelFor(minimax-openai) = %q, want MiniMax-M3", got)
	}
}

// TestManager_LoadModelsFromStore_Empty 空 DB 兜底:不 panic,CastFor 返回零值。
func TestManager_LoadModelsFromStore_Empty(t *testing.T) {
	reg := NewRegistry()
	reg.Register("fake", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "fake"}, nil
	})
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"fake": {Enabled: true, Endpoint: "https://fake.example/v1", Protocol: ProtocolOpenAI},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	if err := mgr.LoadModelsFromStore(context.Background(), fakeModelStore{}); err != nil {
		t.Fatalf("LoadModelsFromStore(empty): %v", err)
	}
	if got := mgr.CostFor("fake", "m1"); got != (ModelCost{}) {
		t.Errorf("CostFor(fake, m1) = %+v, want zero ModelCost", got)
	}
	if got := mgr.DefaultModelFor("fake"); got != "" {
		t.Errorf("DefaultModelFor(fake) = %q, want empty", got)
	}
}

// TestManager_ModelsFor_SharedAcrossProtocolFaces P-model-face fallback 守卫:
// 该面**无归属行**时退回 vendor 级全量清单 —— ModelsFor("minimax") 与
// ModelsFor("minimax-openai") 返回同一清单。
//
// 这条 fallback 覆盖两种正当情形,是整个 face 改动的安全网:
//   - 迁移后尚未同步过的厂商(provider_model_faces 空)→ 否则候选全空 → 全部 503
//   - anthropic 面这类无模型列表端点的面 → 它与 openai 面确实共享同一批模型
//
// (原名保留:2026-08-21 前它守的是「vendor 是模型归属唯一维度」的方案 A 前提;
// 现在归属维度已下沉到面,本测试守的是无归属时的 vendor 级回退。)
func TestManager_ModelsFor_SharedAcrossProtocolFaces(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterWithProtocolVendor("minimax", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "minimax"}, nil
	}, ProtocolAnthropic, "minimax")
	reg.RegisterWithProtocolVendor("minimax-openai", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "minimax-openai"}, nil
	}, ProtocolOpenAI, "minimax")
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"minimax":        {Enabled: true, Endpoint: "https://minimax.example/anthropic", Protocol: ProtocolAnthropic},
			"minimax-openai": {Enabled: true, Endpoint: "https://minimax.example/v1", Protocol: ProtocolOpenAI},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	// 喂 vendor=minimax 的 DB 行,faceRows 为空(未同步过面归属)
	_ = mgr.LoadModelsFromStore(context.Background(), fakeModelStore{rows: []DBModelRow{
		{Vendor: "minimax", ModelID: "MiniMax-M3"},
		{Vendor: "minimax", ModelID: "MiniMax-M2.5"},
	}})

	want := []string{"MiniMax-M2.5", "MiniMax-M3"} // 字典序
	if got := mgr.ModelsFor("minimax"); !equalStrings(got, want) {
		t.Errorf("ModelsFor(minimax) = %v, want %v", got, want)
	}
	if got := mgr.ModelsFor("minimax-openai"); !equalStrings(got, want) {
		t.Errorf("ModelsFor(minimax-openai) = %v, want %v(无归属行 → vendor 级回退)", got, want)
	}
}

// newRightapiLikeManager 构造一个中转站形态的 Manager:同 vendor 三个面,
// 其中两个同协议(openai)但 endpoint 不同、模型互斥 —— 这正是协议不足以
// 作为归属维度的原因,必须用注册面名。
func newRightapiLikeManager(t *testing.T) *Manager {
	t.Helper()
	reg := NewRegistry()
	reg.RegisterWithProtocolVendor("rightapi-codex", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "rightapi-codex"}, nil
	}, ProtocolOpenAI, "rightapi")
	reg.RegisterWithProtocolVendor("rightapi-grok", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "rightapi-grok"}, nil
	}, ProtocolOpenAI, "rightapi")
	reg.RegisterWithProtocolVendor("rightapi-claude", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "rightapi-claude"}, nil
	}, ProtocolAnthropic, "rightapi")
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"rightapi-codex":  {Enabled: true, Endpoint: "https://rightapi.ai/codex/v1", Protocol: ProtocolOpenAI},
			"rightapi-grok":   {Enabled: true, Endpoint: "https://rightapi.ai/grok/v1", Protocol: ProtocolOpenAI},
			"rightapi-claude": {Enabled: true, Endpoint: "https://rightapi.ai/claude-aws", Protocol: ProtocolAnthropic},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return mgr
}

// rightapiStore vendor 级合并清单(定价表)+ 三面各自归属。
// sort_order 模拟各面上游 ListModels 的真实返回下标。
func rightapiStore() fakeModelStore {
	return fakeModelStore{
		rows: []DBModelRow{
			// vendor 级合并清单,首行是 claude-*(claude 面排在 Names() 最前)
			{Vendor: "rightapi", ModelID: "claude-haiku-4-5"},
			{Vendor: "rightapi", ModelID: "claude-opus-5"},
			{Vendor: "rightapi", ModelID: "grok-4.5"},
			{Vendor: "rightapi", ModelID: "gpt-5.4"},
		},
		faceRows: []DBFaceRow{
			{Vendor: "rightapi", Face: "rightapi-claude", ModelID: "claude-haiku-4-5", SortOrder: 0},
			{Vendor: "rightapi", Face: "rightapi-claude", ModelID: "claude-opus-5", SortOrder: 1},
			{Vendor: "rightapi", Face: "rightapi-codex", ModelID: "gpt-5.4", SortOrder: 0},
			{Vendor: "rightapi", Face: "rightapi-grok", ModelID: "grok-4.5", SortOrder: 0},
		},
	}
}

// TestModelsFor_FaceIsolation P-model-face 核心守卫:有归属行时,面之间的模型互相隔离。
// 回归形态(2026-08-21 修的根因):按 vendor 合并会让 codex 面拿到 claude 模型,
// 发给 /codex/v1 上游 → 404 model not found(实测),该面永远轮不到干活。
func TestModelsFor_FaceIsolation(t *testing.T) {
	mgr := newRightapiLikeManager(t)
	if err := mgr.LoadModelsFromStore(context.Background(), rightapiStore()); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}
	cases := []struct {
		face string
		want []string
	}{
		{"rightapi-codex", []string{"gpt-5.4"}},
		{"rightapi-grok", []string{"grok-4.5"}},
		{"rightapi-claude", []string{"claude-haiku-4-5", "claude-opus-5"}},
	}
	for _, tc := range cases {
		if got := mgr.ModelsFor(tc.face); !equalStrings(got, tc.want) {
			t.Errorf("ModelsFor(%s) = %v, want %v(面间必须隔离)", tc.face, got, tc.want)
		}
	}
}

// TestDefaultModelFor_PerFaceOrdering P-model-face:默认模型按**面内** sort_order 取,
// 不是 vendor 级首行。回归形态:vendor 首行是 claude-haiku,会被发给两个 openai 面。
func TestDefaultModelFor_PerFaceOrdering(t *testing.T) {
	mgr := newRightapiLikeManager(t)
	if err := mgr.LoadModelsFromStore(context.Background(), rightapiStore()); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}
	cases := map[string]string{
		"rightapi-codex":  "gpt-5.4",
		"rightapi-grok":   "grok-4.5",
		"rightapi-claude": "claude-haiku-4-5",
	}
	for face, want := range cases {
		if got := mgr.DefaultModelFor(face); got != want {
			t.Errorf("DefaultModelFor(%s) = %q, want %q(面内首个,非 vendor 级首行)", face, got, want)
		}
	}
}

// TestModelsFor_FallbackWhenFaceHasNoRows P-model-face 核心不变式:
// 同 vendor 下**部分**面有归属时,无归属的那个面仍退回 vendor 级全量 ——
// 判定粒度是「面」而不是「vendor」。
//
// 为什么必须按面判定:deepseek 的 openai 面能拉模型列表(有归属行)、anthropic 面
// 返回 NotSupported(无归属行)。若按 vendor 判定「有任何归属行就不 fallback」,
// deepseek-anthropic 会查不到任何模型 → 该面失去全部候选(回归)。
func TestModelsFor_FallbackWhenFaceHasNoRows(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterWithProtocolVendor("deepseek", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "deepseek"}, nil
	}, ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("deepseek-anthropic", func(cfg ProviderConfig) (Provider, error) {
		return &fakeProviderForEndpoint{name: "deepseek-anthropic"}, nil
	}, ProtocolAnthropic, "deepseek")
	mgr := NewManager(reg, zap.NewNop())
	cfg := &ManagerConfig{
		Providers: map[string]ManagerProviderConfig{
			"deepseek":           {Enabled: true, Endpoint: "https://api.deepseek.com", Protocol: ProtocolOpenAI},
			"deepseek-anthropic": {Enabled: true, Endpoint: "https://api.deepseek.com/anthropic", Protocol: ProtocolAnthropic},
		},
	}
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	// openai 面有归属;anthropic 面无(ListModels NotSupported)
	_ = mgr.LoadModelsFromStore(context.Background(), fakeModelStore{
		rows: []DBModelRow{
			{Vendor: "deepseek", ModelID: "deepseek-v4-flash"},
			{Vendor: "deepseek", ModelID: "deepseek-v4-pro"},
		},
		faceRows: []DBFaceRow{
			{Vendor: "deepseek", Face: "deepseek", ModelID: "deepseek-v4-flash", SortOrder: 0},
			{Vendor: "deepseek", Face: "deepseek", ModelID: "deepseek-v4-pro", SortOrder: 1},
		},
	})

	want := []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	if got := mgr.ModelsFor("deepseek"); !equalStrings(got, want) {
		t.Errorf("ModelsFor(deepseek) = %v, want %v", got, want)
	}
	if got := mgr.ModelsFor("deepseek-anthropic"); !equalStrings(got, want) {
		t.Errorf("ModelsFor(deepseek-anthropic) = %v, want %v(无归属 → vendor 级回退,不能为空)", got, want)
	}
	// 默认模型同样回退到 vendor 级首行(sort_order 0)
	if got := mgr.DefaultModelFor("deepseek-anthropic"); got != "deepseek-v4-flash" {
		t.Errorf("DefaultModelFor(deepseek-anthropic) = %q, want deepseek-v4-flash(vendor 级回退)", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
