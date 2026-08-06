// Package mimo — 余额/套餐额度查询 balancer
//
// 背景:MIMO 官方文档无公开余额/套餐额度 REST API(只能看 Web 控制台)。
// 社区逆向出控制台内部端点(https://github.com/farion1231/cc-switch/issues/2488,
// 2026-04~08 多人实测有效):
//   - 套餐用量:GET https://platform.xiaomimimo.com/api/v1/tokenPlan/usage
//   - 按量余额:GET https://platform.xiaomimimo.com/api/v1/balance
//
// 关键约束:
//   - 鉴权是控制台登录 cookie(账号级,非 API key)— 全量 Cookie header 注入,
//     约 1 天过期;过期/失效后 401,轮询退化(不标耗尽、不降档,保守方向),
//     日志提示重新上传 cookie
//   - 未文档化端点,官方不保证稳定;失效可降级为错误码驱动(402/429),同 minimax 先例
//
// 实测响应 schema(2026-08-07,真实账号):
//   usage:
//     {"code":0,"message":"","data":{"usage":{"percent":0.00,"items":[
//       {"name":"plan_total_token","used":0,"limit":11000000000,"percent":0.00},
//       {"name":"compensation_total_token","used":0,"limit":0,"percent":0}]}}}
//   balance:
//     {"code":0,"message":"","data":{"balance":"5.00","frozenBalance":"0.00",
//      "currency":"CNY","giftBalance":"5.00","cashBalance":"0.00"}}
//   balance 字段是字符串;percent 是 0~1 小数。
//
// 决策:
//   - 按 key.BillingSource 分端点:token_plan key → usage 端点(套餐剩余),
//     api key → balance 端点(现金余额)。不能在注册名上分 — poll 循环按
//     vendor pool 去重,同一 vendor 共享 pool 时随机取一个注册名查 balancer,
//     注册名分端点会把 tp-/sk- key 带错端点(2026-08-07 设计修正)
//   - HasQuota:usage → 套餐还有剩余。**percent 是「已用比例」不是「剩余比例」**
//     (实测:used=0 / limit=11B 时 percent=0.00 = 用了 0%,剩余 100%;方向反了
//     会把未用过的套餐判成耗尽 — 2026-08-07 用户实测发现)。剩余% 用
//     items.plan_total_token 的 (limit-used)/limit 算;补偿积分(compensation_
//     total_token)有剩余也计 HasQuota。Raw = 主套餐剩余%(0-100,Kind "percent")
//   - balance → balance > 0,Raw = balance,Kind "currency"
//   - cookie 未配置 → 查询返回错误(poll 跳过、CheckQuota 按未耗尽,保守)
//   - 401/403 → 错误(提示重新上传 cookie);HTTP 5xx → 错误(瞬态,跳过本轮)
package mimo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

const (
	quotaHost          = "https://platform.xiaomimimo.com"
	tokenPlanUsagePath = "/api/v1/tokenPlan/usage"
	balancePath        = "/api/v1/balance"
)

// quotaHostOverride 允许测试 redirect 到 httptest server(生产保持空 → 用 canonical)。
// 同 minimax 的 quotaHostOverride 模式。
var quotaHostOverride string

// quotaCookie 控制台登录 cookie(账号级,全量 Cookie header)。atomic.Value:
// poll goroutine 并发读 + 运行时热更新(API/重启注入)写,免锁。
// 约 1 天过期;空 = balancer 停用(查询返回错误,保守方向)。
var quotaCookie atomic.Value

// SetQuotaCookie 注入/更新控制台登录 cookie(传空串 = 停用 balancer)
func SetQuotaCookie(cookie string) { quotaCookie.Store(cookie) }

func getQuotaCookie() string {
	if v, ok := quotaCookie.Load().(string); ok {
		return v
	}
	return ""
}

func quotaBase() string {
	if quotaHostOverride != "" {
		return quotaHostOverride
	}
	return quotaHost
}

// mimoTokenPlanResp usage 端点响应(2026-08-07 实测结构)
type mimoTokenPlanResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Usage *struct {
			Percent float64 `json:"percent"`
			Items   []struct {
				Name    string  `json:"name"`
				Used    float64 `json:"used"`
				Limit   float64 `json:"limit"`
				Percent float64 `json:"percent"`
			} `json:"items"`
		} `json:"usage"`
	} `json:"data"`
}

// mimoBalanceResp balance 端点响应(balance 字段是字符串!)
type mimoBalanceResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Balance     string `json:"balance"`
		Currency    string `json:"currency"`
		CashBalance string `json:"cashBalance"`
		GiftBalance string `json:"giftBalance"`
	} `json:"data"`
}

// mimoQuotaBalancer 单一 balancer,按 key.BillingSource 分端点(见文件头决策)
type mimoQuotaBalancer struct {
	client *http.Client
}

func newMimoQuotaBalancer() *mimoQuotaBalancer {
	return &mimoQuotaBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *mimoQuotaBalancer) FetchBalance(ctx context.Context, _ string, k *keypool.Key) (quotacheck.Balance, error) {
	// token_plan key → 套餐用量端点;api/free/空 → 按量余额端点
	if k != nil && k.BillingSource == "token_plan" {
		return fetchMimoQuota(ctx, b.client, tokenPlanUsagePath, "mimo:/api/v1/tokenPlan/usage")
	}
	return fetchMimoQuota(ctx, b.client, balancePath, "mimo:/api/v1/balance")
}

// fetchMimoQuota 公共查询:注入当前 cookie → 请求 → 按端点解析。
// key 的凭据用不上(cookie 是账号级),签名保持 Balancer 接口兼容。
func fetchMimoQuota(ctx context.Context, client *http.Client, path, source string) (quotacheck.Balance, error) {
	cookie := getQuotaCookie()
	if cookie == "" {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"},
			fmt.Errorf("mimo quota cookie not configured — set providers.mimo.quota_cookie in config.yaml or POST /api/v1/providers/mimo/quota-cookie")
	}
	return fetchMimoQuotaWithCookie(ctx, client, path, cookie, source)
}

// fetchMimoQuotaWithCookie 公共查询主体:用指定 cookie 请求 → 按端点解析。
func fetchMimoQuotaWithCookie(ctx context.Context, client *http.Client, path, cookie, source string) (quotacheck.Balance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaBase()+path, nil)
	if err != nil {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"}, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "native-llm-gateway/quota-1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"},
			fmt.Errorf("mimo quota cookie expired/invalid (HTTP %d) — re-upload cookie at providers.mimo.quota_cookie", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"},
			fmt.Errorf("mimo quota endpoint HTTP %d", resp.StatusCode)
	}

	if path == tokenPlanUsagePath {
		var r mimoTokenPlanResp
		if err := json.Unmarshal(body, &r); err != nil {
			return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"},
				fmt.Errorf("mimo usage parse: %w", err)
		}
		if r.Code != 0 {
			return quotacheck.Balance{HasQuota: false, Source: source, Kind: "percent"},
				fmt.Errorf("mimo usage code=%d msg=%q", r.Code, r.Message)
		}
		// percent 字段是「已用比例」:used=0 / limit=11B 时 percent=0.00(用了 0%)。
		// 剩余% 以 items 为准:(limit-used)/limit;主套餐(plan_total_token)做 Raw,
		// 补偿积分(compensation_total_token)有剩余也计入 HasQuota。
		mainRemaining := 0.0
		compRemaining := 0.0
		if r.Data.Usage != nil {
			for _, item := range r.Data.Usage.Items {
				if item.Limit <= 0 {
					continue
				}
				rem := (item.Limit - item.Used) / item.Limit * 100
				switch item.Name {
				case "plan_total_token":
					mainRemaining = rem
				case "compensation_total_token":
					compRemaining = rem
				}
			}
		}
		return quotacheck.Balance{
			// 四舍五入到 2 位小数,避免浮点尾巴(0.55×100=55.00000000000001)
			Raw:      math.Round(mainRemaining*100) / 100,
			HasQuota: mainRemaining > 0 || compRemaining > 0,
			Source:   source,
			Kind:     "percent",
		}, nil
	}

	// balance 端点
	var r mimoBalanceResp
	if err := json.Unmarshal(body, &r); err != nil {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "currency"},
			fmt.Errorf("mimo balance parse: %w", err)
	}
	if r.Code != 0 {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "currency"},
			fmt.Errorf("mimo balance code=%d msg=%q", r.Code, r.Message)
	}
	bal, err := strconv.ParseFloat(r.Data.Balance, 64)
	if err != nil {
		return quotacheck.Balance{HasQuota: false, Source: source, Kind: "currency"},
			fmt.Errorf("mimo balance parse %q: %w", r.Data.Balance, err)
	}
	return quotacheck.Balance{
		Raw:      bal,
		HasQuota: bal > 0,
		Source:   source,
		Kind:     "currency",
	}, nil
}

// ValidateQuotaCookie 验证候选 cookie 是否有效(上传/更新前调用)。
// 用候选 cookie 打一次 usage 端点:HTTP 200 + code 0 = 有效(套餐额度为 0 也算有效,
// 只验证凭据,不验证额度)。失败返回错误,调用方不应持久化。
func ValidateQuotaCookie(ctx context.Context, cookie string) error {
	if cookie == "" {
		return fmt.Errorf("empty cookie")
	}
	b := newMimoQuotaBalancer()
	_, err := fetchMimoQuotaWithCookie(ctx, b.client, tokenPlanUsagePath, cookie, "mimo:/validate")
	return err
}

// init 注册 balancer — 4 个注册名都注册同一个实例(vendor 共享 pool,
// poll 去重后随机取一个注册名查 balancer,端点由 key.BillingSource 决定)
func init() {
	b := newMimoQuotaBalancer()
	quotacheck.RegisterBalancer(name, b)
	quotacheck.RegisterBalancer(anthropicName, b)
	quotacheck.RegisterBalancer(tokenPlanName, b)
	quotacheck.RegisterBalancer(tokenPlanAnthropicName, b)
}
