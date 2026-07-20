// Package glm 实现智谱 GLM Provider
// GLM 兼容 OpenAI Chat Completions
package glm

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const name = "glm"

// Provider GLM Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// New 工厂函数
func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("glm requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("glm endpoint is required")
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("glm requires at least one API key")
	}
	pool, ok := cfg.Pool.(*keypool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("glm requires a non-nil keypool.Pool (got %T)", cfg.Pool)
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     pool,
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

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

func (p *Provider) Close() error { return p.base.Close() }

func init() { provider.RegisterGlobal(name, New) }
