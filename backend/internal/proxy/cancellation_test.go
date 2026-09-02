package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

type cancelingProvider struct {
	name   string
	cancel context.CancelFunc
	calls  int
}

type blockingStreamProvider struct {
	name   string
	chunks chan *provider.StreamChunk
}

// closeOnCancelStreamProvider models a raw relay producer that exits by
// closing its channel after the parent context is canceled, without enqueueing
// a context sentinel.
type closeOnCancelStreamProvider struct {
	name string
}

func (p *blockingStreamProvider) Name() string                { return p.name }
func (p *blockingStreamProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *blockingStreamProvider) SetPool(*keypool.Pool)       {}
func (p *blockingStreamProvider) Close() error                { return nil }
func (p *blockingStreamProvider) HealthCheck(context.Context) error {
	return nil
}
func (p *blockingStreamProvider) ListModels(context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *blockingStreamProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (p *blockingStreamProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.chunks, &provider.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
	}, nil
}

func (p *closeOnCancelStreamProvider) Name() string                { return p.name }
func (p *closeOnCancelStreamProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *closeOnCancelStreamProvider) SetPool(*keypool.Pool)       {}
func (p *closeOnCancelStreamProvider) Close() error                { return nil }
func (p *closeOnCancelStreamProvider) HealthCheck(context.Context) error {
	return nil
}
func (p *closeOnCancelStreamProvider) ListModels(context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *closeOnCancelStreamProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (p *closeOnCancelStreamProvider) SendStreamRequest(ctx context.Context, _ *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	ch := make(chan *provider.StreamChunk, 1)
	ch <- &provider.StreamChunk{Data: []byte("data: first\n\n")}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, &provider.Response{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

type flushNotifyWriter struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *flushNotifyWriter) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func (p *cancelingProvider) Name() string                { return p.name }
func (p *cancelingProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *cancelingProvider) SetPool(*keypool.Pool)       {}
func (p *cancelingProvider) Close() error                { return nil }
func (p *cancelingProvider) HealthCheck(context.Context) error {
	return nil
}
func (p *cancelingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *cancelingProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	p.calls++
	p.cancel()
	return nil, context.Canceled
}
func (p *cancelingProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	p.calls++
	p.cancel()
	return nil, nil, context.Canceled
}

func newCancellationLoop(t *testing.T, providers []provider.Provider, pools map[string]*keypool.Pool, routes []router.ProviderRoute) (*Engine, *router.RouteIterator, *provider.Request, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	mgr := newTestManager(t, providers...)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"gpt-4": {Strategy: "priority", Providers: routes},
		},
	})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop()})
	req := &provider.Request{
		TraceID: "test-client-disconnect",
		Model:   "gpt-4",
		Path:    "/v1/chat/completions",
		Body:    []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{},
	}
	iter, err := rtr.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route() error: %v", err)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, req.Path, nil)
	return engine, iter, req, c, w
}

func TestRunCandidateLoop_PreCanceledContextCallsNoProvider(t *testing.T) {
	rec := &attemptRecorder{}
	first := &recordingProvider{name: "first", rec: rec, errType: provider.ErrorTypeConnection}
	second := &recordingProvider{name: "second", rec: rec, succeed: true}
	engine, iter, req, c, w := newCancellationLoop(t,
		[]provider.Provider{first, second},
		map[string]*keypool.Pool{
			"first":  newTestPoolWithTier("first", 1, keypool.BillingSourceAPI),
			"second": newTestPoolWithTier("second", 1, keypool.BillingSourceAPI),
		},
		[]router.ProviderRoute{
			{Name: "first", Model: "gpt-4", Priority: 1},
			{Name: "second", Model: "gpt-4", Priority: 2},
		})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var providerName string
	var lastErr *provider.ProviderError

	engine.runCandidateLoop(c, ctx, req, iter, nil, &providerName, &lastErr, nil)

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("providers called after cancellation: %v", got)
	}
	if lastErr == nil || lastErr.ErrorType != provider.ErrorTypeClientDisconnected {
		t.Fatalf("last error = %#v, want client_disconnected", lastErr)
	}
	if c.Writer.Status() != 499 {
		t.Fatalf("status = %d, want 499", c.Writer.Status())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("client disconnect wrote response body: %q", w.Body.String())
	}
}

func TestRunCandidateLoop_CancelDuringAttemptStopsFailoverAndKeyReporting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := &cancelingProvider{name: "first", cancel: cancel}
	rec := &attemptRecorder{}
	second := &recordingProvider{name: "second", rec: rec, succeed: true}
	firstPool := newTestPoolWithTier("first", 1, keypool.BillingSourceAPI)
	engine, iter, req, c, w := newCancellationLoop(t,
		[]provider.Provider{first, second},
		map[string]*keypool.Pool{
			"first":  firstPool,
			"second": newTestPoolWithTier("second", 1, keypool.BillingSourceAPI),
		},
		[]router.ProviderRoute{
			{Name: "first", Model: "gpt-4", Priority: 1},
			{Name: "second", Model: "gpt-4", Priority: 2},
		})
	var providerName string
	var lastErr *provider.ProviderError

	engine.runCandidateLoop(c, ctx, req, iter, nil, &providerName, &lastErr, nil)

	if first.calls != 1 {
		t.Fatalf("first provider calls = %d, want 1", first.calls)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("failover providers called after cancellation: %v", got)
	}
	if lastErr == nil || lastErr.ErrorType != provider.ErrorTypeClientDisconnected {
		t.Fatalf("last error = %#v, want client_disconnected", lastErr)
	}
	if c.Writer.Status() != 499 {
		t.Fatalf("status = %d, want 499", c.Writer.Status())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("client disconnect wrote response body: %q", w.Body.String())
	}
	key := firstPool.Keys()[0]
	if key.ErrorCount != 0 || key.Status != keypool.KeyStatusActive || key.CircuitOpen {
		t.Fatalf("client cancellation mutated key: errors=%d status=%s circuit=%s",
			key.ErrorCount, key.Status, key.CircuitState)
	}
}

func TestDoStream_CancelAfterCommitKeeps200WithoutSyntheticError(t *testing.T) {
	stream := &blockingStreamProvider{name: "stream", chunks: make(chan *provider.StreamChunk, 1)}
	firstChunk := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	stream.chunks <- &provider.StreamChunk{Data: firstChunk}
	pool := newTestPoolWithTier("stream", 1, keypool.BillingSourceAPI)
	engine, iter, req, _, _ := newCancellationLoop(t,
		[]provider.Provider{stream},
		map[string]*keypool.Pool{"stream": pool},
		[]router.ProviderRoute{{Name: "stream", Model: "gpt-4", Priority: 1}})
	result, err := iter.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	req.IsStream = true

	recorder := httptest.NewRecorder()
	writer := &flushNotifyWriter{ResponseRecorder: recorder, flushed: make(chan struct{}, 4)}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, req.Path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	entry := &accesslog.AccessEntry{}
	type streamResult struct {
		ok   bool
		perr *provider.ProviderError
	}
	done := make(chan streamResult, 1)
	go func() {
		ok, _, perr, _ := engine.doStream(ctx, c, stream, req, result, entry)
		done <- streamResult{ok: ok, perr: perr}
	}()

	select {
	case <-writer.flushed:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("stream did not commit response")
	}

	select {
	case got := <-done:
		if !got.ok || got.perr != nil {
			t.Fatalf("doStream() = ok=%v err=%v, want committed success", got.ok, got.perr)
		}
	case <-time.After(time.Second):
		t.Fatal("doStream did not stop after client cancellation")
	}

	close(stream.chunks)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if entry.ErrorType != string(provider.ErrorTypeClientDisconnected) {
		t.Fatalf("error type = %q, want client_disconnected", entry.ErrorType)
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, firstChunk) {
		t.Fatalf("response bytes changed after cancellation: got=%q want=%q", got, firstChunk)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("event: error")) {
		t.Fatalf("synthetic error event appended after cancellation: %q", recorder.Body.String())
	}
}

func TestDoStream_RawProducerCloseAfterCancelMarksClientDisconnected(t *testing.T) {
	stream := &closeOnCancelStreamProvider{name: "raw-close"}
	pool := newTestPoolWithTier("raw-close", 1, keypool.BillingSourceAPI)
	engine, iter, req, _, _ := newCancellationLoop(t,
		[]provider.Provider{stream},
		map[string]*keypool.Pool{"raw-close": pool},
		[]router.ProviderRoute{{Name: "raw-close", Model: "gpt-4", Priority: 1}})
	result, err := iter.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	req.IsStream = true

	recorder := httptest.NewRecorder()
	writer := &flushNotifyWriter{ResponseRecorder: recorder, flushed: make(chan struct{}, 4)}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, req.Path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := &accesslog.AccessEntry{}
	done := make(chan struct{})
	go func() {
		ok, _, perr, _ := engine.doStream(ctx, c, stream, req, result, entry)
		if !ok || perr != nil {
			t.Errorf("doStream() = ok=%v err=%v, want committed stream", ok, perr)
		}
		close(done)
	}()

	select {
	case <-writer.flushed:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("stream did not commit response")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("doStream did not stop after raw producer close")
	}
	if entry.ErrorType != string(provider.ErrorTypeClientDisconnected) {
		t.Fatalf("error type = %q, want client_disconnected", entry.ErrorType)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}
