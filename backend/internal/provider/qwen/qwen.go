// Package qwen 实现通义千问 Qwen Provider
// 通过阿里云百炼 DashScope 的 OpenAI 兼容模式接入
// 官方文档:https://help.aliyun.com/zh/model-studio/developer-reference/use-qwen-by-calling-api
package qwen

import (
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"context"
	"fmt"

		"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name           = "qwen"
	DefaultEndpoint = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	ChatPath       = "/chat/completions"
)

// DefaultModels Qwen DashScope 商用 Stable 模型(2026-07)
// 完整列表 https://help.aliyun.com/zh/model-studio/completions
// 注意:百炼 DashScope 历史上用过 qwen-turbo / qwen-plus / qwen-max 等 alias,
// 当前主推 qwen3 系列(开源+闭源商用)
var DefaultModels = []string{
	"qwen-plus",          // 通义千问增强版,商用主力
	"qwen-turbo",         // 更快更便宜
	"qwen-max",           // 旗舰
	"qwen-max-latest",    // 旗舰最新版
	"qwen-long",          // 长上下文(1M tokens)
	"qwen-coder-plus",    // 代码专用
	"qwen-coder-turbo",   // 代码更快版
	"qwen-vl-max",        // 多模态视觉
	"qwen-vl-plus",       // 多模态视觉便宜
	"qwen3-235b-a22b",    // Qwen3 旗舰开源 MoE(可通过 API)
	"qwen3-32b",          // Qwen3 dense 32B
	"qwen3-max",          // Qwen3 闭源旗舰(若已 GA)
}

type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("qwen requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("qwen endpoint is required")
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool: toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *Provider) Models() []string            { return p.cfg.Models }
func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }
// SetPool P30:注入 KeyPool(从 DB 读)
func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error                          { return p.base.Close() }

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolOpenAI) }


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
