// Package database — 连接初始化
package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite" // 纯 Go 的 SQLite 驱动,避免 CGO
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wang546673478/native-llm-gateway/internal/config"
)

// Open 根据 config 打开数据库连接
// 支持 sqlite (默认) 和 postgres
func Open(cfg *config.DatabaseConfig) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		NowFunc: func() time.Time { return time.Now().UTC() },
	}

	var (
		db  *gorm.DB
		err error
	)

	switch cfg.Driver {
	case "sqlite":
		if err := ensureDir(cfg.DSN); err != nil {
			return nil, fmt.Errorf("ensure db dir: %w", err)
		}
		// WAL 模式 + busy_timeout 让 SQLite 并发更稳
		dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", cfg.DSN)
		db, err = gorm.Open(sqlite.Open(dsn), gormCfg)
	case "postgres":
		db, err = gorm.Open(postgres.Open(cfg.DSN), gormCfg)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	// 健康检查
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return db, nil
}

// Migrate 用 GORM AutoMigrate 创建/更新所有表
// 与 migrations/*.up.sql 保持一致,SQL 文件保留作为规格引用
func Migrate(db *gorm.DB) error {
	tables := []interface{}{
		&Provider{},
		&ProviderModel{},
		&ModelAlias{},
		&APIKey{},
		&UsageRecord{},
		&RoutingConfig{},
		&GatewayKey{},
	}
	if err := db.AutoMigrate(tables...); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	return nil
}

// ensureDir 对 sqlite 文件路径,确保父目录存在
func ensureDir(dsn string) error {
	dir := filepath.Dir(dsn)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
