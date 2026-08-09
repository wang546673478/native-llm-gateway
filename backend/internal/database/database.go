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
		Logger:  logger.Default.LogMode(logger.Warn),
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
func Migrate(db *gorm.DB) error {
	tables := []interface{}{
		&Provider{},
		&ProviderModel{},
		&ModelAlias{},
		&ProviderAPIKey{}, // P30: 替换 APIKey 表
		&UsageRecord{},
		&RoutingConfig{},
		&GatewayKey{},
		&AccessLog{},       // P67: 接入日志
		&MimoQuotaCookie{}, // P-mimo-quota: MIMO 控制台 cookie(单行)
		&RouteOrder{},      // P-2026-08-10: Level 2/3 排序改写
	}
	if err := db.AutoMigrate(tables...); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	// P27: 一次性数据迁移 — 把旧单值 provider 字段复制到新 JSON 数组字段
	if err := migrateProviderToProviders(db); err != nil {
		return fmt.Errorf("data migrate: %w", err)
	}
	// P-provider-vendor: 协议变体并入厂商名,并标协议
	if err := migrateProviderVendorKeys(db); err != nil {
		return fmt.Errorf("vendor key migrate: %w", err)
	}
	return nil
}

// migrateProviderVendorKeys P-provider-vendor: 把协议变体注册名下的 key 并入厂商名,并标协议
//   - deepseek-anthropic 行 → provider_name='deepseek', protocols='anthropic'
//   - minimax-openai 行     → provider_name='minimax',  protocols='openai'
//
// 主条目(deepseek/minimax)的 key 不标协议 — 保持空 = 全部协议(物理上同一 key 两端点通用,
// 且避免每次重启把用户新加的全协议 key 覆盖成单协议 — 用户 2026-08-04 裁决)。
// 幂等:变体注册名迁移后不再存在,重复执行影响 0 行,无副作用。
func migrateProviderVendorKeys(db *gorm.DB) error {
	stmts := []string{
		`UPDATE provider_api_keys SET provider_name='deepseek', protocols='anthropic' WHERE provider_name='deepseek-anthropic'`,
		`UPDATE provider_api_keys SET provider_name='minimax', protocols='openai' WHERE provider_name='minimax-openai'`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}

// migrateProviderToProviders 把旧 schema 里的 provider 单值(如果有的话)
// 迁移到新 schema 的 providers JSON 数组。
// 用 IFNULL 处理新列(可能为 NULL 或 '[]');老列不存在则 COALESCE 返回 ”。
// 幂等:重复跑不会出错,因为已经迁移过的行 providers != '[]'。
func migrateProviderToProviders(db *gorm.DB) error {
	// 检查老列是否存在
	var hasOldColumn int64
	rows, err := db.Raw(`
		SELECT COUNT(*) FROM pragma_table_info('gateway_keys')
		WHERE name = 'provider'
	`).Rows()
	if err != nil {
		// 非 SQLite 或没 pragma_table_info 函数(SQLite specific)
		// 跳过,假设无老列
		_ = rows
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&hasOldColumn); err != nil {
			return err
		}
	}
	if hasOldColumn == 0 {
		return nil // 无老列,不需要迁移
	}
	// 迁移:把老 provider 值包成 JSON 数组,赋给新 providers 列
	return db.Exec(`
		UPDATE gateway_keys
		SET providers = '["' || COALESCE(provider, '') || '"]'
		WHERE (providers IS NULL OR providers = '' OR providers = '[]')
		  AND COALESCE(provider, '') != ''
	`).Error
}

// ensureDir 对 sqlite 文件路径,确保父目录存在
func ensureDir(dsn string) error {
	dir := filepath.Dir(dsn)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
