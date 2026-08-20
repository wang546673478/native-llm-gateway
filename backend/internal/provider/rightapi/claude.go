// Package rightapi — claude 渠道（Anthropic 协议面，注册名 "rightapi-claude"）。
//
// 端点由 config 提供（不含 /v1，anthropic base 拼 /v1/messages）。Right Code 的
// Claude 有多个渠道，endpoint 换一下即可切换：
//   - AWSQ 逆向渠道：https://rightapi.ai/claude-aws（当前配置）
//   - 官渠：        https://rightapi.ai/claude
//   - 官渠特惠：    https://rightapi.ai/claude-sale
//
// 模型：claude-opus-5 / claude-sonnet-4-6 / claude-haiku-4-5 等（各渠道略有差异，
// claude-aws 比官渠少 claude-fable-5）。
//
// 鉴权兼容 Authorization: Bearer <key> 或 x-api-key。
//
// 注意：标准 Anthropic 协议没有模型列表端点，但 Right Code 中转站给 claude 渠道
// 也提供了一个 OpenAI 兼容的 GET /v1/models（返回 {data:[{id}]}）。这里覆盖
// ListModels 消费该端点（而不是继承 anthropic base 的 ErrListModelsNotSupported），
// 否则 SyncVendorModels 永远拉不到 claude 渠道的模型。
package rightapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

// claude 渠道的模型列表端点相对路径（endpoint 不含 /v1，这里拼 /v1/models）。
const claudeModelsPath = "/v1/models"

// claudeProvider Anthropic 协议 Provider（注册名 "rightapi-claude"）。
type claudeProvider struct {
	base   *anthropic_compatible.Base
	cfg    provider.ProviderConfig
	client *http.Client // 模型列表专用（与 base 的请求客户端分开，互不影响）
}

func newClaude(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("rightapi-claude requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("rightapi-claude endpoint is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &claudeProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     claudeName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     cfg.Pool,
			// 不设 ForceThinkingDisabled：Right Code 走真实 Claude 网关（非 DeepSeek
			// 那种 /anthropic 的 thinking 校验），默认透传 thinking。若实测遇到
			// 400 "thinking ... must be passed back" 再补。
		}),
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}, nil
}

func (p *claudeProvider) Name() string                { return claudeName }
func (p *claudeProvider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (p *claudeProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *claudeProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *claudeProvider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

// ListModels 消费 rightapi claude 渠道的 OpenAI 兼容 GET /v1/models 端点。
// 与 codex/grok 面同构：Bearer 鉴权 + {data:[{id}]} 解析。
func (p *claudeProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(p.cfg.Endpoint, "/")+claudeModelsPath, nil)
	if err != nil {
		return nil, err
	}
	if p.cfg.Pool != nil {
		if k, err := p.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic)); err == nil {
			req.Header.Set("Authorization", "Bearer "+k.Key)
			defer p.cfg.Pool.ReportSuccess(k)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("list models: %s %s → status %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (p *claudeProvider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
	p.cfg.Pool = pool // 外层 cfg 同步,ListModels 读 p.cfg.Pool 取 key
}

func (p *claudeProvider) Close() error { return p.base.Close() }
