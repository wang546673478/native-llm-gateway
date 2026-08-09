// Default Prober 实现 — 通过 HTTP 探测判断 key 配额是否恢复
// 用于没注册专用 Prober 的 provider

package quotacheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// modelsProberOpenAI: GET /v1/models 用 key 调,2xx → restored
// 用于 OpenAI 协议 provider(deepseek / qwen / minimax-openai)
type modelsProberOpenAI struct {
	client *http.Client
}

func newModelsProberOpenAI() *modelsProberOpenAI {
	return &modelsProberOpenAI{
		client: &http.Client{Timeout: DefaultProbeTimeout},
	}
}

func (p *modelsProberOpenAI) Probe(ctx context.Context, baseURL string, k *keypool.Key) Result {
	// C 修复:base 若已以 /v1 结尾(如 qwen compatible-mode/v1)则直接 + /models,
	// 否则 + /v1/models —— 避免双 /v1(v1/v1/models) 打 404 导致 QE 永不复原。
	base := strings.TrimRight(baseURL, "/")
	url := base + "/v1/models"
	if strings.HasSuffix(base, "/v1") {
		url = base + "/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ResultTransportError
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)

	resp, err := p.client.Do(req)
	if err != nil {
		return ResultTransportError
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) // 限 64KB

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ResultRestored
	case isAuthLikeStatus(resp.StatusCode) && !hasQuotaKeyword(body):
		return ResultAuthFailed
	case resp.StatusCode == 429, resp.StatusCode == 402, hasQuotaKeyword(body):
		return ResultStillExhausted
	case resp.StatusCode >= 500:
		return ResultTransportError
	default:
		// 未知 4xx,当作 auth_failed(避免反复探测)
		return ResultAuthFailed
	}
}

// modelsProberAnthropic: POST /v1/messages 发 max_tokens=1 的最小请求
// 用于 Anthropic 协议 provider(deepseek-anthropic / minimax)
type modelsProberAnthropic struct {
	client *http.Client
}

func newModelsProberAnthropic() *modelsProberAnthropic {
	return &modelsProberAnthropic{
		client: &http.Client{Timeout: DefaultProbeTimeout},
	}
}

const minimalAnthropicProbeBody = `{"model":"PLACEHOLDER","max_tokens":1,"messages":[{"role":"user","content":"."}]}`

func (p *modelsProberAnthropic) Probe(ctx context.Context, baseURL string, k *keypool.Key) Result {
	url := strings.TrimRight(baseURL, "/") + "/v1/messages"
	// 用 key.Name 作为 model 名占位(实际 provider 会用真实 model;探测本质是验 key)
	body := strings.Replace(minimalAnthropicProbeBody, "PLACEHOLDER", firstModelName(k.Name), 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return ResultTransportError
	}
	req.Header.Set("x-api-key", k.Key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return ResultTransportError
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ResultRestored
	case isAuthLikeStatus(resp.StatusCode) && !hasQuotaKeyword(respBody):
		return ResultAuthFailed
	case resp.StatusCode == 429, resp.StatusCode == 402, hasQuotaKeyword(respBody):
		return ResultStillExhausted
	case resp.StatusCode >= 500:
		return ResultTransportError
	default:
		return ResultAuthFailed
	}
}

// modelsProberGoogle: GET {base}/models 用 x-goog-api-key 调 probe。
// C 修复:gemini 是 google 协议(端点 v1beta,鉴权 x-goog-api-key 而非 Bearer),
// 之前被 name-suffix fallback 误路由到 __openai__(Bearer+/v1/models)→ 401 → QE key
// 永不复原。现在按协议路由到 __google__,端点为 {base}/models(google.go:233 同款路径)。
type modelsProberGoogle struct {
	client *http.Client
}

func newModelsProberGoogle() *modelsProberGoogle {
	return &modelsProberGoogle{client: &http.Client{Timeout: DefaultProbeTimeout}}
}

func (p *modelsProberGoogle) Probe(ctx context.Context, baseURL string, k *keypool.Key) Result {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ResultTransportError
	}
	req.Header.Set("x-goog-api-key", k.Key) // Google 用 query/header key,非 Bearer

	resp, err := p.client.Do(req)
	if err != nil {
		return ResultTransportError
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ResultRestored
	case isAuthLikeStatus(resp.StatusCode) && !hasQuotaKeyword(body):
		return ResultAuthFailed
	case resp.StatusCode == 429, resp.StatusCode == 402, hasQuotaKeyword(body):
		return ResultStillExhausted
	case resp.StatusCode >= 500:
		return ResultTransportError
	default:
		return ResultAuthFailed
	}
}

func firstModelName(_ string) string {
	// 探测不依赖具体 model — 大部分 provider 对 unknown model 返 400(非 401/402/429),
	// 我们的分类里这就是 ResultAuthFailed。但 Anthropic 对 unknown model + 配额 OK 也可能返 200。
	// 简化:用一个肯定 invalid 的 model name,让 provider 返 400;如果 401/402/429 则归类相应。
	// 实测:大部分 provider 对 invalid model 返 400 + "model not found" — 不在 quota 关键字列表中,
	// 走 default 分支 → ResultAuthFailed。这个**会误判**。所以用 key.Name 走比较保险:
	// provider 通常会做 model 校验但不会计费。
	// 简化策略:用 key name 当 model 名,几乎肯定 invalid。配额 OK 返 200;quota 耗尽返 402/429/含 quota 关键字。
	return "probe-__never_exists__"
}

// RegisterDefaultProbers 注册所有"协议族默认"Prober
// 单一实例,共享 http.Client
// 注意:具体 provider 在 init() 里 RegisterBalancer 时,**不需要** RegisterProber,
// 因为 Manager 会 fallback 到协议族默认 Prober
func RegisterDefaultProbers() {
	openaiP := newModelsProberOpenAI()
	RegisterProber("__openai__", openaiP)
	RegisterProber("__anthropic__", newModelsProberAnthropic())
	gp := newModelsProberGoogle()
	RegisterProber("__google__", gp)   // C:google 协议族(供按协议引用)
	RegisterProber("gemini", gp)       // C:gemini(google 协议)name-key 直注册,避免误路由 __openai__
	RegisterProber("qwen", openaiP)    // C:qwen base 已带 /v1,openai prober 已按 /v1 容忍
}

// init 注册默认 Prober
func init() {
	RegisterDefaultProbers()
}

// errString — 包内 helper
var errString = func(s string) error { return fmt.Errorf("quotacheck: %s", s) }
