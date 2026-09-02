// Package minimax 实现 MiniMax 的 Anthropic 与 OpenAI 协议面。
// minimax.go 提供 Anthropic 注册面，openai.go 提供 minimax-openai；两者归入
// minimax vendor 并共享 Key Pool。包同时处理 MiniMax base_resp 错误形状和余额查询。
// 模型清单和价格由数据库维护，运行契约见 docs/providers.md。
package minimax

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	name            = "minimax"
	DefaultEndpoint = "https://api.minimaxi.com/anthropic"
	ChatPath        = "/v1/messages"
)

type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("minimax requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("minimax endpoint is required")
	}
	return &Provider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }
func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

// DiagnoseKey forwards the explicit read-only probe to the Anthropic
// compatibility base. The base deliberately does not acquire or report keys.
func (p *Provider) DiagnoseKey(ctx context.Context, key *keypool.Key, req provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
	return p.base.DiagnoseKey(ctx, key, req)
}
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}

// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }

func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolAnthropic, name)
	provider.RegisterGlobalWithProtocolVendor(openaiName, NewOpenAI, provider.ProtocolOpenAI, name)
}

// P-provider-vendor: openai 协议实现在 openai.go(注册名 "minimax-openai")。
// 官方文档特性(2026-08-04 全量调研,Anthropic 面):
//   - thinking:仅 disabled / adaptive(MiniMax 扩展);M3 默认关闭(省略 = disabled);
//     M2.x 系列 thinking 恒开不可关
//   - 响应 content 块含 thinking(文本 + signature);多轮必须原样回带 thinking 块(含 signature)
//   - service_tier:standard | priority(1.5x 价,优先准入);standard Anthropic SDK 可能不识别
//   - 缓存:主动缓存(cache_control 断点,5min TTL,最多 4 断点/20 块回溯)支持 M2.x 但不支持 M3;
//     usage.cache_creation_input_tokens / cache_read_input_tokens(message_start 即返回)
//   - tool_choice 仅 auto / none;top_k / stop_sequences / mcp_servers 静默忽略
//   - 无官方余额查询 API — 本包 balancer 用未文档化端点 www.minimaxi.com/v1/token_plan/remains
//   - 错误:HTTP 状态码 + body {type:"error", error:{type,message}};余额不足 1008 / 超套餐 2056 走 base_resp
