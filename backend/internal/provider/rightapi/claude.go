// Package rightapi — claude 渠道（Anthropic 协议面，注册名 "rightapi-claude"）。
//
// 端点 https://rightapi.ai/claude（不含 /v1，anthropic base 拼 /v1/messages）：
//   - /v1/messages → Anthropic Messages API（Claude Code 走这里）
//   - 模型：claude-opus-5 / claude-sonnet-4-6 / claude-fable-5 / claude-haiku-4-5 等
//
// 鉴权兼容 Authorization: Bearer <key> 或 x-api-key。
package rightapi

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

// claudeProvider Anthropic 协议 Provider（注册名 "rightapi-claude"）。
type claudeProvider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

func newClaude(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("rightapi-claude requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("rightapi-claude endpoint is required")
	}
	return &claudeProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     claudeName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     cfg.Pool,
			// 不设 ForceThinkingDisabled：Right Code 走真实 Claude 网关（非 DeepSeek
			// 那种 /anthropic 的 thinking 校验），默认透传 thinking。若实测遇到
			// 400 "thinking ... must be passed back" 再补。
		}),
		cfg: cfg,
	}, nil
}

func (p *claudeProvider) Name() string                { return claudeName }
func (p *claudeProvider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (p *claudeProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *claudeProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *claudeProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *claudeProvider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

func (p *claudeProvider) SetPool(pool *keypool.Pool) { p.base.SetPool(pool) }

func (p *claudeProvider) Close() error { return p.base.Close() }
