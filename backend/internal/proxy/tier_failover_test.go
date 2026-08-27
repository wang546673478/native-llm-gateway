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

// TestTierFailover_TokenPlanToAPI 测试 token_plan 层额度耗尽后降级到 api 层
func TestTierFailover_TokenPlanToAPI(t *testing.T) {
	// 场景:
	// - minimax (token_plan 层) 额度耗尽,返回 402 quota_exceeded
	// - 自动降级到 deepseek (api 层)
	// - deepseek 成功返回

	// 1. 构造两个 provider: minimax (token_plan,额度耗尽), deepseek (api,正常)
	minimax := &fakeStreamProvider{
		name:      "minimax",
		shouldErr: true,
		errType:   provider.ErrorTypeQuotaExceeded,
		errMsg:    "quota exceeded",
	}
	deepseek := &fakeStreamProvider{
		name:      "deepseek",
		shouldErr: false,
	}

	// 2. 构造 Manager 和 Pool
	mgr := newTestManager(t, minimax, deepseek)

	// minimax 是 token_plan 层, deepseek 是 api 层
	pools := map[string]*keypool.Pool{
		"minimax":  newTestPoolWithTier("minimax", 1, keypool.BillingSourceTokenPlan),
		"deepseek": newTestPoolWithTier("deepseek", 1, keypool.BillingSourceAPI),
	}

	// 3. Router (catch_all 自动模式)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	// 4. Engine with QuotaChecker (模拟额度检查)
	quotaChecker := &fakeQuotaChecker{
		quotaExhausted: map[string]bool{
			"minimax": true, // minimax 额度已耗尽
		},
	}
	engine := NewEngine(Config{
		Router:       rtr,
		Logger:       zap.NewNop(),
		MaxRetry:     5,
		QuotaChecker: quotaChecker,
	})

	// 5. 构造请求
	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-tier-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	// 6. 执行路由
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, iter)

	// 7. 第一次尝试 - minimax (token_plan 层) 额度耗尽
	result1, err1 := iter.Next()
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.Equal(t, "minimax", result1.ProviderName)
	assert.Equal(t, string(keypool.BillingSourceTokenPlan), result1.Tier)

	pv1, _ := mgr.Get(result1.ProviderName)
	resp1, perr1 := engine.doRequest(context.Background(), pv1, req, result1)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeQuotaExceeded, perr1.ErrorType)

	t.Logf("minimax (token_plan): quota_exceeded, error=%s", perr1.Message)

	// 8. 第二次尝试 - deepseek (api 层) 成功
	result2, err2 := iter.Next()
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.Equal(t, "deepseek", result2.ProviderName)
	assert.Equal(t, string(keypool.BillingSourceAPI), result2.Tier)

	pv2, _ := mgr.Get(result2.ProviderName)
	resp2, perr2 := engine.doRequest(context.Background(), pv2, req, result2)
	assert.NotNil(t, resp2, "deepseek (api) should succeed")
	assert.Nil(t, perr2)

	t.Logf("✓ Tier failover successful: minimax (token_plan, quota_exceeded) → deepseek (api, success)")
}

// TestRelayPassthrough_FailoverChain 测试多个 provider 的 failover 链
func TestRelayPassthrough_FailoverChain(t *testing.T) {
	// 场景: 5 个 provider A/B/C/D/E, 只有 E 能用
	// - A: 404 model not found
	// - B: 500 server error
	// - C: 连接失败
	// - D: 429 rate limit
	// - E: 成功

	// 1. 构造 5 个 provider
	providerA := &fakeStreamProvider{
		name:      "provider-a",
		shouldErr: true,
		errType:   provider.ErrorTypeModelNotFound,
		errMsg:    "model not found",
	}
	providerB := &fakeStreamProvider{
		name:      "provider-b",
		shouldErr: true,
		errType:   provider.ErrorTypeServerError,
		errMsg:    "internal server error",
	}
	providerC := &fakeStreamProvider{
		name:      "provider-c",
		shouldErr: true,
		errType:   provider.ErrorTypeConnection,
		errMsg:    "connection refused",
	}
	providerD := &fakeStreamProvider{
		name:      "provider-d",
		shouldErr: true,
		errType:   provider.ErrorTypeRateLimit,
		errMsg:    "rate limit exceeded",
	}
	providerE := &fakeStreamProvider{
		name:      "provider-e",
		shouldErr: false, // 只有 E 成功
	}

	// 2. Manager
	mgr := newTestManager(t, providerA, providerB, providerC, providerD, providerE)

	// 3. Pool (每个 provider 1 把 key)
	pools := map[string]*keypool.Pool{
		"provider-a": newTestPool("provider-a", 1),
		"provider-b": newTestPool("provider-b", 1),
		"provider-c": newTestPool("provider-c", 1),
		"provider-d": newTestPool("provider-d", 1),
		"provider-e": newTestPool("provider-e", 1),
	}

	// 4. Router (catch_all 自动模式)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	// 5. Engine
	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 10, // 允许足够多的重试
	})

	// 6. 构造请求
	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	_, _ = gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-chain-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	// 7. 执行路由并逐个尝试
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, iter)

	attempts := []struct {
		expectedName string
		expectedErr  provider.ErrorType
	}{
		{"provider-a", provider.ErrorTypeModelNotFound},
		{"provider-b", provider.ErrorTypeServerError},
		{"provider-c", provider.ErrorTypeConnection},
		{"provider-d", provider.ErrorTypeRateLimit},
	}

	// 尝试 A/B/C/D，都失败
	for i, attempt := range attempts {
		result, err := iter.Next()
		require.NoError(t, err, "iter.Next() should not error for attempt %d", i+1)
		require.NotNil(t, result)
		assert.Equal(t, attempt.expectedName, result.ProviderName, "attempt %d provider name", i+1)

		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.Nil(t, resp)
		require.NotNil(t, perr)
		assert.Equal(t, attempt.expectedErr, perr.ErrorType)

		t.Logf("Attempt %d: %s → %s (%s)", i+1, result.ProviderName, perr.ErrorType, perr.Message)
	}

	// 第 5 次尝试 - E 成功
	resultE, errE := iter.Next()
	require.NoError(t, errE)
	require.NotNil(t, resultE)
	assert.Equal(t, "provider-e", resultE.ProviderName)

	pvE, _ := mgr.Get(resultE.ProviderName)
	respE, perrE := engine.doRequest(context.Background(), pvE, req, resultE)
	assert.NotNil(t, respE, "provider-e should succeed")
	assert.Nil(t, perrE)

	t.Logf("Attempt 5: provider-e → success")
	t.Logf("✓ Provider failover chain complete: A(404) → B(500) → C(connection) → D(429) → E(success)")
}

// ==================== 测试辅助 ====================

// fakeQuotaChecker 模拟额度检查器
type fakeQuotaChecker struct {
	quotaExhausted map[string]bool // provider name → 是否额度耗尽
}

func (q *fakeQuotaChecker) CheckQuota(ctx context.Context, providerName, baseURL string, key *keypool.Key) (bool, error) {
	// 返回 (hasQuota, error)
	hasQuota := !q.quotaExhausted[providerName]
	return hasQuota, nil
}

// newTestPoolWithTier 构造指定 tier 的 Pool
func newTestPoolWithTier(providerName string, keyCount int, tier keypool.BillingSource) *keypool.Pool {
	keys := make([]*keypool.Key, 0, keyCount)
	for i := 1; i <= keyCount; i++ {
		key := &keypool.Key{
			ID:            string(rune('0' + i)),
			Key:           "sk-test-" + providerName + "-" + string(rune('0'+i)),
			ProviderName:  providerName,
			Status:        keypool.KeyStatusActive,
			BillingSource: string(tier),
		}
		keys = append(keys, key)
	}

	return keypool.NewPool(providerName, keys, &keypool.RoundRobinScheduler{}, keypool.Config{
		CoolingDuration: 60 * time.Second,
	})
}
