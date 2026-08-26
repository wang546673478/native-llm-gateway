// Package handler — 管理员认证 API handler
package handler

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/adminauth"
	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/api/http/middleware"
)

// AdminAuthHandler 管理员认证 handler
type AdminAuthHandler struct {
	am *adminauth.Manager
}

// NewAdminAuthHandler 创建 handler
func NewAdminAuthHandler(am *adminauth.Manager) *AdminAuthHandler {
	return &AdminAuthHandler{am: am}
}

// Register 注册路由
func (h *AdminAuthHandler) Register(r *gin.RouterGroup) {
	// 登录端点始终注册(即使未启用,也返回明确错误而非 404)
	r.POST("/auth/login", h.login)

	// 其他端点仅在启用时注册
	if h.am == nil {
		return
	}

	// 需要鉴权的端点
	authed := r.Group("/auth")
	authed.Use(middleware.AdminAuthMiddleware(h.am))
	authed.POST("/logout", h.logout)
	authed.POST("/change-password", h.changePassword)
	authed.GET("/me", h.getCurrentUser)

	// 用户管理（只有 root 可以）
	users := r.Group("/admin-users")
	users.Use(
		middleware.AdminAuthMiddleware(h.am),
		middleware.RequireRole("root"),
	)
	users.GET("", h.listUsers)
	users.POST("", h.createUser)
	users.PUT("/:id", h.updateUser)
	users.DELETE("/:id", h.deleteUser)
}

// login 登录
func (h *AdminAuthHandler) login(c *gin.Context) {
	if h.am == nil {
		c.JSON(503, gin.H{"error": gin.H{"type": "feature_disabled", "message": "admin authentication is disabled"}})
		return
	}

	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"type": "invalid_request", "message": err.Error()}})
		return
	}

	session, err := h.am.Login(req.Username, req.Password, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		if errors.Is(err, adminauth.ErrAccountLocked) {
			c.JSON(403, gin.H{"error": gin.H{"type": "account_locked", "message": err.Error()}})
			return
		}
		c.JSON(401, gin.H{"error": gin.H{"type": "unauthorized", "message": "invalid username or password"}})
		return
	}

	// 设置 HttpOnly Cookie
	maxAge := int(session.ExpiresAt.Sub(time.Now()).Seconds())
	c.SetCookie("session_token", session.Token, maxAge, "/", "", false, true)

	c.JSON(200, gin.H{
		"token":      session.Token,
		"expires_at": session.ExpiresAt,
	})
}

// logout 登出
func (h *AdminAuthHandler) logout(c *gin.Context) {
	token := c.GetHeader("X-Admin-Token")
	if token == "" {
		cookie, _ := c.Cookie("session_token")
		token = cookie
	}

	if token != "" {
		h.am.Logout(token)
	}

	// 清除 Cookie
	c.SetCookie("session_token", "", -1, "/", "", false, true)
	c.JSON(200, gin.H{"message": "logged out"})
}

// changePassword 修改密码
func (h *AdminAuthHandler) changePassword(c *gin.Context) {
	user := c.MustGet("admin_user").(*dbpkg.AdminUser)

	var req struct {
		OldPassword string `json:"old_password" binding:"required"`
		NewPassword string `json:"new_password" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"type": "invalid_request", "message": err.Error()}})
		return
	}

	if err := h.am.ChangePassword(user.ID, req.OldPassword, req.NewPassword); err != nil {
		if errors.Is(err, adminauth.ErrInvalidCredentials) {
			c.JSON(401, gin.H{"error": gin.H{"type": "unauthorized", "message": "old password incorrect"}})
			return
		}
		c.JSON(500, gin.H{"error": gin.H{"type": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(200, gin.H{"message": "password changed"})
}

// getCurrentUser 获取当前登录用户
func (h *AdminAuthHandler) getCurrentUser(c *gin.Context) {
	user := c.MustGet("admin_user").(*dbpkg.AdminUser)
	c.JSON(200, gin.H{
		"id":              user.ID,
		"username":        user.Username,
		"role":            user.Role,
		"enabled":         user.Enabled,
		"last_login_at":   user.LastLoginAt,
		"created_at":      user.CreatedAt,
	})
}

// listUsers 列出所有用户（仅 root）
func (h *AdminAuthHandler) listUsers(c *gin.Context) {
	var users []dbpkg.AdminUser
	if err := h.am.DB().Order("created_at DESC").Find(&users).Error; err != nil {
		c.JSON(500, gin.H{"error": gin.H{"type": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(200, gin.H{"users": users})
}

// createUser 创建用户（仅 root）
func (h *AdminAuthHandler) createUser(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required,min=8"`
		Role     string `json:"role" binding:"required,oneof=root admin readonly"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"type": "invalid_request", "message": err.Error()}})
		return
	}

	user, err := h.am.CreateUser(req.Username, req.Password, req.Role)
	if err != nil {
		c.JSON(500, gin.H{"error": gin.H{"type": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(201, gin.H{"user": user})
}

// updateUser 更新用户（仅 root）
func (h *AdminAuthHandler) updateUser(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Role    *string `json:"role,omitempty"`
		Enabled *bool   `json:"enabled,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"type": "invalid_request", "message": err.Error()}})
		return
	}

	if err := h.am.UpdateUser(id, req.Role, req.Enabled); err != nil {
		c.JSON(500, gin.H{"error": gin.H{"type": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(200, gin.H{"message": "user updated"})
}

// deleteUser 删除用户（仅 root）
func (h *AdminAuthHandler) deleteUser(c *gin.Context) {
	id := c.Param("id")
	if err := h.am.DeleteUser(id); err != nil {
		c.JSON(500, gin.H{"error": gin.H{"type": "internal_error", "message": err.Error()}})
		return
	}

	c.JSON(200, gin.H{"message": "user deleted"})
}
