package mimo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// withMockQuotaHost redirects the hardcoded MIMO console URL to a httptest server.
func withMockQuotaHost(t *testing.T, srvURL string) (restore func()) {
	t.Helper()
	prev := quotaHostOverride
	quotaHostOverride = srvURL
	return func() { quotaHostOverride = prev }
}

// withQuotaCookie sets the account cookie and restores the previous value on cleanup.
func withQuotaCookie(t *testing.T, cookie string) (restore func()) {
	t.Helper()
	prev := getQuotaCookie()
	SetQuotaCookie(cookie)
	return func() { SetQuotaCookie(prev) }
}

// 2026-08-07 实测 MIMO 控制台响应 schema(真实账号):
//   usage:   {"code":0,"message":"","data":{"usage":{"percent":0.00,"items":[
//              {"name":"plan_total_token","used":0,"limit":11000000000,"percent":0.00},
//              {"name":"compensation_total_token","used":0,"limit":0,"percent":0}]}}}
//   balance: {"code":0,"message":"","data":{"balance":"5.00","currency":"CNY",
//              "giftBalance":"5.00","cashBalance":"0.00"}}
// 鉴权失败:HTTP 401 直接返回 {"code":401,"loginUrl":"..."}。

// usageResp 按真实 schema 构造:percent = 已用比例(used/limit),非剩余比例!
// 实测(2026-08-07):used=0 / limit=11B 时 percent=0.00 = 用了 0%,剩余 100%。
func usageResp(planUsed, planLimit, compUsed, compLimit float64) string {
	planPct := 0.0
	if planLimit > 0 {
		planPct = planUsed / planLimit
	}
	compPct := 0.0
	if compLimit > 0 {
		compPct = compUsed / compLimit
	}
	b, _ := json.Marshal(map[string]any{
		"code": 0, "message": "",
		"data": map[string]any{
			"usage": map[string]any{
				"percent": planPct,
				"items": []map[string]any{
					{"name": "plan_total_token", "used": planUsed, "limit": planLimit, "percent": planPct},
					{"name": "compensation_total_token", "used": compUsed, "limit": compLimit, "percent": compPct},
				},
			},
		},
	})
	return string(b)
}

func balanceResp(balance string) string {
	b, _ := json.Marshal(map[string]any{
		"code": 0, "message": "",
		"data": map[string]any{
			"balance": balance, "currency": "CNY",
			"giftBalance": "5.00", "cashBalance": "0.00",
		},
	})
	return string(b)
}

// 用户实测场景(2026-08-07):套餐 11B credits 完全未用 → percent=0.00(已用 0%)
func TestMimoTokenPlanBalancer_FullPlanUnused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPlanUsagePath {
			http.NotFound(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Cookie"), "api-platform_serviceToken") {
			http.Error(w, `{"code":401,"loginUrl":"..."}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(usageResp(0, 11000000000, 0, 0))) // 未用,percent=0
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err != nil {
		t.Fatalf("FetchBalance err: %v", err)
	}
	if !bal.HasQuota {
		t.Error("HasQuota = false, want true (unused plan = 100% remaining)")
	}
	if bal.Raw != 100 {
		t.Errorf("Raw = %v, want 100 (unused plan)", bal.Raw)
	}
	if bal.Kind != "percent" {
		t.Errorf("Kind = %q, want percent", bal.Kind)
	}
	if bal.Source != "mimo:/api/v1/tokenPlan/usage" {
		t.Errorf("Source = %q", bal.Source)
	}
}

// 部分消耗:used 45% → percent=0.45,剩余 55%
func TestMimoTokenPlanBalancer_PartialUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(usageResp(4.95e9, 11e9, 0, 0))) // 用了 45%
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err != nil {
		t.Fatalf("FetchBalance err: %v", err)
	}
	if !bal.HasQuota {
		t.Error("HasQuota = false, want true (45% used, 55% remaining)")
	}
	if bal.Raw != 55 {
		t.Errorf("Raw = %v, want 55", bal.Raw)
	}
}

// 套餐耗尽:used = limit → percent=1.0,剩余 0%
func TestMimoTokenPlanBalancer_Exhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(usageResp(11e9, 11e9, 0, 0))) // used = limit
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err != nil {
		t.Fatalf("FetchBalance err: %v", err)
	}
	if bal.HasQuota {
		t.Error("HasQuota = true, want false (0% remaining)")
	}
	if bal.Raw != 0 {
		t.Errorf("Raw = %v, want 0", bal.Raw)
	}
}

// 主套餐耗尽但补偿积分还有 → 仍算有额度(不变式:token_plan 未耗尽不落 api)
func TestMimoTokenPlanBalancer_CompensationRemaining(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(usageResp(11e9, 11e9, 5e8, 1e9))) // 主套餐 0%,补偿 50%
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err != nil {
		t.Fatalf("FetchBalance err: %v", err)
	}
	if !bal.HasQuota {
		t.Error("HasQuota = false, want true (compensation credits remain)")
	}
	if bal.Raw != 0 {
		t.Errorf("Raw = %v, want 0 (main plan exhausted; display shows main plan)", bal.Raw)
	}
}

func TestMimoBalanceBalancer_ParsesRealSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != balancePath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(balanceResp("5.00"))) // 余额 ¥5
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	// api 层 key(空 BillingSource 也算 api)
	for _, bs := range []string{"api", ""} {
		bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "2", BillingSource: bs})
		if err != nil {
			t.Fatalf("FetchBalance err: %v", err)
		}
		if !bal.HasQuota {
			t.Error("HasQuota = false, want true (¥5 balance)")
		}
		if bal.Raw != 5.0 {
			t.Errorf("Raw = %v, want 5.0", bal.Raw)
		}
		if bal.Kind != "currency" {
			t.Errorf("Kind = %q, want currency", bal.Kind)
		}
	}
}

func TestMimoBalanceBalancer_ZeroBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(balanceResp("0.00")))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="good"`))

	b := newMimoQuotaBalancer()
	bal, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "2", BillingSource: "api"})
	if err != nil {
		t.Fatalf("FetchBalance err: %v", err)
	}
	if bal.HasQuota {
		t.Error("HasQuota = true, want false (¥0 balance)")
	}
}

func TestMimoQuotaBalancer_CookieExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":401,"loginUrl":"..."}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))
	t.Cleanup(withQuotaCookie(t, `api-platform_serviceToken="stale"`))

	b := newMimoQuotaBalancer()
	_, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err == nil {
		t.Fatal("expected error for 401 (cookie expired), got nil")
	}
	if !strings.Contains(err.Error(), "expired/invalid") {
		t.Errorf("error = %q, want 'expired/invalid' hint", err)
	}
}

func TestMimoQuotaBalancer_NoCookie(t *testing.T) {
	// 不设置 cookie(保持包级初始空值)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request sent despite empty cookie")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMimoQuotaBalancer()
	_, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", BillingSource: "token_plan"})
	if err == nil {
		t.Fatal("expected error when cookie not configured, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q, want 'not configured' hint", err)
	}
}
