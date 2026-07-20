// Package auth — DB Repository for Gateway Keys
// 让前端 CRUD 操作持久化到 SQLite/Postgres
package auth

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// KeyStore DB 仓库接口(便于 mock)
type KeyStore interface {
	List(ctx context.Context) ([]dbpkg.GatewayKey, error)
	GetByName(ctx context.Context, name string) (*dbpkg.GatewayKey, error)
	Create(ctx context.Context, k *dbpkg.GatewayKey) error
	Update(ctx context.Context, name string, k *dbpkg.GatewayKey) error
	Delete(ctx context.Context, name string) error
}

// gormKeyStore 基于 GORM 的实现
type gormKeyStore struct {
	db *gorm.DB
}

// NewKeyStore 构造 Repository
func NewKeyStore(db *gorm.DB) KeyStore {
	return &gormKeyStore{db: db}
}

func (s *gormKeyStore) List(ctx context.Context) ([]dbpkg.GatewayKey, error) {
	var out []dbpkg.GatewayKey
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormKeyStore) GetByName(ctx context.Context, name string) (*dbpkg.GatewayKey, error) {
	var k dbpkg.GatewayKey
	if err := s.db.WithContext(ctx).Where("name = ?", name).First(&k).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &k, nil
}

func (s *gormKeyStore) Create(ctx context.Context, k *dbpkg.GatewayKey) error {
	now := time.Now().UTC()
	k.CreatedAt = now
	k.UpdatedAt = now
	return s.db.WithContext(ctx).Create(k).Error
}

func (s *gormKeyStore) Update(ctx context.Context, name string, k *dbpkg.GatewayKey) error {
	k.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Model(&dbpkg.GatewayKey{}).
		Where("name = ?", name).
		Updates(map[string]interface{}{
			"key_hash":       k.KeyHash,
			"allowed_models": k.AllowedModels,
			"rpm":            k.RPM,
			"tpm":            k.TPM,
			"enabled":        k.Enabled,
			"updated_at":     k.UpdatedAt,
		}).Error
}

func (s *gormKeyStore) Delete(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Where("name = ?", name).Delete(&dbpkg.GatewayKey{}).Error
}
