// Package kimi 实现 Moonshot Kimi Provider
//
// 基于官方文档重写:
//
//   文档集:https://platform.kimi.com/docs/overview
//   协议:   OpenAI Chat Completions 兼容
//   Base URL: https://api.moonshot.cn  (中国;国际走 platform.kimi.ai,key 不通用)
//   端点:   POST {base}/v1/chat/completions
//   鉴权:   Authorization: Bearer {MOONSHOT_API_KEY}
//
// 关键差异(与标准 OpenAI Chat Completions):
//   1. 模型命名:Moonshot 自有体系,常见:
//      - kimi-k3           旗舰,1M 上下文,顶层 reasoning_effort(low/high/max)
//      - kimi-k2.7-code    代码专用,256K 上下文,支持 thinking mode + 图像/视频输入
//      - kimi-k2.7-code-highspeed  代码高速版
//      - kimi-k2.6         通用,256K 上下文,支持 thinking / 非 thinking 模式
//      - kimi-k2.5         通用
//      注:"kimi-k2-0905-preview" / "kimi-latest" / "kimi-thinking-preview" 已弃用;
//         "moonshot-v1-*" 老系列不对新用户开放
//   2. 扩展参数(通过 body 透传,Gateway 不需要特殊处理):
//      - thinking:     extra_body={"thinking": {...}}(thinking mode 开关)
//      - reasoning_effort:kimi-k3 顶层字段,"low"/"high"/"max"
//      - partial:      assistant 消息字段 {"partial": true}(partial mode)
//   3. 错误码:HTTP 状态码 + body 中 error.type,
//      常见类型:content_filter / invalid_request_error / engine_overloaded_error
//      / exceeded_current_quota_error / rate_limit_reached_error
//      401 区分:platform.kimi.com(中国)与 platform.kimi.ai(国际)key 不通用
//      openai_compatible.Base.ClassifyErrorWithBody 已支持通用分类
//
// Anthropic 兼容入口(kimi-anthropic provider)走 https://api.moonshot.cn/anthropic,
// 共享同一组 API Key — 给 Claude Code / Anthropic SDK 用户使用。
//
// 实现策略:继承 openai_compatible.Base,启用 StreamUsage 让流式响应末尾带 usage。
package kimi

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name = "kimi"
	// DefaultEndpoint Moonshot Kimi 官方 base URL(国内 platform.kimi.com)
	// 国际用户请用 https://api.moonshot.ai
	DefaultEndpoint = "https://api.moonshot.cn"
	// ChatPath Kimi 走标准 /v1/chat/completions
	ChatPath = "/v1/chat/completions"
)

// DefaultModels Kimi 当前可用 Stable 模型(2026-07)
// 来源:platform.kimi.com/docs/overview + platform.kimi.com/docs/models
// 注:moonshot-v1-* / kimi-latest / kimi-thinking-preview / kimi-k2-0905-preview 全部已弃用
var DefaultModels = []string{
	"kimi-k3",                  // 当前最新旗舰,1M 上下文
	"kimi-k2.7-code",           // 代码专用(2.7)
	"kimi-k2.7-code-highspeed", // 代码高速版
	"kimi-k2.6",                // 通用,256K 上下文,thinking + 非 thinking
	"kimi-k2.5",                // 通用
}

// Provider Kimi Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("kimi requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("kimi endpoint is required")
	}

	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:        name,
			Endpoint:    cfg.Endpoint,
			Timeout:     cfg.Timeout,
			ChatPath:    ChatPath,
			StreamUsage: true, // Kimi 支持 stream_options.include_usage,Gateway 端才能正确计费
			Pool:        toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

// Models 返回 cfg 里配置的模型;若为空,返回 Kimi 默认模型列表
// 与 glm / deepseek 行为一致,避免配置里没填 models 时返回空数组
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
	provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolOpenAI)
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