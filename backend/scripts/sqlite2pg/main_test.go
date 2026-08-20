// sqlite2pg 一致性校验单元测试
//
// 需要 PostgreSQL 的用例用 PG_TEST_DSN 门控(未设置 t.Skip,沿用仓库既有约定):
//
//	PG_TEST_DSN="postgres://gateway:密码@127.0.0.1:5432/gateway_test" go test ./scripts/sqlite2pg/
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wang546673478/native-llm-gateway/internal/database"
)

func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN 未设置,跳过需要 PostgreSQL 的校验用例")
	}
	return dsn
}

// openSQLite 临时文件库(单连接,避免 :memory: 多连接各自独立实例)
func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

// openPG 连接 PG_TEST_DSN 指向的测试库,清空全部 9 张表(独立测试库,不含生产数据)
func openPG(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgDSN(t)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	var names []string
	for _, sp := range specs {
		names = append(names, sp.name)
	}
	if err := db.Exec("TRUNCATE " + strings.Join(names, ", ") + " RESTART IDENTITY CASCADE").Error; err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

// seed 往两库写入同一份已知数据(纳秒时间 / float / 敏感字段 / 覆盖 5 张表,其余留空)
func seed(t *testing.T, src, dst *gorm.DB) {
	t.Helper()
	t0 := time.Date(2026, 8, 7, 10, 0, 0, 123456789, time.UTC) // 纳秒精度,测 PG 微秒舍入
	// CreatedAt/UpdatedAt 全部显式给值 — 否则两库 Create 时 gorm 各自填充 now(),
	// 毫秒级差异造成误报
	providers := []database.Provider{
		{Name: "deepseek", Protocol: "openai", Endpoint: "https://api.deepseek.com", Enabled: true, Timeout: 60, BillingSource: "api", CreatedAt: t0, UpdatedAt: t0},
		{Name: "minimax", Protocol: "openai", Endpoint: "https://api.minimax.chat", Enabled: true, Timeout: 60, BillingSource: "token_plan", CreatedAt: t0, UpdatedAt: t0},
	}
	models := []database.ProviderModel{
		{Vendor: "deepseek", ModelID: "deepseek-v4-flash", CostPerMillionInput: 0.42, CostPerMillionOutput: 1.68, CreatedAt: t0},
		{Vendor: "deepseek", ModelID: "deepseek-v4-pro", CostPerMillionInput: 0.5, CostPerMillionOutput: 2.0, CreatedAt: t0.Add(time.Second)},
	}
	keys := []database.GatewayKey{
		{Name: "k1", KeyHash: "sk-abcdef1234567890", Providers: "[]", AllowedModels: `["*"]`, RPM: 100, TPM: 500000, Enabled: true, CreatedAt: t0, UpdatedAt: t0},
		{Name: "k2", KeyHash: "sk-9999999999999999", Providers: "[]", AllowedModels: `["*"]`, RPM: 10, TPM: 1000, Enabled: false, CreatedAt: t0, UpdatedAt: t0},
	}
	cookie := []database.MimoQuotaCookie{{ID: 1, Cookie: "mimo-session-cookie-very-secret", UpdatedAt: t0}}

	for _, db := range []*gorm.DB{src, dst} {
		// 逐条 Create:批量 Create 会预分配 id(查 max(id)),PG 序列不推进,
		// 导致 setval 抽查误报;单条 Create 走 nextval,贴近真实写入路径
		for _, p := range providers {
			if err := db.Create(&p).Error; err != nil {
				t.Fatalf("seed provider %s: %v", p.Name, err)
			}
		}
		for _, m := range models {
			if err := db.Create(&m).Error; err != nil {
				t.Fatalf("seed model %s: %v", m.ModelID, err)
			}
		}
		for _, k := range keys {
			if err := db.Create(&k).Error; err != nil {
				t.Fatalf("seed key %s: %v", k.Name, err)
			}
		}
		for _, c := range cookie {
			if err := db.Create(&c).Error; err != nil {
				t.Fatalf("seed cookie: %v", err)
			}
		}
	}
}

// captureStdout 捕获 f 执行期间的 stdout(verifyDatabase 用 fmt.Printf 输出)
func captureStdout(f func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out)
}

func TestVerifyIdentical(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	var failed int
	var err error
	out := captureStdout(func() {
		failed, err = verifyDatabase(ctx, src, dst, 20)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != 0 {
		t.Fatalf("expected 0 failed tables, got %d\n%s", failed, out)
	}
	// 纳秒时间跨 PG 微秒舍入(≤1µs)不得误报 — seed 的 t0 已覆盖该路径
	if !strings.Contains(out, "VERIFY OK: 9/9 tables") {
		t.Fatalf("expected VERIFY OK summary, got:\n%s", out)
	}
}

func TestVerifyDetectsDiff(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	// 改 dst 一把 key 的 RPM(仅非敏感字段)
	if err := dst.Model(&database.GatewayKey{}).Where("id = ?", 1).UpdateColumn("rpm", 999).Error; err != nil {
		t.Fatalf("mutate: %v", err)
	}

	var failed int
	var err error
	out := captureStdout(func() {
		failed, err = verifyDatabase(ctx, src, dst, 20)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != 1 {
		t.Fatalf("expected 1 failed table, got %d", failed)
	}
	if !strings.Contains(out, `DIFF gateway_keys id=1 field=rpm src="100" dst="999"`) {
		t.Fatalf("expected precise DIFF line, got:\n%s", out)
	}
	if !strings.Contains(out, "VERIFY FAIL: 1/9 tables failed, 1 diffs") {
		t.Fatalf("expected VERIFY FAIL summary, got:\n%s", out)
	}
}

func TestVerifyTimeDiff(t *testing.T) {
	// 真实时间差异(1s)必须报出;微秒舍入(seed 的 t0)不误报
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	t1 := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	if err := dst.Model(&database.ProviderModel{}).Where("id = ?", 1).UpdateColumn("created_at", t1).Error; err != nil {
		t.Fatalf("mutate: %v", err)
	}

	var failed int
	var err error
	out := captureStdout(func() {
		failed, err = verifyDatabase(ctx, src, dst, 20)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != 1 {
		t.Fatalf("expected 1 failed table, got %d", failed)
	}
	if !strings.Contains(out, "field=created_at") {
		t.Fatalf("expected created_at DIFF, got:\n%s", out)
	}
}

func TestVerifyMissingRow(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	if err := dst.Where("id = ?", 2).Delete(&database.GatewayKey{}).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}

	var failed int
	var err error
	out := captureStdout(func() {
		failed, err = verifyDatabase(ctx, src, dst, 20)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != 1 {
		t.Fatalf("expected 1 failed table, got %d", failed)
	}
	if !strings.Contains(out, "MISSING gateway_keys id=2  (dst 无此行)") {
		t.Fatalf("expected MISSING line, got:\n%s", out)
	}
}

func TestVerifySensitiveMasked(t *testing.T) {
	// 敏感字段(key_hash)差异必须打码:输出含 masked,绝不出现明文
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	if err := dst.Model(&database.GatewayKey{}).Where("id = ?", 1).UpdateColumn("key_hash", "sk-LEAKED-LEAKED-LEAKED").Error; err != nil {
		t.Fatalf("mutate: %v", err)
	}

	var failed int
	var err error
	out := captureStdout(func() {
		failed, err = verifyDatabase(ctx, src, dst, 20)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != 1 {
		t.Fatalf("expected 1 failed table, got %d", failed)
	}
	if !strings.Contains(out, "field=key_hash") || !strings.Contains(out, "(masked)") {
		t.Fatalf("expected masked DIFF for key_hash, got:\n%s", out)
	}
	for _, secret := range []string{"sk-abcdef1234567890", "sk-9999999999999999", "sk-LEAKED-LEAKED-LEAKED"} {
		if strings.Contains(out, secret) {
			t.Fatalf("sensitive value leaked in output: %s\n%s", secret, out)
		}
	}
}

func TestVerifyColumnMismatch(t *testing.T) {
	// schema 漂移必须硬失败(列集合不一致),而不是静默错比
	ctx := context.Background()
	src := openSQLite(t)
	dst := openPG(t)
	seed(t, src, dst)

	if err := dst.Exec("ALTER TABLE gateway_keys ADD COLUMN extra_col int").Error; err != nil {
		t.Fatalf("alter: %v", err)
	}
	defer func() {
		_ = dst.Exec("ALTER TABLE gateway_keys DROP COLUMN extra_col") // 清理,保证用例独立
	}()

	_, err := verifyDatabase(ctx, src, dst, 20)
	if err == nil {
		t.Fatal("expected hard error on column mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "列集合不一致") {
		t.Fatalf("expected 列集合不一致 error, got: %v", err)
	}
}
