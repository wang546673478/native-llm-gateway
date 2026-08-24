// Package openai_compatible - Generic Provider
// 通用 OpenAI 兼容 Provider,用于动态配置的第三方服务(如 TokenMarket 等中转站)
package openai_compatible

import (
	"context"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func init() {
	// 注册通用 OpenAI 兼容 factory,供 registry.createCompatible 使用
	provider.RegisterGlobalWithProtocolVendor("__generic_openai__", NewGeneric, provider.ProtocolOpenAI, "")
}

// Generic 是通用的 OpenAI 兼容 Provider 包装器
type Generic struct {
	base *Base
	name string
}

// NewGeneric 创建通用 OpenAI 兼容 Provider
func NewGeneric(cfg provider.ProviderConfig) (provider.Provider, error) {
	base := NewBase(Config{
		Name:          cfg.Name,
		Endpoint:      cfg.Endpoint,
		Timeout:       cfg.Timeout,
		Pool:          cfg.Pool,
		BillingSource: cfg.BillingSource,
	})
	return &Generic{
		base: base,
		name: cfg.Name,
	}, nil
}

func (p *Generic) Name() string                  { return p.name }
func (p *Generic) Protocol() provider.Protocol   { return provider.ProtocolOpenAI }
func (p *Generic) SetPool(pool *keypool.Pool)    { p.base.SetPool(pool) }
func (p *Generic) Close() error                  { return p.base.Close() }

func (p *Generic) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *Generic) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *Generic) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

func (p *Generic) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}
