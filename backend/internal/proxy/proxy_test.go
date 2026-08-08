package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
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
	// 流中途错误:设置后,数据 chunk 发完后、EOF 之前发一个 Err chunk
	streamMidErr error
	// 触发错误的 error(如果设置,SendRequest 返回这个)
	err error
	// Task 5: 按 key ID 区分的错误(换 key 重试用) — 命中时优先于 p.err
	errByKey map[string]error
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

// errFor 返回本请求应触发的错误:按 key ID 优先,否则全局 err
func (p *fakeProvider) errFor(req *provider.Request) error {
	if req.Key != nil && p.errByKey != nil {
		if err, ok := p.errByKey[req.Key.ID]; ok {
			return err
		}
	}
	return p.err
}

func (p *fakeProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.recordCall(req)
	if err := p.errFor(req); err != nil {
		return nil, err
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
	if err := p.errFor(req); err != nil {
		return nil, nil, err
	}
	ch := make(chan *provider.StreamChunk, len(p.streamChunks)+2)
	for _, c := range p.streamChunks {
		ch <- &provider.StreamChunk{Data: c}
	}
	if p.streamMidErr != nil {
		// 模拟流中途错误(上游断流 / 连接被杀):数据 chunk 之后、EOF 之前
		ch <- &provider.StreamChunk{Err: p.streamMidErr}
	}
	ch <- &provider.StreamChunk{Err: io.EOF}
	close(ch)
	hdrs := http.Header{}
	hdrs.Set("Content-Type", "text/event-stream")
	return ch, &provider.Response{StatusCode: 200, Headers: hdrs}, nil
}

func (p *fakeProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeProvider) SetPool(*keypool.Pool)                 {}
func (p *fakeProvider) Close() error                          { return nil }

// buildEngine 构造一个挂上 fake provider + 路由的 Engine
// 返回 (engine, rec) — rec 记录所有用量上报,测试断言用
func buildEngine(t *testing.T, p *fakeProvider, aliases map[string]router.AliasConfig) (*Engine, *recordingUsage) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)

	reg := provider.NewRegistry()
	reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
		return p, nil
	})
	mgr := provider.NewManager(reg, zap.NewNop())
	mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{
		Providers: map[string]provider.ManagerProviderConfig{
			p.Name(): {Enabled: true, Protocol: p.Protocol(), Models: p.models},
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
	rec := &recordingUsage{}

	// no-op accesslog recorder:使 entry 非 nil(与生产一致),doStream 才能把
	// 流中途错误预设到 entry.ErrorType,usage 记录据此上报 error_type
	accessR, err := accesslog.NewRecorder(accesslog.RecorderConfig{}, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("new accesslog recorder: %v", err)
	}

	engine := NewEngine(Config{
		Router:  r,
		Logger:  zap.NewNop(),
		Usage:   rec,
		Metrics: NoopMetricsRecorder{},

		AccessLog: accessR,
	})

	return engine, rec
}

type recordingUsage struct {
	mu      sync.Mutex
	records []*UsageRecord
}

func (r *recordingUsage) Record(u *UsageRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, u)
}

// snapshot 返回已记录用量的副本(测试断言用)
func (r *recordingUsage) snapshot() []*UsageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*UsageRecord(nil), r.records...)
}

func TestProxy_NonStream_PassesThroughBodyAndAuth(t *testing.T) {
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"deepseek-chat"},
		respStatus: 200,
		respBody:   `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	}
	e, _ := buildEngine(t, p, map[string]router.AliasConfig{
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

	// P-catch-all: body 里的 model 按路由结果重写为真实模型名(deepseek-chat),
	// 上游收到解析后的名字而非 alias 名 — 严格校验的 provider(如 DeepSeek)才能正常工作
	wantBody := `{"messages":[{"content":"hello","role":"user"}],"model":"deepseek-chat"}`
	if string(p.gotBody) != wantBody {
		t.Errorf("body modified!\n  got:  %s\n  want: %s", p.gotBody, wantBody)
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
	e, _ := buildEngine(t, p, map[string]router.AliasConfig{
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
	e, _ := buildEngine(t, p, map[string]router.AliasConfig{
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
	e, _ := buildEngine(t, p, map[string]router.AliasConfig{
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
	e, _ := buildEngine(t, p, map[string]router.AliasConfig{
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

// TestProxy_Stream_MidError_NotMaskedAsOK — 流中途错误(上游断流 / 连接被杀,
// 如 server write_timeout 掐断)必须:
//  1. 不触发 failover / 重试(HTTP 头已发出、状态码锁死 200,换 provider 也没用)
//  2. 给客户端写 error event(而不是静默结束流)
//  3. 用量记录的 error_type = stream_interrupted(access log 不再伪装成 ok)
//
// 回归:2026-08-05 Claude Code "Connection closed mid-response" —
// 网关 120s 掐断流后 access log 记 200 ok,失败完全隐身。
func TestProxy_Stream_MidError_NotMaskedAsOK(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n"),
	}
	p := &fakeProvider{
		name: "fake", proto: provider.ProtocolOpenAI, models: []string{"m"},
		streamChunks: chunks,
		streamMidErr: errors.New("context canceled"),
	}
	e, rec := buildEngine(t, p, map[string]router.AliasConfig{
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

	// 1. 状态码只能是 200(头已发出)——错误通过 error event 表达
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (headers already committed)", w.Code)
	}
	// 2. 客户端必须收到 error event
	if !strings.Contains(w.Body.String(), `"type":"stream_error"`) {
		t.Errorf("body missing stream_error event: %s", w.Body.String())
	}
	// 3. 不重试 / 不 failover(链上只有 1 个 provider,调用次数必须为 1)
	p.mu.Lock()
	calls := p.callCount
	p.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider called %d times, want 1 (no failover after stream starts)", calls)
	}
	// 4. 用量记录必须带 error_type,不能是空(伪装成 ok)
	recs := rec.snapshot()
	if len(recs) != 1 {
		t.Fatalf("usage records = %d, want 1", len(recs))
	}
	if recs[0].ErrorType != "stream_interrupted" {
		t.Errorf("usage error_type = %q, want %q", recs[0].ErrorType, "stream_interrupted")
	}
}

func TestExtractModelAndStream(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
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

// ============================================================================
// Task 5: 分层 failover — 错误分类路由(行为矩阵 6 行)
// ============================================================================

// mkPool 构造一个含若干 key 的 Pool,全部属于指定 tier(BillingSource)。
// key ID 用纯数字(parseKeyIDUint 能解析,AcquireFromTierExcluding 的排除才生效)
func mkPool(providerName string, keyIDs []string, tier string) *keypool.Pool {
	now := time.Now()
	keys := make([]*keypool.Key, 0, len(keyIDs))
	for _, id := range keyIDs {
		keys = append(keys, &keypool.Key{
			ID: id, ProviderName: providerName, Name: id, Key: "sk-" + id,
			Status: keypool.KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	return keypool.NewPool(providerName, keys, nil, keypool.Config{})
}

// buildEngineMulti 构造多 provider / 每 provider 多 key 的 Engine(Task 5 测试用)
// pools: providerName → Pool;providers 里每个都要有对应的 Manager 配置
// opts: 可选的 Config 覆盖(如注入 Authenticator 走白名单路径)
func buildEngineMulti(t *testing.T, providers []*fakeProvider, pools map[string]*keypool.Pool, aliases map[string]router.AliasConfig, opts ...func(*Config)) (*Engine, *recordingUsage) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)

	reg := provider.NewRegistry()
	for _, p := range providers {
		p := p
		reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) { return p, nil })
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	cfg := provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{}}
	for _, p := range providers {
		cfg.Providers[p.Name()] = provider.ManagerProviderConfig{
			Enabled: true, Protocol: p.Protocol(), Models: p.models,
		}
	}
	if err := mgr.LoadFromConfig(context.Background(), &cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	r := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{Aliases: aliases})

	rec := &recordingUsage{}
	accessR, err := accesslog.NewRecorder(accesslog.RecorderConfig{}, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("new accesslog recorder: %v", err)
	}
	engineCfg := Config{
		Router: r, Logger: zap.NewNop(), Usage: rec, Metrics: NoopMetricsRecorder{}, AccessLog: accessR,
		// 注入默认 QuotaChecker — 测试走全局 quotacheck balancer registry(与生产一致)
		QuotaChecker: CheckQuotaFunc(func(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error) {
			return quotacheck.CheckQuota(ctx, providerName, baseURL, k)
		}),
	}
	for _, o := range opts {
		o(&engineCfg)
	}
	return NewEngine(engineCfg), rec
}

// doProxyRequest 发一个非流式 /v1/chat/completions 请求
func doProxyRequest(e *Engine, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.POST("/v1/chat/completions", e.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// t5Alias 返回标准的两候选 alias(minimax token_plan → deepseek api)
func t5Alias(extra ...router.ProviderRoute) map[string]router.AliasConfig {
	providers := []router.ProviderRoute{
		{Name: "mm", Model: "m", Priority: 1},
		{Name: "ds", Model: "m", Priority: 2},
	}
	providers = append(providers, extra...)
	return map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: providers},
	}
}

// errBalancer 一个永远返回错误的 fake Balancer(CheckQuota 查询失败 → 未知)
type errBalancer struct{}

func (errBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	return quotacheck.Balance{}, errors.New("balance query failed")
}

// exhaustedBalancer 一个确认「余额耗尽」的 fake Balancer(CheckQuota → has=false)
type exhaustedBalancer struct{}

func (exhaustedBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	return quotacheck.Balance{HasQuota: false}, nil
}

//  1. 网络类层内穷尽 → 失败返回,不降档(不变式)
//     provider mm(token_plan) 2 把 key 都返回 connection 错误;
//     provider ds(api) healthy → 最终必须 502/超时,请求不能到 ds
func TestProxy_NetworkExhaustedInTier_FailsWithoutDowngrade(t *testing.T) {
	connErr := &provider.ProviderError{ProviderName: "mm", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey: map[string]error{"1": connErr, "2": connErr}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502(token_plan 层网络穷尽不降档); body = %s", w.Code, w.Body.String())
	}
	if ds.callCount != 0 {
		t.Errorf("deepseek called %d times, want 0(层内未确认耗尽,绝不能落 api 层)", ds.callCount)
	}
	if mm.callCount != 2 {
		t.Errorf("minimax called %d times, want 2(两把 key 各试一次:原 key + 换 key 重试)", mm.callCount)
	}
}

//  2. 额度类全层穷尽 → 降档 api 层成功
//     mm(token_plan) 返回 quota_exceeded;ds(api) healthy → 200,provider=ds
func TestProxy_QuotaExhausted_DowngradesToApi(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "mm", StatusCode: http.StatusPaymentRequired, ErrorType: provider.ErrorTypeQuotaExceeded, Message: "quota exceeded"}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(额度耗尽应降档到 api); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ds-ok") {
		t.Errorf("response = %s, want deepseek 的响应(降档后 provider=deepseek)", w.Body.String())
	}
	if mm.callCount != 1 || ds.callCount != 1 {
		t.Errorf("call counts: mm=%d ds=%d, want 1/1", mm.callCount, ds.callCount)
	}
}

//  3. 换 key 重试:key-1 connection 失败 → 同 provider key-2 成功(不走 failover)
//     mm 2 把 key:key-1 connection 错误,key-2 healthy → 200,provider=mm
func TestProxy_RetryWithSecondKey(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey:   map[string]error{"1": &provider.ProviderError{ProviderName: "mm", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}},
		respStatus: 200, respBody: `{"id":"mm-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(换 key 重试成功); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mm-ok") {
		t.Errorf("response = %s, want minimax 的响应", w.Body.String())
	}
	if mm.callCount != 2 {
		t.Errorf("minimax called %d times, want 2(key-1 失败 + key-2 成功)", mm.callCount)
	}
	// 第二次请求必须用的是 key-2(Auth header 应为 Bearer sk-2)
	if mm.gotAuth != "Bearer sk-2" {
		t.Errorf("final request auth = %q, want Bearer sk-2(换 key 后必须用新 key 发)", mm.gotAuth)
	}
}

//  4. 主动查询失败 → 按未耗尽:继续层内尝试(不降档)
//     mm(token_plan, 1 把 key connection 失败,CheckQuota 返回 error)
//     ds(api) healthy → 请求失败返回,不进 ds
func TestProxy_CheckQuotaError_StaysInTier(t *testing.T) {
	// 注册一个 balance 查询必失败的 balancer(查询失败 = 未知 → 按未耗尽处理)
	quotacheck.RegisterBalancer("mm-qerr", errBalancer{})

	mm := &fakeProvider{name: "mm-qerr", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "mm-qerr", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm-qerr": mkPool("mm-qerr", []string{"1"}, "token_plan"),
		"ds":      mkPool("ds", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "mm-qerr", Model: "m", Priority: 1},
			{Name: "ds", Model: "m", Priority: 2},
		}},
	})

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502(额度查询失败=未知,按未耗尽不降档); body = %s", w.Code, w.Body.String())
	}
	if ds.callCount != 0 {
		t.Errorf("deepseek called %d times, want 0(无额度证据,请求不能到 api 层)", ds.callCount)
	}
	if mm.callCount != 1 {
		t.Errorf("minimax called %d times, want 1", mm.callCount)
	}
}

//  5. 同层换 provider:mm 全网络失败 → 同层 kimi(token_plan) 成功
//     两个 token_plan provider + 一个 api provider → 200,provider=kimi
func TestProxy_SameTierNextProvider(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "mm", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}}
	kimi := &fakeProvider{name: "kimi", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"kimi-ok"}`}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, kimi, ds}, map[string]*keypool.Pool{
		"mm":   mkPool("mm", []string{"1"}, "token_plan"),
		"kimi": mkPool("kimi", []string{"1"}, "token_plan"),
		"ds":   mkPool("ds", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "mm", Model: "m", Priority: 1},
			{Name: "kimi", Model: "m", Priority: 2},
			{Name: "ds", Model: "m", Priority: 3},
		}},
	})

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(同层下个 provider 承接); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "kimi-ok") {
		t.Errorf("response = %s, want kimi 的响应(同层 token_plan 换 provider)", w.Body.String())
	}
	if ds.callCount != 0 {
		t.Errorf("deepseek called %d times, want 0(同层还有候选,不该降档到 api)", ds.callCount)
	}
}

//  6. 不可重试错误直接失败:invalid_request 不重试不降档(现有语义回归)
//     mm 返 400 invalid_request;ds(api) healthy → 400 透传,请求不能到 ds
func TestProxy_InvalidRequest_NoRetry(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "mm", StatusCode: http.StatusBadRequest, ErrorType: provider.ErrorTypeInvalidRequest, Message: "bad model"}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400(invalid_request 透传); body = %s", w.Code, w.Body.String())
	}
	if ds.callCount != 0 {
		t.Errorf("deepseek called %d times, want 0(不可重试错误不 failover)", ds.callCount)
	}
	if mm.callCount != 1 {
		t.Errorf("minimax called %d times, want 1(不重试)", mm.callCount)
	}
}

//  7. 审阅修复 FIX 4:主动查询确认耗尽(has=false)→ 额度证据 → 降档 api 成功
//     mm(token_plan) 网络类失败 + balancer 返回 HasQuota=false;ds(api) healthy → 200
func TestProxy_CheckQuotaConfirmedExhausted_DowngradesToApi(t *testing.T) {
	quotacheck.RegisterBalancer("mm-exhausted", exhaustedBalancer{})

	mm := &fakeProvider{name: "mm-exhausted", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "mm-exhausted", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm-exhausted": mkPool("mm-exhausted", []string{"1"}, "token_plan"),
		"ds":           mkPool("ds", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "mm-exhausted", Model: "m", Priority: 1},
			{Name: "ds", Model: "m", Priority: 2},
		}},
	})

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(主动查询确认耗尽 → 允许降档); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ds-ok") {
		t.Errorf("response = %s, want deepseek 的响应", w.Body.String())
	}
	if mm.callCount != 1 || ds.callCount != 1 {
		t.Errorf("call counts: mm=%d ds=%d, want 1/1", mm.callCount, ds.callCount)
	}
}

//  8. 审阅修复 FIX 1:auth(403)错误也走换 key 重试(决策表 row 3)
//     mm 2 把 key:key-1 auth 403,key-2 healthy → 200 经 key-2,不走 failover
func TestProxy_AuthError_SwapsKey(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey:   map[string]error{"1": &provider.ProviderError{ProviderName: "mm", StatusCode: http.StatusForbidden, ErrorType: provider.ErrorTypeAuth, Message: "invalid api key"}},
		respStatus: 200, respBody: `{"id":"mm-ok"}`}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(auth 换 key 重试成功); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mm-ok") {
		t.Errorf("response = %s, want minimax 的响应(auth 是 key 问题,换 key 即解决,不 failover)", w.Body.String())
	}
	if mm.callCount != 2 {
		t.Errorf("minimax called %d times, want 2(key-1 auth 失败 + key-2 成功)", mm.callCount)
	}
	if mm.gotAuth != "Bearer sk-2" {
		t.Errorf("final request auth = %q, want Bearer sk-2", mm.gotAuth)
	}
	if ds.callCount != 0 {
		t.Errorf("deepseek called %d times, want 0(auth 换 key 解决,不该降档)", ds.callCount)
	}
}

//  9. 审阅修复 FIX 2:maxRetry 封顶不截断层内降档(每层安全阀)
//     3 个 token_plan provider 全 quota_exceeded + api healthy,maxRetry=3 → 200 降档
func TestProxy_MaxRetryDoesNotBlockQuotaDowngrade(t *testing.T) {
	mkQuotaProvider := func(name string) *fakeProvider {
		return &fakeProvider{name: name, proto: provider.ProtocolOpenAI, models: []string{"m"},
			err: &provider.ProviderError{ProviderName: name, StatusCode: http.StatusPaymentRequired, ErrorType: provider.ErrorTypeQuotaExceeded, Message: "quota exceeded"}}
	}
	q1 := mkQuotaProvider("q1")
	q2 := mkQuotaProvider("q2")
	q3 := mkQuotaProvider("q3")
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{q1, q2, q3, ds}, map[string]*keypool.Pool{
		"q1": mkPool("q1", []string{"1"}, "token_plan"),
		"q2": mkPool("q2", []string{"1"}, "token_plan"),
		"q3": mkPool("q3", []string{"1"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "q1", Model: "m", Priority: 1},
			{Name: "q2", Model: "m", Priority: 2},
			{Name: "q3", Model: "m", Priority: 3},
			{Name: "ds", Model: "m", Priority: 4},
		}},
	})

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(3 层候选全额度耗尽,降档 api 成功); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ds-ok") {
		t.Errorf("response = %s, want deepseek 的响应(maxRetry 封顶不能截断有证据的降档)", w.Body.String())
	}
	for _, q := range []*fakeProvider{q1, q2, q3} {
		if q.callCount != 1 {
			t.Errorf("provider %s called %d times, want 1", q.name, q.callCount)
		}
	}
	if ds.callCount != 1 {
		t.Errorf("deepseek called %d times, want 1", ds.callCount)
	}
}

//  10. 审阅修复 FIX 3:白名单 skip 不阻断层推进(旧语义回归)
//     token_plan 候选被白名单跳过、api 候选在白名单内 → 200(不能 403 整层判死)
func TestProxy_WhitelistSkip_AdvancesTier(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"model-tp"},
		respStatus: 200, respBody: `{"id":"mm-ok"}`}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"model-api"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	gk := &auth.GatewayKey{ID: "k1", Name: "test-key", AllowedModels: []string{"model-api"}}
	authn := auth.New(nil)
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "mm", Model: "model-tp", Priority: 1},
			{Name: "ds", Model: "model-api", Priority: 2},
		}},
	}, func(cfg *Config) { cfg.Authenticator = authn })

	gr := gin.New()
	gr.Use(func(c *gin.Context) {
		c.Set("gateway_key", gk)
		c.Set("gateway_key_id", gk.ID)
	})
	gr.POST("/v1/chat/completions", e.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	gr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(token_plan 纯 skip 层应自由推进到 api); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ds-ok") {
		t.Errorf("response = %s, want deepseek 的响应(白名单 skip 不算层失败,不阻断降档路径)", w.Body.String())
	}
	if mm.callCount != 0 {
		t.Errorf("minimax called %d times, want 0(白名单外候选不应发请求)", mm.callCount)
	}
}

//  11. 第二轮审阅 I-1:换 key 后二次尝试返回额度类错误 → 证据不能丢(网络源)
//     mm(token_plan) key-1 connection 错 → 换 key-2 → key-2 quota_exceeded;
//     ds(api) healthy → 200 经 ds(swap 尝试的额度证据驱动降档)
func TestProxy_SwapQuotaError_DowngradesToApi(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey: map[string]error{
			"1": &provider.ProviderError{ProviderName: "mm", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"},
			"2": &provider.ProviderError{ProviderName: "mm", StatusCode: http.StatusPaymentRequired, ErrorType: provider.ErrorTypeQuotaExceeded, Message: "quota exceeded"},
		}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(换 key 后的额度错误也是降档证据); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ds-ok") {
		t.Errorf("response = %s, want deepseek 的响应(swap 尝试的额度证据应驱动降档)", w.Body.String())
	}
	if mm.callCount != 2 {
		t.Errorf("minimax called %d times, want 2(key-1 connection + key-2 quota)", mm.callCount)
	}
	if ds.callCount != 1 {
		t.Errorf("deepseek called %d times, want 1(证据确认后降档)", ds.callCount)
	}
}

//  12. 第二轮审阅 I-2:降档后每层重置 maxRetry 预算(自然切换路径)
//     tp 2 候选全 quota_exceeded(消耗 2 次 < maxRetry=3,经自然路径跨层)+
//     api 2 候选(api-a 错、api-b healthy)→ 200 经 api-b
//     (不重置时 api 层只剩 maxRetry-2=1 次机会,api-b 永远轮不到)
func TestProxy_DowngradeResetsRetryBudget(t *testing.T) {
	mkQuotaProvider := func(name string) *fakeProvider {
		return &fakeProvider{name: name, proto: provider.ProtocolOpenAI, models: []string{"m"},
			err: &provider.ProviderError{ProviderName: name, StatusCode: http.StatusPaymentRequired, ErrorType: provider.ErrorTypeQuotaExceeded, Message: "quota exceeded"}}
	}
	q1 := mkQuotaProvider("q1")
	q2 := mkQuotaProvider("q2")
	apiA := &fakeProvider{name: "api-a", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{ProviderName: "api-a", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}}
	apiB := &fakeProvider{name: "api-b", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: 200, respBody: `{"id":"api-b-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{q1, q2, apiA, apiB}, map[string]*keypool.Pool{
		"q1":    mkPool("q1", []string{"1"}, "token_plan"),
		"q2":    mkPool("q2", []string{"1"}, "token_plan"),
		"api-a": mkPool("api-a", []string{"1"}, "api"),
		"api-b": mkPool("api-b", []string{"1"}, "api"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "q1", Model: "m", Priority: 1},
			{Name: "q2", Model: "m", Priority: 2},
			{Name: "api-a", Model: "m", Priority: 3},
			{Name: "api-b", Model: "m", Priority: 4},
		}},
	})

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(降档后 api 层预算重置,maxRetry=2 也应试到 api-b); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "api-b-ok") {
		t.Errorf("response = %s, want api-b 的响应(降档后 api 层两个候选都被试)", w.Body.String())
	}
	for _, q := range []*fakeProvider{q1, q2} {
		if q.callCount != 1 {
			t.Errorf("provider %s called %d times, want 1", q.name, q.callCount)
		}
	}
	if apiA.callCount != 1 || apiB.callCount != 1 {
		t.Errorf("api call counts: api-a=%d api-b=%d, want 1/1", apiA.callCount, apiB.callCount)
	}
}

//  13. 第二轮审阅 I-3:换 key 重试不跨出 GatewayKey 绑定的 ProviderKeyIDs(P34)
//     mm 2 把 key:key-1 connection 错且在集合内,key-2 healthy 但不在集合
//     (gk.ProviderKeyIDs=[1])→ 502,mm.callCount==1(换 key 未跨出集合)
func TestProxy_SwapToOtherKey_RespectsProviderKeyIDs(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey:   map[string]error{"1": &provider.ProviderError{ProviderName: "mm", ErrorType: provider.ErrorTypeConnection, Message: "conn refused"}},
		respStatus: 200, respBody: `{"id":"mm-ok"}`}
	gk := &auth.GatewayKey{ID: "k1", Name: "test-key", ProviderKeyIDs: []uint{1}}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
	}, map[string]router.AliasConfig{
		"x": {Strategy: "priority", Providers: []router.ProviderRoute{
			{Name: "mm", Model: "m", Priority: 1},
		}},
	})

	gr := gin.New()
	gr.Use(func(c *gin.Context) {
		c.Set("gateway_key", gk)
		c.Set("gateway_key_id", gk.ID)
	})
	gr.POST("/v1/chat/completions", e.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	gr.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502(换 key 不能跨出 ProviderKeyIDs 集合); body = %s", w.Code, w.Body.String())
	}
	if mm.callCount != 1 {
		t.Errorf("minimax called %d times, want 1(集合外 healthy key-2 不得被 swap 使用)", mm.callCount)
	}
}

// TestStreamBuffer_NoLeakAcrossChunks 验证 F4 streamCnt 不会因为 per-chunk
// 调用而泄漏 — 这是旧实现里的一个 critical bug:
//
//	旧 acquireStreamSlot 在 appendStreamChunk 内被调用,每个 SSE chunk 都
//	+1,但 finalizeStream 只 -1 一次。一个 N-chunk 的 stream 会泄漏 N-1
//	的计数,长 SSE 响应会让计数器永久超过 1000,后续 stream 的 chunk
//	全部被静默丢弃,body 不入 buffer。
//
// 修复后,acquireStreamSlot 只在 doStream 开始前调一次,appendStreamChunk
// 是 lookup-only。本测试覆盖真实工作模式:
//
//	Part 1: 5 个 stream × 300 chunk,全部 finalize → streamCnt 必须归 0。
//	Part 2: F4 cap — 1500 并发 acquire,前 1000 成功,后 500 拒绝;
//	        finalize 全部 1000 个成功 slot 后 → streamCnt 必须归 0。
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

// TestProxy_WhitelistSkipsCandidates P-catch-all:
// 白名单按候选逐个校验(跳过式)— 第一个候选白名单外 → 跳过继续试白名单内的;
// 链上全部被排除 → 403 model_not_allowed(不是 502)
func TestProxy_WhitelistSkipsCandidates(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	pa := &fakeProvider{name: "pa", proto: provider.ProtocolOpenAI, models: []string{"model-a"},
		respStatus: 200, respBody: `{"id":"a"}`}
	pb := &fakeProvider{name: "pb", proto: provider.ProtocolOpenAI, models: []string{"model-b"},
		respStatus: 200, respBody: `{"id":"b"}`}

	reg := provider.NewRegistry()
	for _, p := range []*fakeProvider{pa, pb} {
		p := p
		reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) { return p, nil })
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{
		Providers: map[string]provider.ManagerProviderConfig{
			"pa": {Enabled: true, Protocol: provider.ProtocolOpenAI, Models: []string{"model-a"}},
			"pb": {Enabled: true, Protocol: provider.ProtocolOpenAI, Models: []string{"model-b"}},
		},
	}); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	now := time.Now()
	mkPool := func(name string) *keypool.Pool {
		return keypool.NewPool(name, []*keypool.Key{{
			ID: name + "-1", ProviderName: name, Name: "k1", Key: "sk-fake",
			Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
		}}, nil, keypool.Config{})
	}
	pools := map[string]*keypool.Pool{"pa": mkPool("pa"), "pb": mkPool("pb")}

	r := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"x": {Strategy: "priority", Providers: []router.ProviderRoute{
				{Name: "pa", Model: "model-a", Priority: 1},
				{Name: "pb", Model: "model-b", Priority: 2},
			}},
		},
	})

	doReq := func(allowed []string) *httptest.ResponseRecorder {
		t.Helper()
		gk := &auth.GatewayKey{ID: "k1", Name: "test-key", AllowedModels: allowed}
		authn := auth.New(nil)
		eng := NewEngine(Config{
			Router: r, Logger: zap.NewNop(),
			Usage: NoopUsageRecorder{}, Metrics: NoopMetricsRecorder{},
			Authenticator: authn,
		})
		gr := gin.New()
		gr.Use(func(c *gin.Context) {
			c.Set("gateway_key", gk)
			c.Set("gateway_key_id", gk.ID)
		})
		gr.POST("/v1/chat/completions", eng.HandleRequest)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
		w := httptest.NewRecorder()
		gr.ServeHTTP(w, req)
		return w
	}

	// 白名单只含 model-b:pa(model-a)被跳过,请求落到 pb
	w1 := doReq([]string{"model-b"})
	if w1.Code != 200 {
		t.Fatalf("status = %d, want 200(跳过 pa 继续 pb); body = %s", w1.Code, w1.Body.String())
	}
	if !strings.Contains(w1.Body.String(), `"id":"b"`) {
		t.Errorf("response = %s, want pb 的响应(白名单外候选被跳过)", w1.Body.String())
	}

	// 白名单不含任何候选:全部跳过 → 403 model_not_allowed
	w2 := doReq([]string{"model-z"})
	if w2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "model_not_allowed") {
		t.Errorf("body = %s, want type=model_not_allowed", w2.Body.String())
	}
}

// TestStripResponsesReasoning P-responses:
// 剥离 input 里的 reasoning 项和 message 内容块里的 reasoning_text —
// 跨厂商切换时 MiniMax 的推理块会被 DeepSeek 400 拒收
func TestStripResponsesReasoning(t *testing.T) {
	body := `{
		"model": "gpt-5-codex",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "thinking..."}], "content": "enc", "encrypted_content": "xyz"},
			{"type": "function_call", "call_id": "fc_1", "name": "apply_patch", "arguments": "{}"},
			{"type": "message", "role": "assistant", "content": [
				{"type": "output_text", "text": "ok"},
				{"type": "reasoning_text", "text": "old thinking"}
			]}
		],
		"stream": true
	}`

	out, stripped := stripResponsesReasoning([]byte(body))
	if !stripped {
		t.Error("strip should report reasoning was removed")
	}
	var parsed struct {
		Input     []map[string]any `json:"input"`
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("strip produced invalid JSON: %v", err)
	}
	if len(parsed.Input) != 3 {
		t.Fatalf("input items = %d, want 3(reasoning 项被剥离)", len(parsed.Input))
	}
	for _, it := range parsed.Input {
		if it["type"] == "reasoning" {
			t.Errorf("reasoning item survived strip: %+v", it)
		}
	}
	// 最后一条 message 的 reasoning_text 块应被剥掉
	last := parsed.Input[len(parsed.Input)-1]
	content := last["content"].([]any)
	if len(content) != 1 {
		t.Errorf("message content blocks = %d, want 1(reasoning_text 块被剥离)", len(content))
	}
	// 剥离推理 + 带工具往返 → 强制 effort=none(DeepSeek 校验)
	if parsed.Reasoning == nil || parsed.Reasoning.Effort != "none" {
		t.Errorf("reasoning = %+v, want effort=none(有工具往返时必须显式关闭)", parsed.Reasoning)
	}
	// 非 JSON 原样返回
	raw := []byte(`not-json`)
	if got, _ := stripResponsesReasoning(raw); string(got) != string(raw) {
		t.Error("non-JSON body should pass through unchanged")
	}
	// 无工具往返时不强制 effort(纯续接请求 DeepSeek 接受)
	noTools := `{"model":"m","input":[{"type":"reasoning","id":"r1","summary":[]}],"stream":false}`
	out2, _ := stripResponsesReasoning([]byte(noTools))
	if string(out2) != `{"input":[],"model":"m","stream":false}` {
		t.Errorf("no-tool-rounds strip = %s, want 不注入 reasoning", out2)
	}
}

// TestProxy_CatchAll_ModelNotFoundFailsOver P-catch-all-mismatch:
// catch_all 场景(客户端模型名是标签,候选目标模型 ≠ 客户端名)下,候选返回
// model_not_found(400 Unsupported model / 404 image input 类)→ 不 fatal,
// 继续同层下一个候选 — 2026-08-07 实测:Claude Code 历史带截图 image 块,
// mimo-v2.5-pro(不支持图片输入)404,原逻辑整请求失败,minimax 本可承接
func TestProxy_CatchAll_ModelNotFoundFailsOver(t *testing.T) {
	strict := &fakeProvider{
		name: "mimo-token-plan-anthropic", proto: provider.ProtocolAnthropic, models: []string{"mimo-v2.5-pro"},
		err: &provider.ProviderError{
			ProviderName: "mimo-token-plan-anthropic", StatusCode: http.StatusNotFound,
			ErrorType: provider.ErrorTypeModelNotFound,
			Message:   "No endpoints found that support image input",
		},
	}
	lenient := &fakeProvider{
		name: "minimax", proto: provider.ProtocolAnthropic, models: []string{"MiniMax-M3"},
		respStatus: http.StatusOK,
		respBody:   `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`,
	}
	pools := map[string]*keypool.Pool{
		"mimo-token-plan-anthropic": mkPool("mimo-token-plan-anthropic", []string{"1"}, "token_plan"),
		"minimax":                   mkPool("minimax", []string{"2"}, "token_plan"),
	}
	gin.SetMode(gin.ReleaseMode)
	reg := provider.NewRegistry()
	for _, p := range []*fakeProvider{strict, lenient} {
		p := p
		reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) { return p, nil })
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	provCfg := map[string]provider.ManagerProviderConfig{
		"mimo-token-plan-anthropic": {Enabled: true, Protocol: strict.Protocol(), Models: strict.models},
		"minimax":                   {Enabled: true, Protocol: lenient.Protocol(), Models: lenient.models},
	}
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: provCfg}); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	r := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases:  map[string]router.AliasConfig{},
		CatchAll: &router.AliasConfig{Alias: "*"}, // catch_all 自动模式
	})
	rec := &recordingUsage{}
	accessR, err := accesslog.NewRecorder(accesslog.RecorderConfig{}, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("new accesslog recorder: %v", err)
	}
	e := NewEngine(Config{Router: r, Logger: zap.NewNop(), Usage: rec, Metrics: NoopMetricsRecorder{}, AccessLog: accessR})

	gr := gin.New()
	gr.POST("/v1/messages", e.HandleRequest)
	// 客户端发探测名 claude-opus-5(标签)— catch_all 映射到各候选默认模型
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (request must survive candidate mismatch)", w.Code, w.Body.String())
	}
	if strict.callCount != 1 {
		t.Errorf("strict provider called %d times, want 1 (tried, then failover)", strict.callCount)
	}
	if lenient.callCount != 1 {
		t.Errorf("lenient provider called %d times, want 1 (should serve the request)", lenient.callCount)
	}
	// 第二个候选收到的 body 模型名被改写为它的目标模型(MiniMax-M3),不是客户端标签
	if !strings.Contains(string(lenient.gotBody), `"model":"MiniMax-M3"`) {
		t.Errorf("lenient got body model = %s, want rewritten MiniMax-M3", lenient.gotBody)
	}
}

// TestProxy_RateLimit_SwapsKeyInProvider P-ratelimit-not-quota:
// 429 纯限流(rate_limit,无额度 body)→ 同 provider 换 key 重试,不推进候选
// (2026-08-07 实测:minimax key-8 限流被当额度类,跳过 key-7 直接走 mimo —
// 用户质疑「额度没用完怎么用 mimo」,修正为与网络类同构:换 key 重试)
func TestProxy_RateLimit_SwapsKeyInProvider(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		errByKey: map[string]error{"1": &provider.ProviderError{
			ProviderName: "mm", StatusCode: http.StatusTooManyRequests,
			ErrorType: provider.ErrorTypeRateLimit, Message: "rate limited"}},
		respStatus: http.StatusOK, respBody: `{"id":"mm-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(换 key 重试成功); body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mm-ok") {
		t.Errorf("response = %s, want mm 的响应", w.Body.String())
	}
	if mm.callCount != 2 {
		t.Errorf("callCount = %d, want 2 (key-1 限流 → key-2 重试,不跳 provider)", mm.callCount)
	}
}

// TestProxy_RateLimit_NoDowngradeEvidence P-ratelimit-not-quota:
// 429 纯限流不产生额度证据 — token_plan 层全 key 限流(换 key 穷尽)→
// 层内无证据 → 不降档 api(请求失败返回),api provider 不应被调用
func TestProxy_RateLimit_NoDowngradeEvidence(t *testing.T) {
	mm := &fakeProvider{name: "mm", proto: provider.ProtocolOpenAI, models: []string{"m"},
		err: &provider.ProviderError{
			ProviderName: "mm", StatusCode: http.StatusTooManyRequests,
			ErrorType: provider.ErrorTypeRateLimit, Message: "rate limited"}}
	ds := &fakeProvider{name: "ds", proto: provider.ProtocolOpenAI, models: []string{"m"},
		respStatus: http.StatusOK, respBody: `{"id":"ds-ok"}`}
	e, _ := buildEngineMulti(t, []*fakeProvider{mm, ds}, map[string]*keypool.Pool{
		"mm": mkPool("mm", []string{"1", "2"}, "token_plan"),
		"ds": mkPool("ds", []string{"1"}, "api"),
	}, t5Alias())

	w := doProxyRequest(e, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 5xx(限流不是额度证据,不降档); body = %s", w.Code, w.Body.String())
	}
	if ds.callCount != 0 {
		t.Errorf("ds called %d times, want 0 (限流不降档,api 层不应被用)", ds.callCount)
	}
	if mm.callCount != 2 {
		t.Errorf("mm callCount = %d, want 2 (两把 key 各试一次)", mm.callCount)
	}
}
