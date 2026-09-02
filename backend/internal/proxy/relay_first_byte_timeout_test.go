package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

type relayStreamBehavior int

const (
	relayBlocksBeforeHeaders relayStreamBehavior = iota
	relayReturnsSilentBody
	relayReturnsData
	relayReturnsPingThenDelayedData
	relayReturnsPingThenError
	relayFirstKeyFailsThenSecondBlocks
	relayReturnsRawTransportError
	relayReturnsEmptyBeforeFirstByte
	relayReturnsIncompleteStream
)

type relayBudgetProvider struct {
	name     string
	behavior relayStreamBehavior
	delay    time.Duration
	calls    atomic.Int32
}

type failingBodyWriter struct {
	*httptest.ResponseRecorder
}

func (w *failingBodyWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic client write failure")
}

type ttftRecord struct {
	provider, model, requestSize, phase string
	duration                            time.Duration
}

type relayTTFTMetrics struct {
	mu      sync.Mutex
	records []ttftRecord
	events  map[relayEventKey]int
	active  map[string]int
}

type relayEventKey struct {
	provider string
	event    string
	stage    string
}

func (m *relayTTFTMetrics) RecordRequest(string, int, time.Duration, bool, string) {}
func (m *relayTTFTMetrics) RecordStreamTTFT(providerName, model, requestSize, phase string, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, ttftRecord{
		provider: providerName, model: model, requestSize: requestSize, phase: phase, duration: duration,
	})
}
func (m *relayTTFTMetrics) RecordRelayEvent(providerName, event, stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.events == nil {
		m.events = make(map[relayEventKey]int)
	}
	m.events[relayEventKey{provider: providerName, event: event, stage: stage}]++
}
func (m *relayTTFTMetrics) AddRelayActiveUpstreams(providerName string, delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active = make(map[string]int)
	}
	m.active[providerName] += delta
}
func (m *relayTTFTMetrics) phases() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	phases := make(map[string]bool, len(m.records))
	for _, record := range m.records {
		phases[record.phase] = true
	}
	return phases
}
func (m *relayTTFTMetrics) eventCount(providerName, event, stage string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[relayEventKey{provider: providerName, event: event, stage: stage}]
}
func (m *relayTTFTMetrics) activeCount(providerName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[providerName]
}

func (p *relayBudgetProvider) Name() string                { return p.name }
func (p *relayBudgetProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *relayBudgetProvider) SetPool(*keypool.Pool)       {}
func (p *relayBudgetProvider) Close() error                { return nil }
func (p *relayBudgetProvider) HealthCheck(context.Context) error {
	return nil
}
func (p *relayBudgetProvider) ListModels(context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *relayBudgetProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}
func (p *relayBudgetProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	p.calls.Add(1)
	switch p.behavior {
	case relayBlocksBeforeHeaders:
		<-ctx.Done()
		return nil, nil, ctx.Err()
	case relayReturnsSilentBody:
		return make(chan *provider.StreamChunk), &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
	case relayReturnsData:
		ch := make(chan *provider.StreamChunk, 2)
		ch <- &provider.StreamChunk{Data: []byte("data: second\n\n")}
		ch <- &provider.StreamChunk{Err: io.EOF}
		close(ch)
		return ch, &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
	case relayReturnsPingThenDelayedData:
		ch := make(chan *provider.StreamChunk, 3)
		go func() {
			defer close(ch)
			if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Data: []byte(": PING\n\n")}) {
				return
			}
			timer := time.NewTimer(p.delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return
			}
			if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Data: []byte("data: first\n\n")}) {
				return
			}
			provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Err: io.EOF})
		}()
		return ch, &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
	case relayReturnsPingThenError:
		ch := make(chan *provider.StreamChunk, 2)
		ch <- &provider.StreamChunk{Data: []byte(": PING\n\n")}
		ch <- &provider.StreamChunk{Err: errors.New("upstream closed")}
		close(ch)
		return ch, &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
	case relayFirstKeyFailsThenSecondBlocks:
		if req != nil && req.Key != nil && req.Key.ID == "1" {
			return nil, nil, &provider.ProviderError{
				ProviderName: p.name,
				ErrorType:    provider.ErrorTypeConnection,
				Message:      "synthetic first-key connection failure",
			}
		}
		ch := make(chan *provider.StreamChunk)
		go func() {
			<-ctx.Done()
			provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Err: ctx.Err()})
			close(ch)
		}()
		return ch, &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
	case relayReturnsRawTransportError:
		return nil, nil, errors.New("synthetic raw transport error")
	case relayReturnsEmptyBeforeFirstByte:
		ch := make(chan *provider.StreamChunk)
		close(ch)
		return ch, &provider.Response{StatusCode: http.StatusOK, Headers: http.Header{}}, nil
	case relayReturnsIncompleteStream:
		return nil, nil, nil
	default:
		return nil, nil, errors.New("unknown behavior")
	}
}

func TestRelayEmptyStreamBeforeFirstByteFailsOver(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayReturnsEmptyBeforeFirstByte}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, time.Second, first, second)

	w := runRelayBudgetRequest(t, engine)
	if w.Code != http.StatusOK || w.Body.String() != "data: second\n\n" {
		t.Fatalf("response = %d %q, want second relay response", w.Code, w.Body.String())
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("calls first/second = %d/%d, want 1/1", first.calls.Load(), second.calls.Load())
	}
}

func TestRelayIncompleteStreamResponseFailsOver(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayReturnsIncompleteStream}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, time.Second, first, second)

	w := runRelayBudgetRequest(t, engine)
	if w.Code != http.StatusOK || w.Body.String() != "data: second\n\n" {
		t.Fatalf("response = %d %q, want second relay response", w.Code, w.Body.String())
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("calls first/second = %d/%d, want 1/1", first.calls.Load(), second.calls.Load())
	}
}

func newRelayBudgetEngine(t *testing.T, budget time.Duration, providers ...*relayBudgetProvider) *Engine {
	t.Helper()
	reg := provider.NewRegistry()
	cfg := provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{}}
	pools := make(map[string]*keypool.Pool, len(providers))
	routes := make([]router.ProviderRoute, 0, len(providers))
	for i, p := range providers {
		p := p
		reg.RegisterWithProtocolVendorRelay(p.name, func(provider.ProviderConfig) (provider.Provider, error) {
			return p, nil
		}, provider.ProtocolOpenAI, p.name, true)
		cfg.Providers[p.name] = provider.ManagerProviderConfig{
			Enabled: true, Endpoint: "http://relay.invalid", Protocol: provider.ProtocolOpenAI,
		}
		pools[p.name] = newTestPoolWithTier(p.name, 1, keypool.BillingSourceAPI)
		routes = append(routes, router.ProviderRoute{Name: p.name, Model: "gpt-4", Priority: i + 1})
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"gpt-4": {Strategy: "priority", Providers: routes},
		},
	})
	return NewEngine(Config{
		Router: rtr, Logger: zap.NewNop(), RelayFirstByteTimeout: budget,
	})
}

func runRelayBudgetRequest(t *testing.T, engine *Engine) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", engine.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRelayFirstByteTimeout_FailsOverBeforeAndAfterHeaders(t *testing.T) {
	for _, behavior := range []relayStreamBehavior{relayBlocksBeforeHeaders, relayReturnsSilentBody} {
		t.Run(map[relayStreamBehavior]string{
			relayBlocksBeforeHeaders: "before headers",
			relayReturnsSilentBody:   "headers without body",
		}[behavior], func(t *testing.T) {
			first := &relayBudgetProvider{name: "first", behavior: behavior}
			second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
			engine := newRelayBudgetEngine(t, 25*time.Millisecond, first, second)
			metrics := &relayTTFTMetrics{}
			engine.metrics = metrics
			logCore, observedLogs := observer.New(zap.DebugLevel)
			engine.logger = zap.New(logCore)

			started := time.Now()
			w := runRelayBudgetRequest(t, engine)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("first-byte failover took %v, want < 1s", elapsed)
			}
			if w.Code != http.StatusOK || w.Body.String() != "data: second\n\n" {
				t.Fatalf("response = %d %q, want second relay response", w.Code, w.Body.String())
			}
			if first.calls.Load() != 1 || second.calls.Load() != 1 {
				t.Fatalf("calls first/second = %d/%d, want 1/1", first.calls.Load(), second.calls.Load())
			}
			stage := map[relayStreamBehavior]string{
				relayBlocksBeforeHeaders: "headers",
				relayReturnsSilentBody:   "body",
			}[behavior]
			if got := metrics.eventCount("first", "first_byte_timeout", stage); got != 1 {
				t.Fatalf("first-byte timeout event %q count = %d, want 1", stage, got)
			}
			if got := metrics.eventCount("second", "response_committed", "data"); got != 1 {
				t.Fatalf("second response committed event count = %d, want 1", got)
			}
			for _, name := range []string{"first", "second"} {
				if got := metrics.activeCount(name); got != 0 {
					t.Fatalf("active upstreams for %s = %d, want 0", name, got)
				}
			}
			if got := metrics.eventCount("first", "body_mismatch", "request") +
				metrics.eventCount("second", "body_mismatch", "request"); got != 0 {
				t.Fatalf("body mismatch events = %d, want 0", got)
			}
			failedLogs := observedLogs.FilterMessage("candidate failed, failover").All()
			if len(failedLogs) == 0 {
				t.Fatal("candidate failure log missing")
			}
			fields := failedLogs[0].ContextMap()
			if fields["passthrough"] != true || fields["first_byte_stage"] != stage || fields["response_committed"] != false {
				t.Fatalf("candidate log contract fields = %#v", fields)
			}
		})
	}
}

func TestRelayFirstByteTimeout_StopsAfterPingAndDoesNotFailover(t *testing.T) {
	first := &relayBudgetProvider{
		name: "first", behavior: relayReturnsPingThenDelayedData, delay: 80 * time.Millisecond,
	}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, 20*time.Millisecond, first, second)
	metrics := &relayTTFTMetrics{}
	engine.metrics = metrics

	w := runRelayBudgetRequest(t, engine)
	if w.Code != http.StatusOK || w.Body.String() != ": PING\n\ndata: first\n\n" {
		t.Fatalf("response = %d %q, want committed first relay stream", w.Code, w.Body.String())
	}
	if second.calls.Load() != 0 {
		t.Fatalf("second relay called %d times after ping committed response", second.calls.Load())
	}
	phases := metrics.phases()
	for _, phase := range []string{"body", "ping", "data"} {
		if !phases[phase] {
			t.Fatalf("TTFT phase %q not recorded; got %v", phase, phases)
		}
	}
	if got := metrics.eventCount("first", "response_committed", "ping"); got != 1 {
		t.Fatalf("ping commit events = %d, want 1", got)
	}
	if got := metrics.eventCount("second", "candidate_attempt", "none"); got != 0 {
		t.Fatalf("candidate attempts after ping commit = %d, want 0", got)
	}
	if got := metrics.eventCount("first", "switch_after_response_committed", "none"); got != 0 {
		t.Fatalf("switch-after-commit events = %d, want 0", got)
	}
	if got := metrics.activeCount("first"); got != 0 {
		t.Fatalf("active upstreams after stream = %d, want 0", got)
	}
}

func TestRelayFirstByteTimeout_DefaultAndOverride(t *testing.T) {
	if got := NewEngine(Config{}).relayFirstByteTimeout; got != DefaultRelayFirstByteTimeout {
		t.Fatalf("default relay first-byte timeout = %v, want %v", got, DefaultRelayFirstByteTimeout)
	}
	if got := NewEngine(Config{RelayFirstByteTimeout: 42 * time.Second}).relayFirstByteTimeout; got != 42*time.Second {
		t.Fatalf("configured relay first-byte timeout = %v, want 42s", got)
	}
}

func TestRelayFirstByteTimeout_SharedAcrossReplacementKey(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayFirstKeyFailsThenSecondBlocks}
	engine := newRelayBudgetEngine(t, 35*time.Millisecond, first)
	engine.router.SetPool("first", newTestPoolWithTier("first", 2, keypool.BillingSourceAPI))

	started := time.Now()
	w := runRelayBudgetRequest(t, engine)
	elapsed := time.Since(started)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := first.calls.Load(); got != 2 {
		t.Fatalf("relay key attempts = %d, want exactly 2", got)
	}
	// The second key must consume only the remainder of the first candidate's
	// budget. Allow scheduling jitter, but reject a full budget reset.
	if elapsed > 55*time.Millisecond {
		t.Fatalf("shared candidate budget took %v, appears to have been reset", elapsed)
	}
}

func TestRelayBudgetContextExpired_DistinguishesLocalCancelFromParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	child, cancelChild := context.WithCancel(parent)
	defer cancelChild()
	ctx := context.WithValue(parent, candidateBudgetParentKey{}, parent)
	ctx = context.WithValue(ctx, candidateBudgetDeadlineKey{}, time.Now().Add(-time.Second))

	cancelChild()
	if !relayBudgetContextExpired(ctx, child) {
		t.Fatal("expired local relay child cancellation was not recognized")
	}

	cancelParent()
	if relayBudgetContextExpired(ctx, child) {
		t.Fatal("parent cancellation was misclassified as local relay timeout")
	}
}

func TestRelayStreamRawTransportErrorReportsKeyOnce(t *testing.T) {
	first := &relayBudgetProvider{name: "raw-transport", behavior: relayReturnsRawTransportError}
	engine := newRelayBudgetEngine(t, 20*time.Millisecond, first)

	w := runRelayBudgetRequest(t, engine)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if got := first.calls.Load(); got != 1 {
		t.Fatalf("raw transport provider calls = %d, want 1", got)
	}

	pool := engine.router.Pool("raw-transport")
	if pool == nil {
		t.Fatal("pool missing")
	}
	keys := pool.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	if keys[0].ErrorCount != 1 {
		t.Fatalf("raw stream transport error ErrorCount = %d, want 1", keys[0].ErrorCount)
	}
}

func TestRelayCommittedStreamErrorDoesNotInjectOrFailover(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayReturnsPingThenError}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, 20*time.Millisecond, first, second)
	metrics := &relayTTFTMetrics{}
	engine.metrics = metrics

	w := runRelayBudgetRequest(t, engine)
	if w.Code != http.StatusOK || w.Body.String() != ": PING\n\n" {
		t.Fatalf("response = %d %q, want only upstream ping bytes", w.Code, w.Body.String())
	}
	if second.calls.Load() != 0 {
		t.Fatalf("second relay called %d times after response commit", second.calls.Load())
	}
	if got := metrics.eventCount("first", "stream_interrupted", "upstream_error"); got != 1 {
		t.Fatalf("stream interruption events = %d, want exactly 1", got)
	}
	if got := metrics.eventCount("first", "response_committed", "ping"); got != 1 {
		t.Fatalf("response commit events = %d, want 1", got)
	}
	if got := metrics.eventCount("second", "candidate_attempt", "none"); got != 0 {
		t.Fatalf("candidate attempts after committed interruption = %d, want 0", got)
	}
	if got := metrics.activeCount("first"); got != 0 {
		t.Fatalf("active upstreams after interruption = %d, want 0", got)
	}
}

func TestRelayLifecycle_PreCanceledRequestStartsNoCandidate(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayReturnsData}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, 20*time.Millisecond, first, second)
	metrics := &relayTTFTMetrics{}
	engine.metrics = metrics
	logCore, observedLogs := observer.New(zap.InfoLevel)
	engine.logger = zap.New(logCore)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", engine.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 499 {
		t.Fatalf("status = %d, want 499", w.Code)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 0 {
		t.Fatalf("provider calls after cancellation = %d/%d, want 0/0", first.calls.Load(), second.calls.Load())
	}
	for _, name := range []string{"first", "second"} {
		if got := metrics.eventCount(name, "candidate_attempt", "none"); got != 0 {
			t.Fatalf("candidate attempts for %s after cancellation = %d, want 0", name, got)
		}
		if got := metrics.activeCount(name); got != 0 {
			t.Fatalf("active upstreams for %s = %d, want 0", name, got)
		}
	}
	summaries := observedLogs.FilterMessage("candidate chain canceled").All()
	if len(summaries) != 1 {
		t.Fatalf("cancellation summaries = %d, want 1", len(summaries))
	}
	fields := summaries[0].ContextMap()
	if fields["candidate_count"] != int64(0) || fields["post_cancel_candidate_count"] != int64(0) {
		t.Fatalf("cancellation summary fields = %#v", fields)
	}
}

func TestRelayLifecycle_ClientWriteFailureStopsCandidatesAndLogsSummary(t *testing.T) {
	first := &relayBudgetProvider{name: "first", behavior: relayReturnsData}
	second := &relayBudgetProvider{name: "second", behavior: relayReturnsData}
	engine := newRelayBudgetEngine(t, 20*time.Millisecond, first, second)
	metrics := &relayTTFTMetrics{}
	engine.metrics = metrics
	logCore, observedLogs := observer.New(zap.InfoLevel)
	engine.logger = zap.New(logCore)

	req := &provider.Request{
		Method: http.MethodPost, Path: "/v1/chat/completions", Headers: http.Header{},
		Body: []byte(`{"model":"gpt-4","stream":true}`), RequestedModel: "gpt-4",
		RoutingModel: "gpt-4", Model: "gpt-4", IsStream: true, TraceID: "write-failure-trace",
	}
	iter, err := engine.router.Route(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	writer := &failingBodyWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, req.Path, nil)
	var providerName string
	var lastErr *provider.ProviderError

	engine.runCandidateLoop(c, context.Background(), req, iter, nil, &providerName, &lastErr, nil)

	if first.calls.Load() != 1 || second.calls.Load() != 0 {
		t.Fatalf("provider calls after client write failure = %d/%d, want 1/0", first.calls.Load(), second.calls.Load())
	}
	if lastErr == nil || lastErr.ErrorType != provider.ErrorTypeClientDisconnected {
		t.Fatalf("last error = %#v, want client_disconnected", lastErr)
	}
	if got := metrics.eventCount("first", "stream_interrupted", "client_disconnected"); got != 1 {
		t.Fatalf("client-disconnected stream events = %d, want 1", got)
	}
	if got := metrics.activeCount("first"); got != 0 {
		t.Fatalf("active upstreams after client write failure = %d, want 0", got)
	}
	summaries := observedLogs.FilterMessage("candidate chain canceled").All()
	if len(summaries) != 1 {
		t.Fatalf("cancellation summaries = %d, want 1", len(summaries))
	}
	fields := summaries[0].ContextMap()
	if fields["candidate_count"] != int64(1) || fields["post_cancel_candidate_count"] != int64(0) {
		t.Fatalf("cancellation summary fields = %#v", fields)
	}
}
