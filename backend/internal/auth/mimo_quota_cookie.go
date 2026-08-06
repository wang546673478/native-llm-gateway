// Package auth — MIMO 控制台 cookie 仓库(单行表,id 恒为 1)
// P-mimo-quota: cookie 是账号级凭据(约 1 天过期),持久化到 DB 让网关
// 重启不丢;运行时通过管理 API 热更新(POST /api/v1/providers/mimo/quota-cookie)。
package auth

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// MimoQuotaCookieStore MIMO 控制台 cookie 仓库(敏感凭据,Get 供内部注入,不外发)
type MimoQuotaCookieStore interface {
	// Get 返回当前持久化的 cookie;无记录返回 (nil, nil)
	Get(ctx context.Context) (*dbpkg.MimoQuotaCookie, error)
	// Upsert 写入单行(id=1)
	Upsert(ctx context.Context, cookie string) error
}

type gormMimoQuotaCookieStore struct{ db *gorm.DB }

// NewMimoQuotaCookieStore 构造 cookie 仓库
func NewMimoQuotaCookieStore(db *gorm.DB) MimoQuotaCookieStore {
	return &gormMimoQuotaCookieStore{db: db}
}

func (s *gormMimoQuotaCookieStore) Get(ctx context.Context) (*dbpkg.MimoQuotaCookie, error) {
	var row dbpkg.MimoQuotaCookie
	err := s.db.WithContext(ctx).First(&row, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *gormMimoQuotaCookieStore) Upsert(ctx context.Context, cookie string) error {
	row := &dbpkg.MimoQuotaCookie{
		ID:        1,
		Cookie:    cookie,
		UpdatedAt: time.Now().UTC(),
	}
	return s.db.WithContext(ctx).Save(row).Error // Save 单行 upsert(id=1 固定)
}
