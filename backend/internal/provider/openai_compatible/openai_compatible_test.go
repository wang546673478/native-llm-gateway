package openai_compatible

import (
	"bufio"
	"context"
	"encoding/json"
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
		Body:   []byte(`{"model":"m","messages":[]}`),
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
		w.Write([]byte(`{"error":{"message":"rate limit"}}`))
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
	if pool.Status().DisabledKeys != 1 {
		t.Errorf("expected key disabled, got %+v", pool.Status())
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
	if u.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", u.PromptTokens)
	}
	if u.RawUsage["prompt_cache_hit_tokens"] != 80 {
		t.Errorf("prompt_cache_hit_tokens in RawUsage = %v, want 80", u.RawUsage["prompt_cache_hit_tokens"])
	}
	if u.RawUsage["reasoning_tokens"] != 30 {
		t.Errorf("reasoning_tokens in RawUsage = %v, want 30", u.RawUsage["reasoning_tokens"])
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
		"":      0,
		"30":    30 * time.Second,
		"120":   120 * time.Second,
		"junk":  0,
	}
	for in, want := range cases {
		got := parseRetryAfter(in)
		if got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
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
