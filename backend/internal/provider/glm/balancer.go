// P68 + B-glm-quota: GLM 额度查询 — 官方插件(zai-org/zai-coding-plugins
// glm-plan-usage)暴露的 monitor 端点,标准 API key 也能用(2026-08-05 实测)。
//
//	GET {host}/api/monitor/usage/quota/limit
//
//	响应:{
//	  "code":200,"msg":"操作成功","success":true,
//	  "data":{"level":"lite","limits":[
//	    {"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":2000,
//	     "currentValue":2035,"remaining":0,"percentage":100,
//	     "nextResetTime":1785910103775},
//	    ...
//	  ]}
//	}
//
// 语义:
//   - limits[] 是多个滚动额度窗(unit=小时);任一窗耗尽,请求就会失败(1113 余额不足)
//   - percentage = 已用百分比(100 = 耗尽);remaining = 窗内剩余额度
//   - HasQuota = 所有窗都有剩余(max(percentage) < 100)
//   - Raw = 最紧窗口的「剩余百分比」(100 - 最大已用)— 与 minimax balancer 同语义:
//     percent 类型的 Raw 一律是剩余百分比(0 = 耗尽,100 = 满),UI 直接展示
//   - 认证:Authorization 直接放原始 token(官方插件即此用法;实测 Bearer 也可)
package glm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

// glmBalancer GET /api/monitor/usage/quota/limit,解析 limits 数组
type glmBalancer struct {
	client *http.Client
}

func newGlmBalancer() *glmBalancer {
	return &glmBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

// glmQuotaResp 官方 monitor 端点响应
type glmQuotaResp struct {
	Code    int          `json:"code"`
	Success bool         `json:"success"`
	Data    glmQuotaData `json:"data"`
}

type glmQuotaData struct {
	Level  string          `json:"level"`
	Limits []glmQuotaLimit `json:"limits"`
}

// glmQuotaLimit 单个滚动额度窗
type glmQuotaLimit struct {
	Type          string  `json:"type"` // CREDIT_LIMIT / TOKENS_LIMIT / TIME_LIMIT
	Unit          int     `json:"unit"` // 窗口小时数
	Usage         float64 `json:"usage"`
	CurrentValue  float64 `json:"currentValue"`
	Remaining     float64 `json:"remaining"`
	Percentage    float64 `json:"percentage"` // 已用百分比,100 = 耗尽
	NextResetTime int64   `json:"nextResetTime"`
}

func (b *glmBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	// monitor 端点与 provider endpoint 同 host、不同 path 根(/api/paas/v4 或
	// /api/anthropic)→ 剥 path 只取 scheme+host,否则会拼成不存在的 URL。
	// 兜底是官方 host,允许测试用 httptest server 覆盖(传任意 URL 即可)。
	host := "https://open.bigmodel.cn"
	if u, err := url.Parse(baseURL); err == nil && u.Scheme != "" && u.Host != "" {
		host = u.Scheme + "://" + u.Host
	}
	u := host + "/api/monitor/usage/quota/limit"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	// 官方插件用法:Authorization 直接放原始 token(实测 Bearer 也可)。
	// 与请求路径(openai_compatible Bearer)及 deepseek/minimax balancer 对齐,统一 Bearer,
	// 避免同一 k.Key 监控端点与请求面鉴权头不一致(上游收紧 auth 时 balancer 才不先挂)。
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/monitor/usage/quota/limit",
			Kind:     "percent",
		}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// key 废了
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "glm:/api/monitor/usage/quota/limit",
			Kind:     "percent",
		}, fmt.Errorf("glm quota auth: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/monitor/usage/quota/limit",
			Kind:     "percent",
		}, fmt.Errorf("glm quota http %d", resp.StatusCode)
	}

	var parsed glmQuotaResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/monitor/usage/quota/limit",
			Kind:     "percent",
		}, err
	}
	if parsed.Code != 200 || !parsed.Success {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "glm:/api/monitor/usage/quota/limit",
			Kind:     "percent",
		}, fmt.Errorf("glm quota code %d: %s", parsed.Code, body)
	}

	// 任一窗耗尽(percentage>=100)→ 请求会 1113 → HasQuota=false;
	// Raw = 最紧窗口的剩余百分比(100-已用;limits 为空 = 无窗口限制 → 100% 可用)
	var maxUsedPct float64
	for _, l := range parsed.Data.Limits {
		if l.Percentage > maxUsedPct {
			maxUsedPct = l.Percentage
		}
	}
	return quotacheck.Balance{
		Raw:      100 - maxUsedPct,
		HasQuota: maxUsedPct < 100,
		Source:   "glm:/api/monitor/usage/quota/limit",
		Kind:     "percent",
	}, nil
}

func init() {
	b := newGlmBalancer()
	quotacheck.RegisterBalancer("glm", b)
	quotacheck.RegisterBalancer("glm-anthropic", b)
}
