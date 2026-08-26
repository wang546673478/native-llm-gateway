package adminauth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/wang546673478/native-llm-gateway/internal/database"
)

// EnsureRootUser 确保数据库中至少有一个 root 用户
// 如果不存在，创建默认的 root 用户
// 返回：是否创建了新用户、错误
func EnsureRootUser(db *gorm.DB) (bool, error) {
	// 检查是否已有 root 用户
	var count int64
	if err := db.Model(&database.AdminUser{}).Where("role = ?", "root").Count(&count).Error; err != nil {
		return false, fmt.Errorf("check root user: %w", err)
	}

	if count > 0 {
		// 已存在 root 用户，无需创建
		return false, nil
	}

	// 创建默认 root 用户
	// 默认用户名: admin
	// 默认密码: Gateway@2026
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("Gateway@2026"), bcrypt.DefaultCost)
	if err != nil {
		return false, fmt.Errorf("hash password: %w", err)
	}

	rootUser := &database.AdminUser{
		Username:       "admin",
		PasswordHash:   string(passwordHash),
		Role:           "root",
		Enabled:        true,
		FailedAttempts: 0,
		LockedUntil:    nil,
	}

	if err := db.Create(rootUser).Error; err != nil {
		return false, fmt.Errorf("create root user: %w", err)
	}

	return true, nil
}
