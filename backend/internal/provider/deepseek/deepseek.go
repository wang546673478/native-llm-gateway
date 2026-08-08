// Package deepseek 实现 DeepSeek Provider(OpenAI + Anthropic 两种协议)
//
// 基于官方文档 https://api-docs.deepseek.com 重写:
//
// 关键差异(与标准 OpenAI Chat Completions):
//  1. 端点路径是 /chat/completions(没有 /v1 前缀!)
//  2. 支持 thinking 模式:"thinking": {"type": "enabled"}
//  3. 启用 thinking 后,响应中增加 reasoning_content 字段
//  4. usage 增加 prompt_cache_hit_tokens / prompt_cache_miss_tokens
//     和 completion_tokens_details.reasoning_tokens
//  5. 模型:deepseek-v4-flash / deepseek-v4-pro
//     (deepseek-chat / deepseek-reasoner 于 2026/07/24 弃用)
//
// 实现策略:继承 openai_compatible.Base,通过 Config.ChatPath 覆盖端点,
// 启用 StreamUsage 让流式响应末尾带 usage。
// P-provider-vendor: anthropic 协议实现在 anthropic.go(注册名 "deepseek-anthropic")。
// 官方文档特性(2026-08-04 全量调研,OpenAI 面):
//   - thinking:默认 enabled;reasoning_effort: low|high|max(medium/xhigh 映射 high)
//   - 响应 choices[].message.reasoning_content;流式在 delta.reasoning_content;
//     usage.completion_tokens_details.reasoning_tokens 计思维链 token
//   - 思考模式下 temperature/top_p 不生效;带 tools 时必须逐轮回传 reasoning_content,否则 400
//   - KV cache:自动开启无参数;usage.prompt_cache_hit_tokens / prompt_cache_miss_tokens
//     (prompt_tokens = hit + miss;缓存价仅为未命中价 2%~0.8%)
//   - JSON output:response_format={"type":"json_object"};prompt 须含 "json" 字样
//   - FIM / Chat Prefix / Responses API:需要 /beta base_url 或独立端点,本包未实现(YAGNI)
//   - 峰谷定价预告:高峰(北京 9-12 / 14-18 点)2 倍价,生效后需调整定价配置
package deepseek

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name = "deepseek"
	// DefaultEndpoint DeepSeek 官方 base URL
	DefaultEndpoint = "https://api.deepseek.com"
	// ChatPath DeepSeek 用 /chat/completions,不是 /v1/chat/completions
	ChatPath = "/chat/completions"
)

// 默认模型列表(2026-07 最新版)
// 注:deepseek-chat / deepseek-reasoner 已于 2026/07/24 弃用,Gateway
// 不再默认导出;老用户配置仍可用,但建议尽快迁移到 v4
var DefaultModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
}

// Provider DeepSeek Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("deepseek requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek endpoint is required")
	}

	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			ChatPath: ChatPath, // DeepSeek 关键差异:无 /v1 前缀
			// P-responses: endpoint 无 /v1 → 透传 /v1/responses(官方原生支持,Codex 兼容)
			ResponsesPath: "/v1/responses",
			StreamUsage:   true, // 让流式末尾带 usage,便于 Gateway 端记账
			Pool:          toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

// Models 返回 cfg 里配置的模型;若为空,返回 DeepSeek v4 默认列表
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
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name)
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
