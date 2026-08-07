// sqlite2pg — SQLite → PostgreSQL 一次性数据迁移工具
//
// 用法(backend 模块内):
//
//	go run ./scripts/sqlite2pg -src /tmp/gateway-data/gateway.db \
//	  -dst "postgres://gateway:CHANGE_ME@localhost:5432/gateway"
//
// 逻辑:
//  1. 目标 PG 先 database.Migrate 建好全部表(含数据迁移函数,幂等)
//  2. 逐表从 SQLite 读(struct 模型 — 时间/数值类型由 gorm 驱动自动转换)
//  3. 批量写入 PG,保留原 id;完成后对每张表 setval 序列到 max(id),
//     避免后续自增主键冲突
//
// 幂等:--clean 默认开启,搬数据前 TRUNCATE 目标表(重复跑不会叠加);
// --clean=false 可在已有数据的库上追加。
//
// 注意:敏感数据(provider_api_keys.key_hash 等)直接在两库间搬移,
// 本工具应在可信主机上运行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"reflect"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wang546673478/native-llm-gateway/internal/database"
)

// tableSpec 一张表的迁移规格:表名 + gorm 模型(字段映射由 struct 标签驱动)
type tableSpec struct {
	name  string
	model any
}

// specs 迁移表清单 — 顺序敏感:providers 先于 provider_models(AutoMigrate
// 建的 FK 约束);其余表无相互依赖
var specs = []tableSpec{
	{"providers", &database.Provider{}},
	{"provider_models", &database.ProviderModel{}},
	{"model_aliases", &database.ModelAlias{}},
	{"provider_api_keys", &database.ProviderAPIKey{}},
	{"gateway_keys", &database.GatewayKey{}},
	{"routing_configs", &database.RoutingConfig{}},
	{"usage_records", &database.UsageRecord{}},
	{"access_logs", &database.AccessLog{}},
	{"mimo_quota_cookie", &database.MimoQuotaCookie{}},
}

func main() {
	srcDSN := flag.String("src", "/tmp/gateway-data/gateway.db", "SQLite 源库文件路径")
	dstDSN := flag.String("dst", "", "PostgreSQL 目标 DSN(必填,如 postgres://user:pass@host:5432/db)")
	clean := flag.Bool("clean", true, "搬移前清空目标表(默认开启,幂等)")
	flag.Parse()

	if *dstDSN == "" {
		log.Fatal("必须提供 -dst PostgreSQL DSN")
	}

	ctx := context.Background()
	silent := logger.Default.LogMode(logger.Silent)

	src, err := gorm.Open(sqlite.Open(*srcDSN), &gorm.Config{Logger: silent})
	if err != nil {
		log.Fatalf("打开 SQLite 源库 %s: %v", *srcDSN, err)
	}
	dst, err := gorm.Open(postgres.Open(*dstDSN), &gorm.Config{Logger: silent})
	if err != nil {
		log.Fatalf("连接 PostgreSQL: %v", err)
	}

	// 1. 目标建表(幂等)
	if err := database.Migrate(dst); err != nil {
		log.Fatalf("PG migrate: %v", err)
	}

	// 2. 可选清空(TRUNCATE ... CASCADE 处理 FK;RESTART IDENTITY 重置自增)
	if *clean {
		for _, sp := range specs {
			if err := dst.Exec(fmt.Sprintf("TRUNCATE %s RESTART IDENTITY CASCADE", sp.name)).Error; err != nil {
				log.Fatalf("清空目标表 %s: %v", sp.name, err)
			}
		}
		log.Println("已清空目标表(--clean)")
	}

	// 3. 逐表搬移
	total := 0
	for _, sp := range specs {
		n, err := migrateTable(ctx, src, dst, sp)
		if err != nil {
			log.Fatalf("迁移 %s: %v", sp.name, err)
		}
		total += n
		log.Printf("  %-18s %6d 行", sp.name, n)
	}

	// 4. 自增序列对齐(显式插入 id 后 PG 序列未前进,不 setval 会主键冲突)
	for _, sp := range specs {
		// 表名来自上方白名单,安全拼接
		q := fmt.Sprintf(
			"SELECT setval(pg_get_serial_sequence('%s','id'), GREATEST((SELECT COALESCE(MAX(id),1) FROM %s), 1))",
			sp.name, sp.name)
		if err := dst.Exec(q).Error; err != nil {
			log.Fatalf("setval %s: %v", sp.name, err)
		}
	}

	log.Printf("完成 — 共迁移 %d 行。现在可以把 config.yaml 的 database.driver 切到 postgres。", total)
}

// migrateTable 把一张表从 SQLite 读到 struct 切片,再批量写入 PG
func migrateTable(ctx context.Context, src, dst *gorm.DB, sp tableSpec) (int, error) {
	// 用反射构造「该模型类型的空切片」— 让 gorm 按模型列映射读/写
	rows := newSlice(sp.model)
	if err := src.WithContext(ctx).Table(sp.name).Find(rows).Error; err != nil {
		return 0, fmt.Errorf("read sqlite: %w", err)
	}
	if err := dst.WithContext(ctx).Table(sp.name).Create(rows).Error; err != nil {
		return 0, fmt.Errorf("write pg: %w", err)
	}
	return sliceLen(rows), nil
}

// newSlice 构造 model 对应类型的空切片(如 []database.GatewayKey)
func newSlice(model any) any {
	t := reflect.TypeOf(model) // *database.GatewayKey
	return reflect.New(reflect.SliceOf(t.Elem())).Interface()
}

// sliceLen 返回反射切片的长度
func sliceLen(s any) int {
	return reflect.ValueOf(s).Elem().Len()
}
