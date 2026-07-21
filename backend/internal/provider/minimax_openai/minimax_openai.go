// Package minimax_openai 实现 MiniMax 的 OpenAI 兼容端点
//
// 官方文档:https://platform.minimaxi.com/docs/api-reference/text-openai-api
//
// 端点(2026-07):
//   POST https://api.minimaxi.com/v1/chat/completions
//   鉴权:Authorization: Bearer <API_KEY>  (OpenAI 标准)
//
// 与 minimax (Anthropic 兼容) 共享模型列表:
//   - MiniMax-M3           (1M tokens,旗舰)
//   - MiniMax-M2.7 / M2.7-highspeed
//   - MiniMax-M2.5 / M2.5-highspeed
//   - MiniMax-M2.1 / M2.1-highspeed
//   - MiniMax-M2
//
// M3 专属参数(在请求 body 的 extra_body 字段下,不是顶层):
//   - thinking:         {"type": "adaptive"|"disabled"}  (M2.x 不可关闭)
//   - reasoning_split:  true → 思考走 reasoning_content / reasoning_details
//                       false → 思考嵌在 content 里的 <think>...</think> 标签
//   - service_tier:     "standard"(默认) | "priority" (1.5x 价格,优先准入)
//
// 不同温度:
//   - temperature: [0, 2],默认 1,推荐 1.0
//   - top_p:       [0, 1],M3 默认 0.95,M2.x 默认 0.9
//   - 不支持: presence_penalty / frequency_penalty / logit_bias / n>1
//
// 流式:支持 stream_options.include_usage=true
package minimax_openai

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name            = "minimax-openai"
	DefaultEndpoint = "https://api.minimaxi.com/v1"
	ChatPath        = "/chat/completions" // OpenAI 兼容端点带 /v1 前缀的 base 后面接 /chat/completions
	// 注意:base URL 已经是 https://api.minimaxi.com/v1,这里 ChatPath 不要带 /v1
	// 否则会拼成 https://api.minimaxi.com/v1/v1/chat/completions
)

// DefaultModels MiniMax 当前可用模型(2026-07)
// 与 minimax(anthropic)共享
var DefaultModels = []string{
	"MiniMax-M3",
	"MiniMax-M2.7",
	"MiniMax-M2.7-highspeed",
	"MiniMax-M2.5",
	"MiniMax-M2.5-highspeed",
	"MiniMax-M2.1",
	"MiniMax-M2.1-highspeed",
	"MiniMax-M2",
}

// Provider 包装 openai_compatible.Base
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("minimax-openai requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("minimax-openai endpoint is required")
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:      name,
			Endpoint:  cfg.Endpoint,
			Timeout:   cfg.Timeout,
			ChatPath:  ChatPath, // /chat/completions(无 /v1 前缀,因为 base 已含)
			Pool:      toPool(cfg.Pool),
			StreamUsage: true,  // MiniMax 支持 stream_options.include_usage
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                 { return name }
func (p *Provider) Protocol() provider.Protocol  { return provider.ProtocolOpenAI }
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
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

// init 自动注册到 Registry
func init() {
	provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolOpenAI)
}

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