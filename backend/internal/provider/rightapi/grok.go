// Package rightapi — grok 渠道（OpenAI 协议面，注册名 "rightapi-grok"）。
//
// 端点 https://rightapi.ai/grok/v1（已含 /v1）：
//   - /responses → Responses API（grok 官方配置走 api_backend="responses"）
//   - /models    → 模型列表（grok-4.5 / grok-4.6）
package rightapi

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	grokResponsesPath = "/responses"
	grokModelsPath    = "/models"
)

// grokProvider OpenAI 协议 Provider（响应面注册名 "rightapi-grok"）。
type grokProvider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

func newGrok(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("rightapi-grok requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("rightapi-grok endpoint is required")
	}
	return &grokProvider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     grokName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			// Grok 走 Responses API；ChatPath 也设为不带 /v1（端点已含 /v1）
			ChatPath:      "/chat/completions",
			ResponsesPath: grokResponsesPath,
			ModelsPath:    grokModelsPath, // 端点已含 /v1 → /models
			StreamUsage:   true,
			BillingSource: cfg.BillingSource,
			Pool:          cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *grokProvider) Name() string                { return grokName }
func (p *grokProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *grokProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *grokProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *grokProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *grokProvider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

func (p *grokProvider) SetPool(pool *keypool.Pool) { p.base.SetPool(pool) }

func (p *grokProvider) Close() error { return p.base.Close() }
