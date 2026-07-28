// P68: DeepSeek quota balance 探测
// https://api-docs.deepseek.com/api-get-user-balance
// GET {endpoint}/user/balance
// 响应:{"is_available": true, "balance": ["12.34", "CNY"]}
package deepseek

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

// deepseekBalancer GET /user/balance,解析 balance 数组第一个元素
type deepseekBalancer struct {
	client *http.Client
}

func newDeepseekBalancer() *deepseekBalancer {
	return &deepseekBalancer{client: &http.Client{}}
}

// deepseekBalanceResp 文档实际字段名是 is_available + balance 数组
type deepseekBalanceResp struct {
	IsAvailable bool `json:"is_available"`
	Balance     []string `json:"balance"`
}

func (b *deepseekBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	url := strings.TrimRight(baseURL, "/") + "/user/balance"
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
		// key 废了
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "deepseek:/user/balance",
		}, fmt.Errorf("deepseek balance auth: %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "deepseek:/user/balance",
		}, fmt.Errorf("deepseek balance http %d", resp.StatusCode)
	}

	var parsed deepseekBalanceResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{}, err
	}

	raw := 0.0
	if len(parsed.Balance) > 0 {
		_, _ = fmt.Sscanf(parsed.Balance[0], "%f", &raw)
	}
	return quotacheck.Balance{
		Raw:      raw,
		HasQuota: parsed.IsAvailable && raw > 0,
		Source:   "deepseek:/user/balance",
	}, nil
}

// init 注册 Balancer(给 deepseek + deepseek-anthropic 两条线)
func init() {
	b := newDeepseekBalancer()
	quotacheck.RegisterBalancer("deepseek", b)
	quotacheck.RegisterBalancer("deepseek-anthropic", b)
}
