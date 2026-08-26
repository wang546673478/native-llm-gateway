// Package middleware — HTTP 中间件
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/wang546673478/native-llm-gateway/internal/adminauth"
	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// AdminAuthMiddleware 管理员鉴权中间件
func AdminAuthMiddleware(am *adminauth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 从 Cookie 或 Header 读 token
		//    优先 Header: X-Admin-Token (方便 API 调试工具)
		//    后备 Cookie: session_token (浏览器)
		token := c.GetHeader("X-Admin-Token")
		if token == "" {
			cookie, _ := c.Cookie("session_token")
			token = cookie
		}

		if token == "" {
			c.AbortWithStatusJSON(401, gin.H{
				"error": gin.H{
					"type":    "unauthorized",
					"message": "missing session token",
				},
			})
			return
		}

		// 2. 验证 session
		user, err := am.ValidateSession(token)
		if err != nil {
			c.AbortWithStatusJSON(401, gin.H{
				"error": gin.H{
					"type":    "unauthorized",
					"message": "invalid or expired session",
				},
			})
			return
		}

		// 3. 写入 context
		c.Set("admin_user", user)
		c.Next()
	}
}

// RequireRole 权限检查中间件 — 只允许特定角色访问
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userVal, exists := c.Get("admin_user")
		if !exists {
			c.AbortWithStatusJSON(500, gin.H{
				"error": gin.H{
					"type":    "internal_error",
					"message": "admin_user not found in context",
				},
			})
			return
		}

		user := userVal.(*dbpkg.AdminUser)
		for _, role := range roles {
			if user.Role == role {
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(403, gin.H{
			"error": gin.H{
				"type":    "forbidden",
				"message": "insufficient permissions",
			},
		})
	}
}
