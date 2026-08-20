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
func (p *fakeProviderForEndpoint) Models() []string { return []string{"m1"} }
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
				Models:   []string{"m1"},
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
	rows []DBModelRow
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
				Models:   []string{"MiniMax-M3"},
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
			"fake": {Enabled: true, Endpoint: "https://fake.example/v1", Protocol: ProtocolOpenAI, Models: []string{"m1"}},
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
