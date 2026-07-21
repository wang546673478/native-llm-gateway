// Package minimax 实现 MiniMax(MiniMax 稀宇科技)Provider
//
// 官方文档:https://platform.minimaxi.com/docs/api-reference/api-overview
//
// MiniMax 提供两种 API 端点:
//   1. Anthropic 兼容(推荐):POST https://api.minimaxi.com/anthropic/v1/messages
//   2. OpenAI 兼容:POST https://api.minimaxi.com/v1/chat/completions
//
// 鉴权:Authorization: Bearer <API_KEY>
// Anthropic 兼容时还需要 anthropic-version header(anthropic_compatible.Base 已加)
//
// 当前可用模型(2026-07):
//   - MiniMax-M3           (1M tokens,旗舰)
//   - MiniMax-M2.7 / M2.7-highspeed
//   - MiniMax-M2.5 / M2.5-highspeed
//   - MiniMax-M2.1 / M2.1-highspeed
//   - MiniMax-M2
//
// M3 专属参数(通过 extra_body 传):
//   - thinking: {"type": "adaptive"|"disabled"}  (M2.x 不可关闭)
//   - reasoning_split: true 把思考内容分到 reasoning_details 字段
//   - service_tier: "standard"|"priority"       (priority 1.5x 价格,优先准入)
//
// 这里采用 Anthropic 兼容协议(官方推荐);若需 OpenAI 兼容可新建 minimax-openai 包。
package minimax

import (
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"context"
	"fmt"

		"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name           = "minimax"
	DefaultEndpoint = "https://api.minimaxi.com/anthropic"
	ChatPath       = "/v1/messages"
)

// DefaultModels MiniMax 当前可用模型(2026-07)
// 完整列表见 https://platform.minimaxi.com/docs/api-reference/api-overview
var DefaultModels = []string{
	"MiniMax-M3",             // 1M tokens,旗舰
	"MiniMax-M2.7",           // 204,800
	"MiniMax-M2.7-highspeed", // 204,800,更便宜更快
	"MiniMax-M2.5",           // 204,800
	"MiniMax-M2.5-highspeed", // 204,800
	"MiniMax-M2.1",           // 204,800(早期稳定版)
	"MiniMax-M2.1-highspeed",
	"MiniMax-M2",             // 204,800(基础)
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

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolAnthropic) }
