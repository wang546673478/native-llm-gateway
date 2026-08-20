// Package mimo 实现小米 MiMo Provider(OpenAI + Anthropic 两种协议 × 两套端点)
//
// 基于官方文档 https://mimo.mi.com/docs/zh-CN/quick-start/summary/welcome 全量调研(2026-08-06):
//
// 关键差异(与标准 OpenAI Chat Completions):
//  1. 两套端点/两套 key,互相不通用:
//     - 按量付费:https://api.xiaomimimo.com/v1(key 格式 sk-xxx,billing api)
//     - Token Plan:https://token-plan-cn.xiaomimimo.com/v1(key 格式 tp-xxx,billing token_plan)
//     鉴权:Authorization: Bearer 或 api-key: header 二选一(网关用 Bearer)
//  2. 原生支持 Responses API(/v1/responses,Codex 透传)— responses_api: true
//  3. 思考模式:chat 面非标参数 "thinking":{"type":"enabled|disabled"}(SDK 需 extra_body);
//     responses 面标准 "reasoning":{"effort":"none|low|medium|high"}(none=关,其余=开,
//     档位无差异化)— 与网关 stripResponsesReasoning 强制 effort=none 语义一致,零改动兼容
//  4. 思考模式不支持自定义 temperature/top_p(强制 1.0/0.95);max_completion_tokens
//     同时限制思考与最终答案长度
//  5. 多轮工具调用 + 思考模式:必须完整回传 reasoning_content,否则 400
//     (chat 面文档建议保留全部历史 reasoning_content;Responses 面网关剥离推理块后
//     已显式 effort=none 声明不启用 thinking,不触发该校验)
//  6. usage:prompt cache hit/miss 分列;completion_tokens_details.reasoning_tokens 计思维链
//  7. 模型:mimo-v2.5-pro / mimo-v2.5(上下文 1M,最大输出 128K);
//     mimo-v2-pro / mimo-v2-omni / mimo-v2-flash 等 v2 系列已于 2026-06-30 弃用,勿配置
//  8. 错误码:402 = 按量付费余额不足;429 = 限流 或 套餐额度耗尽(双义,body 区分信号
//     官方未文档化 — 实测项);421 内容过滤;403 区域/风控;400 含「thinking 模式下
//     reasoning_content 未回传」;套餐额度耗尽时停服不超额
//  9. 无官方余额查询 API(只有控制台页面)→ 本包不注册 balancer,pool 走 probe 模式
//     (glm/qwen/gemini 同款):额度靠 402/429 错误码标记,每次请求重新探测,充值即恢复
//  10. 套餐条款(用户已知悉并选择接入):Token Plan 配额仅允许在编程工具中使用,
//     禁止以 API 调用形式用于自动化脚本和自定义应用后端;夜间消耗 0.8x
//  11. Web Search 按次计费(¥16/1000 次),不含在 token 价内
//  12. 定价(¥/M tokens,国内):mimo-v2.5-pro cache命中 ¥0.025 / 未命中 ¥3.00 / 输出 ¥6.00;
//     mimo-v2.5 cache命中 ¥0.02 / 未命中 ¥1.00 / 输出 ¥2.00(÷1000 进 cost_per_1k)
//
// 实现策略:继承 openai_compatible.Base,端点已含 /v1 → ChatPath 用默认
// /v1/chat/completions,ResponsesPath 覆盖为 /responses;启用 StreamUsage
// 让流式末尾带 usage。anthropic 协议实现在 anthropic.go。
package mimo

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name = "mimo"
	// tokenPlanName Token Plan 端点(套餐,key 格式 tp-xxx)— 与 name 共享 vendor 的 key 池
	tokenPlanName = "mimo-token-plan"
	// DefaultEndpoint 按量付费官方 base URL(已含 /v1)
	DefaultEndpoint = "https://api.xiaomimimo.com/v1"
	// DefaultTokenPlanEndpoint Token Plan 官方 base URL(已含 /v1;套餐专用 tp- key)
	DefaultTokenPlanEndpoint = "https://token-plan-cn.xiaomimimo.com/v1"
	// ChatPath 留空用默认 /v1/chat/completions(端点已含 /v1)
	// ResponsesPath 端点已含 /v1 → 相对路径 /responses
	responsesPath = "/responses"
)

// Provider MiMo Provider(OpenAI 协议面)
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数,符合 provider.Factory 签名(按量付费端点)
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	return newOpenAI(cfg, name)
}

// NewTokenPlan 工厂函数 — Token Plan 端点(套餐),与 New 共用实现,仅注册名不同
func NewTokenPlan(cfg provider.ProviderConfig) (provider.Provider, error) {
	return newOpenAI(cfg, tokenPlanName)
}

// newOpenAI 共用的 openai 面构造:协议校验 + endpoint 校验
func newOpenAI(cfg provider.ProviderConfig, regName string) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("%s requires protocol=openai, got %q", regName, cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%s endpoint is required", regName)
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:          regName,
			Endpoint:      cfg.Endpoint,
			Timeout:       cfg.Timeout,
			ChatPath:      "",            // 端点已含 /v1 → 默认 /v1/chat/completions
			ResponsesPath: responsesPath, // 原生支持 Responses API,端点已含 /v1 → /responses
			StreamUsage:   true,          // 流式末尾带 usage,网关才能记账
			Pool:          cfg.Pool,
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

// init 自动注册到 Registry(4 个注册名,vendor 都是 mimo → 共享同一个 key 池;
// sk-/tp- 两套 key 靠 DB 里的 per-key BillingSource 过滤隔离,加 key 时选类型)
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(tokenPlanName, NewTokenPlan, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name)
	provider.RegisterGlobalWithProtocolVendor(tokenPlanAnthropicName, NewTokenPlanAnthropic, provider.ProtocolAnthropic, name)
}

// toPool 把 cfg.Pool (interface{}) 安全转成 *keypool.Pool
