// Package deepseek 实现 DeepSeek Provider(OpenAI + Anthropic 两种协议)
//
// P-provider-vendor: 本包同时承载两个注册名:
//   - "deepseek"           → New(OpenAI 协议,本文件下方 deepseek.go)
//   - "deepseek-anthropic" → NewAnthropic(Anthropic 兼容,本文件)
//
// Anthropic 兼容端点(官方文档 https://api-docs.deepseek.com/zh-cn/guides/anthropic_api):
//   - base URL: https://api.deepseek.com/anthropic
//   - 鉴权:     x-api-key: {DEEPSEEK_API_KEY}(anthropic-version / anthropic-beta 头被忽略)
//   - 端点:     /v1/messages(相对 base URL 拼接)
//
// 官方文档特性(2026-08-04 全量调研):
//   - 模型映射:claude-opus* → deepseek-v4-pro;claude-haiku*/claude-sonnet* → deepseek-v4-flash;
//     任何不支持的模型名自动映射到 deepseek-v4-flash(不报错 — 网关需显式校验模型名)
//   - 支持:max_tokens / stop_sequences / stream / system / temperature(0.0~2.0)/ top_p / thinking
//   - 忽略:budget_tokens / container / mcp_servers / service_tier / top_k / cache_control
//   - 消息块:支持 text / thinking / tool_use / tool_result / server_tool_use / web_search_tool_result;
//     不支持 image / document / search_result / redacted_thinking / code_execution_tool_result
//   - 注意:Anthropic 模式的 usage 响应字段官方文档未公开,网关按 Anthropic 标准字段解析(实测确认)
package deepseek

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	anthropicName = "deepseek-anthropic"
	// anthropic 默认端点 https://api.deepseek.com/anthropic — 由 config.yaml 的 endpoint 提供
)

// AnthropicProvider Anthropic 兼容 Provider(协议面是 anthropic,账号与 openai 面同一组 key)
// 命名加 Anthropic 前缀:与 deepseek.go 的 Provider(OpenAI 面)区分,Go 包内类型名不能重复
type AnthropicProvider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// NewAnthropic Anthropic 协议工厂函数
func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("deepseek-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek-anthropic endpoint is required")
	}
	return &AnthropicProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     anthropicName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     provider.ToPool(cfg.Pool),
			// P-deepseek-thinking: DeepSeek /anthropic 的 thinking 校验会拒绝
			// compact 后的历史(400 content[].thinking ... must be passed back),
			// 强制 disabled 绕开(flash 本来就是非 thinking 模型)
			ForceThinkingDisabled: cfg.ForceThinkingDisabled,
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
