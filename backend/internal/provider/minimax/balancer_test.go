package minimax

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// withMockQuotaHost redirects the hardcoded MiniMax quota URL to a httptest server.
// Production: leave quotaHostOverride empty (canonical www.minimaxi.com is used).
// Tests: t.Cleanup(thisFn) restores the previous value.
func withMockQuotaHost(t *testing.T, srvURL string) (restore func()) {
	t.Helper()
	prev := quotaHostOverride
	quotaHostOverride = srvURL
	return func() { quotaHostOverride = prev }
}

// 2026-08-04 实测 MiniMax /v1/token_plan/remains 响应 schema:
//   {
//     "model_remains": [{ "model_name": "general",
//                         "current_interval_remaining_percent": 43,
//                         "current_weekly_remaining_percent": 76, ... }],
//     "base_resp": { "status_code": 0, "status_msg": "success" }
//   }
// 鉴权 / 业务错误:HTTP 仍 200,真正信号在 base_resp.status_code。

func TestMiniMaxBalancer_ParsesRealSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{
				{
					"model_name":                         "general",
					"current_interval_remaining_percent": 43.0,
					"current_weekly_remaining_percent":   76.0,
				},
				{
					"model_name":                         "video",
					"current_interval_remaining_percent": 100.0,
					"current_weekly_remaining_percent":   100.0,
				},
			},
			"base_resp": map[string]interface{}{
				"status_code": 0,
				"status_msg":  "success",
			},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "good-key"})
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	// Raw = MIN(43, 100) = 43 — 最严约束
	if got.Raw != 43.0 {
		t.Errorf("Raw = %v, want 43.0 (MIN of all model percentages)", got.Raw)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true (Raw=43 > 0)")
	}
	if got.Kind != "percent" {
		t.Errorf("Kind = %q, want %q", got.Kind, "percent")
	}
}

func TestMiniMaxBalancer_OneModelExhaustedZeroOutsWhole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{
				{"model_name": "general", "current_interval_remaining_percent": 50.0},
				{"model_name": "video", "current_interval_remaining_percent": 0.0},
			},
			"base_resp": map[string]interface{}{"status_code": 0, "status_msg": "success"},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Raw != 0 {
		t.Errorf("Raw = %v, want 0 (MIN of 50,0)", got.Raw)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (video at 0%%)")
	}
}

func TestMiniMaxBalancer_AllModelsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{
				{"model_name": "general", "current_interval_remaining_percent": 0.0},
			},
			"base_resp": map[string]interface{}{"status_code": 0, "status_msg": "success"},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, _ := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "k"})
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (all 0%%)")
	}
}

func TestMiniMaxBalancer_EmptyModelRemains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{},
			"base_resp":     map[string]interface{}{"status_code": 0, "status_msg": "success"},
		})
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Errorf("err = %v, want nil (empty list is valid)", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (no models)")
	}
}

func TestMiniMaxBalancer_BaseRespStatusCodeNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail"},"model_remains":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "k"})
	if err == nil {
		t.Fatal("err = nil, want non-nil (base_resp.status_code != 0 should err)")
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false")
	}
}

func TestMiniMaxBalancer_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	_, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "bad-key"})
	if err == nil {
		t.Error("err = nil, want HTTP 401 err")
	}
}

// P-quota-prefer: 1% 余额 = 耗尽(MiniMax chat API 对 1% 直接报 2056 用量上限)
func TestMiniMaxBalancer_OnePercentIsExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{
				{"model_name": "general", "current_interval_remaining_percent": 1.0},
			},
			"base_resp": map[string]interface{}{"status_code": 0, "status_msg": "success"},
		})
	}))
	defer srv.Close()
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "good-key"})
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (1%% = exhausted per chat API)")
	}
}

// P-quota-prefer: 2% 仍有额度
func TestMiniMaxBalancer_TwoPercentHasQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{
				{"model_name": "general", "current_interval_remaining_percent": 2.0},
			},
			"base_resp": map[string]interface{}{"status_code": 0, "status_msg": "success"},
		})
	}))
	defer srv.Close()
	t.Cleanup(withMockQuotaHost(t, srv.URL))

	b := newMiniMaxBalancer()
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{ID: "1", Key: "good-key"})
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true (2%% > 1%%)")
	}
}
