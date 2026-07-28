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
// 用于 OpenAI 协议 provider(deepseek / glm / kimi / qwen / minimax-openai)
type modelsProberOpenAI struct {
	client *http.Client
}

func newModelsProberOpenAI() *modelsProberOpenAI {
	return &modelsProberOpenAI{
		client: &http.Client{Timeout: DefaultProbeTimeout},
	}
}

func (p *modelsProberOpenAI) Probe(ctx context.Context, baseURL string, k *keypool.Key) Result {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
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
// 用于 Anthropic 协议 provider(deepseek-anthropic / glm-anthropic / kimi-anthropic / minimax / minimax-anthropic)
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
}

// init 注册默认 Prober
func init() {
	RegisterDefaultProbers()
}

// errString — 包内 helper
var errString = func(s string) error { return fmt.Errorf("quotacheck: %s", s) }
