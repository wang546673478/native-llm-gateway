package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// 2026-08-04: DeepSeek 官方文档修订后真实字段是 is_available + balance_infos (object 数组),
// 之前版本把这个字段名错记成 balance (string 数组)。修后的 parser 对应真实 schema。

func TestDeepseekBalancer_ParsesBalance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/balance", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available": true,
			"balance_infos": []map[string]interface{}{
				{
					"currency":          "CNY",
					"total_balance":     "110.00",
					"granted_balance":   "10.00",
					"topped_up_balance": "100.00",
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "good-key"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true (total_balance=110.00)")
	}
	if got.Raw != 110.00 {
		t.Errorf("Raw = %v, want 110.00", got.Raw)
	}
}

func TestDeepseekBalancer_MultiCurrencySums(t *testing.T) {
	// CNY + USD 两个 currency 同时存在,Raw 是它们的 total_balance 之和。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available": true,
			"balance_infos": []map[string]interface{}{
				{"currency": "CNY", "total_balance": "50.00"},
				{"currency": "USD", "total_balance": "30.50"},
			},
		})
	}))
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Raw != 80.50 {
		t.Errorf("Raw = %v, want 80.50 (50.00 CNY + 30.50 USD)", got.Raw)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true")
	}
}

func TestDeepseekBalancer_ZeroBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available": false,
			"balance_infos": []map[string]interface{}{
				{"currency": "CNY", "total_balance": "0"},
			},
		})
	}))
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (is_available=false)")
	}
	if got.Raw != 0 {
		t.Errorf("Raw = %v, want 0", got.Raw)
	}
}

func TestDeepseekBalancer_EmptyBalanceInfos(t *testing.T) {
	// is_available=true 但 balance_infos 为空(账户无任何 currency)
	// Raw 应为 0,HasQuota 应为 false。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available":  true,
			"balance_infos": []map[string]interface{}{},
		})
	}))
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (raw=0)")
	}
}

func TestDeepseekBalancer_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()

	b := newDeepseekBalancer()
	_, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "bad-key"})
	if err == nil {
		t.Error("expected error on 401")
	}
}

func TestDeepseekBalancer_MalformedAmountSkipped(t *testing.T) {
	// 某个 currency 的 total_balance 不是合法数字 — 跳过那个,其他汇总仍 OK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available": true,
			"balance_infos": []map[string]interface{}{
				{"currency": "CNY", "total_balance": "12.50"},
				{"currency": "USD", "total_balance": "not-a-number"},
			},
		})
	}))
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Raw != 12.50 {
		t.Errorf("Raw = %v, want 12.50 (parse failure of USD skipped)", got.Raw)
	}
	_ = time.Second
}
