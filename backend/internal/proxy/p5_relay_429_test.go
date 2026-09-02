package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/relay"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestRelayPassthrough429_SwitchesKeyWithRealHTTPCount verifies the complete
// relay path with a real HTTP server. A transparent relay must expose the
// first 429 to Proxy, which immediately switches to the sibling key. If the
// protocol base retries internally, key-1 would be observed more than once.
func TestRelayPassthrough429_SwitchesKeyWithRealHTTPCount(t *testing.T) {
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
			var seen []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-Api-Key")
				if key == "" {
					key = r.Header.Get("Authorization")
				}
				mu.Lock()
				seen = append(seen, key)
				mu.Unlock()

				if key == "key-1" || key == "Bearer key-1" {
					w.Header().Set("Retry-After", "0")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"synthetic throttle"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"relay-key-2-ok"}`)
			}))
			defer func() {
				upstream.CloseClientConnections()
				upstream.Close()
			}()

			engine, pool, cleanup := newP5RelayEngine(t, tc.name, tc.proto, tc.path, upstream.URL, nil)
			defer cleanup()
			_ = pool // keep the helper's pool available for future state assertions

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.POST(tc.path, engine.HandleRequest)
			body := `{"model":"synthetic-model","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "relay-key-2-ok") {
				t.Fatalf("body = %q, want second-key response", w.Body.String())
			}
			mu.Lock()
			got := append([]string(nil), seen...)
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("real upstream request count = %d (%v), want exactly 2", len(got), got)
			}
			wantFirst := "key-1"
			wantSecond := "key-2"
			if tc.proto == provider.ProtocolOpenAI {
				wantFirst = "Bearer key-1"
				wantSecond = "Bearer key-2"
			}
			if got[0] != wantFirst || got[1] != wantSecond {
				t.Fatalf("credential sequence = %v, want [%q %q]", got, wantFirst, wantSecond)
			}
		})
	}
}

// TestRelayPassthrough429_CancelStopsCandidateChain uses a blocked second-key
// request to prove that parent cancellation does not start another relay key
// or a later provider. The assertion is based on real HTTP traffic, rather
// than fake Provider method calls.
func TestRelayPassthrough429_CancelStopsCandidateChain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto provider.Protocol
		path  string
	}{
		{name: "anthropic", proto: provider.ProtocolAnthropic, path: "/v1/messages"},
		{name: "openai", proto: provider.ProtocolOpenAI, path: "/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secondStarted := make(chan struct{})
			cancelObserved := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var releaseOnce sync.Once
			var mu sync.Mutex
			var seen []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("X-Api-Key")
				if key == "" {
					key = r.Header.Get("Authorization")
				}
				mu.Lock()
				seen = append(seen, key)
				mu.Unlock()
				if key == "key-1" || key == "Bearer key-1" {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"synthetic throttle"}}`)
					return
				}
				once.Do(func() { close(secondStarted) })
				// Commit the response before waiting so the client transport has
				// an active body to cancel when the parent request is canceled.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				select {
				case <-r.Context().Done():
					close(cancelObserved)
				case <-release:
				}
			}))
			defer func() {
				// The synthetic second-key handler intentionally waits for
				// cancellation. Release it explicitly as a final safety net;
				// the assertion below still verifies that request cancellation
				// reached net/http before this cleanup path runs.
				releaseOnce.Do(func() { close(release) })
				upstream.Close()
			}()

			fallback := &fakeProvider{
				name: "fallback-" + tc.name, proto: tc.proto, models: []string{"synthetic-model"},
				respStatus: http.StatusOK, respBody: `{"id":"fallback-must-not-run"}`,
			}
			engine, _, cleanup := newP5RelayEngine(t, "cancel-"+tc.name, tc.proto, tc.path, upstream.URL, fallback)
			defer cleanup()

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.POST(tc.path, engine.HandleRequest)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := `{"model":"synthetic-model","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				r.ServeHTTP(w, req)
				close(done)
			}()

			select {
			case <-secondStarted:
				cancel()
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("replacement-key request was not observed")
			}
			select {
			case <-cancelObserved:
			case <-time.After(2 * time.Second):
				mu.Lock()
				t.Logf("upstream context still active; seen credentials=%v", seen)
				mu.Unlock()
				t.Fatal("upstream request context was not canceled")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("proxy request did not finish after cancellation")
			}

			mu.Lock()
			got := append([]string(nil), seen...)
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("real upstream request count after cancel = %d (%v), want exactly 2", len(got), got)
			}
			fallback.mu.Lock()
			fallbackCalls := fallback.callCount
			fallback.mu.Unlock()
			if fallbackCalls != 0 {
				t.Fatalf("fallback provider calls = %d, want 0 after parent cancellation", fallbackCalls)
			}
		})
	}
}

// newP5RelayEngine wires a real GenericRelayProvider into the production
// Router/Engine path without running a health check against the test server.
// fallback is optional and, when present, is placed after the relay candidate
// to make the cancellation stop-chain assertion observable.
func newP5RelayEngine(
	t *testing.T,
	name string,
	proto provider.Protocol,
	path string,
	baseURL string,
	fallback *fakeProvider,
) (*Engine, *keypool.Pool, func()) {
	t.Helper()
	relayProvider, err := relay.NewGenericRelayProvider(relay.Config{
		Name: name, BaseURL: baseURL, ProtocolMode: "single", PrimaryProtocol: proto, Timeout: 5,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	keys := []*keypool.Key{
		{ID: "1", Name: "key-1", ProviderName: name, Key: "key-1", Status: keypool.KeyStatusActive, BillingSource: "api"},
		{ID: "2", Name: "key-2", ProviderName: name, Key: "key-2", Status: keypool.KeyStatusActive, BillingSource: "api"},
	}
	pool := keypool.NewPool(name, keys, nil, keypool.Config{})
	relayProvider.SetPool(pool)

	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(name, func(provider.ProviderConfig) (provider.Provider, error) {
		return relayProvider, nil
	}, proto, name, true)
	if fallback != nil {
		reg.Register(fallback.Name(), func(provider.ProviderConfig) (provider.Provider, error) {
			return fallback, nil
		})
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	mgr.SetForTesting(name, relayProvider)
	providers := []router.ProviderRoute{{Name: name, Model: "synthetic-model", Priority: 1}}
	pools := map[string]*keypool.Pool{name: pool}
	if fallback != nil {
		mgr.SetForTesting(fallback.Name(), fallback)
		fallbackPool := keypool.NewPool(fallback.Name(), []*keypool.Key{{
			ID: "9", Name: "fallback", ProviderName: fallback.Name(), Key: "fallback-key",
			Status: keypool.KeyStatusActive, BillingSource: "api",
		}}, nil, keypool.Config{})
		fallback.SetPool(fallbackPool)
		providers = append(providers, router.ProviderRoute{Name: fallback.Name(), Model: "synthetic-model", Priority: 2})
		pools[fallback.Name()] = fallbackPool
	}
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"synthetic-model": {Strategy: "priority", Providers: providers},
		},
	})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop(), Metrics: NoopMetricsRecorder{}})
	cleanup := func() {
		_ = relayProvider.Close()
	}
	_ = path // path documents the protocol endpoint used by the caller
	return engine, pool, cleanup
}
