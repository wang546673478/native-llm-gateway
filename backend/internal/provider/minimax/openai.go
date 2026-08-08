// Package minimax 实现 MiniMax Provider(Anthropic + OpenAI 两种协议)
//
// P-provider-vendor: 本包同时承载两个注册名:
//   - "minimax"        → New(Anthropic 协议,minimax.go)
//   - "minimax-openai" → NewOpenAI(OpenAI 兼容,本文件)
//
// OpenAI 兼容端点(官方文档 https://platform.minimaxi.com/docs/api-reference/text-openai-api):
//   - base URL: https://api.minimaxi.com/v1(国内站;国际站 api.minimax.io)
//   - 端点:POST /chat/completions(OpenAI 标准)
//   - 鉴权:Authorization: Bearer <API_KEY>
//
// 官方文档特性(2026-08-04 全量调研,与 Anthropic 面不同处):
//   - thinking:默认开启 adaptive(M3);省略 = adaptive;M2.x 无法关闭
//   - reasoning_split(extra_body):true → thinking 拆到 message.reasoning_content +
//     reasoning_details[];false/缺省 → thinking 以 <think>...</think> 标签内嵌在 content 里,
//     多轮必须原样回传完整 content,否则思维链断裂
//   - service_tier: "standard"(默认) | "priority"(1.5x 价);OpenAI SDK 需 extra_body
//   - max_tokens 已废弃,用 max_completion_tokens;n 仅支持 1;
//     presence_penalty / frequency_penalty / logit_bias 忽略
//   - usage:prompt_tokens_details.cached_tokens(自动缓存命中,按缓存价计费);
//     流式 usage 默认 null,需 stream_options.include_usage=true(网关已默认注入)
//   - 缓存:自动 Prompt 缓存(≥512 tokens,服务端自动,支持 M3/M2.7/M2.5/M2.1);
//     M3 输入 >512k tokens 时按长上下文价计费(含缓存命中 tokens)
package minimax

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	openaiName     = "minimax-openai"
	openaiChatPath = "/chat/completions" // base 已含 /v1,不要再拼
	// P-responses: endpoint 已含 /v1(https://api.minimaxi.com/v1)→ 透传 /responses
	// 不拼 /v1,否则双前缀。MiniMax 官方原生支持 Responses API(Codex)
	openaiResponsesPath = "/responses"
)

// OpenAIProvider OpenAI 兼容 Provider
// 命名加 OpenAI 前缀:与 minimax.go 的 Provider(Anthropic 面)区分,Go 包内类型名不能重复
type OpenAIProvider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// NewOpenAI OpenAI 协议工厂函数
func NewOpenAI(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("minimax-openai requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("minimax-openai endpoint is required")
	}
	return &OpenAIProvider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:          openaiName,
			Endpoint:      cfg.Endpoint,
			Timeout:       cfg.Timeout,
			ChatPath:      openaiChatPath,
			ResponsesPath: openaiResponsesPath, // P-responses
			StreamUsage:   true,                // MiniMax 支持 stream_options.include_usage
			Pool:          provider.ToPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *OpenAIProvider) Name() string                { return openaiName }
func (p *OpenAIProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *OpenAIProvider) Models() []string {
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models
	}
	return DefaultModels // 与 anthropic 面共用(同包变量,内容相同)
}

func (p *OpenAIProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *OpenAIProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}
func (p *OpenAIProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *OpenAIProvider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *OpenAIProvider) Close() error { return p.base.Close() }
