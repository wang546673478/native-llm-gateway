package glm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// 2026-08-05: GLM monitor 端点实测 schema(官方插件 zai-org/zai-coding-plugins
// glm-plan-usage 同款):limits[] 滚动额度窗,percentage=已用百分比。

// TestGlmBalancer_HasRoom — 所有窗口都有剩余 → HasQuota=true,
// Raw=最紧窗口的剩余百分比(与 minimax 同语义:percent 的 Raw 是剩余量)
func TestGlmBalancer_HasRoom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "success": true,
			"data": map[string]interface{}{
				"level": "lite",
				"limits": []map[string]interface{}{
					{"type": "CREDIT_LIMIT", "unit": 3, "usage": 2000, "currentValue": 800, "remaining": 1200, "percentage": 40, "nextResetTime": 1785910103775},
					{"type": "CREDIT_LIMIT", "unit": 6, "usage": 10000, "currentValue": 8067, "remaining": 1932, "percentage": 80, "nextResetTime": 1786323757998},
				},
			},
		})
	}))
	defer srv.Close()

	b := newGlmBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true (both windows have room)")
	}
	if got.Raw != 20 {
		t.Errorf("Raw = %v, want 20 (100 - tightest used 80 = remaining)", got.Raw)
	}
	if got.Kind != "percent" {
		t.Errorf("Kind = %q, want %q", got.Kind, "percent")
	}
}

// TestGlmBalancer_AnyWindowExhausted — 任一窗耗尽(percentage=100)
// → HasQuota=false(请求会 1113 余额不足)
func TestGlmBalancer_AnyWindowExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "success": true,
			"data": map[string]interface{}{
				"level": "lite",
				"limits": []map[string]interface{}{
					{"type": "CREDIT_LIMIT", "unit": 3, "usage": 2000, "currentValue": 2035, "remaining": 0, "percentage": 100, "nextResetTime": 1785910103775},
					{"type": "CREDIT_LIMIT", "unit": 6, "usage": 10000, "currentValue": 8067, "remaining": 1932, "percentage": 80, "nextResetTime": 1786323757998},
				},
			},
		})
	}))
	defer srv.Close()

	b := newGlmBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.HasQuota {
		t.Error("HasQuota = true, want false (3h window exhausted)")
	}
	if got.Raw != 0 {
		t.Errorf("Raw = %v, want 0 (exhausted window, 0 remaining)", got.Raw)
	}
}

// TestGlmBalancer_NoLimits — 无窗口限制(正式付费账户)→ HasQuota=true,Raw=100
func TestGlmBalancer_NoLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200, "success": true,
			"data": map[string]interface{}{"level": "pro", "limits": []map[string]interface{}{}},
		})
	}))
	defer srv.Close()

	b := newGlmBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.HasQuota {
		t.Error("HasQuota = false, want true (no limits = nothing blocks)")
	}
	if got.Raw != 100 {
		t.Errorf("Raw = %v, want 100 (no limits = 100%% remaining)", got.Raw)
	}
}

// TestGlmBalancer_AuthFailure — 401 → 返回 error(quotacheck 据此不标 QUOTA_EXCEEDED)
func TestGlmBalancer_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	b := newGlmBalancer()
	_, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "bad"})
	if err == nil {
		t.Fatal("want auth error, got nil")
	}
}
