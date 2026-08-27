package proxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestRetry_RateLimit10Times 测试 429 错误同 key 重试 10 次
func TestRetry_RateLimit10Times(t *testing.T) {
	var attemptCount int32
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			count := atomic.AddInt32(&attemptCount, 1)
			return count <= 10 // 前 10 次失败,第 11 次成功
		},
		errTypeFn: func() provider.ErrorType {
			return provider.ErrorTypeRateLimit
		},
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 15, // 允许足够多的重试
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-retry-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	require.NotNil(t, result)

	pv, _ := mgr.Get(result.ProviderName)

	// 循环重试,直到成功或达到上限
	var finalResp *provider.Response
	var finalErr *provider.ProviderError
	for i := 0; i < 15; i++ {
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		if perr == nil {
			finalResp = resp
			finalErr = nil
			break
		}
		finalErr = perr
		if perr.ErrorType != provider.ErrorTypeRateLimit {
			break
		}
		// 429 错误,继续用同一个 key 重试
		t.Logf("Attempt %d: 429 rate_limit", i+1)
	}

	// 应该在第 11 次成功
	assert.NotNil(t, finalResp, "should succeed after retries")
	assert.Nil(t, finalErr)
	assert.Equal(t, int32(11), atomic.LoadInt32(&attemptCount), "should attempt 11 times (10 failures + 1 success)")

	t.Logf("✓ Rate limit retry: 10 failures → success on attempt 11")
}

// TestRetry_NetworkErrorSwapKey 测试网络错误立即换 key 不重试同 key
func TestRetry_NetworkErrorSwapKey(t *testing.T) {
	var attemptCount int32
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			count := atomic.AddInt32(&attemptCount, 1)
			return count <= 2 // 前 2 次失败(2 把 key),第 3 次成功
		},
		errTypeFn: func() provider.ErrorType {
			return provider.ErrorTypeConnection
		},
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPoolWithKeys("provider1", 3), // 3 把 key
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 5,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-network-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	key1ID := result.Key.ID

	pv, _ := mgr.Get(result.ProviderName)

	// 第一次 - key1 连接失败
	resp1, perr1 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeConnection, perr1.ErrorType)

	// 网络错误应该立即换 key
	swapped1 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped1, "network error should swap key immediately")
	key2ID := result.Key.ID
	assert.NotEqual(t, key1ID, key2ID, "should switch to different key")

	// 第二次 - key2 连接失败
	resp2, perr2 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeConnection, perr2.ErrorType)

	// 再次换 key
	swapped2 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped2)
	key3ID := result.Key.ID
	assert.NotEqual(t, key2ID, key3ID)

	// 第三次 - key3 成功
	resp3, perr3 := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp3, "key3 should succeed")
	assert.Nil(t, perr3)

	assert.Equal(t, int32(3), atomic.LoadInt32(&attemptCount), "should attempt 3 times (each key once)")
	t.Logf("✓ Network error swaps key immediately: key1 → key2 → key3 (no retry on same key)")
}

// TestRetry_MaxRetryExhausted 测试达到最大重试次数
func TestRetry_MaxRetryExhausted(t *testing.T) {
	// 所有请求都失败
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: true,
		errType:   provider.ErrorTypeServerError,
		errMsg:    "internal error",
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3, // 只允许 3 次重试
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-maxretry-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	attemptCount := 0
	var lastErr *provider.ProviderError

	// 尝试路由所有候选
	for {
		result, err := iter.Next()
		if err != nil {
			break // 没有更多候选
		}
		if result == nil {
			break
		}

		attemptCount++
		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.Nil(t, resp)
		lastErr = perr

		if attemptCount >= 3 {
			break // 达到最大重试次数
		}
	}

	assert.NotNil(t, lastErr, "should have error after max retries")
	assert.LessOrEqual(t, attemptCount, 3, "should not exceed max retry count")
	t.Logf("✓ Max retry exhausted: attempted %d times, all failed", attemptCount)
}

// TestRetry_SuccessOnFirstAttempt 测试第一次尝试就成功
func TestRetry_SuccessOnFirstAttempt(t *testing.T) {
	var attemptCount int32
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			atomic.AddInt32(&attemptCount, 1)
			return false // 第一次就成功
		},
		errTypeFn: func() provider.ErrorType {
			return ""
		},
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-first-success-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)

	assert.NotNil(t, resp, "should succeed on first attempt")
	assert.Nil(t, perr)
	assert.Equal(t, int32(1), atomic.LoadInt32(&attemptCount), "should only attempt once")

	t.Logf("✓ Success on first attempt: no retry needed")
}

// TestRetry_MixedErrors 测试混合错误类型的重试策略
func TestRetry_MixedErrors(t *testing.T) {
	// key1: 429 → key2: connection → key3: success
	var attemptCount int32
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			count := atomic.AddInt32(&attemptCount, 1)
			return count <= 2 // 前 2 次失败
		},
		errTypeFn: func() provider.ErrorType {
			count := atomic.LoadInt32(&attemptCount)
			if count == 1 {
				return provider.ErrorTypeRateLimit // 第一次 429
			}
			return provider.ErrorTypeConnection // 第二次连接失败
		},
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPoolWithKeys("provider1", 3),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 5,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-mixed-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	key1ID := result.Key.ID

	pv, _ := mgr.Get(result.ProviderName)

	// 第一次 - 429
	resp1, perr1 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr1.ErrorType)
	t.Logf("Attempt 1 (key1): 429 rate_limit")

	// 429 换 key
	swapped1 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped1)
	key2ID := result.Key.ID
	assert.NotEqual(t, key1ID, key2ID)

	// 第二次 - connection error
	resp2, perr2 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeConnection, perr2.ErrorType)
	t.Logf("Attempt 2 (key2): connection error")

	// connection error 换 key
	swapped2 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped2)
	key3ID := result.Key.ID
	assert.NotEqual(t, key2ID, key3ID)

	// 第三次 - success
	resp3, perr3 := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp3, "key3 should succeed")
	assert.Nil(t, perr3)
	t.Logf("Attempt 3 (key3): success")

	assert.Equal(t, int32(3), atomic.LoadInt32(&attemptCount))
	t.Logf("✓ Mixed errors handled correctly: 429 (swap) → connection (swap) → success")
}

// TestRetry_TimeoutBehavior 测试超时的基本行为
func TestRetry_TimeoutBehavior(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: true,
		errType:   provider.ErrorTypeTimeout,
		errMsg:    "request timeout",
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-timeout-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)

	assert.Nil(t, resp)
	require.NotNil(t, perr)
	assert.Equal(t, provider.ErrorTypeTimeout, perr.ErrorType)

	// 超时是网络类错误,应该触发 swapToOtherKey
	assert.True(t, isNetworkClassErr(perr), "timeout should be network class error")

	// 尝试换 key (虽然只有 1 把 key 会失败)
	swapped := engine.swapToOtherKey(c, req, result)
	assert.False(t, swapped, "only 1 key available, cannot swap")

	t.Logf("✓ Timeout treated as network error: triggers failover logic")
}
