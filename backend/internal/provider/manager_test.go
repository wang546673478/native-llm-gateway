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
func (p *fakeProviderForEndpoint) SetPool(*keypool.Pool)                 {}
func (p *fakeProviderForEndpoint) Close() error                          { return nil }

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
