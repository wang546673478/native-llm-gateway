// Package gemini 实现 Google Gemini Provider
// 官方文档:https://ai.google.dev/gemini-api/docs
// 协议:Google Generative AI (generateContent / streamGenerateContent)
// 鉴权:x-goog-api-key header(不是 ?key= query,避免 key 进 URL 日志)
// 端点:POST {endpoint}/models/{model}:generateContent
// Body 格式:{contents: [{parts: [{text: "..."}], role: "user"}]}
// Usage 字段:promptTokenCount / candidatesTokenCount / totalTokenCount /
//   cachedContentTokenCount / thoughtsTokenCount
package gemini

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/google"
)

const (
	name           = "gemini"
	DefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta"
	ChatPath       = "" // Gemini 不用 chat path,model 拼在 URL 里
)

// DefaultModels Google Gemini 当前 Stable 模型(2026-07)
// 完整列表见 https://ai.google.dev/gemini-api/docs/models
// 重要:"gemini-2.0-flash" / "gemini-1.5-pro" 已停用!
// Gemini 1.5 整个系列已弃用
// Gemini 2.0 整个系列已 shut down
// 当前 stable:Gemini 2.5 + Gemini 3.x
var DefaultModels = []string{
	"gemini-2.5-flash",      // stable,推荐默认
	"gemini-2.5-flash-lite", // stable,便宜
	"gemini-2.5-pro",        // stable,强
	"gemini-3.5-flash",      // stable,新
	"gemini-3.1-flash-lite", // stable
}

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
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("gemini requires at least one API key")
	}
	pool, ok := cfg.Pool.(*keypool.Pool)
	if !ok || pool == nil {
		return nil, fmt.Errorf("gemini requires a non-nil keypool.Pool (got %T)", cfg.Pool)
	}
	return &Provider{
		base: google.NewBase(google.Config{
			Name:     name,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     pool,
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return name }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolGoogle }
func (p *Provider) Models() []string            { return p.cfg.Models }
func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }
func (p *Provider) Close() error                          { return p.base.Close() }

func init() { provider.RegisterGlobalWithProtocol(name, New, provider.ProtocolGoogle) }
