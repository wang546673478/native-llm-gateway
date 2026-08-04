package minimax

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

func newTestKey() *keypool.Key {
	return &keypool.Key{ID: "1", Name: "test", Key: "fake-subscription-key"}
}

func TestMiniMaxBalancer_ParsesBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/token_plan/remains") {
			t.Errorf("path = %s, want suffix /v1/token_plan/remains", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer fake-subscription-key" {
			t.Errorf("auth = %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance": 12.34}`))
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if !bal.HasQuota {
		t.Errorf("HasQuota = false, want true (balance=12.34)")
	}
	if bal.Raw != 12.34 {
		t.Errorf("Raw = %v, want 12.34", bal.Raw)
	}
	if !strings.Contains(bal.Source, "minimax") {
		t.Errorf("Source = %q, want minimax prefix", bal.Source)
	}
}

func TestMiniMaxBalancer_ZeroBalanceIsExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance": 0}`))
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if bal.HasQuota {
		t.Error("HasQuota = true, want false (balance=0)")
	}
}

func TestMiniMaxBalancer_FieldNameVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"remains", `{"remains": 5.0}`, true},
		{"quota_remaining", `{"quota_remaining": 5.0}`, true},
		{"available", `{"available": 5.0}`, true},
		{"all-missing", `{"foo": 1.0}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			b := newMiniMaxBalancer()
			bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
			if err != nil {
				t.Fatalf("FetchBalance: %v", err)
			}
			if bal.HasQuota != tc.want {
				t.Errorf("HasQuota = %v, want %v", bal.HasQuota, tc.want)
			}
		})
	}
}

func TestMiniMaxBalancer_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "auth fail", http.StatusUnauthorized)
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	_, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
