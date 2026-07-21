// Package deepseek_anthropic 实现 DeepSeek 的 Anthropic 兼容端点
//
// 官方文档 https://api-docs.deepseek.com:
//   base URL: https://api.deepseek.com/anthropic
//   鉴权:     x-api-key: {DEEPSEEK_API_KEY}
//             anthropic-version: 2023-06-01
//   端点:     POST /v1/messages
//
// 与标准 MiniMax 不同的是 DeepSeek 这里用 /anthropic 子路径作为 base URL,
// 所以 endpoint 字段填 https://api.deepseek.com/anthropic,实际请求会拼成
// https://api.deepseek.com/anthropic/v1/messages
//
// 用 anthropic_compatible.Base 实现,共享 SSE 解析、Usage 解析等逻辑。
package deepseek_anthropic

import (
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"context"
	"fmt"

		"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name = "deepseek-anthropic"
	// DefaultEndpoint DeepSeek 官方 Anthropic 兼容 base URL
	DefaultEndpoint = "https://api.deepseek.com/anthropic"
)

// DefaultModels DeepSeek 在 Anthropic 模式下支持的模型
var DefaultModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
}

// Provider DeepSeek Anthropic 兼容 Provider
type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("deepseek-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek-anthropic endpoint is required")
	}
	return &Provider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (p *Provider) Models() []string {
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models
	}
	return DefaultModels
}

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolAnthropic) }
