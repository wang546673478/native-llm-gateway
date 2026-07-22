// Package glm 实现智谱 GLM Provider
//
// 基于智谱官方文档 https://docs.bigmodel.cn/cn/api/introduction 重写:
//
// 协议:智谱 GLM 是 OpenAI Chat Completions 兼容协议
//   - Base URL: https://open.bigmodel.cn/api/paas/v4
//   - 端点:   POST {base}/chat/completions  (没有 /v1 前缀)
//   - 鉴权:   Authorization: Bearer {API_KEY}
//
// 关键差异(与标准 OpenAI Chat Completions):
//   1. 端点路径是 /chat/completions(没有 /v1 前缀,DeepSeek 也类似)
//   2. 模型命名体系:
//      - glm-4.7        最新旗舰(2025-12),32K 上下文,8K 输出,支持 thinking + function calling
//      - glm-4.7-flash  轻量免费版(2026-01),替代 glm-4.5-flash(2026-01-30 下线)
//      - glm-4.6        上一代旗舰(2025-10),200K 上下文
//      - glm-4.6v       多模态版
//      - glm-4-air      / glm-4-airx / glm-4-long / glm-4-flashx / glm-4-plus  持续维护
//      注:"glm-4.5-flash" 已下线;不存在的命名:glm-4.6 / glm-4.7 + "-free" 后缀
//   3. 流式响应支持 stream_options.include_usage=true,
//      最后一个 chunk 带 usage(prompt_tokens / completion_tokens / total_tokens)
//      与标准 OpenAI 兼容 — openai_compatible.Base 已处理
//
// 实现策略:继承 openai_compatible.Base,通过 Config.ChatPath 覆盖端点,
// 启用 StreamUsage 让流式响应末尾带 usage。
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
	// DefaultEndpoint 智谱官方 base URL(官方文档 §1)
	DefaultEndpoint = "https://open.bigmodel.cn/api/paas/v4"
	// ChatPath GLM 端点(官方:无 /v1 前缀)
	ChatPath = "/chat/completions"
)

// DefaultModels 智谱 GLM 当前可用 Stable 模型(2026-07)
// 来源:官方 BigModel 模型广场 + 智谱公众号/官网公告
//   - glm-4.7-flash 是 glm-4.5-flash 的替代(后者 2026-01-30 下线)
//   - 不再列出 glm-4.5-flash / deepseek-chat 等弃用模型
//   - glm-4-long / glm-4-plus / glm-4-air / glm-4-airx / glm-4-flashx 持续维护
var DefaultModels = []string{
	"glm-4.7",        // 最新旗舰(2025-12 开源),32K 上下文,8K 输出
	"glm-4.7-flash",  // 最新轻量免费(替代 glm-4.5-flash)
	"glm-4.6",        // 上一代旗舰(2025-10),200K 上下文
	"glm-4.6v",       // 多模态
	"glm-4-long",     // 长上下文
	"glm-4-plus",     // 高级版
	"glm-4-flash",    // 免费稳定
	"glm-4-flashx",   // 高速版
	"glm-4-air",      // 轻量
	"glm-4-airx",     // 增强轻量
}

// Provider GLM Provider
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
			ChatPath:    ChatPath, // GLM 关键差异:无 /v1 前缀
			StreamUsage: true,    // 流式末尾带 usage,Gateway 才能正确计费
			Pool:        toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

// Models 返回 cfg 里配置的模型;若为空,返回 GLM 默认模型列表
// 与 DeepSeek / MiniMax 行为一致,避免配置里没填 models 时返回空数组
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