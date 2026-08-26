// Package adminauth — 管理员鉴权(登录/session/防暴力破解)
package adminauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrAccountLocked      = errors.New("account locked due to too many failed attempts")
	ErrAccountDisabled    = errors.New("account disabled")
	ErrInvalidSession     = errors.New("invalid or expired session")
)

// Config 管理器配置
type Config struct {
	MaxFailedAttempts int           // 最大失败次数,默认 5
	LockDuration      time.Duration // 锁定时长,默认 15 分钟
	SessionTTL        time.Duration // session 过期时间,默认 24 小时
}

// DefaultConfig 默认配置
func DefaultConfig() Config {
	return Config{
		MaxFailedAttempts: 5,
		LockDuration:      15 * time.Minute,
		SessionTTL:        24 * time.Hour,
	}
}

// Manager 管理员鉴权管理器
type Manager struct {
	db  *gorm.DB
	cfg Config
}

// NewManager 创建管理器
func NewManager(db *gorm.DB, cfg Config) *Manager {
	return &Manager{db: db, cfg: cfg}
}

// Login 登录
func (m *Manager) Login(username, password, ip, userAgent string) (*dbpkg.AdminSession, error) {
	var user dbpkg.AdminUser
	if err := m.db.Where("username = ?", username).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	// 检查账号是否启用
	if !user.Enabled {
		return nil, ErrAccountDisabled
	}

	// 检查是否被锁定
	if user.LockedUntil != nil && user.LockedUntil.After(time.Now()) {
		return nil, ErrAccountLocked
	}

	// 验证密码
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		// 密码错误,增加失败次数
		return m.handleFailedLogin(&user)
	}

	// 密码正确,清零失败次数,生成 session
	return m.handleSuccessfulLogin(&user, ip, userAgent)
}

// handleFailedLogin 处理登录失败
func (m *Manager) handleFailedLogin(user *dbpkg.AdminUser) (*dbpkg.AdminSession, error) {
	user.FailedAttempts++
	if user.FailedAttempts >= m.cfg.MaxFailedAttempts {
		lockUntil := time.Now().Add(m.cfg.LockDuration)
		user.LockedUntil = &lockUntil
	}
	if err := m.db.Save(user).Error; err != nil {
		return nil, err
	}
	if user.LockedUntil != nil {
		return nil, ErrAccountLocked
	}
	return nil, ErrInvalidCredentials
}

// handleSuccessfulLogin 处理登录成功
func (m *Manager) handleSuccessfulLogin(user *dbpkg.AdminUser, ip, userAgent string) (*dbpkg.AdminSession, error) {
	// 清零失败次数和锁定状态
	now := time.Now()
	user.FailedAttempts = 0
	user.LockedUntil = nil
	user.LastLoginAt = &now
	if err := m.db.Save(user).Error; err != nil {
		return nil, err
	}

	// 生成 session token
	token, err := generateToken()
	if err != nil {
		return nil, err
	}

	session := &dbpkg.AdminSession{
		UserID:    user.ID,
		Token:     token,
		IPAddress: ip,
		UserAgent: userAgent,
		ExpiresAt: time.Now().Add(m.cfg.SessionTTL),
	}
	if err := m.db.Create(session).Error; err != nil {
		return nil, err
	}

	return session, nil
}

// ValidateSession 验证 session
func (m *Manager) ValidateSession(token string) (*dbpkg.AdminUser, error) {
	var session dbpkg.AdminSession
	if err := m.db.Where("token = ? AND expires_at > ?", token, time.Now()).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidSession
		}
		return nil, err
	}

	var user dbpkg.AdminUser
	if err := m.db.First(&user, session.UserID).Error; err != nil {
		return nil, err
	}

	if !user.Enabled {
		return nil, ErrAccountDisabled
	}

	return &user, nil
}

// Logout 登出(删除 session)
func (m *Manager) Logout(token string) error {
	return m.db.Where("token = ?", token).Delete(&dbpkg.AdminSession{}).Error
}

// ChangePassword 修改密码
func (m *Manager) ChangePassword(userID uint, oldPassword, newPassword string) error {
	var user dbpkg.AdminUser
	if err := m.db.First(&user, userID).Error; err != nil {
		return err
	}

	// 验证旧密码
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPassword)); err != nil {
		return ErrInvalidCredentials
	}

	// 生成新密码 hash
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	user.PasswordHash = string(hash)
	return m.db.Save(&user).Error
}

// CleanExpiredSessions 定时清理过期 session
func (m *Manager) CleanExpiredSessions(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.db.Where("expires_at < ?", time.Now()).Delete(&dbpkg.AdminSession{})
		case <-ctx.Done():
			return
		}
	}
}

// DB 返回底层 DB 连接(供 handler 查询)
func (m *Manager) DB() *gorm.DB {
	return m.db
}

// CreateUser 创建用户(仅 root 可调用,handler 层已做权限检查)
func (m *Manager) CreateUser(username, password, role string) (*dbpkg.AdminUser, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &dbpkg.AdminUser{
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
		Enabled:      true,
	}
	if err := m.db.Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

// UpdateUser 更新用户(仅 root 可调用)
func (m *Manager) UpdateUser(id string, role *string, enabled *bool) error {
	updates := make(map[string]interface{})
	if role != nil {
		updates["role"] = *role
	}
	if enabled != nil {
		updates["enabled"] = *enabled
	}
	if len(updates) == 0 {
		return nil
	}
	return m.db.Model(&dbpkg.AdminUser{}).Where("id = ?", id).Updates(updates).Error
}

// DeleteUser 删除用户(仅 root 可调用)
func (m *Manager) DeleteUser(id string) error {
	return m.db.Transaction(func(tx *gorm.DB) error {
		// 删除用户的所有 session
		if err := tx.Where("user_id = ?", id).Delete(&dbpkg.AdminSession{}).Error; err != nil {
			return err
		}
		// 删除用户
		return tx.Delete(&dbpkg.AdminUser{}, id).Error
	})
}

// generateToken 生成 session token (32 字节随机数 hex 编码 = 64 字符)
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
