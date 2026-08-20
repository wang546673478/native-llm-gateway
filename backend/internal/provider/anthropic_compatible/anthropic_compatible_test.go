package anthropic_compatible

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// TestParseAnthropicUsage_Model P65: 验证 Anthropic 响应顶层 model 字段被抽到 Usage.Model
func TestParseAnthropicUsage_Model(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"type": "message",
		"model": "MiniMax-M3",
		"role": "assistant",
		"content": [{"type": "text", "text": "hi"}],
		"usage": {
			"input_tokens": 20,
			"output_tokens": 8,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens": 5
		}
	}`)
	u := parseAnthropicUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "MiniMax-M3" {
		t.Errorf("Model = %q, want MiniMax-M3", u.Model)
	}
	if u.PromptTokens != 20 {
		t.Errorf("PromptTokens = %d, want 20", u.PromptTokens)
	}
	if u.CompletionTokens != 8 {
		t.Errorf("CompletionTokens = %d, want 8", u.CompletionTokens)
	}
	if u.CacheReadTokens != 5 {
		t.Errorf("CacheReadTokens = %d, want 5", u.CacheReadTokens)
	}
	expectedTotal := 20 + 8 + 0 + 5
	if u.TotalTokens != expectedTotal {
		t.Errorf("TotalTokens = %d, want %d", u.TotalTokens, expectedTotal)
	}
}

// TestParseAnthropicUsage_MissingModel P65: 响应无 model 字段时 Usage.Model 为空
func TestParseAnthropicUsage_MissingModel(t *testing.T) {
	body := []byte(`{"id":"msg_02","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	u := parseAnthropicUsage(body)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.Model != "" {
		t.Errorf("Model = %q, want empty", u.Model)
	}
}

// TestExtractAnthropicStreamUsage_Model P65: 验证流式 message_start 抽 model
func TestExtractAnthropicStreamUsage_Model(t *testing.T) {
	// message_start 事件:event: + data: 行
	event := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"MiniMax-M3\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n")

	var input, output, cacheCreate, cacheRead int
	var model string
	extractAnthropicStreamUsage(event, &input, &output, &cacheCreate, &cacheRead, &model)

	if model != "MiniMax-M3" {
		t.Errorf("model = %q, want MiniMax-M3", model)
	}
	if input != 10 {
		t.Errorf("input = %d, want 10", input)
	}
}

// TestExtractAnthropicStreamUsage_MessageDelta P65: 验证 message_delta 抽 output_tokens
func TestExtractAnthropicStreamUsage_MessageDelta(t *testing.T) {
	event := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n")

	var input, output, cacheCreate, cacheRead int
	var model string
	extractAnthropicStreamUsage(event, &input, &output, &cacheCreate, &cacheRead, &model)

	if output != 15 {
		t.Errorf("output = %d, want 15", output)
	}
	// message_delta 没有 message.model,model 应该保持原值
	if model != "" {
		t.Errorf("model = %q, want empty (message_delta 不抽 model)", model)
	}
}

// silence unused imports for parallel test
var _ = json.Unmarshal

// P-quota-minimax: MiniMax base_resp 错误识别(HTTP 200 + body 错误 → ProviderError)
// MiniMax 是唯一走 anthropic_compatible 且有 base_resp 错误体的 provider

func newTestPool(t *testing.T) *keypool.Pool {
	t.Helper()
	now := time.Now()
	return keypool.NewPool("test", []*keypool.Key{{
		ID: "k1", ProviderName: "test", Name: "k1", Key: "sk-test",
		Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
}

func TestSendRequest_MiniMaxBaseRespQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	// 余额 5%(可被获取)但 poll 数据过期 >5min → 守卫不拦,2056 仍分类 quota
	ks := pool.KeyPtrs()
	ks[0].Remaining = 5
	ks[0].LastPolledAt = time.Now().Add(-10 * time.Minute)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
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
	if got := pool.Status().QuotaExceededKeys; got != 1 {
		t.Errorf("quota_exceeded keys = %d, want 1", got)
	}
}

func TestSendStreamRequest_MiniMaxBaseRespQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("event: error\ndata: {\"base_resp\":{\"status_code\":2056,\"status_msg\":\"plan limit\"}}\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	// 余额 5%(可被获取)但 poll 数据过期 >5min → 守卫不拦,2056 仍分类 quota
	ks := pool.KeyPtrs()
	ks[0].Remaining = 5
	ks[0].LastPolledAt = time.Now().Add(-10 * time.Minute)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, resp, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[],"stream":true}`),
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
		if c := <-ch; c != nil {
			t.Errorf("unexpected chunk: %+v", c)
		}
	}
	if got := pool.Status().QuotaExceededKeys; got != 1 {
		t.Errorf("quota_exceeded keys = %d, want 1", got)
	}
}

// TestSendRequest_ForceThinkingDisabled P-deepseek-thinking: deepseek-anthropic 开启
// force_thinking_disabled 时,上行 body 的 thinking 字段必须被重写成 disabled —
// DeepSeek /anthropic 在 thinking 模式下校验历史 assistant 消息必须回带 thinking 块
// (Claude Code compact 会剥离),否则 400 "content[].thinking ... must be passed back"
func TestSendRequest_ForceThinkingDisabled(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"m1","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	// 开启 ForceThinkingDisabled
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool, ForceThinkingDisabled: true})

	_, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		// 模拟 Claude Code:thinking adaptive(DeepSeek 不认 adaptive,按 enabled 处理 → 严格校验)
		Body: []byte(`{"model":"m","max_tokens":1,"thinking":{"type":"adaptive"},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	th, ok := req["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body thinking = %v, want map", req["thinking"])
	}
	if th["type"] != "disabled" {
		t.Errorf("thinking.type = %v, want disabled", th["type"])
	}
}

// TestSendRequest_NoForceThinkingDisabled 对照:不开 flag 时 thinking 原样透传
func TestSendRequest_NoForceThinkingDisabled(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"m1","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	_, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"thinking":{"type":"adaptive"},"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	th, ok := req["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body thinking = %v, want map", req["thinking"])
	}
	if th["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive (passthrough)", th["type"])
	}
}

// TestSendStreamRequest_ForceThinkingDisabled 流式路径同样重写 thinking —
// Claude Code 的真实请求都是 stream=true,deepseek 校验在两条路径都生效
func TestSendStreamRequest_ForceThinkingDisabled(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool, ForceThinkingDisabled: true})

	ch, resp, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"thinking":{"type":"adaptive"},"messages":[],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("SendStreamRequest failed: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	// drain channel
	for range ch {
	}
	var req map[string]any
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	th, ok := req["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body thinking = %v, want map", req["thinking"])
	}
	if th["type"] != "disabled" {
		t.Errorf("thinking.type = %v, want disabled", th["type"])
	}
}

// markKeyHealthy P-quota-guard 测试辅助:模拟 balancer 刚轮询过、余额充足
func markKeyHealthy(t *testing.T, pool *keypool.Pool) {
	t.Helper()
	ks := pool.KeyPtrs()
	if len(ks) != 1 {
		t.Fatalf("expected 1 key, got %d", len(ks))
	}
	ks[0].Remaining = 99
	ks[0].LastPolledAt = time.Now()
}

// TestSendRequest_BalanceGuardQuotaToRateLimit P-quota-guard:
// 有余额(99%)却收到 MiniMax 2056(套餐耗尽) → 降级 rate_limit,key 不被误杀成 QUOTA_EXCEEDED
func TestSendRequest_BalanceGuardQuotaToRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"base_resp":{"status_code":2056,"status_msg":"已达到 Token Plan 用量上限"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	markKeyHealthy(t, pool)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
	})
	if resp != nil {
		t.Fatalf("resp should be nil, got %+v", resp)
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.ProviderError", err)
	}
	if pe.ErrorType != provider.ErrorTypeRateLimit {
		t.Errorf("error type = %q, want rate_limit (balance guard downgrade)", pe.ErrorType)
	}
	if got := pool.Status().QuotaExceededKeys; got != 0 {
		t.Errorf("quota_exceeded keys = %d, want 0 (key must not be killed)", got)
	}
}

// TestSendRequest_BalanceGuardExhaustedStillQE 对照:最近轮询确认余额 0 的 key
// 收到 2056 → 照常 QUOTA_EXCEEDED(真耗尽路径不受宽松守卫影响)
func TestSendRequest_BalanceGuardExhaustedStillQE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"base_resp":{"status_code":2056,"status_msg":"已达到 Token Plan 用量上限"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	// 余额 5%(可被获取)但 poll 数据过期 >5min → 守卫不拦,2056 仍分类 quota
	// (真耗尽 key 现在会被获取逻辑直接跳过,这条路径由 poll 连续 2 轮确认 QE 接管)
	ks := pool.KeyPtrs()
	ks[0].Remaining = 5
	ks[0].LastPolledAt = time.Now().Add(-10 * time.Minute)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	_, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.ProviderError", err)
	}
	if pe.ErrorType != provider.ErrorTypeQuotaExceeded {
		t.Errorf("error type = %q, want quota_exceeded (no balance → trust upstream)", pe.ErrorType)
	}
	if got := pool.Status().QuotaExceededKeys; got != 1 {
		t.Errorf("quota_exceeded keys = %d, want 1", got)
	}
}

// TestSendRequest_RetryOnRateLimit P-quota-guard-retry: 限流 429 → 等 1s 重试一次 → 200
func TestSendRequest_RetryOnRateLimit(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"m1","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if requests != 2 {
		t.Errorf("upstream requests = %d, want 2 (one retry)", requests)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	if got := pool.Status().QuotaExceededKeys; got != 0 {
		t.Errorf("quota_exceeded keys = %d, want 0", got)
	}
}

// TestSendRequest_GuardDowngradeThenRetry 用户场景完整链:healthy key + 429(2056 文本)
// → 守卫降级 rate_limit → 重试 → 200,请求留在本 provider
func TestSendRequest_GuardDowngradeThenRetry(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			// MiniMax 瞬时限流:429 + rate_limit_error + 2056 文本(关键词拦不住)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"已达到 Token Plan 用量上限：请升级 Token Plan 套餐或购买积分补充用量。 (2056)"},"request_id":"x"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"m1","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	markKeyHealthy(t, pool)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	resp, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if requests != 2 {
		t.Errorf("upstream requests = %d, want 2 (downgrade → retry)", requests)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	if got := pool.Status().QuotaExceededKeys; got != 0 {
		t.Errorf("quota_exceeded keys = %d, want 0", got)
	}
}

// TestSendStreamRequest_GuardDowngradeThenRetry 流式路径:peek 到 2056 + healthy key
// → 守卫降级 rate_limit → 重试 → 200 流
func TestSendStreamRequest_GuardDowngradeThenRetry(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{"base_resp":{"status_code":2056,"status_msg":"已达到 Token Plan 用量上限"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
	}))
	defer upstream.Close()

	pool := newTestPool(t)
	markKeyHealthy(t, pool)
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	ch, resp, err := b.SendStreamRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[],"stream":true}`),
	})
	if err != nil {
		t.Fatalf("SendStreamRequest failed: %v", err)
	}
	if requests != 2 {
		t.Errorf("upstream requests = %d, want 2", requests)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	for range ch {
	}
	if got := pool.Status().QuotaExceededKeys; got != 0 {
		t.Errorf("quota_exceeded keys = %d, want 0", got)
	}
}

// P-key-mismatch: SendRequest 用 req.Key(路由层 acquire 的 key)发请求,
// 429 上报只冷却这一把 key — 不误标同 provider 的 healthy key
// (2026-08-06 实测:weige 429 把 key-1 误标 COOLING,双 key 同时冷却全链掉 deepseek)
func TestSendRequest_UsesRouteKey_429OnlyCooldownsThatKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer upstream.Close()

	pool := newTestPool(t) // 1 把 key(k1),补一把 k2 测「只冷却实际使用的 key」
	ks := pool.KeyPtrs()
	ks = append(ks, &keypool.Key{
		ID: "k2", ProviderName: "test", Name: "k2", Key: "sk-test-2",
		Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	b := NewBase(Config{Name: "test", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})

	// 路由层已 acquire ks[1](第 2 把)— 传给 SendRequest
	_, err := b.SendRequest(context.Background(), &provider.Request{
		Method: "POST", Path: "/v1/messages",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"m","max_tokens":1,"messages":[]}`),
		Key:     ks[1],
	})
	if err == nil {
		t.Fatal("expected 429 error")
	}

	// 只有 ks[1] 被冷却,ks[0] 保持 ACTIVE(修复前 ks[0] 也会被误标)
	if ks[1].Status != keypool.KeyStatusCooling {
		t.Errorf("ks[1] status = %q, want COOLING", ks[1].Status)
	}
	if ks[0].Status != keypool.KeyStatusActive {
		t.Errorf("ks[0] status = %q, want ACTIVE (healthy key must not be cooldowned)", ks[0].Status)
	}
}

func TestListModels_NotSupported(t *testing.T) {
	b := NewBase(Config{Name: "test", Endpoint: "http://example.com"})
	got, err := b.ListModels(context.Background())
	if !errors.Is(err, provider.ErrListModelsNotSupported) {
		t.Errorf("err = %v, want ErrListModelsNotSupported", err)
	}
	if got != nil {
		t.Errorf("models = %v, want nil", got)
	}
}
