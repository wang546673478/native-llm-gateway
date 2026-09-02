package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// quotaSwitchProvider makes the key selected by the router observable. It is
// deliberately provider-level (rather than pool-level) so the test exercises
// the same tryCandidate path used by production providers.
type quotaSwitchProvider struct {
	name       string
	quotaKeys  map[string]bool
	mu         sync.Mutex
	calls      []string
	blockAfter string
}

func (p *quotaSwitchProvider) Name() string                { return p.name }
func (p *quotaSwitchProvider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }
func (p *quotaSwitchProvider) SetPool(*keypool.Pool)       {}
func (p *quotaSwitchProvider) Close() error                { return nil }
func (p *quotaSwitchProvider) HealthCheck(context.Context) error {
	return nil
}
func (p *quotaSwitchProvider) ListModels(context.Context) ([]string, error) {
	return []string{"claude-opus-5"}, nil
}
func (p *quotaSwitchProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, errors.New("stream not used")
}
func (p *quotaSwitchProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	keyID := ""
	if req != nil && req.Key != nil {
		keyID = req.Key.ID
	}
	p.mu.Lock()
	p.calls = append(p.calls, keyID)
	p.mu.Unlock()
	if keyID == p.blockAfter {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.quotaKeys[keyID] {
		return nil, &provider.ProviderError{
			ProviderName: p.name,
			StatusCode:   http.StatusPaymentRequired,
			ErrorType:    provider.ErrorTypeQuotaExceeded,
			Message:      "synthetic quota",
		}
	}
	return &provider.Response{StatusCode: http.StatusOK, Headers: http.Header{}, Body: []byte(`{"ok":true}`)}, nil
}

func (p *quotaSwitchProvider) callIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func newQuotaLoop(t *testing.T, first, second provider.Provider, firstPool, secondPool *keypool.Pool, ctx context.Context) (*httptest.ResponseRecorder, *quotaSwitchProvider, *quotaSwitchProvider) {
	t.Helper()
	mgr := newTestManager(t, first, second)
	rtr := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{
		first.Name():  firstPool,
		second.Name(): secondPool,
	}, router.Config{CatchAll: &router.AliasConfig{}})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop()})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	req := &provider.Request{
		TraceID: "quota-second-key",
		Model:   "claude-opus-5",
		Path:    "/v1/messages",
		Body:    []byte(`{"model":"claude-opus-5","messages":[]}`),
		Headers: http.Header{},
	}
	iter, err := rtr.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	var providerName string
	var lastErr *provider.ProviderError
	engine.runCandidateLoop(c, ctx, req, iter, nil, &providerName, &lastErr, nil)
	// runCandidateLoop is called directly (without gin's outer ServeHTTP), so
	// commit the selected status before inspecting httptest.ResponseRecorder.
	c.Writer.WriteHeaderNow()
	return w, first.(*quotaSwitchProvider), second.(*quotaSwitchProvider)
}

func makeQuotaPool(name string, mode keypool.QuotaRecoveryMode, protocols string) *keypool.Pool {
	now := time.Now()
	keys := []*keypool.Key{
		{ID: "1", ProviderName: name, Key: "key-one", Status: keypool.KeyStatusActive, BillingSource: string(keypool.BillingSourceTokenPlan), Protocols: protocols, CreatedAt: now, UpdatedAt: now},
		{ID: "2", ProviderName: name, Key: "key-two", Status: keypool.KeyStatusActive, BillingSource: string(keypool.BillingSourceTokenPlan), Protocols: protocols, CreatedAt: now.Add(time.Millisecond), UpdatedAt: now},
	}
	return keypool.NewPool(name, keys, &keypool.RoundRobinScheduler{}, keypool.Config{QuotaRecovery: mode})
}

func TestQuotaExceededTriesSameProviderSecondKey(t *testing.T) {
	first := &quotaSwitchProvider{name: "first", quotaKeys: map[string]bool{"1": true}}
	second := &quotaSwitchProvider{name: "second"}
	w, gotFirst, gotSecond := newQuotaLoop(t, first, second,
		makeQuotaPool("first", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic)),
		makeQuotaPool("second", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic)), context.Background())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := gotFirst.callIDs(); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("first provider calls = %v, want [1 2]", got)
	}
	if got := gotSecond.callIDs(); len(got) != 0 {
		t.Fatalf("next provider calls = %v, want none", got)
	}
}

func TestQuotaExceededBothKeysThenNextProvider(t *testing.T) {
	first := &quotaSwitchProvider{name: "first", quotaKeys: map[string]bool{"1": true, "2": true}}
	second := &quotaSwitchProvider{name: "second"}
	w, gotFirst, gotSecond := newQuotaLoop(t, first, second,
		makeQuotaPool("first", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic)),
		makeQuotaPool("second", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic)), context.Background())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := gotFirst.callIDs(); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("first provider calls = %v, want [1 2]", got)
	}
	if got := gotSecond.callIDs(); len(got) != 1 {
		t.Fatalf("next provider calls = %v, want one", got)
	}
}

func TestQuotaExceededProbeStillExcludesFirstKey(t *testing.T) {
	first := &quotaSwitchProvider{name: "first", quotaKeys: map[string]bool{"1": true}}
	second := &quotaSwitchProvider{name: "second"}
	w, gotFirst, _ := newQuotaLoop(t, first, second,
		makeQuotaPool("first", keypool.QuotaRecoveryProbe, string(provider.ProtocolAnthropic)),
		makeQuotaPool("second", keypool.QuotaRecoveryProbe, string(provider.ProtocolAnthropic)), context.Background())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := gotFirst.callIDs(); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("probe calls = %v, want [1 2]", got)
	}
}

func TestParentDeadlineStopsBeforeSecondProviderAndDoesNotReportKey(t *testing.T) {
	first := &quotaSwitchProvider{name: "first", blockAfter: "1"}
	second := &quotaSwitchProvider{name: "second"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	firstPool := makeQuotaPool("first", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic))
	w, gotFirst, gotSecond := newQuotaLoop(t, first, second,
		firstPool,
		makeQuotaPool("second", keypool.QuotaRecoveryPoll, string(provider.ProtocolAnthropic)), ctx)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s calls=%v/%v", w.Code, w.Body.String(), gotFirst.callIDs(), gotSecond.callIDs())
	}
	if got := gotFirst.callIDs(); len(got) != 1 {
		t.Fatalf("first provider calls = %v, want one", got)
	}
	if got := gotSecond.callIDs(); len(got) != 0 {
		t.Fatalf("second provider calls = %v, want none", got)
	}
	key := firstPool.Keys()[0]
	if key.ErrorCount != 0 || key.Status != keypool.KeyStatusActive {
		t.Fatalf("unrelated key state = %+v", key)
	}
}
