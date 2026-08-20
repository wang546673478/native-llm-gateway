// Package rightapi — codex 渠道（OpenAI 协议面，注册名 "rightapi-codex"）。
//
// 端点 https://rightapi.ai/codex/v1（已含 /v1）：
//   - /responses          → Responses API（Codex CLI 用，wire_api="responses"）
//   - /chat/completions   → OpenAI Chat Completions（由 responses 反转而来，
//     文档提示：不支持缓存、system prompt 会被替换为 codex 默认 instructions）
//   - /models             → 模型列表（返回标准 OpenAI {data:[{id}]}，含 codex 系列 gpt-* 模型）
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
	codexChatPath      = "/chat/completions"
	codexResponsesPath = "/responses"
	codexModelsPath    = "/models"
)

// codexProvider OpenAI 协议 Provider（响应面注册名 "rightapi-codex"）。
type codexProvider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

func newCodex(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("rightapi-codex requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("rightapi-codex endpoint is required")
	}
	return &codexProvider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:          codexName,
			Endpoint:      cfg.Endpoint,
			Timeout:       cfg.Timeout,
			ChatPath:      codexChatPath,      // 端点已含 /v1
			ResponsesPath: codexResponsesPath, // 原生支持 Responses API
			ModelsPath:    codexModelsPath,    // 端点已含 /v1 → /models
			StreamUsage:   true,
			BillingSource: cfg.BillingSource, // 按本面计费源取 key
			Pool:          cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *codexProvider) Name() string                { return codexName }
func (p *codexProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *codexProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *codexProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *codexProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *codexProvider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

func (p *codexProvider) SetPool(pool *keypool.Pool) { p.base.SetPool(pool) }

func (p *codexProvider) Close() error { return p.base.Close() }
