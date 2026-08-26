package openai_compatible

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func newTestPool(t *testing.T, keys ...string) *keypool.Pool {
	t.Helper()
	now := time.Now()
	kks := make([]*keypool.Key, len(keys))
	for i, k := range keys {
		kks[i] = &keypool.Key{
			ID: "k" + k, ProviderName: "test", Name: k, Key: k,
			Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
		}
	}
	return keypool.NewPool("test", kks, nil, keypool.Config{})
}

func TestSendRequest_Success(t *testing.T) {
	var gotAuth, gotBody, gotTrace string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTrace = r.Header.Get("X-Request-Id")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test-1")
	b := NewBase(Config{
		Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool,
	})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","messages":[]}`),
		TraceID: "trace-abc",
	})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if gotAuth != "Bearer sk-test-1" {
		t.Errorf("Authorization = %q, want Bearer sk-test-1", gotAuth)
	}
	if gotTrace != "trace-abc" {
		t.Errorf("X-Request-Id = %q, want trace-abc", gotTrace)
	}
	if gotBody != `{"model":"m","messages":[]}` {
		t.Errorf("body modified: %s", gotBody)
	}
	if resp.Usage == nil {
		t.Fatal("Usage should be parsed")
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 10 {
		t.Errorf("usage wrong: %+v", resp.Usage)
	}

	// KeyPool 应该收到成功
	if pool.Status().ActiveKeys != 1 {
		t.Errorf("active keys = %d, want 1", pool.Status().ActiveKeys)
	}
}

func TestSendRequest_RateLimitTriggersCooling(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"too many requests"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-a", "sk-b")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	_, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	var pe *provider.ProviderError
	if !errorsAs(err, &pe) {
		t.Fatalf("err is not ProviderError: %T", err)
	}
	if pe.ErrorType != provider.ErrorTypeRateLimit {
		t.Errorf("errType = %s, want rate_limit", pe.ErrorType)
	}
	if pe.StatusCode != 429 {
		t.Errorf("statusCode = %d, want 429", pe.StatusCode)
	}
	if pe.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", pe.RetryAfter)
	}

	// 至少一个 Key 应该在 COOLING 状态
	st := pool.Status()
	if st.CoolingKeys == 0 {
		t.Errorf("expected at least 1 COOLING key, got status %+v", st)
	}
}

func TestSendRequest_AuthErrorDisablesKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-bad")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	_, err := b.SendRequest(context.Background(), &provider.Request{
		Body: []byte(`{}`),
	})
	var pe *provider.ProviderError
	if !errorsAs(err, &pe) {
		t.Fatalf("not ProviderError: %T", err)
	}
	if pe.ErrorType != provider.ErrorTypeAuth {
		t.Errorf("errType = %s, want auth", pe.ErrorType)
	}
	if pool.Status().CoolingKeys != 1 {
		t.Errorf("expected key cooling (auth → cooling, no DISABLED), got %+v", pool.Status())
	}
}

func TestSendStreamRequest_StreamsSSEChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world"}}]}`,
			`data: [DONE]`,
		} {
			w.Write([]byte(chunk + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-stream")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, hdr, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("SendStreamRequest: %v", err)
	}
	if hdr.StatusCode != 200 {
		t.Errorf("hdr.StatusCode = %d, want 200", hdr.StatusCode)
	}

	var received []string
	for c := range ch {
		if c.Err != nil {
			if c.Err != io.EOF {
				t.Errorf("unexpected stream err: %v", c.Err)
			}
			continue
		}
		received = append(received, string(c.Data))
	}
	if len(received) < 3 {
		t.Fatalf("got %d chunks, want >=3", len(received))
	}

	// 验证 SSE 格式还原
	full := strings.Join(received, "")
	if !strings.Contains(full, "Hello") {
		t.Errorf("missing 'Hello': %s", full)
	}
	if !strings.Contains(full, "world") {
		t.Errorf("missing 'world': %s", full)
	}
	if !strings.Contains(full, "[DONE]") {
		t.Errorf("missing [DONE]: %s", full)
	}
}

func TestParseOpenAIUsage_Missing(t *testing.T) {
	u := parseOpenAIUsage([]byte(`{"id":"x"}`))
	if u != nil {
		t.Errorf("expected nil usage when field absent, got %+v", u)
	}
}

// TestParseOpenAIUsage_Model P65: 验证 OpenAI 响应顶层 model 字段被抽到 Usage.Model
func TestParseOpenAIUsage_Model(t *testing.T) {
	body := []byte(`{
		"id": "x",
		"model": "deepseek-v4-pro",
		"choices": [{"message": {"content": "hi"}}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "deepseek-v4-pro" {
		t.Errorf("Model = %q, want deepseek-v4-pro", u.Model)
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Errorf("usage wrong: %+v", u)
	}
}

// TestParseOpenAIUsage_MissingModel P65: 响应无 model 字段时 Usage.Model 为空字符串
func TestParseOpenAIUsage_MissingModel(t *testing.T) {
	body := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "" {
		t.Errorf("Model = %q, want empty (no model field in body)", u.Model)
	}
}

func TestParseOpenAIUsage_InvalidJSON(t *testing.T) {
	u := parseOpenAIUsage([]byte(`not json`))
	if u != nil {
		t.Errorf("expected nil usage on invalid json, got %+v", u)
	}
}

func TestParseOpenAIUsage_DeepSeekExtensions(t *testing.T) {
	// DeepSeek 完整 usage 格式,含 cache 和 reasoning
	body := []byte(`{
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150,
			"prompt_cache_hit_tokens": 80,
			"prompt_cache_miss_tokens": 20,
			"completion_tokens_details": {
				"reasoning_tokens": 30
			}
		}
	}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	// P-cache-dedup: prompt_tokens(100)是含缓存的完整输入,契约要求 PromptTokens 只算
	// 未命中部分 → 100 - 80 = 20,正好等于上游自己给的 prompt_cache_miss_tokens。
	// 这个巧合是校验点:减法口径与 DeepSeek 官方的 miss 字段必须一致。
	if u.PromptTokens != 20 {
		t.Errorf("PromptTokens = %d, want 20 (= prompt_tokens 100 - hit 80 = prompt_cache_miss_tokens)", u.PromptTokens)
	}
	// P-provider-vendor: cached_tokens 缺省 0 时,CacheReadTokens = prompt_cache_hit_tokens
	if u.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80 (prompt_cache_hit_tokens)", u.CacheReadTokens)
	}
	// 契约不变式:两个量互斥且加起来等于完整输入(否则 ComputeCost 会重复计费)
	if u.PromptTokens+u.CacheReadTokens != 100 {
		t.Errorf("PromptTokens(%d) + CacheReadTokens(%d) = %d, want 100 (完整输入)",
			u.PromptTokens, u.CacheReadTokens, u.PromptTokens+u.CacheReadTokens)
	}
	if u.RawUsage["prompt_cache_hit_tokens"] != 80 {
		t.Errorf("prompt_cache_hit_tokens in RawUsage = %v, want 80", u.RawUsage["prompt_cache_hit_tokens"])
	}
	if u.RawUsage["reasoning_tokens"] != 30 {
		t.Errorf("reasoning_tokens in RawUsage = %v, want 30", u.RawUsage["reasoning_tokens"])
	}
}

func TestParseOpenAIUsage_MiniMaxCachedTokens(t *testing.T) {
	// P-provider-vendor: OpenAI 标准 prompt_tokens_details.cached_tokens(MiniMax 等)
	body := []byte(`{
		"model": "MiniMax-M3",
		"usage": {
			"prompt_tokens": 1200,
			"completion_tokens": 300,
			"total_tokens": 1500,
			"prompt_tokens_details": {"cached_tokens": 800}
		}
	}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("parseOpenAIUsage returned nil")
	}
	if u.CacheReadTokens != 800 {
		t.Fatalf("CacheReadTokens = %d, want 800", u.CacheReadTokens)
	}
	// P-cache-dedup: MiniMax 只给「完整输入 + cached」两个数,没有 miss 字段,
	// uncached 必须靠减法算:1200 - 800 = 400。
	if u.PromptTokens != 400 {
		t.Fatalf("PromptTokens = %d, want 400 (= prompt_tokens 1200 - cached 800)", u.PromptTokens)
	}
}

func TestParseOpenAIUsage_CachedTokensSum(t *testing.T) {
	// P-provider-vendor: CacheReadTokens = prompt_cache_hit_tokens + cached_tokens
	// 两字段同时出现时相加(DeepSeek 风格与 OpenAI 标准并存)
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"usage": {
			"prompt_tokens": 2000,
			"completion_tokens": 100,
			"total_tokens": 2100,
			"prompt_cache_hit_tokens": 80,
			"prompt_cache_miss_tokens": 1920,
			"prompt_tokens_details": {"cached_tokens": 800}
		}
	}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("parseOpenAIUsage returned nil")
	}
	if u.CacheReadTokens != 880 {
		t.Fatalf("CacheReadTokens = %d, want 880 (80 + 800)", u.CacheReadTokens)
	}
	// P-cache-dedup: 两种风格并存时也走同一条减法,2000 - 880 = 1120。
	// 注意这里刻意**不**等于上游的 prompt_cache_miss_tokens(1920)——这是人造 body,
	// 真实上游不会同时给两套自相矛盾的 cache 字段。校验的是减法口径统一,不是数字巧合。
	if u.PromptTokens != 1120 {
		t.Fatalf("PromptTokens = %d, want 1120 (= 2000 - 880)", u.PromptTokens)
	}
}

func TestSendRequest_CustomChatPath(t *testing.T) {
	// DeepSeek 用 /chat/completions 而非 /v1/chat/completions
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{
		Name:     "deepseek",
		Endpoint: upstream.URL,
		Timeout:  5 * time.Second,
		Pool:     pool,
		ChatPath: "/chat/completions", // 关键:模拟 DeepSeek 的路径
	})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Body: []byte(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if resp == nil {
		t.Fatal("resp nil")
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions (DeepSeek 用此路径)", gotPath)
	}
}

func TestInjectStreamUsage(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		contains string
		notEqual string
	}{
		{"empty", ``, `"stream_options":{"include_usage":true}`, ``},
		{"empty obj", `{}`, `"stream_options":{"include_usage":true}`, ``},
		{"already has", `{"stream_options":{"include_usage":false}}`, `"include_usage":false`, ``},
		{"with content", `{"model":"m","stream":true}`, `"stream_options":{"include_usage":true}`, ``},
		{"invalid json", `not json`, ``, `not json`}, // 解析失败时返回原 body
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(injectStreamUsage([]byte(tt.in)))
			if tt.contains != "" && !contains(got, tt.contains) {
				t.Errorf("got %s, should contain %s", got, tt.contains)
			}
			if tt.notEqual != "" && got == tt.notEqual {
				// notEqual 用 contains 反向
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"30":   30 * time.Second,
		"120":  120 * time.Second,
		"junk": 0,
	}
	for in, want := range cases {
		got := provider.ParseRetryAfter(in)
		if got != want {
			t.Errorf("provider.ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHealthCheck_OK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(200)
			w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-h")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Pool: pool})

	if err := b.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

func TestHealthCheck_Upstream5xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-h")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Pool: pool})

	if err := b.HealthCheck(context.Background()); err == nil {
		t.Error("expected error on 503")
	}
}

// helper: errors.As without importing errors just for one call
func errorsAs(err error, target interface{}) bool {
	type wrapper interface{ Unwrap() error }
	for err != nil {
		if pe, ok := target.(**provider.ProviderError); ok {
			if p, ok := err.(*provider.ProviderError); ok {
				*pe = p
				return true
			}
		}
		w, ok := err.(wrapper)
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}

// silence unused
var _ = bufio.NewReader
var _ = json.Marshal

// TestStreamUsageInjectedByDefault P25: 验证 OpenAI 兼容 Provider 默认开启
// stream_options.include_usage=true,这样流式响应最后一个 chunk 带 usage,
// Gateway 才能正确计费。
func TestStreamUsageInjectedByDefault(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{
		Name:     "test",
		Endpoint: upstream.URL,
		Timeout:  5 * time.Second,
		Pool:     pool,
		// 注意:不设置 StreamUsage,验证默认是 true
	})

	_, _, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("SendStreamRequest: %v", err)
	}

	if !bytes.Contains(gotBody, []byte(`"include_usage":true`)) {
		t.Errorf("expected stream_options.include_usage=true by default, got body: %s", gotBody)
	}
}

// TestSendRequest_MiniMaxBaseRespQuota P-quota-minimax:
// MiniMax 余额不足时返回 HTTP 200 + body {"base_resp":{"status_code":1008}}。
// 网关必须识别为 quota_exceeded(failover 到下一 provider)+ key 标 QUOTA_EXCEEDED,
// 而不是当成功响应透传
func TestSendRequest_MiniMaxBaseRespQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","messages":[]}`),
	})
	if resp != nil {
		t.Fatalf("resp should be nil for base_resp error, got %+v", resp)
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.ProviderError", err)
	}
	if pe.ErrorType != provider.ErrorTypeQuotaExceeded {
		t.Errorf("error type = %q, want quota_exceeded", pe.ErrorType)
	}
	// key 应该被标记 QUOTA_EXCEEDED → 下一次请求跳过,推进到下一 provider
	if got := pool.Status().QuotaExceededKeys; got != 1 {
		t.Errorf("quota_exceeded keys = %d, want 1", got)
	}
}

// TestSendStreamRequest_MiniMaxBaseRespQuota P-quota-minimax:
// 流式场景 MiniMax 也可能 HTTP 200 + SSE 帧里的 base_resp 错误。
// SendStreamRequest 必须在客户端收到任何字节前返回错误(此时 failover 还来得及)
func TestSendStreamRequest_MiniMaxBaseRespQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("event: error\ndata: {\"base_resp\":{\"status_code\":2056,\"status_msg\":\"plan limit\"}}\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, resp, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","stream":true}`),
	})
	if err == nil {
		t.Fatal("expected base_resp error from stream request")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.ProviderError", err)
	}
	if pe.ErrorType != provider.ErrorTypeQuotaExceeded {
		t.Errorf("error type = %q, want quota_exceeded", pe.ErrorType)
	}
	if resp != nil {
		t.Errorf("resp should be nil, got %+v", resp)
	}
	if ch != nil {
		// 不应产生任何 chunk(错误在转发前拦截)
		if c := <-ch; c != nil {
			t.Errorf("unexpected chunk: %+v", c)
		}
	}
	if got := pool.Status().QuotaExceededKeys; got != 1 {
		t.Errorf("quota_exceeded keys = %d, want 1", got)
	}
}

// TestSendStreamRequest_SSEStreamErrorFailsFast 证明**接线**生效,不只是分类器认得。
//
// 上游行为抄自生产实测(2026-08-26,tokenmarket-codex):HTTP 200 +
// text/event-stream 头,然后整条流只有一个 response.failed 事件就收流。
// 此前网关把它当成功流透传 → access log 记 200/ok、key 不冷却、不 failover,
// 客户端只看到"流没跑完"。
//
// 断言链对应 failover 的三个必要条件:
//  1. 返 *ProviderError(非 nil)—— attemptOne 靠 ok=false 才进 failover
//  2. 一个 chunk 都没转发 —— 客户端零字节,换 key 重发才是干净的
//  3. 不是 rate_limit —— 否则走同 key 重试 10 次(实测每次 ~32s)
func TestSendStreamRequest_SSEStreamErrorFailsFast(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded for account, please retry later"}}}` + "\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, resp, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","stream":true}`),
	})
	if err == nil {
		t.Fatal("上游流里发了 response.failed 却返回成功 — failover 不会启动")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.ProviderError", err)
	}
	if pe.ErrorType != provider.ErrorTypeServerError {
		t.Errorf("error type = %q, want server_error", pe.ErrorType)
	}
	if pe.ErrorType == provider.ErrorTypeRateLimit {
		t.Error("不能是 rate_limit:会触发同 key 重试 10 次 × ~32s")
	}
	if resp != nil {
		t.Errorf("resp 应为 nil,got %+v", resp)
	}
	if ch != nil {
		if c := <-ch; c != nil {
			t.Errorf("不该转发任何 chunk(客户端必须零字节): %+v", c)
		}
	}
	// key 记了错误(喂 per-key 熔断),但不该被误升级成额度耗尽
	if got := pool.Status().QuotaExceededKeys; got != 0 {
		t.Errorf("并发限制被误判成额度耗尽,QE keys = %d, want 0", got)
	}
}

// TestSendStreamRequest_NormalStreamUnaffected 回归防线:新增的流头检查
// 不能把正常流拦下来。事件里带 error:null(OpenAI Responses 的常态)也必须放过。
func TestSendStreamRequest_NormalStreamUnaffected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","error":null}}` + "\n\n"))
		w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, _, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("正常流被误拦: %v", err)
	}
	var got int
	for c := range ch {
		if c.Err != nil {
			break
		}
		if len(c.Data) > 0 {
			got++
		}
	}
	if got < 3 {
		t.Errorf("转发了 %d 个 chunk,期望 3(created/delta/DONE)", got)
	}
}

func TestListModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":""}]}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	got, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ListModels = %v, want [a b] (empty id filtered)", got)
	}
}

// TestListModels_ModelsPathOverride 守卫 2026-08-20 根因:endpoint 已含版本前缀的
// 厂商(minimax-openai / mimo / glm)必须用 ModelsPath 覆盖成 /models,否则拼出
// /v1/v1/models 这类不存在的路径,上游回 HTML 404,同步报的却是
// "decode models: invalid character '<'" —— 与真因完全无关,极难排查。
func TestListModels_ModelsPathOverride(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/v1/models" { // 只认这一条,拼错就走 404 分支
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(404)
			w.Write([]byte("<html>404 page not found</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[{"id":"MiniMax-M3"}]}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	// 模拟 minimax-openai:endpoint 自带 /v1,ModelsPath 覆盖为 /models
	b := NewBase(Config{
		Name: "vendor-openai", Endpoint: upstream.URL + "/v1",
		ModelsPath: "/models", Timeout: 5 * time.Second, Pool: pool,
	})

	got, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("请求路径 = %q, want /v1/models(而不是双前缀 /v1/v1/models)", gotPath)
	}
	if len(got) != 1 || got[0] != "MiniMax-M3" {
		t.Errorf("ListModels = %v, want [MiniMax-M3]", got)
	}
}

// TestListModels_NonJSONBodyReportsStatus 守卫:上游回 HTML/空 body 时必须报出
// 状态码和路径,而不是把真因埋进 "decode models: ..." 的 JSON 解析错里。
func TestListModels_NonJSONBodyReportsStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		w.Write([]byte("<html>404 page not found</html>"))
	}))
	defer upstream.Close()

	pool := newTestPool(t, "sk-test")
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	_, err := b.ListModels(context.Background())
	if err == nil {
		t.Fatal("ListModels 应当报错")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want 含状态码 404", err)
	}
	if strings.Contains(err.Error(), "decode models") {
		t.Errorf("err = %v, 不应把 404 埋成 decode 错", err)
	}
}

// ── P-responses-usage: OpenAI Responses API 的 usage 形状 ─────────────────
//
// 背景:Codex 客户端走 /v1/responses,usage 字段名与层级都和 Chat Completions
// 不同(input_tokens 而非 prompt_tokens;流式还嵌在 response 里)。此前解析器
// 只认 Chat Completions → gpt-5.6-sol 的 206 条流式记录 input/output/cache 全 0。

// responsesStreamCompletedBody 是实测 tokenmarket-codex(gpt-5.6-sol)的流式末帧,
// 数值取自真实 access log body(trace 6465c492)。
const responsesStreamCompletedBody = `{"type":"response.completed","response":{
	"id":"resp_00fe402b","object":"response","model":"gpt-5.6-sol","status":"completed",
	"usage":{
		"input_tokens":341797,
		"input_tokens_details":{"cached_tokens":331648},
		"output_tokens":156,
		"output_tokens_details":{"reasoning_tokens":0},
		"total_tokens":341953}}}`

func TestParseResponsesUsage_StreamCompleted(t *testing.T) {
	u := parseResponsesUsage([]byte(responsesStreamCompletedBody))
	if u == nil {
		t.Fatal("parseResponsesUsage returned nil — 流式末帧的 usage 必须被认出,否则整条记录 0 token")
	}
	// P-cache-dedup: input_tokens(341797)含缓存,契约口径只算未命中部分:
	// 341797 - 331648 = 10149。这条真实语料的缓存命中率 97%,正是重复计费最严重的形状。
	if u.PromptTokens != 10149 {
		t.Errorf("PromptTokens = %d, want 10149 (= input 341797 - cached 331648)", u.PromptTokens)
	}
	if u.CompletionTokens != 156 {
		t.Errorf("CompletionTokens = %d, want 156", u.CompletionTokens)
	}
	if u.TotalTokens != 341953 {
		t.Errorf("TotalTokens = %d, want 341953", u.TotalTokens)
	}
	if u.CacheReadTokens != 331648 {
		t.Errorf("CacheReadTokens = %d, want 331648", u.CacheReadTokens)
	}
	// Responses 无 cache 创建概念
	if u.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d, want 0", u.CacheCreationTokens)
	}
	// P65: model 从 response 内层取(顶层没有)
	if u.Model != "gpt-5.6-sol" {
		t.Errorf("Model = %q, want gpt-5.6-sol", u.Model)
	}
}

// TestParseResponsesUsage_OpenAISemantics 锁死上游口径 + 契约口径的换算关系。
//
// 上游口径(Responses / Chat Completions):cached 是 input 的**子集**,
//   total = input + output,不含 cache 项。
// 契约口径(provider.Usage):PromptTokens 是**不计 cache** 的输入,与 CacheReadTokens 互斥。
//
// 所以解析后必须满足 prompt + cache_read + completion == total。
// 若哪天有人把 PromptTokens 改回上游原值(含缓存),这条会红:
// 相加会超出 total 一个 cached 的量 —— 那正是 ComputeCost 重复计费的金额。
func TestParseResponsesUsage_OpenAISemantics(t *testing.T) {
	u := parseResponsesUsage([]byte(responsesStreamCompletedBody))
	if u == nil {
		t.Fatal("parseResponsesUsage returned nil")
	}
	if got := u.PromptTokens + u.CacheReadTokens + u.CompletionTokens; got != u.TotalTokens {
		t.Errorf("prompt(%d) + cache_read(%d) + completion(%d) = %d, want total %d — "+
			"PromptTokens 若含缓存会超出 total,缓存部分被 input 价和 cache 价各收一次",
			u.PromptTokens, u.CacheReadTokens, u.CompletionTokens, got, u.TotalTokens)
	}
	// PromptTokens 已扣掉缓存,不能再是缓存的超集(否则说明减法没生效)
	if u.PromptTokens >= u.TotalTokens-u.CompletionTokens && u.CacheReadTokens > 0 {
		t.Errorf("PromptTokens(%d) 仍是完整输入 — 减法没生效", u.PromptTokens)
	}
}

func TestParseResponsesUsage_NonStream(t *testing.T) {
	// 非流式:usage 在顶层,字段名仍是 input_tokens
	body := []byte(`{"id":"resp_x","model":"gpt-5.6-sol","usage":{
		"input_tokens":4390,"input_tokens_details":{"cached_tokens":3840},
		"output_tokens":6,"total_tokens":4396}}`)
	u := parseResponsesUsage(body)
	if u == nil {
		t.Fatal("parseResponsesUsage returned nil for 顶层 usage")
	}
	// P-cache-dedup: 4390 - 3840 = 550 未命中输入
	if u.PromptTokens != 550 || u.CompletionTokens != 6 || u.CacheReadTokens != 3840 {
		t.Errorf("usage wrong: %+v (want prompt=550 completion=6 cache_read=3840)", u)
	}
	if u.Model != "gpt-5.6-sol" {
		t.Errorf("Model = %q, want gpt-5.6-sol", u.Model)
	}
}

// TestParseResponsesUsage_NullUsage 流早期事件带 "usage":null —— 必须返回 nil,
// 否则会在流式循环里把末帧的真实 usage 覆盖成 0(lastUsage 被后写的零值顶掉)。
func TestParseResponsesUsage_NullUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"response.created 顶层 null", `{"type":"response.created","response":{"id":"r","usage":null}}`},
		{"in_progress", `{"type":"response.in_progress","response":{"id":"r","usage":null}}`},
		{"delta 无 usage", `{"type":"response.custom_tool_call_input.delta","delta":"x"}`},
		{"usage 全零", `{"response":{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
		{"非法 JSON", `not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if u := parseResponsesUsage([]byte(c.body)); u != nil {
				t.Errorf("expected nil, got %+v", u)
			}
		})
	}
}

// TestParseResponsesUsage_TotalFallback total_tokens 缺失时用 input+output 兜底
func TestParseResponsesUsage_TotalFallback(t *testing.T) {
	u := parseResponsesUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":20}}`))
	if u == nil {
		t.Fatal("parseResponsesUsage returned nil")
	}
	if u.TotalTokens != 120 {
		t.Errorf("TotalTokens = %d, want 120 (input+output 兜底)", u.TotalTokens)
	}
}

// ── 分派器:两套形状共存 ────────────────────────────────────────────────
//
// DefaultOpenAIUsageParser.Parse 先判 Responses 再回落 Chat Completions。
// 顺序不能反 —— 见 Parse 的注释:两套共用 total_tokens 字段名,而 Chat
// Completions 解析器只判「usage 键存在」就返回,反序会把 Responses 非流式
// body 认成 prompt=0/completion=0 的半条零值记录且永不回落。

func TestUsageParser_DispatchesBothShapes(t *testing.T) {
	p := &DefaultOpenAIUsageParser{}

	t.Run("chat completions 不被 Responses 抢走", func(t *testing.T) {
		u := p.Parse([]byte(`{"model":"deepseek-v4-pro","usage":{
			"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,
			"prompt_tokens_details":{"cached_tokens":800}}}`))
		if u == nil {
			t.Fatal("Parse returned nil for chat completions body")
		}
		// P-cache-dedup: 1200 - 800 = 400 未命中输入
		if u.PromptTokens != 400 || u.CompletionTokens != 300 || u.CacheReadTokens != 800 {
			t.Errorf("chat completions 解析被破坏: %+v (want prompt=400 completion=300 cache_read=800)", u)
		}
	})

	t.Run("responses 流式末帧", func(t *testing.T) {
		u := p.Parse([]byte(responsesStreamCompletedBody))
		if u == nil {
			t.Fatal("Parse returned nil for responses body")
		}
		// P-cache-dedup: 341797 - 331648 = 10149
		if u.PromptTokens != 10149 || u.CompletionTokens != 156 || u.CacheReadTokens != 331648 {
			t.Errorf("responses 解析错: %+v (want prompt=10149 completion=156 cache_read=331648)", u)
		}
	})

	t.Run("responses 非流式不退化成零值行", func(t *testing.T) {
		// 这条是分派顺序的核心回归点:顶层有 usage,但字段名是 Responses 的。
		// 若先跑 Chat Completions,会返回 prompt=0/completion=0/total=4396。
		u := p.Parse([]byte(`{"model":"gpt-5.6-sol","usage":{
			"input_tokens":4390,"input_tokens_details":{"cached_tokens":3840},
			"output_tokens":6,"total_tokens":4396}}`))
		if u == nil {
			t.Fatal("Parse returned nil")
		}
		if u.PromptTokens == 0 || u.CompletionTokens == 0 {
			t.Fatalf("退化成零值行(分派顺序错了): %+v", u)
		}
		// P-cache-dedup: 4390 - 3840 = 550
		if u.PromptTokens != 550 || u.CompletionTokens != 6 {
			t.Errorf("usage wrong: %+v (want prompt=550 completion=6)", u)
		}
	})

	t.Run("两套都不命中返回 nil", func(t *testing.T) {
		if u := p.Parse([]byte(`{"id":"x","choices":[]}`)); u != nil {
			t.Errorf("expected nil, got %+v", u)
		}
	})
}
