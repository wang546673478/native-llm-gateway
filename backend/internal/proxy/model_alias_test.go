package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestModelAlias_ResolvesToActualModel 测试模型别名解析到实际模型
func TestModelAlias_ResolvesToActualModel(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	// 配置别名: gpt4 → gpt-4
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"gpt4": {
				Alias:       "gpt4",
				TargetModel: "gpt-4",
			},
		},
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

	// 请求别名 "gpt4"
	req := &provider.Request{
		TraceID:  "test-alias-001",
		Model:    "gpt4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt4","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	require.NotNil(t, result)

	// 应该解析到 gpt-4
	assert.Equal(t, "gpt-4", result.ModelID, "alias gpt4 should resolve to gpt-4")

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp)
	assert.Nil(t, perr)

	t.Logf("✓ Model alias resolved: gpt4 → gpt-4")
}

// TestModelAlias_MultipleAliasesForSameModel 测试多个别名指向同一模型
func TestModelAlias_MultipleAliasesForSameModel(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	// 配置多个别名指向 gpt-4
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"gpt4": {
				Alias:       "gpt4",
				TargetModel: "gpt-4",
			},
			"chatgpt": {
				Alias:       "chatgpt",
				TargetModel: "gpt-4",
			},
			"best": {
				Alias:       "best",
				TargetModel: "gpt-4",
			},
		},
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	gin.SetMode(gin.TestMode)

	aliases := []string{"gpt4", "chatgpt", "best"}
	for _, alias := range aliases {
		w := &fakeResponseWriter{header: http.Header{}}
		_, _ = gin.CreateTestContext(w)

		req := &provider.Request{
			TraceID:  "test-alias-" + alias,
			Model:    alias,
			Path:     "/v1/chat/completions",
			IsStream: false,
			Body:     []byte(`{"model":"` + alias + `","messages":[{"role":"user","content":"hello"}]}`),
			Headers:  http.Header{},
		}

		iter, err := rtr.Route(context.Background(), req)
		require.NoError(t, err)

		result, err := iter.Next()
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.Equal(t, "gpt-4", result.ModelID, "alias %s should resolve to gpt-4", alias)

		pv, _ := mgr.Get(result.ProviderName)
		resp, perr := engine.doRequest(context.Background(), pv, req, result)
		assert.NotNil(t, resp)
		assert.Nil(t, perr)

		t.Logf("✓ Alias %s → gpt-4", alias)
	}
}

// TestModelAlias_NoAliasUsesDirectModel 测试无别名时直接使用模型名
func TestModelAlias_NoAliasUsesDirectModel(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
	}

	// 无别名配置
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

	// 请求直接模型名
	req := &provider.Request{
		TraceID:  "test-direct-001",
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

	// 应该直接使用 gpt-4
	assert.Equal(t, "gpt-4", result.ModelID, "should use direct model name gpt-4")

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp)
	assert.Nil(t, perr)

	t.Logf("✓ Direct model used without alias: gpt-4")
}

// TestModelAlias_AliasWithProviderBinding 测试别名 + Provider 绑定
func TestModelAlias_AliasWithProviderBinding(t *testing.T) {
	provider1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: false,
	}
	provider2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}

	mgr := newTestManager(t, provider1, provider2)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
		"provider2": newTestPool("provider2", 1),
	}

	// 配置别名 + provider 绑定
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		Aliases: map[string]router.AliasConfig{
			"fast": {
				Alias: "fast",
				Providers: []router.ProviderRoute{
					{
						Name:  "provider1",
						Model: "gpt-3.5-turbo",
					},
				},
			},
		},
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
		TraceID:  "test-alias-provider-001",
		Model:    "fast",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"fast","messages":[{"role":"user","content":"hello"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	result, err := iter.Next()
	require.NoError(t, err)
	require.NotNil(t, result)

	// 应该解析到 gpt-3.5-turbo + 绑定 provider1
	assert.Equal(t, "gpt-3.5-turbo", result.ModelID)
	assert.Equal(t, "provider1", result.ProviderName)

	pv, _ := mgr.Get(result.ProviderName)
	resp, perr := engine.doRequest(context.Background(), pv, req, result)
	assert.NotNil(t, resp)
	assert.Nil(t, perr)

	t.Logf("✓ Alias with provider binding: fast → gpt-3.5-turbo@provider1")
}
