package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// TestRelayPassthrough429_PoolStateAndNoQuotaEvidence locks the P5 contract
// for a pure transient 429: one request per key, both keys cooled exactly once,
// and no token-plan quota evidence. The existing p5 count test proves the
// transport path; this assertion proves the state/evidence side effects too.
func TestRelayPassthrough429_PoolStateAndNoQuotaEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto provider.Protocol
		path  string
	}{
		{name: "anthropic", proto: provider.ProtocolAnthropic, path: "/v1/messages"},
		{name: "openai", proto: provider.ProtocolOpenAI, path: "/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"synthetic transient throttle"}}`)
			}))
			defer upstream.Close()

			engine, pool, cleanup := newP5RelayEngine(t, "p5-state-"+tc.name, tc.proto, tc.path, upstream.URL, nil)
			defer cleanup()
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.POST(tc.path, engine.HandleRequest)
			body := `{"model":"synthetic-model","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			mu.Lock()
			gotRequests := requests
			mu.Unlock()
			if gotRequests != 2 {
				t.Fatalf("real upstream requests = %d, want exactly 2", gotRequests)
			}
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want upstream 429; body=%s", w.Code, w.Body.String())
			}
			keys := pool.Keys()
			if len(keys) != 2 {
				t.Fatalf("pool keys = %d, want 2", len(keys))
			}
			for _, key := range keys {
				if key.Status != keypool.KeyStatusCooling {
					t.Errorf("key %s status = %q, want COOLING", key.ID, key.Status)
				}
				if key.CoolingCount != 1 {
					t.Errorf("key %s cooling count = %d, want 1", key.ID, key.CoolingCount)
				}
				if key.ErrorCount != 1 {
					t.Errorf("key %s error count = %d, want 1 (no duplicate provider/proxy report)", key.ID, key.ErrorCount)
				}
			}
			if got := pool.Status().QuotaExceededKeys; got != 0 {
				t.Fatalf("quota-exceeded keys = %d, want 0 for pure 429", got)
			}
		})
	}
}
