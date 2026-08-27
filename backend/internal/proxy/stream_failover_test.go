package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestProxy_Stream_EarlyFailure_TriggersFailover 验证新的流式 failover 逻辑:
// 如果第一个 chunk 都拿不到(连接失败/立即报错),Gateway 应该在发送 HTTP 200
// 之前就检测到,触发 failover 切换到下一个 provider。
//
// P-stream-failover: 延迟发送 HTTP 200 — 先缓冲第一个 chunk 确认上游能响应。
func TestProxy_Stream_EarlyFailure_TriggersFailover(t *testing.T) {
	// provider1: 连接失败,无法建立流
	p1 := &fakeStreamProvider{
		name:      "provider1",
		shouldErr: true,
		errType:   provider.ErrorTypeConnection,
		errMsg:    "connection refused",
	}
	// provider2: 正常返回
	p2 := &fakeStreamProvider{
		name:      "provider2",
		shouldErr: false,
	}

	mgr := newTestManager(t, p1, p2)
	pools := map[string]*keypool.Pool{
		"provider1": newTestPool("provider1", 1),
		"provider2": newTestPool("provider2", 1),
	}
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})

	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: 3,
	})

	// 执行完整的请求流程(使用 HandleRequest 而不是直接调用 doStream)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", engine.HandleRequest)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 应该成功返回 200(failover 到 provider2 成功了)
	assert.Equal(t, 200, w.Code, "failover should succeed")

	// 响应应该包含数据(来自 provider2)
	body := w.Body.String()
	require.NotEmpty(t, body, "response should not be empty")
	assert.Contains(t, body, "chat.completion", "response should contain completion data from provider2")

	t.Logf("✓ Stream early failure triggered failover: provider1(connection refused) → provider2(success)")
}
