package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

func TestHealthCheck_IsEndpointLivenessOnly(t *testing.T) {
	var requests atomic.Int32
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	now := time.Now()
	pool := keypool.NewPool("google-health", []*keypool.Key{{
		ID: "key-1", ProviderName: "google-health", Key: "secret-google", Name: "primary",
		Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
	before := pool.Keys()
	base := NewBase(Config{Name: "google-health", Endpoint: upstream.URL, Timeout: time.Second, Pool: pool})
	if err := base.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck returned error for reachable 401 endpoint: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("health-check requests = %d, want 1", requests.Load())
	}
	if gotKey != "" {
		t.Errorf("HealthCheck sent x-goog-api-key %q; liveness probe must not select a key", gotKey)
	}
	if after := pool.Keys(); !reflect.DeepEqual(after, before) {
		t.Fatalf("HealthCheck mutated key pool: before=%+v after=%+v", before, after)
	}
}
