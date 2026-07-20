// Package glm 实现智谱 GLM Provider
// GLM 兼容 OpenAI Chat Completions
// 官方文档:https://open.bigmodel.cn/dev/api
package glm

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name           = "glm"
	DefaultEndpoint = "https://open.bigmodel.cn/api/paas/v4"
	ChatPath       = "/chat/completions"
)

// DefaultModels 智谱 GLM 商用 Stable 模型(2026-07)
// 完整列表见 https://open.bigmodel.cn/dev/api
// 注意:"glm-4.6" / "glm-4.7" 这种命名不存在!
// 智谱实际命名:glm-4-flash(免费)/ glm-4-air / glm-4-airx / glm-4-long / glm-4-plus / glm-4-flashx
// GLM-4.5-Flash 已下线(2026-01-30),自动路由到 GLM-4.7-Flash(后者 2026-01-20 开源)
var DefaultModels = []string{
	"glm-4-flash",  // 免费,稳定,适合大多数场景
	"glm-4.7-flash", // 最新免费(替代 glm-4.5-flash)
	"glm-4-flashx", // 高速版
	"glm-4-air",    // 轻量
	"glm-4-airx",   // 增强轻量
	"glm-4-long",   // 长上下文
	"glm-4-plus",   // 高级版
	"glm-4",        // 基础
}

// Provider GLM Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("glm requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("glm endpoint is required")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("glm requires at least one API key")
	}
	pool, ok := cfg.Pool.(*keypool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("glm requires a non-nil keypool.Pool (got %T)", cfg.Pool)
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *Provider) Models() []string            { return p.cfg.Models }

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

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolOpenAI) }
