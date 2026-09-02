package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/relay"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// p4RelayRequest is the subset of an upstream request whose values must stay
// identical when quota failover changes only the upstream credential.
type p4RelayRequest struct {
	key     string
	body    []byte
	query   string
	headers http.Header
}

// TestP4RelayQuota_RealHTTPSecondKey verifies direct quota failover through
// the complete Engine/Router/GenericRelay path. The test server sees the
// original body, raw query, and end-to-end headers twice; only x-api-key is
// expected to differ.
func TestP4RelayQuota_RealHTTPSecondKey(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			var seen []p4RelayRequest
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					return
				}
				key := r.Header.Get("X-Api-Key")
				mu.Lock()
				seen = append(seen, p4RelayRequest{
					key: key, body: bytes.Clone(body), query: r.URL.RawQuery,
					headers: r.Header.Clone(),
				})
				attempt := len(seen)
				mu.Unlock()

				if attempt == 1 {
					w.Header().Set("Content-Type", "application/problem+json")
					w.Header().Add("X-Upstream-Attempt", "first")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"type":"quota_exceeded","message":"quota exhausted"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Add("X-Upstream-Attempt", "second")
				_, _ = io.WriteString(w, `{"id":"quota-key-2-ok"}`)
			}))
			defer upstream.Close()

			engine, pool, cleanup := newP4RelayEngine(t, "p4-quota-"+strings.ToLower(http.StatusText(status)), upstream.URL, provider.ProtocolAnthropic, keypool.QuotaRecoveryPoll, nil)
			defer cleanup()

			body := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
			requestHeaders := http.Header{
				"Accept":             {"application/vnd.client+json"},
				"Accept-Language":    {"zh-CN", "en-US"},
				"Anthropic-Beta":     {"feature-a", "feature-b"},
				"Anthropic-Version":  {"2099-01-01"},
				"User-Agent":         {"p4-test/1.0"},
				"X-Custom-Extension": {"one", "two"},
			}
			w := serveP4RelayRequest(t, engine, "/v1/messages?opaque=a%2Fb&x=1&x=2", body, requestHeaders, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "quota-key-2-ok") {
				t.Fatalf("body = %q, want second-key response", w.Body.String())
			}

			mu.Lock()
			got := append([]p4RelayRequest(nil), seen...)
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("upstream requests = %d (%v), want exactly 2", len(got), got)
			}
			if got[0].key != "key-1" || got[1].key != "key-2" {
				t.Fatalf("key sequence = [%q %q], want [key-1 key-2]", got[0].key, got[1].key)
			}
			if !bytes.Equal(got[0].body, body) || !bytes.Equal(got[1].body, body) {
				t.Fatalf("request body changed across key failover: first=%q second=%q want=%q", got[0].body, got[1].body, body)
			}
			if got[0].query != "opaque=a%2Fb&x=1&x=2" || got[1].query != got[0].query {
				t.Fatalf("raw query values = [%q %q], want exact original", got[0].query, got[1].query)
			}
			for _, name := range []string{"Accept", "Accept-Language", "Anthropic-Beta", "Anthropic-Version", "User-Agent", "X-Custom-Extension"} {
				if !reflect.DeepEqual(got[0].headers.Values(name), requestHeaders.Values(name)) ||
					!reflect.DeepEqual(got[1].headers.Values(name), requestHeaders.Values(name)) {
					t.Fatalf("header %s changed: first=%v second=%v want=%v", name, got[0].headers.Values(name), got[1].headers.Values(name), requestHeaders.Values(name))
				}
			}
			if got[0].headers.Get("Authorization") != "" || got[1].headers.Get("Authorization") != "" {
				t.Fatalf("gateway authorization leaked upstream: first=%#v second=%#v", got[0].headers, got[1].headers)
			}

			keys := pool.Keys()
			if keys[0].Status != keypool.KeyStatusQuotaExceeded {
				t.Fatalf("first key status = %q, want QUOTA_EXCEEDED", keys[0].Status)
			}
			if keys[1].Status != keypool.KeyStatusActive || keys[1].TotalRequests != 1 {
				t.Fatalf("second key state = %+v, want active with one request", keys[1])
			}
		})
	}
}

// TestP4RelayQuota_StreamingAnthropicRawBytes covers the pre-commit stream
// branch: the first key returns quota before any SSE byte, then key-2's exact
// stream (including a comment/PING) is returned to the caller unchanged.
func TestP4RelayQuota_StreamingAnthropicRawBytes(t *testing.T) {
	const rawSSE = ": ping-from-upstream\r\n\r\nevent: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\ndata: [DONE]\n\n"
	var mu sync.Mutex
	var keys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		mu.Lock()
		keys = append(keys, key)
		attempt := len(keys)
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"type":"quota_exceeded","message":"quota exhausted"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("X-Upstream-Stream", "raw")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rawSSE)
	}))
	defer upstream.Close()

	engine, pool, cleanup := newP4RelayEngine(t, "p4-stream-quota", upstream.URL, provider.ProtocolAnthropic, keypool.QuotaRecoveryPoll, nil)
	defer cleanup()
	body := []byte(`{"model":"claude-opus-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	w := serveP4RelayRequest(t, engine, "/v1/messages?stream=1", body, http.Header{"Accept": {"text/event-stream"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), []byte(rawSSE)) {
		t.Fatalf("stream bytes changed:\n got  %q\n want %q", w.Body.Bytes(), rawSSE)
	}
	if w.Header().Get("X-Upstream-Stream") != "raw" {
		t.Fatalf("upstream response header missing: %#v", w.Header())
	}
	mu.Lock()
	gotKeys := append([]string(nil), keys...)
	mu.Unlock()
	if !reflect.DeepEqual(gotKeys, []string{"key-1", "key-2"}) {
		t.Fatalf("key sequence = %v, want [key-1 key-2]", gotKeys)
	}
	if st := pool.Keys()[0].Status; st != keypool.KeyStatusQuotaExceeded {
		t.Fatalf("first key status = %q, want QUOTA_EXCEEDED", st)
	}
	if st := pool.Keys()[1].Status; st != keypool.KeyStatusActive {
		t.Fatalf("second key status = %q, want ACTIVE", st)
	}
}

// TestP4RelayQuota_GatewayKeyWhitelistDoesNotEscape verifies that a Gateway
// Key bound only to key-1 cannot make quota failover acquire key-2.
func TestP4RelayQuota_GatewayKeyWhitelistDoesNotEscape(t *testing.T) {
	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"quota_exceeded","message":"quota exhausted"}}`)
	}))
	defer upstream.Close()

	engine, pool, cleanup := newP4RelayEngine(t, "p4-whitelist", upstream.URL, provider.ProtocolAnthropic, keypool.QuotaRecoveryPoll, nil)
	defer cleanup()
	gk := &auth.GatewayKey{ID: "gateway-test", Name: "gateway-test", ProviderKeyIDs: []uint{1}}
	w := serveP4RelayRequest(t, engine, "/v1/messages", []byte(`{"model":"claude-opus-5","messages":[]}`), http.Header{}, gk)
	if requests != 1 {
		t.Fatalf("upstream requests = %d, want exactly 1 (key-2 is outside whitelist)", requests)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("status = 200, whitelist unexpectedly escaped; body=%s", w.Body.String())
	}
	if pool.Keys()[1].TotalRequests != 0 || pool.Keys()[1].Status != keypool.KeyStatusActive {
		t.Fatalf("key-2 was touched despite whitelist: %+v", pool.Keys()[1])
	}
}

// newP4RelayEngine builds a real relay provider with a two-key token-plan
// pool. It deliberately loads model metadata without probing the test server,
// keeping the assertions limited to the request under test.
func newP4RelayEngine(t *testing.T, name, baseURL string, proto provider.Protocol, recovery keypool.QuotaRecoveryMode, fallback provider.Provider) (*Engine, *keypool.Pool, func()) {
	t.Helper()
	relayProvider, err := relay.NewGenericRelayProvider(relay.Config{
		Name: name, BaseURL: baseURL, ProtocolMode: "single", PrimaryProtocol: proto, Timeout: 5,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	now := time.Now()
	pool := keypool.NewPool(name, []*keypool.Key{
		{ID: "1", Name: "key-1", ProviderName: name, Key: "key-1", Status: keypool.KeyStatusActive, BillingSource: string(keypool.BillingSourceTokenPlan), Protocols: string(proto), CreatedAt: now, UpdatedAt: now},
		{ID: "2", Name: "key-2", ProviderName: name, Key: "key-2", Status: keypool.KeyStatusActive, BillingSource: string(keypool.BillingSourceTokenPlan), Protocols: string(proto), CreatedAt: now.Add(time.Millisecond), UpdatedAt: now},
	}, &keypool.StickyScheduler{}, keypool.Config{QuotaRecovery: recovery})
	relayProvider.SetPool(pool)

	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(name, func(provider.ProviderConfig) (provider.Provider, error) {
		return relayProvider, nil
	}, proto, name, true)
	mgr := provider.NewManager(reg, zap.NewNop())
	mgr.SetForTesting(name, relayProvider)
	rows := []provider.DBModelRow{{Vendor: name, ModelID: "claude-opus-5"}}
	if err := mgr.LoadModelsFromStore(context.Background(), p4ModelStore{rows: rows}); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}

	providers := []router.ProviderRoute{{Name: name, Model: "claude-opus-5", Priority: 1}}
	pools := map[string]*keypool.Pool{name: pool}
	if fallback != nil {
		mgr.SetForTesting(fallback.Name(), fallback)
		providers = append(providers, router.ProviderRoute{Name: fallback.Name(), Model: "claude-opus-5", Priority: 2})
	}
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"claude-opus-5": {Strategy: "priority", Providers: providers},
		},
	})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop(), Metrics: NoopMetricsRecorder{}, RelayFirstByteTimeout: 2 * time.Second})
	return engine, pool, func() { _ = relayProvider.Close() }
}

// p4ModelStore is intentionally tiny; unlike the production DB adapter it
// only supplies deterministic model metadata for the integration harness.
type p4ModelStore struct{ rows []provider.DBModelRow }

func (s p4ModelStore) All(context.Context) ([]provider.DBModelRow, error) {
	return append([]provider.DBModelRow(nil), s.rows...), nil
}
func (s p4ModelStore) ListByVendor(context.Context, string) ([]provider.DBModelRow, error) {
	return append([]provider.DBModelRow(nil), s.rows...), nil
}
func (s p4ModelStore) AllFaces(context.Context) ([]provider.DBFaceRow, error) { return nil, nil }

func serveP4RelayRequest(t *testing.T, engine *Engine, target string, body []byte, headers http.Header, gk *auth.GatewayKey) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/messages", engine.HandleRequest)
	if gk != nil {
		// Middleware runs before HandleRequest and mirrors the auth middleware's
		// context contract used by production routing.
		r = gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(auth.GatewayKeyContextField, gk)
			c.Set(auth.GatewayKeyContextIDField, gk.ID)
		})
		r.POST("/v1/messages", engine.HandleRequest)
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
