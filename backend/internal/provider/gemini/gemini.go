// Package gemini 实现 Google Gemini Provider
// 官方文档:https://ai.google.dev/gemini-api/docs
// 协议:Google Generative AI (generateContent / streamGenerateContent)
// 鉴权:x-goog-api-key header(不是 ?key= query,避免 key 进 URL 日志)
// 端点:POST {endpoint}/models/{model}:generateContent
// Body 格式:{contents: [{parts: [{text: "..."}], role: "user"}]}
// Usage 字段:promptTokenCount / candidatesTokenCount / totalTokenCount /
//
//	cachedContentTokenCount / thoughtsTokenCount
package gemini

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/google"
)

const (
	name            = "gemini"
	DefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta"
	ChatPath        = "" // Gemini 不用 chat path,model 拼在 URL 里
)

type Provider struct {
	base *google.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolGoogle {
		return nil, fmt.Errorf("gemini requires protocol=google, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("gemini endpoint is required")
	}
	return &Provider{
		base: google.NewBase(google.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     cfg.Pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolGoogle }
func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
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

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolGoogle) }

// toPool 把 cfg.Pool (interface{}) 安全转成 *keypool.Pool
