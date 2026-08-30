// Package mimo 实现 MiMo 的按量与 Token Plan 两套端点，以及 OpenAI 与
// Anthropic 两种协议面。四个注册面归入 mimo vendor，通过每把 Key 的
// billing_source 隔离端点。余额查询使用管理端配置的控制台 Cookie；模型清单和价格
// 由数据库维护。完整运行契约见 docs/providers.md。
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
	// 以下三个路径都相对于「已含 /v1 的 endpoint」,一律不带 /v1 前缀 ——
	// 早期 ChatPath 留空吃默认 /v1/chat/completions,拼出 /v1/v1/chat/completions,
	// 导致 mimo / mimo-token-plan 两个 openai 面从未成功过(usage_records 0 条),
	// 流量全靠 anthropic 面兜着才没暴露(2026-08-20 查同步问题时顺带发现)。
	chatPath      = "/chat/completions"
	responsesPath = "/responses"
	modelsPath    = "/models"
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
			ChatPath:      chatPath,      // 端点已含 /v1 → 不再拼 /v1
			ResponsesPath: responsesPath, // 原生支持 Responses API,端点已含 /v1 → /responses
			ModelsPath:    modelsPath,    // 同上:端点已含 /v1 → /models
			// mimo 两个 openai 面端点不同且 key 与端点绑定(tp- ↔ token-plan、
			// sk- ↔ api,交叉必 401)→ ListModels/HealthCheck 必须按本面计费源取 key
			BillingSource: cfg.BillingSource,
			StreamUsage:   true, // 流式末尾带 usage,网关才能记账
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
