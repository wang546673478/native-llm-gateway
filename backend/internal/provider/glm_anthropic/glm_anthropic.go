// Package glm_anthropic 实现智谱 GLM 的 Anthropic 兼容端点
//
// 智谱官方为 Claude Code / Anthropic SDK 用户提供的兼容入口:
//   - 文档说明:https://docs.bigmodel.cn/cn/guide/start/introduction
//     (在 /cn/guide/develop/ 文档集里有相关迁移指南,
//      官方也发了"Claude API 用户特别搬家计划"公告)
//   - base URL: https://open.bigmodel.cn/api/anthropic
//   - 鉴权:     x-api-key: {ZHIPU_API_KEY}
//               anthropic-version: 2023-06-01
//   - 端点:     POST /v1/messages
//
// 端点结构与 deepseek_anthropic 完全相同 — endpoint 字段填
// https://open.bigmodel.cn/api/anthropic,实际请求会拼成
// https://open.bigmodel.cn/api/anthropic/v1/messages
//
// 与 glm(OpenAI 兼容)对比:
//   - 同一组智谱 API key 两个端点都能用(参考 DeepSeek 行为)
//   - 走 Anthropic 协议便于 Claude Code / Anthropic SDK 直接对接
//   - 支持的模型命名一致:glm-4.7 / glm-4.6 / glm-4.6v 等
//
// 实现策略:继承 anthropic_compatible.Base,共享 SSE / Usage 解析。
package glm_anthropic

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name = "glm-anthropic"
	// DefaultEndpoint 智谱官方 Anthropic 兼容 base URL
	// 用户 Claude Code 迁移:ANTHROPIC_BASE_URL=https://open.bigmodel.cn/api/anthropic
	DefaultEndpoint = "https://open.bigmodel.cn/api/anthropic"
)

// DefaultModels GLM 在 Anthropic 模式下支持的模型(2026-07)
// 与 glm(OpenAI 兼容)命名一致 — Anthropic 兼容层是协议适配,不改模型 ID
var DefaultModels = []string{
	"glm-4.7",       // 最新旗舰(2025-12 开源),32K 上下文,8K 输出
	"glm-4.7-flash", // 最新轻量免费(替代 glm-4.5-flash)
	"glm-4.6",       // 上一代旗舰(2025-10),200K 上下文
	"glm-4.6v",      // 多模态
	"glm-4-long",    // 长上下文
	"glm-4-plus",    // 高级版
	"glm-4-flash",   // 免费稳定
	"glm-4-flashx",  // 高速版
	"glm-4-air",     // 轻量
	"glm-4-airx",    // 增强轻量
}

// Provider GLM Anthropic 兼容 Provider
type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("glm-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("glm-anthropic endpoint is required")
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

// Models 返回 cfg 里配置的模型;若为空,返回 GLM 默认模型列表
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