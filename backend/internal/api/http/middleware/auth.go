// Package middleware Gin 中间件
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/auth"
)

// AuthMiddleware 验证 Gateway API Key
//   - 从 Authorization: Bearer <token> 提取
//   - 写入 gin.Context("gateway_key")
//   - 401 if invalid
func AuthMiddleware(a *auth.Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, err := a.Authenticate(c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"type": "unauthorized", "message": err.Error()},
			})
			return
		}
		c.Set("gateway_key", key)
		c.Set("gateway_key_id", key.ID)
		c.Next()
	}
}

// RateLimitMiddleware 按 gateway key 做 RPM 限流
func RateLimitMiddleware(a *auth.Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.GetString("gateway_key_id")
		if keyID == "" {
			c.Next()
			return
		}
		// 拿到 key 查 RPM
		v, ok := c.Get("gateway_key")
		if !ok {
			c.Next()
			return
		}
		gk := v.(*auth.GatewayKey)
		if !a.AllowRequest(keyID, gk.RateLimit.RPM) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{"type": "rate_limit_exceeded", "message": "gateway key rate limit exceeded"},
			})
			return
		}
		c.Next()
	}
}
