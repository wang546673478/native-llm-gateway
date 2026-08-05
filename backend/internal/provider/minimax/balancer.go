// P68 + P-quota-balance: MiniMax token-plan quota 探测
// GET https://www.minimaxi.com/v1/token_plan/remains
//
// 实测响应(2026-08-04):
//   {
//     "model_remains": [
//       {
//         "model_name": "general",
//         "current_interval_total_count": 0,
//         "current_interval_usage_count": 0,
//         "current_interval_status": 1,
//         "current_interval_remaining_percent": 43,
//         "current_weekly_total_count": 0,
//         "current_weekly_usage_count": 0,
//         "current_weekly_status": 1,
//         "current_weekly_remaining_percent": 76,
//         "weekly_boost_permille": 1500,
//         "start_time": ...,
//         "end_time": ...,
//         "remains_time": ...,
//         "weekly_start_time": ...,
//         "weekly_end_time": ...,
//         "weekly_remains_time": ...
//       },
//       ... 其他模型(每个模型一条)
//     ],
//     "base_resp": {
//       "status_code": 0,
//       "status_msg": "success"
//     }
//   }
//
// 关键 schema 笔记:
//   - 没有 quota_remaining / remains / balance / available 这些字段,那些是早期猜的。
//   - 鉴权失败:HTTP 仍返 200,真正的 status_code 在 body.base_resp.status_code 里(非 0 即失败)。
//   - 余额单位是百分比,不是 CNY/USD 金额;每个模型一行,current_interval_remaining_percent 是当前 5h 窗口剩余百分比。
//
// 决策:
//   - Raw = MIN(current_interval_remaining_percent)跨所有模型(最严约束,任一模型耗光就算)。
//     单位是百分比(0-100),UI 颜色阈值 10% 直接可用。
//   - HasQuota = is_available(base_resp.status_code==0) && Raw > 0。
//   - model_remains 为空(账户无可用模型)视作 Raw=0、HasQuota=false。
//
// 官方文档(2026-08-04 调研):无公开余额/套餐额度查询 REST API —
// 余额与 Token Plan 用量只能在 Web 控制台查看;API 侧只能靠错误码被动感知:
//   base_resp.status_code = 1008(余额不足)/ 2056(超出 Token Plan 限制)
// 本 balancer 使用的 https://www.minimaxi.com/v1/token_plan/remains 为未文档化端点,
// 实测有效(2026-08-04),官方不保证稳定性;如失效,可降级为错误码驱动。

package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

// miniMaxBalancer 拉 MiniMax token-plan 余额(取百分比最严约束)
type miniMaxBalancer struct {
	client *http.Client
}

func newMiniMaxBalancer() *miniMaxBalancer {
	return &miniMaxBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

// miniMaxModelRemains 单个模型当前的余额窗口(5h + 周)
type miniMaxModelRemains struct {
	ModelName                       string  `json:"model_name"`
	CurrentIntervalRemainingPercent float64 `json:"current_interval_remaining_percent"`
	CurrentWeeklyRemainingPercent   float64 `json:"current_weekly_remaining_percent"`
}

// miniMaxBalanceResp 整段响应;base_resp 字段承载鉴权 / 业务 status
type miniMaxBalanceResp struct {
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	ModelRemains []miniMaxModelRemains `json:"model_remains"`
}

// quotaHostOverride 允许测试 redirect 到 httptest server(生产保持空 → 用 canonical)。
// 用 package-level 而非实例字段,因为 Balancer 注册在 init() 不方便构造时注入。
var quotaHostOverride string

func (b *miniMaxBalancer) FetchBalance(ctx context.Context, _ string, k *keypool.Key) (quotacheck.Balance, error) {
	// P-quota-balance:MiniMax 配额端点和 chat 端点 host 不同:
	//   chat   = https://api.minimaxi.com/anthropic   (config 里的 endpoint)
	//   quota  = https://www.minimaxi.com/v1/token_plan/remains
	// 所以 baseURL 参数(配置里的 chat endpoint)不能直接用 — 直接 hardcode 配额 URL。
	// 测试用 quotaHostOverride 覆盖。
	host := quotaHostOverride
	if host == "" {
		host = "https://www.minimaxi.com"
	}
	url := host + "/v1/token_plan/remains"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, fmt.Errorf("minimax quota auth: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, fmt.Errorf("minimax quota http %d", resp.StatusCode)
	}

	var parsed miniMaxBalanceResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, err
	}

	// 鉴权/业务错误承载在 body 里,不是 HTTP status — 这种 case 必须显式返 err
	// 不然会被外层 pollAllBalancers 当作 "ok 但 HasQuota=false" 走 default 分支,看不到真实错误。
	if parsed.BaseResp.StatusCode != 0 {
		return quotacheck.Balance{
				Raw:      0,
				HasQuota: false,
				Source:   "minimax:/v1/token_plan/remains",
				Kind:     "percent",
			}, fmt.Errorf("minimax base_resp status_code=%d msg=%s",
				parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}

	// 没有 model_remains — 账户无任何可用模型(理论上 Token Plan 至少 "general")。
	if len(parsed.ModelRemains) == 0 {
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, nil
	}

	// Raw = MIN(current_interval_remaining_percent)跨所有模型
	// 含义:账户中"最紧"的模型剩余百分比 — 任一模型 0% 即视为账户不可用
	minPct := 100.0
	for _, m := range parsed.ModelRemains {
		if m.CurrentIntervalRemainingPercent < minPct {
			minPct = m.CurrentIntervalRemainingPercent
		}
	}

	return quotacheck.Balance{
		Raw: minPct,
		// P-quota-prefer: 阈值对齐 chat API 行为 — MiniMax 对 1% 余额的 key
		// 直接报 429 "已达到 Token Plan 用量上限 (2056)"(实测 2026-08-06,
		// weige 1% 被拒),所以 1% 视为耗尽;配合 poll 连续 2 轮确认标 QE
		HasQuota: minPct > 1,
		Source:   "minimax:/v1/token_plan/remains",
		Kind:     "percent",
	}, nil
}

func init() {
	b := newMiniMaxBalancer()
	quotacheck.RegisterBalancer("minimax", b)
	quotacheck.RegisterBalancer("minimax-openai", b)
}
