// Package deepseek 实现 DeepSeek Provider
// DeepSeek 使用 OpenAI Chat Completions API(完全兼容),
// 所以其实例在 P9 之前只占位,真正的 HTTP 逻辑由
// openai_compatible 基类提供(后续阶段注入)。
package deepseek

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

const name = "deepseek"

// Provider DeepSeek Provider(占位实现)
type Provider struct {
	cfg    provider.ProviderConfig
	logger *zap.Logger
}

// New 工厂函数,符合 provider.Factory 签名
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("deepseek requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek endpoint is required")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("deepseek requires at least one API key")
	}
	return &Provider{cfg: cfg}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *Provider) Models() []string            { return p.cfg.Models }

func (p *Provider) HealthCheck(ctx context.Context) error {
	// P2 占位:不真发请求,避免启动时无谓外部调用
	// P9 会用 openai_compatible 基类的 GET /models
	return nil
}

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, &provider.ProviderError{
		ProviderName: name,
		StatusCode:   501,
		ErrorType:    provider.ErrorTypeServerError,
		Message:      "deepseek: SendRequest not implemented yet (planned in P9)",
	}
}

func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, &provider.ProviderError{
		ProviderName: name,
		StatusCode:   501,
		ErrorType:    provider.ErrorTypeServerError,
		Message:      "deepseek: SendStreamRequest not implemented yet (planned in P9)",
	}
}

func (p *Provider) Close() error {
	return nil
}

// init 自动注册到 Registry
func init() {
	provider.RegisterGlobal(name, New)
}
