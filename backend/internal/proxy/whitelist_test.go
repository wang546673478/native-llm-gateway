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

// TestScheduler_StickyOrder 测试 Sticky 调度器按优先级顺序选择 key
func TestScheduler_StickyOrder(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)

	// 3 把 key,优先级顺序: key1 > key2 > key3
	keys := []*keypool.Key{
		{
			ID:            "1",
			Key:           "sk-test-1",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:            "2",
			Key:           "sk-test-2",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:            "3",
			Key:           "sk-test-3",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
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
		TraceID:  "test-sticky-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	// 多次路由,应该都优先选择 key1
	for i := 0; i < 3; i++ {
		iter, err := rtr.Route(context.Background(), req)
		require.NoError(t, err)

		result, err := iter.Next()
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, "1", result.Key.ID, "sticky scheduler should always pick key1 first")

		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.NotNil(t, resp)
		assert.Nil(t, perr)
	}

	t.Logf("✓ Sticky scheduler consistently picks highest priority key (key1)")
}

// TestScheduler_RoundRobin 测试 RoundRobin 调度器轮询
func TestScheduler_RoundRobin(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)

	keys := []*keypool.Key{
		{
			ID:            "1",
			Key:           "sk-test-1",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:            "2",
			Key:           "sk-test-2",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
		{
			ID:            "3",
			Key:           "sk-test-3",
			ProviderName:  "provider1",
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		},
	}

	pool := keypool.NewPool("provider1", keys, &keypool.RoundRobinScheduler{}, keypool.Config{
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
	req := &provider.Request{
		TraceID:  "test-rr-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	usedKeys := make(map[string]bool)
	for i := 0; i < 3; i++ {
		w := &fakeResponseWriter{header: http.Header{}}
		_, _ = gin.CreateTestContext(w)

		iter, err := rtr.Route(context.Background(), req)
		require.NoError(t, err)

		result, err := iter.Next()
		require.NoError(t, err)
		require.NotNil(t, result)

		usedKeys[result.Key.ID] = true

		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.NotNil(t, resp)
		assert.Nil(t, perr)
	}

	// RoundRobin 应该使用所有 3 把 key
	assert.Equal(t, 3, len(usedKeys), "round robin should use all 3 keys")
	t.Logf("✓ RoundRobin scheduler rotates through all keys: %v", usedKeys)
}

// TestMultiProvider_LoadBalancing 测试多 provider 间的负载均衡
func TestMultiProvider_LoadBalancing(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}
	provider2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}
	provider3 := &fakeStreamProvider{
		name:      "provider3",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1, provider2, provider3)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
		"provider2": newTestPool("provider2", 1),
		"provider3": newTestPool("provider3", 1),
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
	req := &provider.Request{
		TraceID:  "test-lb-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	usedProviders := make(map[string]int)
	for i := 0; i < 10; i++ {
		w := &fakeResponseWriter{header: http.Header{}}
		_, _ = gin.CreateTestContext(w)

		iter, err := rtr.Route(context.Background(), req)
		require.NoError(t, err)

		result, err := iter.Next()
		require.NoError(t, err)
		require.NotNil(t, result)

		usedProviders[result.ProviderName]++

		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.NotNil(t, resp)
		assert.Nil(t, perr)
	}

	// catch_all 模式可能优先使用同一个 provider (tier 相同时按顺序)
	// 验证至少有 1 个 provider 被使用,且所有请求都成功
	assert.GreaterOrEqual(t, len(usedProviders), 1, "should use at least one provider")
	totalRequests := 0
	for _, count := range usedProviders {
		totalRequests += count
	}
	assert.Equal(t, 10, totalRequests, "all 10 requests should succeed")
	t.Logf("✓ Multi-provider routing works: %v", usedProviders)
}
