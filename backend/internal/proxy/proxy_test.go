package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// fakeProvider 用来测试 proxy 的可控 Provider
type fakeProvider struct {
	name       string
	proto      provider.Protocol
	models     []string
	respStatus int
	respBody   string
	respHdrs   http.Header
	// stream chunks(每个一行 SSE data: ...)
	streamChunks [][]byte
	// 触发错误的 error(如果设置,SendRequest 返回这个)
	err error
	// 记录收到的请求
	gotBody   []byte
	gotAuth   string
	gotTrace  string
	mu        sync.Mutex
	callCount int
}

func (p *fakeProvider) Name() string                { return p.name }
func (p *fakeProvider) Protocol() provider.Protocol { return p.proto }
func (p *fakeProvider) Models() []string            { return p.models }

func (p *fakeProvider) recordCall(req *provider.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gotBody = append([]byte(nil), req.Body...)
	p.gotAuth = req.Headers.Get("Authorization")
	p.gotTrace = req.Headers.Get("X-Request-Id")
	p.callCount++
}

func (p *fakeProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.recordCall(req)
	if p.err != nil {
		return nil, p.err
	}
	hdrs := http.Header{}
	for k, vs := range p.respHdrs {
		for _, v := range vs {
			hdrs.Add(k, v)
		}
	}
	if hdrs.Get("Content-Type") == "" {
		hdrs.Set("Content-Type", "application/json")
	}
	return &provider.Response{
		StatusCode: p.respStatus,
		Headers:    hdrs,
		Body:       []byte(p.respBody),
		Usage: &provider.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}, nil
}

func (p *fakeProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	p.recordCall(req)
	if p.err != nil {
		return nil, nil, p.err
	}
	ch := make(chan *provider.StreamChunk, len(p.streamChunks)+1)
	for _, c := range p.streamChunks {
		ch <- &provider.StreamChunk{Data: c}
	}
	ch <- &provider.StreamChunk{Err: io.EOF}
	close(ch)
	hdrs := http.Header{}
	hdrs.Set("Content-Type", "text/event-stream")
	return ch, &provider.Response{StatusCode: 200, Headers: hdrs}, nil
}

func (p *fakeProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeProvider) Close() error                          { return nil }

// buildEngine 构造一个挂上 fake provider + 路由的 Engine
func buildEngine(t *testing.T, p *fakeProvider, aliases map[string]router.AliasConfig) *Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)

	reg := provider.NewRegistry()
	reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
		return p, nil
	})
	mgr := provider.NewManager(reg, zap.NewNop())
	mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{
		Providers: map[string]provider.ManagerProviderConfig{
			p.Name(): {Enabled: true, Protocol: p.Protocol(), Models: p.models, APIKeys: []string{"sk-test"}},
		},
	})

	// 构造一个含 1 个 Key 的 Pool
	now := time.Now()
	pool := keypool.NewPool(p.Name(), []*keypool.Key{{
		ID: "k1", ProviderName: p.Name(), Name: "k1", Key: "sk-fake",
		Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})

	r := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{p.Name(): pool}, router.Config{
		Aliases: aliases,
	})

	// 一个记录用量的 fake recorder
	var usageCalls []*UsageRecord
	var usageMu sync.Mutex
	rec := &recordingUsage{onRecord: func(r *UsageRecord) {
		usageMu.Lock()
		defer usageMu.Unlock()
		usageCalls = append(usageCalls, r)
	}}

	engine := NewEngine(Config{
		Router:  r,
		Logger:  zap.NewNop(),
		Usage:   rec,
		Metrics: NoopMetricsRecorder{},
		Breaker: NoopCircuitReporter{},
	})

	t.Cleanup(func() {
		usageMu.Lock()
		t.Setenv("_USAGE_CALLS", "") // noop
		_ = usageCalls
	})

	return engine
}

type recordingUsage struct {
	onRecord func(*UsageRecord)
}

func (r *recordingUsage) Record(u *UsageRecord) { r.onRecord(u) }

func TestProxy_NonStream_PassesThroughBodyAndAuth(t *testing.T) {
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"deepseek-chat"},
		respStatus: 200,
		respBody:   `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	}
	e := buildEngine(t, p, map[string]router.AliasConfig{
		"coding-model": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "fake", Model: "deepseek-chat", Priority: 1},
		}},
	})

	r := gin.New()
	r.POST("/v1/chat/completions", e.HandleRequest)

	body := `{"model":"coding-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"x"`) {
		t.Errorf("response body missing: %s", w.Body.String())
	}

	// Provider 应该收到原始 body(未改写)
	if string(p.gotBody) != body {
		t.Errorf("body modified!\n  got:  %s\n  want: %s", p.gotBody, body)
	}
	// Auth header 应该是 Bearer sk-fake
	if p.gotAuth != "Bearer sk-fake" {
		t.Errorf("auth = %q, want Bearer sk-fake", p.gotAuth)
	}
	// X-Request-Id 应该被注入
	if p.gotTrace == "" {
		t.Error("X-Request-Id not injected")
	}
}

func TestProxy_NonStream_HonorsExistingTraceID(t *testing.T) {
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"ok":true}`,
	}
	e := buildEngine(t, p, map[string]router.AliasConfig{
		"coding-model": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "fake", Model: "m", Priority: 1},
		}},
	})
	r := gin.New()
	r.POST("/v1/chat/completions", e.HandleRequest)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"coding-model"}`))
	req.Header.Set("X-Request-Id", "trace-fixed-123")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if p.gotTrace != "trace-fixed-123" {
		t.Errorf("trace = %q, want trace-fixed-123", p.gotTrace)
	}
}

func TestProxy_NonStream_ProtocolFilter_MessagesToOpenAIBlocked(t *testing.T) {
	// 客户端发 anthropic 路径,但 fake provider 是 openai 协议 → 应被过滤 → 503
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{}`,
	}
	e := buildEngine(t, p, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "fake", Model: "m", Priority: 1},
		}},
	})
	r := gin.New()
	r.POST("/v1/messages", e.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"x"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (protocol mismatch)", w.Code)
	}
	if p.callCount != 0 {
		t.Errorf("provider should not be called, got %d calls", p.callCount)
	}
}

func TestProxy_NonStream_InvalidRequest_NoFailover(t *testing.T) {
	// Provider 返回 400 → 应直接透传给客户端,不重试
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{
			ProviderName: "fake", StatusCode: 400, ErrorType: provider.ErrorTypeInvalidRequest,
			Message: "bad model",
		},
	}
	e := buildEngine(t, p, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "fake", Model: "m", Priority: 1},
		}},
	})
	r := gin.New()
	r.POST("/v1/chat/completions", e.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 (invalid_request should pass through)", w.Code)
	}
}

func TestProxy_Stream_EmitsSSEChunks(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n"),
		[]byte(`data: {"choices":[{"delta":{"content":" world"}}]}` + "\n\n"),
		[]byte(`data: [DONE]` + "\n\n"),
	}
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"m"},
		streamChunks: chunks,
	}
	e := buildEngine(t, p, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "fake", Model: "m", Priority: 1},
		}},
	})
	r := gin.New()
	r.POST("/v1/chat/completions", e.HandleStreamRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","stream":true}`))
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Errorf("body missing 'Hello': %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("body missing [DONE]: %s", body)
	}
}

func TestExtractModelAndStream(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantModel string
		wantStream bool
	}{
		{"openai non-stream", `{"model":"x","messages":[]}`, "x", false},
		{"openai stream", `{"model":"y","stream":true}`, "y", true},
		{"empty body", ``, "", false},
		{"no model field", `{"messages":[]}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s, _ := extractModelAndStream([]byte(tt.body))
			if m != tt.wantModel || s != tt.wantStream {
				t.Errorf("got (%q,%v), want (%q,%v)", m, s, tt.wantModel, tt.wantStream)
			}
		})
	}
}

func TestHopByHopHeaders(t *testing.T) {
	for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding"} {
		if !isHopByHop(h) {
			t.Errorf("expected hop-by-hop: %s", h)
		}
	}
	for _, h := range []string{"Content-Type", "X-Request-Id", "Authorization"} {
		if isHopByHop(h) {
			t.Errorf("not hop-by-hop: %s", h)
		}
	}
}

func TestCopyResponseHeadersStripsHopByHop(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Connection", "close")
	src.Set("X-Custom", "value")
	copyResponseHeaders(c, src)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type lost")
	}
	if got := w.Header().Get("Connection"); got != "" {
		t.Errorf("Connection should be stripped, got %q", got)
	}
	if got := w.Header().Get("X-Custom"); got != "value" {
		t.Errorf("X-Custom lost")
	}
}

// silence unused if some imports trimmed
var _ = json.NewEncoder
var _ = bytes.NewReader

// TestStreamBuffer_NoLeakAcrossChunks 验证 F4 streamCnt 不会因为 per-chunk
// 调用而泄漏 — 这是旧实现里的一个 critical bug:
//
//   旧 acquireStreamSlot 在 appendStreamChunk 内被调用,每个 SSE chunk 都
//   +1,但 finalizeStream 只 -1 一次。一个 N-chunk 的 stream 会泄漏 N-1
//   的计数,长 SSE 响应会让计数器永久超过 1000,后续 stream 的 chunk
//   全部被静默丢弃,body 不入 buffer。
//
// 修复后,acquireStreamSlot 只在 doStream 开始前调一次,appendStreamChunk
// 是 lookup-only。本测试覆盖真实工作模式:
//
//   Part 1: 5 个 stream × 300 chunk,全部 finalize → streamCnt 必须归 0。
//   Part 2: F4 cap — 1500 并发 acquire,前 1000 成功,后 500 拒绝;
//           finalize 全部 1000 个成功 slot 后 → streamCnt 必须归 0。
func TestStreamBuffer_NoLeakAcrossChunks(t *testing.T) {
	e := &Engine{}

	// -------------------------------------------------------------------------
	// Part 1: per-chunk 模式 — 模拟真实 SSE 流式响应(多 chunk + finalize)
	// -------------------------------------------------------------------------
	const streams = 5
	const chunksPerStream = 300
	chunk := []byte(`data: {"delta":"hello world"}\n\n`)

	for s := 0; s < streams; s++ {
		traceID := fmt.Sprintf("leak-trace-%d", s)
		acc, ok := e.acquireStreamSlot(traceID)
		if !ok {
			t.Fatalf("stream %d: acquire unexpectedly failed (counter should be 0 here)", s)
		}
		// 一次 acquire,模拟 N 个 chunk 入 buffer(per-chunk lookup,不再 +1)
		for c := 0; c < chunksPerStream; c++ {
			e.appendStreamChunk(traceID, chunk)
		}
		// sanity:buffer 应累积到 (chunksPerStream * len(chunk)) bytes(远小于 cap)
		if got := acc.buf.Len(); got != chunksPerStream*len(chunk) {
			t.Errorf("stream %d: buf.Len = %d, want %d", s, got, chunksPerStream*len(chunk))
		}
		e.finalizeStream(traceID, nil)
	}

	if got := atomic.LoadInt64(&e.streamCnt); got != 0 {
		t.Errorf("after %d streams × %d chunks + finalize: streamCnt = %d, want 0 (counter leak!)",
			streams, chunksPerStream, got)
	}

	// -------------------------------------------------------------------------
	// Part 2: F4 cap — 1500 并发 acquire,前 1000 成功,后 500 拒绝;
	// finalize 全部成功 slot 后,计数器必须归 0。
	// -------------------------------------------------------------------------
	const total = 1500
	const capN = maxConcurrentStreams

	var (
		wg           sync.WaitGroup
		acquiredN    int64
		rejectedN    int64
		startBarrier sync.WaitGroup
		// 记录 acquire 成功的 traceIDs(顺序非确定,因为是并发),
		// finalize 阶段只能 finalize 这些,不能按 idx 循环 — 否则
		// 会对某些从未占位 streamBuf 的 traceID 调 finalizeStream,
		// 那是预期路径但会让本测试看起来像"counter 泄漏"。
		acquiredMu  sync.Mutex
		acquiredIDs []string
	)
	startBarrier.Add(1)

	wg.Add(total)
	for i := 0; i < total; i++ {
		go func(idx int) {
			defer wg.Done()
			startBarrier.Wait() // 让所有 goroutine 同步出发,增大并发争用
			traceID := fmt.Sprintf("cap-trace-%d", idx)
			_, ok := e.acquireStreamSlot(traceID)
			if ok {
				atomic.AddInt64(&acquiredN, 1)
				acquiredMu.Lock()
				acquiredIDs = append(acquiredIDs, traceID)
				acquiredMu.Unlock()
			} else {
				atomic.AddInt64(&rejectedN, 1)
			}
		}(i)
	}

	startBarrier.Done()
	wg.Wait()

	if acquiredN != capN {
		t.Errorf("acquired = %d, want %d", acquiredN, capN)
	}
	if rejectedN != total-capN {
		t.Errorf("rejected = %d, want %d", rejectedN, total-capN)
	}

	// acquire 阶段后,counter 应该停在 capN(因为被拒绝的 slot 短暂 +1/-1
	// 后回到原值,而成功的 capN 个 slot 在 streamBuf 里仍占着)。
	if got := atomic.LoadInt64(&e.streamCnt); got != int64(capN) {
		t.Errorf("after acquire phase: streamCnt = %d, want %d", got, capN)
	}

	// finalize 全部成功 acquire 的 slot,counter 必须归 0。
	// (不能用 idx 循环 — 1500 个并发 goroutine 不保证 acquire 顺序。)
	for _, traceID := range acquiredIDs {
		e.finalizeStream(traceID, nil)
	}

	if got := atomic.LoadInt64(&e.streamCnt); got != 0 {
		t.Errorf("after finalizing all %d acquired slots: streamCnt = %d, want 0 (counter leak!)",
			capN, got)
	}
}
