package router

import (
	"context"
	"testing"

	"go.uber.org/zap"

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
