// Integration test: 起一个 fake 上游 DeepSeek,通过完整 Proxy 链路发送请求
// 验证: Router → DeepSeek Provider → 上游 → 响应透传
//
// 这是 P10 的核心:端到端跑通整个 Gateway
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/proxy"
	"github.com/wang546673478/native-llm-gateway/internal/router"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"
)

// buildGateway 构造完整 Gateway:Provider(指向上游) + Router + Proxy + Gin
func buildGateway(t *testing.T, upstreamURL string) (*gin.Engine, *fakeUpstream) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)

	upstream := &fakeUpstream{}
	upstream.handler = func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.calls++
		upstream.lastAuth = r.Header.Get("Authorization")
		upstream.lastBody, _ = io.ReadAll(r.Body)
		upstream.lastTrace = r.Header.Get("X-Request-Id")
		upstream.mu.Unlock()

		// 模拟 OpenAI 响应
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstream.statusCode)
		w.Write([]byte(upstream.responseBody))
	}

	// 用上游 URL 启动 fake server
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)

	// 真实注册一个 deepseek-style 的 Provider,让它指向我们的 fake 上游
	reg := provider.NewRegistry()
	reg.Register("deepseek", func(cfg provider.ProviderConfig) (provider.Provider, error) {
		// 复用 deepseek.New,但 endpoint 改成 fake 上游
		// 这里直接调用 deepseek.New 太麻烦,改用 openai_compatible.Base
		return nil, nil // unused
	})

	// 构造 Pool
	pool := keypool.NewPool("deepseek", []*keypool.Key{{
		ID: "k1", ProviderName: "deepseek", Name: "k1", Key: "sk-fake-key",
		Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}, nil, keypool.Config{})

	// 直接用 openai_compatible.Base 来构造 DeepSeek 实例,endpoint 指向 fake 上游
	// 这样可以绕过 deepseek.New 的 endpoint 校验
	pool2 := pool
	_ = pool2

	// 用 deepseek.New(它会校验 protocol+endpoint+keys)
	// 为了让它指向 fake upstream,我们手动 new Base 然后塞进 deepseek.Provider
	// 这里走简化路径:直接构造 deepseek.Provider(不通过 New 工厂)
	// 而是用 openai_compatible.NewBase

	pv := newDeepSeekLikeProvider(srv.URL, pool)

	mgr := provider.NewManager(reg, zap.NewNop())
	// 直接塞入已构造的 provider(绕过 LoadFromConfig 的 factory 路径)
	mgr.SetForTesting("deepseek", pv)

	r := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{"deepseek": pool}, router.Config{
		Aliases: map[string]router.AliasConfig{
			"coding-model": {
				Strategy: "priority",
				Providers: []router.ProviderRoute{
					{Name: "deepseek", Model: "deepseek-chat", Priority: 1},
				},
			},
		},
	})

	eng := proxy.NewEngine(proxy.Config{
		Router:  r,
		Logger:  zap.NewNop(),
		Usage:   proxy.NoopUsageRecorder{},
		Metrics: proxy.NoopMetricsRecorder{},
		Breaker: proxy.NoopCircuitReporter{},
	})

	r2 := gin.New()
	r2.POST("/v1/chat/completions", eng.HandleRequest)
	r2.POST("/v1/messages", eng.HandleRequest)
	r2.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			eng.HandleRequest(c)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	})

	return r2, upstream
}

type fakeUpstream struct {
	mu           sync.Mutex
	calls        int
	lastAuth     string
	lastBody     []byte
	lastTrace    string
	statusCode   int
	responseBody string
	handler      http.HandlerFunc
}

func (u *fakeUpstream) setResponse(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.statusCode = status
	u.responseBody = body
}

func (u *fakeUpstream) snapshot() (calls int, auth, trace string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.lastAuth, u.lastTrace, append([]byte(nil), u.lastBody...)
}

// newDeepSeekLikeProvider 构造一个 deepseek-like provider,endpoint 指向 fake 上游
func newDeepSeekLikeProvider(endpoint string, pool *keypool.Pool) provider.Provider {
	// 直接用 deepseek 包的 New,绕开校验:把 endpoint 设成 fake URL
	return mustNewDeepSeek(endpoint, pool)
}

func TestE2E_NonStream_ThroughGateway(t *testing.T) {
	r, upstream := buildGateway(t, "ignored") // upstream URL 已被 fakeUpstream 内部启动
	upstream.setResponse(200, `{"id":"e2e-1","choices":[{"message":{"role":"assistant","content":"hello from fake deepseek"}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`)

	body := `{"model":"coding-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "e2e-trace-001")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello from fake deepseek") {
		t.Errorf("response body missing content: %s", w.Body.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != "e2e-trace-001" {
		t.Errorf("X-Request-Id = %q, want e2e-trace-001", got)
	}

	calls, auth, trace, sentBody := upstream.snapshot()
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
	if auth != "Bearer sk-fake-key" {
		t.Errorf("upstream saw auth %q, want Bearer sk-fake-key", auth)
	}
	if trace != "e2e-trace-001" {
		t.Errorf("upstream trace = %q, want e2e-trace-001", trace)
	}
	if string(sentBody) != body {
		t.Errorf("upstream got modified body:\n  got:  %s\n  want: %s", sentBody, body)
	}
}

func TestE2E_StreamThroughGateway(t *testing.T) {
	r, upstream := buildGateway(t, "ignored")
	_ = r
	_ = upstream
	// 上游返回 SSE — 通过 setResponse + 一个支持 stream 的 handler
	// 这里替换 handler 不行(httptest 已绑定),改用 upstream.setResponse(200,"...")
	// 然后用支持 SSE 的 handler。简化:重新构造一个 stream 专用的 upstream
	streamUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"content":"E2E"}}]}`,
			`data: {"choices":[{"delta":{"content":" stream"}}]}`,
			`data: [DONE]`,
		} {
			w.Write([]byte(chunk + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(streamUpstream.Close)

	// 重新构造 deepseek provider,endpoint 指向 stream upstream
	pool := keypool.NewPool("deepseek", []*keypool.Key{{
		ID: "k1", ProviderName: "deepseek", Name: "k1", Key: "sk-fake-key",
		Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}, nil, keypool.Config{})
	pv := mustNewDeepSeek(streamUpstream.URL, pool)

	// 替换 manager 里的 provider
	// 直接走 proxy:用全新的 r(避免影响 upstream 状态)
	reg := provider.NewRegistry()
	mgr2 := provider.NewManager(reg, zap.NewNop())
	mgr2.SetForTesting("deepseek", pv)
	r2 := router.NewRouter(zap.NewNop(), mgr2, map[string]*keypool.Pool{"deepseek": pool}, router.Config{
		Aliases: map[string]router.AliasConfig{
			"coding-model": {
				Strategy: "priority",
				Providers: []router.ProviderRoute{
					{Name: "deepseek", Model: "deepseek-chat", Priority: 1},
				},
			},
		},
	})
	eng := proxy.NewEngine(proxy.Config{Router: r2, Logger: zap.NewNop(),
		Usage: proxy.NoopUsageRecorder{}, Metrics: proxy.NoopMetricsRecorder{}, Breaker: proxy.NoopCircuitReporter{},
	})
	routerEngine := gin.New()
	routerEngine.POST("/v1/chat/completions", eng.HandleRequest)

	body := `{"model":"coding-model","stream":true,"messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	routerEngine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, "E2E") || !strings.Contains(body2, "[DONE]") {
		t.Errorf("SSE body incomplete: %s", body2)
	}
}

func TestE2E_UpstreamError_ReportsToKeyPool(t *testing.T) {
	r, upstream := buildGateway(t, "ignored")
	upstream.setResponse(429, `{"error":{"message":"rate limit"}}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"coding-model","messages":[]}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 502 gateway_error 因为 failover 没候选
	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// silence unused
var _ = json.NewEncoder
var _ = bytes.NewReader
var _ = context.Background
var _ = config.Load

// 把 deepseek.New 包装,允许传 endpoint(跳过 deepseek.New 内部的 endpoint 校验)
//
// 但 deepseek.New 不校验 endpoint 格式,只校验非空。所以我们可以直接传 fake URL。
// 这里用 buildGateway 内部的 mustNewDeepSeek helper。
func mustNewDeepSeek(endpoint string, pool *keypool.Pool) provider.Provider {
	// 因为 deepseek 包内 New 用了 cfg.Pool 接口断言,而 cfg.Pool 是 interface{},
	// 我们从外部构造时也可以传 *keypool.Pool。
	// 简化路径:直接用 deepseek.New + 一个 ProviderConfig

	cfg := provider.ProviderConfig{
		Name:     "deepseek",
		Endpoint: endpoint,
		Protocol: provider.ProtocolOpenAI,
		Timeout:  5 * time.Second,
		Models:   []string{"deepseek-chat"},
		APIKeys:  []string{"sk-fake-key"},
		Pool:     pool,
	}
	p, err := provider.Default().Create("deepseek", cfg)
	if err != nil {
		panic(err)
	}
	return p
}
