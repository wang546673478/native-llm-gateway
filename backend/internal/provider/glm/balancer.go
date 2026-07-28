// P68: GLM (Zhipu) quota usage 探测
// https://bigmodel.cn/dev/api/usage
// GET {endpoint}/api/usage
// 响应:{"code":200,"data":{"limit":3000.0,"used":1500.0,"nextResetTime":"..."}}
//
// 注意:GLM 真实 endpoint 是 https://open.bigmodel.cn/api/paas/v4
// usage API 路径是 /api/usage → 完整 URL: https://open.bigmodel.cn/api/usage
// 因此我们不需要从 cfg.Endpoint 推,直接 hardcode bigmodel.cn 域名
package glm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

// glmBalancer 拉 GLM 套餐余额
type glmBalancer struct {
	client *http.Client
}

func newGLMBalancer() *glmBalancer {
	return &glmBalancer{client: &http.Client{}}
}

// glmUsageResp GLM /api/usage 响应
type glmUsageResp struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Limit float64 `json:"limit"`
		Used  float64 `json:"used"`
	} `json:"data"`
}

func (b *glmBalancer) FetchBalance(ctx context.Context, _ string, k *keypool.Key) (quotacheck.Balance, error) {
	// GLM usage API 用独立的 host,跟 chat endpoint 不同
	// 直接 hardcode 官方域名(覆盖 cfg 传进来的)
	url := "https://open.bigmodel.cn/api/usage"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/usage",
		}, fmt.Errorf("glm usage auth: %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/usage",
		}, fmt.Errorf("glm usage http %d", resp.StatusCode)
	}

	var parsed glmUsageResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{}, err
	}
	if parsed.Code != 200 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/usage",
		}, fmt.Errorf("glm usage code=%d msg=%s", parsed.Code, parsed.Msg)
	}

	remaining := parsed.Data.Limit - parsed.Data.Used
	return quotacheck.Balance{
		Raw:      remaining,
		HasQuota: remaining > 0,
		Source:   strings.TrimPrefix(url, "https://"),
	}, nil
}

// init 注册 Balancer
func init() {
	b := newGLMBalancer()
	quotacheck.RegisterBalancer("glm", b)
	quotacheck.RegisterBalancer("glm-anthropic", b)
}
