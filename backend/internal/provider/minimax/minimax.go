// Package minimax 实现 Anthropic 兼容的 Provider
//
// 规格书里叫 "MiniMax" 是占位符名,实际可指向任何 Anthropic Messages 兼容的端点:
// - Anthropic Claude API:https://api.anthropic.com
// - 其他走 Anthropic 兼容协议的 vendor(比如部分国内厂商)
// 鉴权:x-api-key header + anthropic-version: 2023-06-01
// 端点:POST /v1/messages
//
// 包名沿用 minimax 是因为规格书定义如此,代码里把它当作"通用 Anthropic 兼容 Provider"
// 真实部署时把 config.endpoint 改成目标 vendor 的 base URL 即可
package minimax

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name           = "minimax"
	DefaultEndpoint = "https://api.anthropic.com"
	ChatPath       = "/v1/messages"
)

// DefaultModels 通用 Anthropic 模型
// 实际可用模型取决于指向的 vendor
var DefaultModels = []string{
	"claude-sonnet-4-5",
	"claude-3-5-sonnet-20241022",
	"claude-3-5-haiku-20241022",
	"claude-3-opus-20240229",
}

type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("minimax requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("minimax endpoint is required")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("minimax requires at least one API key")
	}
	pool, ok := cfg.Pool.(*keypool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("minimax requires a non-nil keypool.Pool (got %T)", cfg.Pool)
	}
	return &Provider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }
func (p *Provider) Models() []string            { return p.cfg.Models }
func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }
func (p *Provider) Close() error                          { return p.base.Close() }

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolAnthropic) }
