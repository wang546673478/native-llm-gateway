// Package deepseek 实现 DeepSeek Provider
//
// 基于官方文档 https://api-docs.deepseek.com 重写:
//
// 关键差异(与标准 OpenAI Chat Completions):
//   1. 端点路径是 /chat/completions(没有 /v1 前缀!)
//   2. 支持 thinking 模式:"thinking": {"type": "enabled"}
//   3. 启用 thinking 后,响应中增加 reasoning_content 字段
//   4. usage 增加 prompt_cache_hit_tokens / prompt_cache_miss_tokens
//      和 completion_tokens_details.reasoning_tokens
//   5. 模型:deepseek-v4-flash / deepseek-v4-pro
//      (deepseek-chat / deepseek-reasoner 于 2026/07/24 弃用)
//
// 实现策略:继承 openai_compatible.Base,通过 Config.ChatPath 覆盖端点,
// 启用 StreamUsage 让流式响应末尾带 usage。
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

// 默认模型列表(2026-07 最新版)
// 注:deepseek-chat / deepseek-reasoner 已于 2026/07/24 弃用,Gateway
// 不再默认导出;老用户配置仍可用,但建议尽快迁移到 v4
var DefaultModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
}

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
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("deepseek requires at least one API key")
	}
	pool, ok := cfg.Pool.(*keypool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("deepseek requires a non-nil keypool.Pool (got %T)", cfg.Pool)
	}

	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:       name,
			Endpoint:   cfg.Endpoint,
			Timeout:    cfg.Timeout,
			Pool:       pool,
			ChatPath:   ChatPath, // DeepSeek 关键差异:无 /v1 前缀
			StreamUsage: true,    // 让流式末尾带 usage,便于 Gateway 端记账
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

// Models 返回 cfg 里配置的模型;若为空,返回 DeepSeek v4 默认列表
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

func (p *Provider) Close() error { return p.base.Close() }

// init 自动注册到 Registry
func init() {
	provider.RegisterGlobal(name, New)
}
