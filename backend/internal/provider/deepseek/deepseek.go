// Package deepseek 实现 DeepSeek 的 OpenAI 与 Anthropic 协议面。
// OpenAI 面使用 /chat/completions 和 /v1/responses；Anthropic 面实现在
// anthropic.go。两个注册面归入 deepseek vendor 并共享 Key Pool。
// 模型清单和价格由数据库维护，运行契约见 docs/providers.md。
package deepseek

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name = "deepseek"
	// DefaultEndpoint DeepSeek 官方 base URL
	DefaultEndpoint = "https://api.deepseek.com"
	// ChatPath DeepSeek 用 /chat/completions,不是 /v1/chat/completions
	ChatPath = "/chat/completions"
)

// Provider DeepSeek Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("deepseek requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek endpoint is required")
	}

	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			ChatPath: ChatPath, // DeepSeek 关键差异:无 /v1 前缀
			// P-responses: endpoint 无 /v1 → 透传 /v1/responses(官方原生支持,Codex 兼容)
			ResponsesPath: "/v1/responses",
			StreamUsage:   true,              // 让流式末尾带 usage,便于 Gateway 端记账
			BillingSource: cfg.BillingSource, // 按本面计费源取 key
			Pool:          cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

// init 自动注册到 Registry
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name)
}
