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
			"balance":      []string{"42.50", "CNY"},
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
		t.Errorf("HasQuota = false, want true")
	}
	if got.Raw != 42.5 {
		t.Errorf("Raw = %v, want 42.5", got.Raw)
	}
}

func TestDeepseekBalancer_AuthError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/balance", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newDeepseekBalancer()
	_, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "bad-key"})
	if err == nil {
		t.Error("expected error on 401")
	}
}

func TestDeepseekBalancer_ZeroBalance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/balance", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"is_available": false,
			"balance":      []string{"0", "CNY"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newDeepseekBalancer()
	got, err := b.FetchBalance(context.Background(), srv.URL, &keypool.Key{ID: "1", Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (balance=0)")
	}
	_ = time.Second
}
