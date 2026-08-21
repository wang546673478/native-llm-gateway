// Package rightapi — gemini 渠道（OpenAI 协议面，注册名 "rightapi-gemini"）。
//
// 端点 https://rightapi.ai/gemini/v1（已含 /v1），Right Code 把 Google Gemini
// 转成 OpenAI 兼容的 chat/completions：
//   - /chat/completions → chat 面（POST，模型如 gemini-3.1-pro / gemini-3.5-flash）
//   - /models            → 模型列表（标准 OpenAI {data:[{id}]}）
//
// 与 codex 面的差别：gemini 渠道没有原生 Responses API，只走 chat/completions
// （responses_api 不标）。鉴权走 Authorization: Bearer <key>（中转站上层已收敛）。
package rightapi

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

// 端点已含 /v1 → path 都用不带 /v1 的相对路径（避免拼出 /v1/v1/...，见踩坑 #23/mimo 先例）
const (
	geminiChatPath   = "/chat/completions"
	geminiModelsPath = "/models"
)

// geminiProvider OpenAI 协议 Provider（注册名 "rightapi-gemini"）。
type geminiProvider struct {
	base *openai_compatible.Base
}

func newGemini(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("rightapi-gemini requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("rightapi-gemini endpoint is required")
	}
	return &geminiProvider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     geminiName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			ChatPath: geminiChatPath, // 端点已含 /v1
			// 不设 ResponsesPath：gemini 渠道无原生 Responses API，只走 chat/completions
			ModelsPath:    geminiModelsPath, // 端点已含 /v1 → /models
			StreamUsage:   true,
			BillingSource: cfg.BillingSource,
			Pool:          cfg.Pool,
		}),
	}, nil
}

func (p *geminiProvider) Name() string                { return geminiName }
func (p *geminiProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *geminiProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *geminiProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *geminiProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *geminiProvider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

func (p *geminiProvider) SetPool(pool *keypool.Pool) { p.base.SetPool(pool) }

func (p *geminiProvider) Close() error { return p.base.Close() }
