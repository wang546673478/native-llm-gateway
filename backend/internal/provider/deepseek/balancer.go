// P68 + P-quota-balance: DeepSeek quota balance 探测
// https://api-docs.deepseek.com/api-get-user-balance
// GET {endpoint}/user/balance
// 响应:{
//   "is_available": true,
//   "balance_infos": [
//     {
//       "currency": "CNY",
//       "total_balance": "110.00",
//       "granted_balance": "10.00",
//       "topped_up_balance": "100.00"
//     }
//   ]
// }
//
// 之前(本文件最初版本)把字段名错记成 `balance`(string 数组),实际是
// `balance_infos`(object 数组)。这造成 P-quota-balance 启用后,每次 poll
// 都拿到 raw=0,HasQuota=false,key 被反复标 QUOTA_EXCEEDED,前端永远红 ¥0.00。
// 修:按官方 schema 重写,所有金额从 string 解析成 float;按 currency 汇总
// 出 Raw(同时支持 CNY + USD 账户的情况,sum 两个 currency 的 total_balance,
// HasQuota 用 is_available 判定)。
package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

// deepseekBalancer GET /user/balance,解析 balance_infos 数组
type deepseekBalancer struct {
	client *http.Client
}

func newDeepseekBalancer() *deepseekBalancer {
	return &deepseekBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

// deepseekBalanceResp 按 DeepSeek 官方 schema:is_available + balance_infos 数组
type deepseekBalanceResp struct {
	IsAvailable  bool                 `json:"is_available"`
	BalanceInfos []deepseekBalanceRow `json:"balance_infos"`
}

// deepseekBalanceRow 单个 currency 桶;所有 amount 是 string(避免浮点丢精度)。
type deepseekBalanceRow struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

func (b *deepseekBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	// P-quota-balance:DeepSeek 的 quota 端点固定是 https://api.deepseek.com/user/balance,
	// 不管 anthropic 还是 openai 协议线 — 用同一个账号余额。config 里的 endpoint 包含 path
	//(e.g. "/anthropic"),需要剥 path 只取 scheme+host,否则会拼成不存在的 URL。
	// 兜底是官方 host,允许测试用 httptest server 覆盖(传任意 URL 即可)。
	host := "https://api.deepseek.com"
	if u, err := url.Parse(baseURL); err == nil && u.Scheme != "" && u.Host != "" {
		host = u.Scheme + "://" + u.Host
	}
	url := host + "/user/balance"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "deepseek:/user/balance",
			Kind:     "currency",
		}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// key 废了
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "deepseek:/user/balance",
			Kind:     "currency",
		}, fmt.Errorf("deepseek balance auth: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "deepseek:/user/balance",
			Kind:     "currency",
		}, fmt.Errorf("deepseek balance http %d", resp.StatusCode)
	}

	var parsed deepseekBalanceResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "deepseek:/user/balance",
			Kind:     "currency",
		}, err
	}

	// 汇总所有 currency 的 total_balance(账户可能同时有 CNY + USD)
	var raw float64
	for _, row := range parsed.BalanceInfos {
		v, err := strconv.ParseFloat(row.TotalBalance, 64)
		if err != nil {
			continue // 单 currency 解析失败不影响其他
		}
		raw += v
	}

	return quotacheck.Balance{
		Raw:      raw,
		HasQuota: parsed.IsAvailable && raw > 0,
		Source:   "deepseek:/user/balance",
		Kind:     "currency",
	}, nil
}

func init() {
	b := newDeepseekBalancer()
	quotacheck.RegisterBalancer("deepseek", b)
	quotacheck.RegisterBalancer("deepseek-anthropic", b)
}
