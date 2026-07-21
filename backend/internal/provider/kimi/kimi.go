// Package kimi 实现 Moonshot Kimi Provider
// Kimi 兼容 OpenAI Chat Completions,支持 128K 上下文
// 官方文档:https://platform.moonshot.cn/docs/intro
package kimi

import (
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"context"
	"fmt"

		"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name           = "kimi"
	DefaultEndpoint = "https://api.moonshot.cn"
	ChatPath       = "/v1/chat/completions"
)

// DefaultModels Kimi 当前在用模型(2026-07)
// 完整列表见 https://platform.kimi.ai/docs/models
// 注意:
//   - moonshot-v1-* 系列已不对新用户开放,仅老用户可用
//   - kimi-k2-0905-preview / kimi-latest / kimi-thinking-preview 全部已弃用
//   - 当前主推 kimi-k3 / kimi-k2.7-code / kimi-k2.6 / kimi-k2.5
var DefaultModels = []string{
	"kimi-k3",                  // 当前最新
	"kimi-k2.7-code",           // 代码专用(2.7)
	"kimi-k2.7-code-highspeed", // 代码高速版
	"kimi-k2.6",
	"kimi-k2.5",
}

type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("kimi requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("kimi endpoint is required")
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool: toPool(cfg.Pool),
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
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }
// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error                          { return p.base.Close() }

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolOpenAI) }


// toPool 把 cfg.Pool (interface{}) 安全转成 *keypool.Pool
func toPool(p interface{}) *keypool.Pool {
	if p == nil {
		return nil
	}
	if pp, ok := p.(*keypool.Pool); ok {
		return pp
	}
	return nil
}
