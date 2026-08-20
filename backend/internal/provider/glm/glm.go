// Package glm 实现 GLM Provider(智谱 AI,OpenAI + Anthropic 两种协议)
//
// 基于官方文档 https://docs.bigmodel.cn(2026-08-05 全量调研)实现:
//
// OpenAI 面关键事实:
//  1. 端点 https://open.bigmodel.cn/api/paas/v4(已含 /api/paas/v4 版本前缀,
//     chat path 用 /chat/completions,不是 /v1/chat/completions)
//  2. 认证:标准 Bearer(与 OpenAI 一致)
//  3. 流式:标准 SSE,结束带 usage(StreamUsage 可记账)
//  4. GLM-5.2 支持思考模式:请求 thinking + reasoning_effort(默认 max),
//     响应 choices[].message / 流式 delta 带 reasoning_content
//  5. 上下文缓存自动生效,usage 结构与 OpenAI 一致
//  6. 官方文档无 /responses API → 不配 ResponsesPath,responses_api: false
//  7. 额度查询:官方插件(zai-org/zai-coding-plugins)暴露的 monitor 端点
//     GET {host}/api/monitor/usage/quota/limit,标准 API key 可用 → 注册 balancer,
//     poll 模式:百分比展示 + 滚动窗口重置后自动恢复(见 balancer.go)
//  8. 模型:glm-5.2(旗舰,1M 上下文 / 128K 输出)、glm-5.1、glm-5、glm-5-turbo、
//     glm-4.7、glm-4.6、glm-4.5
//
// Anthropic 面(anthropic.go,注册名 "glm-anthropic"):
//   - base URL https://open.bigmodel.cn/api/anthropic,与 openai 面共用同一 API key
//
// 注意:GLM Coding Plan 套餐需配置专属 Coding 端点(见官方文档),本包只做标准 API 计费
package glm

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name = "glm"
	// DefaultEndpoint 智谱开放平台 API 端点(已含 /api/paas/v4 前缀)
	DefaultEndpoint = "https://open.bigmodel.cn/api/paas/v4"
	// ChatPath 端点已含版本前缀 → /chat/completions(不是 /v1/chat/completions)
	ChatPath = "/chat/completions"
)

// Provider GLM Provider(仅 OpenAI 协议面)
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("glm requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("glm endpoint is required")
	}

	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:        name,
			Endpoint:    cfg.Endpoint,
			Timeout:     cfg.Timeout,
			ChatPath:    ChatPath, // 端点已含 v4 前缀 → 无 /v1
			StreamUsage: true,     // 流式末尾带 usage,便于 Gateway 端记账
			Pool:        cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

// init 自动注册到 Registry(vendor = 厂商名)
// 官方 Claude API 兼容 → anthropic 面也要注册,两个注册名共享同一 key 池
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name)
}

// toPool 把 cfg.Pool (interface{}) 安全转成 *keypool.Pool
