package glm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// 拦截所有出网请求(因为 Balancer hardcode bigmodel.cn 域名)
// 我们用一个自定义 transport
type roundtripperFunc func(*http.Request) (*http.Response, error)

func (f roundtripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withTestClient(t *testing.T, handler http.HandlerFunc) *glmBalancer {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// 替换 host
	b := &glmBalancer{client: &http.Client{
		Transport: roundtripperFunc(func(r *http.Request) (*http.Response, error) {
			r.URL.Scheme = "http"
			r.URL.Host = srv.URL[len("http://"):]
			return srv.Client().Do(r)
		}),
	}}
	return b
}

func TestGLMBalancer_ParsesRemaining(t *testing.T) {
	b := withTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200,
			"data": map[string]interface{}{
				"limit": 3000.0,
				"used":  1500.0,
			},
		})
	})
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{Key: "good"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.HasQuota {
		t.Errorf("HasQuota = false, want true (remaining=1500)")
	}
	if got.Raw != 1500.0 {
		t.Errorf("Raw = %v, want 1500", got.Raw)
	}
}

func TestGLMBalancer_NoQuotaWhenExhausted(t *testing.T) {
	b := withTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 200,
			"data": map[string]interface{}{
				"limit": 1000.0,
				"used":  1000.0,
			},
		})
	})
	got, err := b.FetchBalance(context.Background(), "", &keypool.Key{Key: "k"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.HasQuota {
		t.Errorf("HasQuota = true, want false (remaining=0)")
	}
}

func TestGLMBalancer_AuthError(t *testing.T) {
	b := withTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	})
	_, err := b.FetchBalance(context.Background(), "", &keypool.Key{Key: "bad"})
	if err == nil {
		t.Error("expected error on 401")
	}
}
