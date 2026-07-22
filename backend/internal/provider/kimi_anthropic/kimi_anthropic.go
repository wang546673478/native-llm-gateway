// Package kimi_anthropic 实现 Moonshot Kimi 的 Anthropic 兼容端点
//
// Moonshot 官方为 Claude Code / Anthropic SDK 用户提供的兼容入口:
//   - 文档:https://platform.kimi.com/docs/overview (国内 platform.kimi.com)
//     注意 platform.kimi.com(中国)和 platform.kimi.ai(国际)账号/key 不通用
//   - 端点: https://api.moonshot.cn/anthropic
//   - 鉴权: x-api-key: {MOONSHOT_API_KEY}
//          anthropic-version: 2023-06-01
//   - 端点: POST /v1/messages
//
// Claude Code 接入(实测可用的方案):
//   ANTHROPIC_BASE_URL=https://api.moonshot.cn/anthropic
//   ANTHROPIC_AUTH_TOKEN={YOUR_API_KEY}
//   ANTHROPIC_MODEL=kimi-k2.5
//
// 端点结构与 deepseek_anthropic / glm_anthropic 完全相同 —
// endpoint 字段填 https://api.moonshot.cn/anthropic,实际请求会拼成
// https://api.moonshot.cn/anthropic/v1/messages
//
// 与 kimi(OpenAI 兼容)对比:
//   - 同一组 Moonshot API key 两个端点都能用(参考 DeepSeek 行为)
//   - 走 Anthropic 协议便于 Claude Code / Anthropic SDK 直接对接
//   - 支持的模型命名一致:kimi-k3 / kimi-k2.7-code / kimi-k2.6 等
//
// 实现策略:继承 anthropic_compatible.Base,共享 SSE / Usage 解析。
package kimi_anthropic

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name = "kimi-anthropic"
	// DefaultEndpoint Moonshot 官方 Anthropic 兼容 base URL(国内)
	// 国际用户请用 https://api.moonshot.ai/anthropic
	DefaultEndpoint = "https://api.moonshot.cn/anthropic"
)

// DefaultModels Kimi 在 Anthropic 模式下支持的模型(2026-07)
// 与 kimi(OpenAI 兼容)命名一致 — Anthropic 兼容层是协议适配,不改模型 ID
var DefaultModels = []string{
	"kimi-k3",                  // 当前最新旗舰,1M 上下文
	"kimi-k2.7-code",           // 代码专用
	"kimi-k2.7-code-highspeed", // 代码高速版
	"kimi-k2.6",                // 通用,256K 上下文
	"kimi-k2.5",                // 通用
}

// Provider Kimi Anthropic 兼容 Provider
type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("kimi-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("kimi-anthropic endpoint is required")
	}
	return &Provider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

// Models 返回 cfg 里配置的模型;若为空,返回 Kimi 默认模型列表
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

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

// init 自动注册到 Registry
func init() {
	provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolAnthropic)
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