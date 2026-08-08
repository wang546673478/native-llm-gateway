// Package mimo — anthropic 协议面(本文件)
//
// 本包同时承载四个注册名:
//   - "mimo"                  → New(OpenAI 协议,按量付费端点,mimo.go)
//   - "mimo-token-plan"       → NewTokenPlan(OpenAI 协议,套餐端点,mimo.go)
//   - "mimo-anthropic"        → NewAnthropic(Anthropic 兼容,按量付费端点,本文件)
//   - "mimo-token-plan-anthropic" → NewTokenPlanAnthropic(Anthropic 兼容,套餐端点,本文件)
//
// Anthropic 兼容端点(官方文档 https://mimo.mi.com/docs/zh-CN/api/chat/anthropic-api):
//   - base URL(按量付费):https://api.xiaomimimo.com/anthropic
//   - base URL(套餐):    https://token-plan-cn.xiaomimimo.com/anthropic
//   - 鉴权:api-key: 或 Authorization: Bearer(anthropic-version 头由 Base 自动加)
//   - 端点:/v1/messages(相对 base URL 拼接)
//
// 特性(与 openai 面同源,见 mimo.go header):
//   - 消息块支持 text / image(仅 mimo-v2.5)/ tool_use / tool_result / thinking
//   - thinking 参数:type enabled|disabled(默认 enabled);思考模式不支持自定义
//     temperature(强制 1.0);max_tokens 含思考
//   - 多轮工具调用 + thinking 模式需回传 thinking 块 — Claude Code compact 会剥离
//     thinking 块,若实测触发 400,把对应 config 块的 force_thinking_disabled 置 true
//     (与 DeepSeek 同款机制,deepseek 实测 200 / 400 复现,见 anthropic_compatible.Config)
//   - 未设 ForceThinkingDisabled 默认关:保留 MIMO 深度思考能力,实测后按需开启
package mimo

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	anthropicName          = "mimo-anthropic"
	tokenPlanAnthropicName = "mimo-token-plan-anthropic"
	// anthropic 默认端点由 config.yaml 的 endpoint 提供(api.xiaomimimo.com/anthropic /
	// token-plan-cn.xiaomimimo.com/anthropic)
)

// AnthropicProvider Anthropic 兼容 Provider(协议面是 anthropic,与 openai 面共享 key 池)
// 命名加 Anthropic 前缀:与 mimo.go 的 Provider(OpenAI 面)区分,Go 包内类型名不能重复
type AnthropicProvider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// NewAnthropic Anthropic 协议工厂(按量付费端点)
func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	return newAnthropic(cfg, anthropicName)
}

// NewTokenPlanAnthropic Anthropic 协议工厂(Token Plan 套餐端点)
func NewTokenPlanAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	return newAnthropic(cfg, tokenPlanAnthropicName)
}

// newAnthropic 共用的 anthropic 面构造:协议校验 + endpoint 校验
func newAnthropic(cfg provider.ProviderConfig, regName string) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("%s requires protocol=anthropic, got %q", regName, cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%s endpoint is required", regName)
	}
	return &AnthropicProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     regName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     toPool(cfg.Pool),
			// ForceThinkingDisabled 不设默认开:MIMO v2.5 是 thinking 模型,
			// 强制 disabled 会损失深度思考能力;compact 后 400 实测触发再开
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
