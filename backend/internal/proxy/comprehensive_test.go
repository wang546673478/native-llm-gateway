package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestKeyStatus_QuotaExceededSkipped 测试 QUOTA_EXCEEDED key 被跳过
func TestKeyStatus_QuotaExceededSkipped(t *testing.T) {
	// provider1 有 3 把 key: key1(QE), key2(QE), key3(正常)
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)

	// 构造 3 把 key,前两把标记为 QUOTA_EXCEEDED
	keys := []*keypool.Key{
		{
			ID:           "1",
			Key:          "sk-test-1",
			ProviderName: "provider1",
			Status:       keypool.KeyStatusQuotaExceeded,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:           "2",
			Key:          "sk-test-2",
			ProviderName: "provider1",
			Status:       keypool.KeyStatusQuotaExceeded,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:           "3",
			Key:          "sk-test-3",
			ProviderName: "provider1",
			Status:       keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
	}

	pool := keypool.NewPool("provider1", keys, &keypool.StickyScheduler{}, keypool.Config{
		CoolingDuration: 60 * time.Second,
	})

	pools := map[string]*keypool.Pool{
		"provider1": pool,
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
		TraceID:  "test-qe-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	// 执行路由
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	require.NotNil(t, result)

	// 应该跳过 key1 和 key2,直接拿到 key3
	assert.Equal(t, "3", result.Key.ID, "should skip QUOTA_EXCEEDED keys and get key3")
	assert.Equal(t, keypool.KeyStatusActive, result.Key.Status)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp, "key3 should succeed")
	assert.Nil(t, perr)

	t.Logf("✓ QUOTA_EXCEEDED keys skipped: key1(QE) + key2(QE) → key3(ACTIVE) success")
}

// TestKeyStatus_CoolingKeySkipped 测试 COOLING key 在冷却期内被跳过
func TestKeyStatus_CoolingKeySkipped(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)

	now := time.Now()
	// key1 冷却中(还有 5 分钟), key2 正常
	keys := []*keypool.Key{
		{
			ID:           "1",
			Key:          "sk-test-1",
			ProviderName: "provider1",
			Status:       keypool.KeyStatusCooling,
			CoolingUntil: now.Add(5 * time.Minute),
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:           "2",
			Key:          "sk-test-2",
			ProviderName: "provider1",
			Status:       keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
	}

	pool := keypool.NewPool("provider1", keys, &keypool.StickyScheduler{}, keypool.Config{
		CoolingDuration: 60 * time.Second,
	})

	pools := map[string]*keypool.Pool{
		"provider1": pool,
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
		TraceID:  "test-cooling-001",
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

	// 应该跳过 COOLING key1,拿到 key2
	assert.Equal(t, "2", result.Key.ID, "should skip COOLING key and get key2")
	assert.Equal(t, keypool.KeyStatusActive, result.Key.Status)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp, "key2 should succeed")
	assert.Nil(t, perr)

	t.Logf("✓ COOLING key skipped: key1(COOLING, %s left) → key2(ACTIVE) success",
		result.Key.CoolingUntil.Sub(now).Round(time.Second))
}

// TestThreeTierFallback 测试三层降级: token_plan → api → free
func TestThreeTierFallback(t *testing.T) {
	// 3 个 provider: tp(token_plan,QE), api(api,QE), free(free,正常)
	providerTP := &fakeStreamProvider{
		name:      "provider-tp",
		shouldErr: true,
		errType:   provider.ErrorTypeQuotaExceeded,
		errMsg:    "quota exceeded",
	}
	providerAPI := &fakeStreamProvider{
		name:      "provider-api",
		shouldErr: true,
		errType:   provider.ErrorTypeQuotaExceeded,
		errMsg:    "quota exceeded",
	}
	providerFree := &fakeStreamProvider{
		name:      "provider-free",
		shouldErr: false,
	}

	mgr := newTestManager(t, providerTP, providerAPI, providerFree)

	pools := map[string]*keypool.Pool{
		"provider-tp":   newTestPoolWithTier("provider-tp", 1, keypool.BillingSourceTokenPlan),
		"provider-api":  newTestPoolWithTier("provider-api", 1, keypool.BillingSourceAPI),
		"provider-free": newTestPoolWithTier("provider-free", 1, keypool.BillingSourceFree),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	quotaChecker := &fakeQuotaChecker{
		quotaExhausted: map[string]bool{
			"provider-tp":  true,
			"provider-api": true,
		},
	}

	engine := NewEngine(Config{
		Router:       rtr,
		Logger:       zap.NewNop(),
		MaxRetry:     5,
		QuotaChecker: quotaChecker,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-3tier-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	// 第一次: token_plan 层
	result1, err1 := iter.Next()
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.Equal(t, "provider-tp", result1.ProviderName)
	assert.Equal(t, string(keypool.BillingSourceTokenPlan), result1.Tier)

	pv1, _ := mgr.Get(result1.ProviderName)
	resp1, perr1 := engine.doRequest(context.Background(), pv1, req, result1)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeQuotaExceeded, perr1.ErrorType)
	t.Logf("Tier 1 (token_plan): provider-tp → quota_exceeded")

	// 第二次: api 层
	result2, err2 := iter.Next()
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.Equal(t, "provider-api", result2.ProviderName)
	assert.Equal(t, string(keypool.BillingSourceAPI), result2.Tier)

	pv2, _ := mgr.Get(result2.ProviderName)
	resp2, perr2 := engine.doRequest(context.Background(), pv2, req, result2)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeQuotaExceeded, perr2.ErrorType)
	t.Logf("Tier 2 (api): provider-api → quota_exceeded")

	// 第三次: free 层
	result3, err3 := iter.Next()
	require.NoError(t, err3)
	require.NotNil(t, result3)
	assert.Equal(t, "provider-free", result3.ProviderName)
	assert.Equal(t, string(keypool.BillingSourceFree), result3.Tier)

	pv3, _ := mgr.Get(result3.ProviderName)
	resp3, perr3 := engine.doRequest(context.Background(), pv3, req, result3)
	assert.NotNil(t, resp3, "free tier should succeed")
	assert.Nil(t, perr3)
	t.Logf("Tier 3 (free): provider-free → success")

	t.Logf("✓ Three-tier fallback complete: token_plan(QE) → api(QE) → free(success)")
}

// TestAllKeysFailThenSwitchProvider 测试同 provider 所有 key 失败后才换 provider
func TestAllKeysFailThenSwitchProvider(t *testing.T) {
	// provider1 有 3 把 key,全部 429
	callCount := 0
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			callCount++
			return callCount <= 3 // 前 3 次(3 把 key)全失败
		},
		errTypeFn: func() provider.ErrorType {
			return provider.ErrorTypeRateLimit
		},
	}
	provider2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1, provider2)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPoolWithKeys("provider1", 3), // 3 把 key
		"provider2": newTestPool("provider2", 1),
	}

	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 10,
	})

	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-allkeys-001",
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
	assert.Equal(t, "provider1", result.ProviderName)
	key1ID := result.Key.ID

	pv1, _ := mgr.Get(result.ProviderName)

	// 第一把 key 失败
	resp1, perr1 := engine.doRequest(context.Background(), pv1, req, result)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr1.ErrorType)
	t.Logf("provider1 key1 → 429")

	// 换第二把 key
	swapped1 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped1)
	key2ID := result.Key.ID
	assert.NotEqual(t, key1ID, key2ID)

	resp2, perr2 := engine.doRequest(context.Background(), pv1, req, result)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr2.ErrorType)
	t.Logf("provider1 key2 → 429")

	// 换第三把 key
	swapped2 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped2)
	key3ID := result.Key.ID
	assert.NotEqual(t, key2ID, key3ID)

	resp3, perr3 := engine.doRequest(context.Background(), pv1, req, result)
	assert.Nil(t, resp3)
	require.NotNil(t, perr3)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr3.ErrorType)
	t.Logf("provider1 key3 → 429")

	// 第四把 key 不存在,swapToOtherKey 返回 false
	swapped3 := engine.swapToOtherKey(c, req, result)
	assert.False(t, swapped3, "provider1 all keys exhausted, should not swap")

	// 切换到 provider2
	result2, err2 := iter.Next()
	require.NoError(t, err2)
	assert.Equal(t, "provider2", result2.ProviderName)

	pv2, _ := mgr.Get(result2.ProviderName)
	resp4, perr4 := engine.doRequest(context.Background(), pv2, req, result2)
	assert.NotNil(t, resp4, "provider2 should succeed")
	assert.Nil(t, perr4)

	t.Logf("✓ All keys exhausted then switch: provider1(key1/2/3 all 429) → provider2(success)")
}

// TestPolledQuotaExhausted 测试轮询确认余额耗尽的 key 被跳过
func TestPolledQuotaExhausted(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)

	now := time.Now()
	// key1 余额轮询确认为 0, key2 正常
	keys := []*keypool.Key{
		{
			ID:            "1",
			Key:           "sk-test-1",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
			Remaining:     0,
			LastPolledAt:  now.Add(-1 * time.Minute), // 1 分钟前轮询过
			QuotaKind:     "currency",
		},
		{
			ID:            "2",
			Key:           "sk-test-2",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
			Remaining:     100,
			LastPolledAt:  now.Add(-1 * time.Minute),
			QuotaKind:     "currency",
		},
	}

	pool := keypool.NewPool("provider1", keys, &keypool.StickyScheduler{}, keypool.Config{
		CoolingDuration: 60 * time.Second,
	})

	pools := map[string]*keypool.Pool{
		"provider1": pool,
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
		TraceID:  "test-polled-001",
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

	// 应该跳过 key1 (Remaining=0),拿到 key2
	assert.Equal(t, "2", result.Key.ID, "should skip polled exhausted key and get key2")
	assert.Equal(t, float64(100), result.Key.Remaining)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp, "key2 should succeed")
	assert.Nil(t, perr)

	t.Logf("✓ Polled quota exhausted key skipped: key1(Remaining=0) → key2(Remaining=100) success")
}
