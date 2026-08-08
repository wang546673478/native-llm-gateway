// PG 集成测试(env-gated):PostgreSQL 驱动兼容性验证。
//
// 运行:PG_TEST_DSN=postgres://user:pass@host:5432/dbname go test ./internal/database/ -run TestPostgres
// 未设置 PG_TEST_DSN 时跳过(本机默认跳过;CI 用 postgres service 设置后自动执行)。
//
// 覆盖当前代码里所有跨驱动敏感的 SQL:
//   - Open(postgres driver) + Migrate 全表 AutoMigrate
//   - migrateProviderToProviders 的 pragma_table_info(SQLite 专属)在 PG 下走「跳过」分支而非报错
//   - migrateProviderVendorKeys 幂等重跑
//   - mimo_quota_cookie 单行 Save upsert(PG 的 ON CONFLICT 路径)
//   - accesslog:CAST(id AS TEXT) 子查询 + fillKeyNames/fillGatewayKeyNames 名字现查
//   - DeleteOlderThan 参数化删除
package database_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
)

// TestPostgresIntegration 全链路 PG 兼容性验证(见文件头注释)
func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skip postgres integration test")
	}
	ctx := context.Background()

	// 幂等重建 schema(本地重跑安全;CI 每次全新库也一样)
	db, err := database.Open(&config.DatabaseConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;").Error; err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	// 1. Migrate:全表 AutoMigrate + 两个数据迁移函数。
	//    migrateProviderToProviders 的 pragma_table_info 在 PG 报错 → 必须走跳过分支
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate on postgres: %v", err)
	}

	// 2. Migrate 幂等重跑(Migrate 内部含 migrateProviderVendorKeys,重复执行影响 0 行)
	if err := database.Migrate(db); err != nil {
		t.Fatalf("vendor key migrate re-run: %v", err)
	}

	// 3. mimo_quota_cookie 单行 upsert(id=1 固定,第二次 Save 必须覆盖)
	if err := db.Save(&database.MimoQuotaCookie{ID: 1, Cookie: "first", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	if err := db.Save(&database.MimoQuotaCookie{ID: 1, Cookie: "second", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	var cookie database.MimoQuotaCookie
	if err := db.First(&cookie, 1).Error; err != nil {
		t.Fatalf("read cookie: %v", err)
	}
	if cookie.Cookie != "second" {
		t.Errorf("upsert cookie = %q, want %q(第二次 Save 必须覆盖第一行)", cookie.Cookie, "second")
	}

	// 4. accesslog:CAST(id AS TEXT) IN 子查询 + 名字现查 + retention 删除
	gw := &database.GatewayKey{Name: "pg-gw-key", KeyHash: "hash-gw"}
	if err := db.Create(gw).Error; err != nil {
		t.Fatalf("seed gateway key: %v", err)
	}
	pk := &database.ProviderAPIKey{ProviderName: "deepseek", Name: "pg-provider-key", KeyHash: "hash-pk"}
	if err := db.Create(pk).Error; err != nil {
		t.Fatalf("seed provider key: %v", err)
	}
	now := time.Now().UTC()
	store := accesslog.NewStore(db)
	entry := &accesslog.AccessEntry{
		TraceID:       "pg-trace-1",
		CreatedAt:     now,
		GatewayKeyID:  "1",
		ProviderKeyID: "1",
		Method:        "POST",
		Path:          "/v1/messages",
		StatusCode:    200,
		ErrorType:     "ok",
		LatencyMs:     12,
	}
	if err := store.Insert(ctx, entry); err != nil {
		t.Fatalf("insert access log on postgres: %v", err)
	}

	// List:名字必须由 fill* 现查填充(CAST(id AS TEXT) IN 在 PG 下工作)
	out, err := store.List(ctx, accesslog.QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list access logs on postgres: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("list len = %d, want 1", len(out))
	}
	if out[0].GatewayKeyName != "pg-gw-key" {
		t.Errorf("GatewayKeyName = %q, want %q(PG 的 CAST(id AS TEXT) 子查询/现查失败)", out[0].GatewayKeyName, "pg-gw-key")
	}
	if out[0].ProviderKeyName != "pg-provider-key" {
		t.Errorf("ProviderKeyName = %q, want %q", out[0].ProviderKeyName, "pg-provider-key")
	}

	// 按名字过滤走子查询
	byName, err := store.List(ctx, accesslog.QueryFilter{GatewayKey: "pg-gw-key", Limit: 10})
	if err != nil {
		t.Fatalf("list by gateway key name: %v", err)
	}
	if len(byName) != 1 {
		t.Errorf("filter by name len = %d, want 1", len(byName))
	}

	// retention 删除:参数化 created_at < cutoff
	if n, err := store.DeleteOlderThan(ctx, now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("delete older than (future cutoff) = %d, %v; want 0, nil", n, err)
	}
	if n, err := store.DeleteOlderThan(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("delete older than (past cutoff) = %d, %v; want 1, nil", n, err)
	}

	// 5. usage CreateInBatches(PG 批量插入)
	usages := make([]database.UsageRecord, 0, 3)
	for i := 0; i < 3; i++ {
		usages = append(usages, database.UsageRecord{
			TraceID:       "pg-usage-trace",
			ProviderName:  "deepseek",
			ModelID:       "test-model",
			Protocol:      "openai",
			InputTokens:   10,
			OutputTokens:  20,
			TotalTokens:   30,
			Cost:          0.001,
			BillingSource: "api",
		})
	}
	if err := db.CreateInBatches(usages, 2).Error; err != nil {
		t.Fatalf("create in batches on postgres: %v", err)
	}
	var usageCount int64
	if err := db.Model(&database.UsageRecord{}).Count(&usageCount).Error; err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if usageCount != 3 {
		t.Errorf("usage count = %d, want 3", usageCount)
	}
}
