package proxy

import (
	"context"
	"errors"
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

// TestFailover_StreamDisconnect 测试流式请求中途断流后切换到下一个 provider
func TestFailover_StreamDisconnect(t *testing.T) {
	// 1. 构造三个 provider: provider1(会断流), provider2(正常), provider3(备用)
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: true,
		errType:   provider.ErrorTypeConnection,
		errMsg:    "connection reset by peer",
	}
	provider2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}
	provider3 := &fakeStreamProvider{
		name:      "provider3",
		shouldErr: false,
	}

	// 2. 构造 Manager 和 Pool
	mgr := newTestManager(t, provider1, provider2, provider3)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
		"provider2": newTestPool("provider2", 1),
		"provider3": newTestPool("provider3", 1),
	}

	// 3. 构造 Router (catch_all 自动模式)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{}, // 空规则 = 自动模式
	})

	// 4. 构造 Engine
	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	// 5. 构造请求
	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-trace-001",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: true,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`),
		Headers:  http.Header{},
	}

	// 6. 执行路由
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, iter)

	// 7. 第一次尝试 - provider1 断流
	result1, err1 := iter.Next()
	require.NoError(t, err1)
	require.NotNil(t, result1)
	assert.Equal(t, "provider1", result1.ProviderName)

	// 模拟 attemptOne 调用
	pv1, _ := mgr.Get(result1.ProviderName)
	_, _, perr1, _ := engine.doStream(context.Background(), c, pv1, req, result1, nil)
	require.NotNil(t, perr1, "provider1 should fail with connection error")
	assert.Equal(t, provider.ErrorTypeConnection, perr1.ErrorType)

	// 验证错误是网络类(应该触发 failover)
	assert.True(t, isNetworkClassErr(perr1), "connection error should be network class")

	// 8. failover 到第二个 provider - provider2 成功
	result2, err2 := iter.Next()
	require.NoError(t, err2)
	require.NotNil(t, result2)
	assert.Equal(t, "provider2", result2.ProviderName)

	pv2, _ := mgr.Get(result2.ProviderName)
	ok2, _, perr2, _ := engine.doStream(context.Background(), c, pv2, req, result2, nil)
	assert.True(t, ok2, "provider2 should succeed")
	assert.Nil(t, perr2, "provider2 should not have error")

	t.Logf("✓ Failover successful: provider1 (connection error) → provider2 (success)")
}

// TestFailover_RateLimitThenSwitch 测试 429 限流后切换 key 和 provider
func TestFailover_RateLimitThenSwitch(t *testing.T) {
	// 1. 构造两个 provider: provider1(429), provider2(正常)
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: true,
		errType:   provider.ErrorTypeRateLimit,
		errMsg:    "rate limit exceeded",
	}
	provider2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}

	// 2. provider1 有两把 key
	mgr := newTestManager(t, provider1, provider2)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPoolWithKeys("provider1", 2), // 2 把 key
		"provider2": newTestPool("provider2", 1),
	}

	// 3. Router
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	// 4. Engine
	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	// 5. 构造请求
	gin.SetMode(gin.TestMode)
	w := &fakeResponseWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(w)
	req := &provider.Request{
		TraceID:  "test-trace-002",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	// 6. 路由到 provider1
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result1, err1 := iter.Next()
	require.NoError(t, err1)
	assert.Equal(t, "provider1", result1.ProviderName)
	firstKeyID := result1.Key.ID

	// 7. 第一次尝试 - provider1 key1 返回 429
	pv1, _ := mgr.Get(result1.ProviderName)
	resp1, perr1 := engine.doRequest(context.Background(), pv1, req, result1)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr1.ErrorType)

	// 8. swapToOtherKey - 切换到 provider1 的第二把 key
	swapped := engine.swapToOtherKey(c, req, result1)
	assert.True(t, swapped, "should swap to another key in same provider")
	assert.NotEqual(t, firstKeyID, result1.Key.ID, "should use different key")

	// 9. 第二次尝试 - provider1 key2 仍然 429
	resp2, perr2 := engine.doRequest(context.Background(), pv1, req, result1)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeRateLimit, perr2.ErrorType)

	// 10. 两把 key 都 429,failover 到 provider2
	result2, err2 := iter.Next()
	require.NoError(t, err2)
	assert.Equal(t, "provider2", result2.ProviderName)

	pv2, _ := mgr.Get(result2.ProviderName)
	resp3, perr3 := engine.doRequest(context.Background(), pv2, req, result2)
	assert.NotNil(t, resp3, "provider2 should succeed")
	assert.Nil(t, perr3)

	t.Logf("✓ Failover successful: provider1 key1 (429) → provider1 key2 (429) → provider2 (success)")
}

// TestFailover_ConnectionErrorSwapKey 测试连接错误后在同一 provider 内换 key
func TestFailover_ConnectionErrorSwapKey(t *testing.T) {
	// provider1 有 3 把 key: key1(断流), key2(断流), key3(正常)
	callCount := 0
	provider1 := &fakeStreamProviderDynamic{
		name: "provider1",
		shouldErrFn: func() bool {
			callCount++
			return callCount <= 2 // 前两次失败,第三次成功
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
	c, _ := gin.CreateTestContext(nil)
	req := &provider.Request{
		TraceID:  "test-trace-003",
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

	// 第一次尝试 - key1 连接失败
	pv, _ := mgr.Get(result.ProviderName)
	resp1, perr1 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp1)
	require.NotNil(t, perr1)
	assert.Equal(t, provider.ErrorTypeConnection, perr1.ErrorType)

	// 换 key - key2
	swapped1 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped1)
	key2ID := result.Key.ID
	assert.NotEqual(t, key1ID, key2ID)

	// 第二次尝试 - key2 连接失败
	resp2, perr2 := engine.doRequest(context.Background(), pv, req, result)
	assert.Nil(t, resp2)
	require.NotNil(t, perr2)
	assert.Equal(t, provider.ErrorTypeConnection, perr2.ErrorType)

	// 换 key - key3
	swapped2 := engine.swapToOtherKey(c, req, result)
	assert.True(t, swapped2)
	key3ID := result.Key.ID
	assert.NotEqual(t, key2ID, key3ID)

	// 第三次尝试 - key3 成功
	resp3, perr3 := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp3, "provider1 key3 should succeed")
	assert.Nil(t, perr3)

	t.Logf("✓ Key swap successful: key1 (connection) → key2 (connection) → key3 (success)")
	t.Logf("  Total attempts: %d, Keys used: %s → %s → %s", callCount, key1ID, key2ID, key3ID)
}

// ==================== 测试辅助函数 ====================

// fakeStreamProvider 模拟流式 provider
type fakeStreamProvider struct {
	name      string
	shouldErr bool
	errType   provider.ErrorType
	errMsg    string
}

func (p *fakeStreamProvider) Name() string                { return p.name }
func (p *fakeStreamProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *fakeStreamProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if p.shouldErr {
		return nil, &provider.ProviderError{
			ProviderName: p.name,
			ErrorType:    p.errType,
			Message:      p.errMsg,
			StatusCode:   0,
		}
	}
	return &provider.Response{
		StatusCode: 200,
		Body:       []byte(`{"id":"test","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		Headers:    http.Header{},
	}, nil
}

func (p *fakeStreamProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if p.shouldErr {
		return nil, nil, &provider.ProviderError{
			ProviderName: p.name,
			ErrorType:    p.errType,
			Message:      p.errMsg,
			StatusCode:   0,
		}
	}

	ch := make(chan *provider.StreamChunk, 2)
	go func() {
		ch <- &provider.StreamChunk{Data: []byte(`data: {"choices":[{"delta":{"content":"Hello"}}]}`)}
		ch <- &provider.StreamChunk{Data: []byte(`data: [DONE]`)}
		close(ch)
	}()

	return ch, &provider.Response{StatusCode: 200, Headers: http.Header{}}, nil
}

func (p *fakeStreamProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeStreamProvider) ListModels(ctx context.Context) ([]string, error) {
	return []string{"gpt-4", "gpt-3.5-turbo"}, nil
}
func (p *fakeStreamProvider) SetPool(*keypool.Pool) {}
func (p *fakeStreamProvider) Close() error          { return nil }

// fakeStreamProviderDynamic 动态决定是否失败
type fakeStreamProviderDynamic struct {
	name        string
	shouldErrFn func() bool
	errTypeFn   func() provider.ErrorType
}

func (p *fakeStreamProviderDynamic) Name() string                { return p.name }
func (p *fakeStreamProviderDynamic) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *fakeStreamProviderDynamic) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if p.shouldErrFn() {
		return nil, &provider.ProviderError{
			ProviderName: p.name,
			ErrorType:    p.errTypeFn(),
			Message:      "error",
		}
	}
	return &provider.Response{
		StatusCode: 200,
		Body:       []byte(`{"choices":[{"message":{"content":"ok"}}]}`),
		Headers:    http.Header{},
	}, nil
}

func (p *fakeStreamProviderDynamic) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, errors.New("not implemented")
}

func (p *fakeStreamProviderDynamic) HealthCheck(ctx context.Context) error { return nil }
func (p *fakeStreamProviderDynamic) ListModels(ctx context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *fakeStreamProviderDynamic) SetPool(*keypool.Pool) {}
func (p *fakeStreamProviderDynamic) Close() error          { return nil }

// newTestManager 构造测试用 Manager
func newTestManager(t *testing.T, providers ...provider.Provider) *provider.Manager {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range providers {
		p := p
		reg.Register(p.Name(), func(cfg provider.ProviderConfig) (provider.Provider, error) {
			return p, nil
		})
	}
	mgr := provider.NewManager(reg, zap.NewNop())

	// 加载 providers - 使用 LoadFromConfig
	providerConfigs := make(map[string]provider.ManagerProviderConfig)
	for _, p := range providers {
		providerConfigs[p.Name()] = provider.ManagerProviderConfig{
			Enabled:  true,
			Endpoint: "http://localhost:8080",
			Protocol: p.Protocol(),
			Timeout:  30 * time.Second,
		}
	}

	cfg := &provider.ManagerConfig{
		Providers: providerConfigs,
	}
	_ = mgr.LoadFromConfig(context.Background(), cfg)

	// 加载模型数据 - 为每个 provider 设置默认模型
	modelRows := make([]provider.DBModelRow, 0)
	for _, p := range providers {
		models, _ := p.ListModels(context.Background())
		for _, m := range models {
			modelRows = append(modelRows, provider.DBModelRow{
				Vendor:  p.Name(),
				ModelID: m,
			})
		}
	}

	// 使用 fakeModelStore 加载模型
	_ = mgr.LoadModelsFromStore(context.Background(), fakeModelStore{rows: modelRows})

	return mgr
}


// newTestPool 构造测试用 Pool (单个 key)
func newTestPool(providerName string, keyCount int) *keypool.Pool {
	return newTestPoolWithKeys(providerName, keyCount)
}

// newTestPoolWithKeys 构造测试用 Pool (多个 key)
func newTestPoolWithKeys(providerName string, keyCount int) *keypool.Pool {
	keys := make([]*keypool.Key, 0, keyCount)
	for i := 1; i <= keyCount; i++ {
		key := &keypool.Key{
			ID:            string(rune('0' + i)),
			Key:           "sk-test-" + providerName + "-" + string(rune('0'+i)),
			ProviderName:  providerName,
			Status:        keypool.KeyStatusActive,
			BillingSource: string(keypool.BillingSourceAPI),
		}
		keys = append(keys, key)
	}

	return keypool.NewPool(providerName, keys, &keypool.RoundRobinScheduler{}, keypool.Config{
		CoolingDuration: 60 * time.Second,
	})
}

// isNetworkClass 判断是否网络类错误
func isNetworkClassErr(pe *provider.ProviderError) bool {
	return pe.ErrorType == provider.ErrorTypeTimeout ||
		pe.ErrorType == provider.ErrorTypeConnection ||
		pe.ErrorType == provider.ErrorTypeServerError
}

// fakeResponseWriter 最小 gin.ResponseWriter 实现
type fakeResponseWriter struct {
	header     http.Header
	statusCode int
	body       []byte
}

func (w *fakeResponseWriter) Header() http.Header              { return w.header }
func (w *fakeResponseWriter) Write(b []byte) (int, error)      { w.body = append(w.body, b...); return len(b), nil }
func (w *fakeResponseWriter) WriteHeader(statusCode int)       { w.statusCode = statusCode }
func (w *fakeResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *fakeResponseWriter) Status() int                       { return w.statusCode }
func (w *fakeResponseWriter) Size() int                         { return len(w.body) }
func (w *fakeResponseWriter) Written() bool                     { return len(w.body) > 0 }
func (w *fakeResponseWriter) WriteHeaderNow()                   {}
func (w *fakeResponseWriter) Pusher() http.Pusher               { return nil }
func (w *fakeResponseWriter) Flush()                            {} // 流式响应需要
