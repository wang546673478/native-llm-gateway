// Package glm 实现 GLM Provider(智谱 AI,OpenAI + Anthropic 两种协议)
//
// P-provider-vendor: 本包同时承载两个注册名:
//   - "glm"           → New(OpenAI 协议,glm.go)
//   - "glm-anthropic" → NewAnthropic(Anthropic 兼容,本文件)
//
// Claude API 兼容(官方文档 https://docs.bigmodel.cn/cn/guide/develop/claude/introduction):
//   - base URL: https://open.bigmodel.cn/api/anthropic
//   - 鉴权:     与 openai 面共用同一 API key(同 vendor → 共享 key 池)
//   - 端点:     /v1/messages(相对 base URL 拼接,Anthropic 标准)
//   - 模型编码: 智谱模型名(glm-5.2 等),不是 claude-*
//   - 官方提示: "某些场景下智谱与 Claude 接口仍存在差异,但不影响整体兼容性"
//
// 实现策略:继承 anthropic_compatible.Base(与 deepseek-anthropic / minimax 的
// anthropic 面同一基座),usage / SSE / thinking 块按 Anthropic 标准解析。
package glm

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	anthropicName = "glm-anthropic"
	// anthropic 端点 https://open.bigmodel.cn/api/anthropic — 由 config.yaml 的 endpoint 提供
)

// AnthropicProvider Anthropic 兼容 Provider(协议面是 anthropic,账号与 openai 面同一组 key)
// 命名加 Anthropic 前缀:与 glm.go 的 Provider(OpenAI 面)区分,Go 包内类型名不能重复
type AnthropicProvider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// NewAnthropic Anthropic 协议工厂函数
func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("glm-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("glm-anthropic endpoint is required")
	}
	return &AnthropicProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     anthropicName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *AnthropicProvider) Name() string                { return anthropicName }
func (p *AnthropicProvider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (p *AnthropicProvider) Models() []string {
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models
	}
	return DefaultModels // 与 openai 面共用(同包变量)
}

func (p *AnthropicProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *AnthropicProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

func (p *AnthropicProvider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *AnthropicProvider) Close() error { return p.base.Close() }
