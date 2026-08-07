// sqlite2pg — SQLite → PostgreSQL 数据迁移 + 一致性校验工具
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
//  4. 迁移完成后默认自动校验两库一致性(-verify;也可 -verify-only 单独复核)
//
// 幂等:--clean 默认开启,搬数据前 TRUNCATE 目标表(重复跑不会叠加);
// --clean=false 可在已有数据的库上追加。
//
// 校验(-verify / -verify-only):
//   - 逐表:列集合一致性(防 schema 漂移)→ COUNT → 逐行逐字段对比 →
//     setval 抽查(last_value ≥ MAX(id))
//   - 两库都走 gorm + 同一组 Model struct 扫描(与迁移同一条读取路径);
//     时间截断到微秒 + 2µs 容差(sqlite 存纳秒 / PG timestamptz 微秒舍入),
//     浮点按 IEEE-754 双精度位级比较,其余类型严格相等
//   - 敏感字段(key_hash / cookie)差异只打码输出(长度 + 首尾字符),
//     绝不打印明文
//   - exit code:0 全等 / 1 有差异或错误 / 2 用法错误(flag 包默认)
//
// 注意:敏感数据(provider_api_keys.key_hash 等)直接在两库间搬移,
// 本工具应在可信主机上运行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

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
// 建的 FK 约束);其余表无相互依赖。校验与迁移共用这份清单(唯一维护点)。
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
	verify := flag.Bool("verify", true, "迁移完成后自动校验两库一致性")
	verifyOnly := flag.Bool("verify-only", false, "只校验不迁移(-verify-only 隐含 -verify)")
	maxDiffs := flag.Int("max-diffs", 20, "每表最多打印差异条数")
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

	if *verifyOnly {
		// 只读复核:绝不写库
		failed, err := verifyDatabase(ctx, src, dst, *maxDiffs)
		if err != nil {
			log.Fatalf("verify: %v", err)
		}
		if failed > 0 {
			os.Exit(1)
		}
		return
	}

	total, err := migrateDatabase(ctx, src, dst, *clean)
	if err != nil {
		log.Fatalf("迁移失败: %v", err)
	}
	log.Printf("迁移完成 — 共 %d 行。", total)

	if *verify {
		failed, err := verifyDatabase(ctx, src, dst, *maxDiffs)
		if err != nil {
			log.Fatalf("verify: %v", err)
		}
		if failed > 0 {
			os.Exit(1)
		}
	} else {
		log.Println("(-verify=false) 已跳过一致性校验")
	}
}

// migrateDatabase 建表 → 可选清空 → 逐表搬移 → setval 对齐
func migrateDatabase(ctx context.Context, src, dst *gorm.DB, clean bool) (int, error) {
	// 1. 目标建表(幂等)
	if err := database.Migrate(dst); err != nil {
		return 0, fmt.Errorf("PG migrate: %w", err)
	}

	// 2. 可选清空(TRUNCATE ... CASCADE 处理 FK;RESTART IDENTITY 重置自增)
	if clean {
		for _, sp := range specs {
			if err := dst.Exec(fmt.Sprintf("TRUNCATE %s RESTART IDENTITY CASCADE", sp.name)).Error; err != nil {
				return 0, fmt.Errorf("清空目标表 %s: %w", sp.name, err)
			}
		}
		log.Println("已清空目标表(--clean)")
	}

	// 3. 逐表搬移
	total := 0
	for _, sp := range specs {
		n, err := migrateTable(ctx, src, dst, sp)
		if err != nil {
			return total, fmt.Errorf("迁移 %s: %w", sp.name, err)
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
			return total, fmt.Errorf("setval %s: %w", sp.name, err)
		}
	}
	return total, nil
}

// migrateTable 把一张表从 SQLite 读到 struct 切片,再分批写入 PG
func migrateTable(ctx context.Context, src, dst *gorm.DB, sp tableSpec) (int, error) {
	// 用反射构造「该模型类型的空切片」— 让 gorm 按模型列映射读/写
	rows := newSlice(sp.model)
	if err := src.WithContext(ctx).Table(sp.name).Find(rows).Error; err != nil {
		return 0, fmt.Errorf("read sqlite: %w", err)
	}
	n := sliceLen(rows)
	if n == 0 {
		return 0, nil // 空表跳过写入(gorm Create 空切片报 empty slice found)
	}
	// 分批写入:PG 扩展协议参数上限 65535(usage_records 16 列 × 5673 行 ≈ 9 万
	// 参数会超限;500 行 × 16 列 = 8000 参数,安全)
	const batchSize = 500
	v := reflect.ValueOf(rows).Elem()
	for start := 0; start < v.Len(); start += batchSize {
		end := start + batchSize
		if end > v.Len() {
			end = v.Len()
		}
		part := v.Slice(start, end).Interface()
		if err := dst.WithContext(ctx).Table(sp.name).Create(part).Error; err != nil {
			return 0, fmt.Errorf("write pg (rows %d-%d): %w", start+1, end, err)
		}
	}
	return n, nil
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

// ---------------------------------------------------------------------------
// 一致性校验

// tableResult 单表校验结果
type tableResult struct {
	name      string
	srcCount  int64
	dstCount  int64
	diffTotal int // 差异计数(可能超过打印条数)
}

// verifyDatabase 对 specs 全表执行一致性校验,返回失败(有差异)的表数。
// 硬错误(列集合不一致 / COUNT 不等 / 读取失败 / setval 不足)直接返回 err。
func verifyDatabase(ctx context.Context, src, dst *gorm.DB, maxDiffs int) (int, error) {
	var totalRows, totalDiffs int64
	failed := 0
	for _, sp := range specs {
		res, err := verifyTable(ctx, src, dst, sp, maxDiffs)
		if err != nil {
			return failed, err
		}
		totalRows += res.srcCount
		totalDiffs += int64(res.diffTotal)
		if res.diffTotal > 0 {
			failed++
		}
	}
	if failed == 0 {
		fmt.Printf("VERIFY OK: %d/%d tables, total %d rows, 0 diffs\n",
			len(specs), len(specs), totalRows)
	} else {
		fmt.Printf("VERIFY FAIL: %d/%d tables failed, %d diffs (详情见上,每表最多 %d 条)\n",
			failed, len(specs), totalDiffs, maxDiffs)
	}
	return failed, nil
}

// verifyTable 单表校验:列集合 → COUNT → 逐行对比 → setval 抽查
func verifyTable(ctx context.Context, src, dst *gorm.DB, sp tableSpec, maxDiffs int) (*tableResult, error) {
	res := &tableResult{name: sp.name}
	fmt.Printf("=== verify %s ===\n", sp.name)

	// 1. 列集合一致性(防 schema 漂移 — 换旧二进制/改模型时避免静默错比)
	srcCols, err := sqliteColumns(ctx, src, sp.name)
	if err != nil {
		return nil, fmt.Errorf("read sqlite columns: %w", err)
	}
	dstCols, err := postgresColumns(ctx, dst, sp.name)
	if err != nil {
		return nil, fmt.Errorf("read pg columns: %w", err)
	}
	sort.Strings(srcCols)
	sort.Strings(dstCols)
	if !reflect.DeepEqual(srcCols, dstCols) {
		return nil, fmt.Errorf("%s 列集合不一致:src=%v dst=%v(检查迁移工具与网关是否同版本代码)",
			sp.name, srcCols, dstCols)
	}
	fmt.Printf("columns src=%d dst=%d  OK\n", len(srcCols), len(dstCols))

	// 2. COUNT
	if err := src.WithContext(ctx).Table(sp.name).Count(&res.srcCount).Error; err != nil {
		return nil, fmt.Errorf("count sqlite: %w", err)
	}
	if err := dst.WithContext(ctx).Table(sp.name).Count(&res.dstCount).Error; err != nil {
		return nil, fmt.Errorf("count pg: %w", err)
	}
	if res.srcCount != res.dstCount {
		// 行数不等不中止 — 继续逐行,让 id 对齐报出精确的 MISSING 行
		fmt.Printf("count  src=%d dst=%d  MISMATCH(继续逐行定位)\n", res.srcCount, res.dstCount)
	} else {
		fmt.Printf("count  src=%d dst=%d  OK\n", res.srcCount, res.dstCount)
	}

	// 3. 两边都空才直接过(单边有数据时逐行会报 MISSING)
	if res.srcCount == 0 && res.dstCount == 0 {
		fmt.Println("0 rows (empty, skip)")
		return res, nil
	}

	// 4. 逐行逐字段对比(两库同走 gorm struct 扫描,类型经 Go 值规范化)
	srcRows, err := readTableRows(ctx, src, sp)
	if err != nil {
		return nil, fmt.Errorf("read sqlite rows: %w", err)
	}
	dstRows, err := readTableRows(ctx, dst, sp)
	if err != nil {
		return nil, fmt.Errorf("read pg rows: %w", err)
	}
	_, colNames, types := tableFieldInfo(sp)

	printed := 0
	for i, id := range srcRows.ids {
		sv := srcRows.rows[i]
		dv, ok := dstRows.byID[id]
		if !ok {
			res.diffTotal++
			if printed < maxDiffs {
				fmt.Printf("MISSING %s id=%d  (dst 无此行)\n", sp.name, id)
				printed++
			}
			continue
		}
		for j := range sv {
			if sv[j] == dv[j] {
				continue
			}
			// 时间字段:微秒截断后仍可能差 1µs(PG 舍入),给 2µs 容差
			if isTimeType(types[j]) && !timeDiffers(sv[j], dv[j]) {
				continue
			}
			res.diffTotal++
			if printed < maxDiffs {
				if isSensitive(colNames[j]) {
					// 敏感凭据(key_hash/cookie)只打码输出,绝不打印明文
					fmt.Printf("DIFF %s id=%d field=%s src=%q dst=%q (masked)\n",
						sp.name, id, colNames[j], mask(sv[j]), mask(dv[j]))
				} else {
					fmt.Printf("DIFF %s id=%d field=%s src=%q dst=%q\n",
						sp.name, id, colNames[j], sv[j], dv[j])
				}
				printed++
			}
		}
	}
	for _, id := range dstRows.ids {
		if _, ok := srcRows.byID[id]; !ok {
			res.diffTotal++
			if printed < maxDiffs {
				fmt.Printf("MISSING %s id=%d  (src 无此行)\n", sp.name, id)
				printed++
			}
		}
	}
	fmt.Printf("rows   compared=%d diffs=%d  %s\n", res.srcCount, res.diffTotal,
		map[bool]string{true: "OK", false: "FAIL"}[res.diffTotal == 0])

	// 5. setval 抽查(非空表;无序列的表如 mimo_quota_cookie 自动跳过)
	maxID := srcRows.ids[len(srcRows.ids)-1] // 按 id 升序读,最后一个是 MAX
	ok, err := checkSetval(ctx, dst, sp.name, maxID)
	if err != nil {
		return nil, fmt.Errorf("check setval %s: %w", sp.name, err)
	}
	if !ok {
		return nil, fmt.Errorf("%s setval 未对齐:last_value < MAX(id)=%d", sp.name, maxID)
	}
	fmt.Println("setval last_value >= MAX(id)  OK")
	return res, nil
}

// tableRows 一张表按 id 升序读出的全量行(id → 规范化字段值)
type tableRows struct {
	ids  []uint64
	rows [][]string // rows[i] 对应 ids[i],字段序 = struct 字段序
	byID map[uint64][]string
}

// readTableRows 与迁移同路径:gorm + 模型 struct 全量读出,按 id 升序
func readTableRows(ctx context.Context, db *gorm.DB, sp tableSpec) (*tableRows, error) {
	rows := newSlice(sp.model)
	if err := db.WithContext(ctx).Table(sp.name).Order("id").Find(rows).Error; err != nil {
		return nil, err
	}
	s := reflect.ValueOf(rows).Elem()
	out := &tableRows{
		ids:  make([]uint64, 0, s.Len()),
		rows: make([][]string, 0, s.Len()),
		byID: make(map[uint64][]string, s.Len()),
	}
	for i := 0; i < s.Len(); i++ {
		row := s.Index(i)
		id := row.FieldByName("ID").Uint() // 所有模型都有 ID
		vals := make([]string, 0, row.NumField())
		for j := 0; j < row.NumField(); j++ {
			f := row.Field(j)
			if !f.CanInterface() {
				vals = append(vals, "<unexported>")
				continue
			}
			vals = append(vals, normalizeValue(f))
		}
		out.ids = append(out.ids, id)
		out.rows = append(out.rows, vals)
		out.byID[id] = vals
	}
	return out, nil
}

// tableFieldInfo 模型 struct 的字段名 / gorm 列名 / Go 类型(输出与类型分派用)
func tableFieldInfo(sp tableSpec) (names, colNames []string, types []reflect.Type) {
	t := reflect.TypeOf(sp.model).Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		names = append(names, f.Name)
		types = append(types, f.Type)
		colNames = append(colNames, columnName(f))
	}
	return
}

// columnName 从 gorm tag 提取列名;无 tag 时按 gorm 惯例转 snake_case
// (如 CreatedAt → created_at)
func columnName(f reflect.StructField) string {
	for _, part := range strings.Split(f.Tag.Get("gorm"), ";") {
		if strings.HasPrefix(part, "column:") {
			return strings.TrimPrefix(part, "column:")
		}
	}
	return toSnakeCase(f.Name)
}

// toSnakeCase CamelCase → snake_case(ID → id,ProviderKeyID → provider_key_id)
func toSnakeCase(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				prevLower := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
				nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if prevLower || nextLower {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeValue 单字段规范化 — 两库读出的 Go 值经同一规则序列化后字符串比较。
// 类型分派按 Go 反射类型,不按值内容解析(如 trace_id 长得像时间也不会被当时间比)。
func normalizeValue(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		return normalizeValue(v.Elem())
	}
	switch v.Kind() {
	case reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			// sqlite 驱动存纳秒 TEXT,PG timestamptz 微秒(舍入)— 都截断到微秒 + UTC
			return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
		return fmt.Sprintf("%v", v.Interface())
	case reflect.Float32, reflect.Float64:
		// IEEE-754 双精度,迁移 Create 原值,两侧位级相同;'g',-1 精确往返
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.String:
		return v.String()
	case reflect.Slice, reflect.Array, reflect.Map:
		return fmt.Sprintf("%v", v.Interface())
	default:
		return fmt.Sprintf("%v", v.Interface())
	}
}

var timeType = reflect.TypeOf(time.Time{})

func isTimeType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == timeType
}

// timeDiffers 两个已截断到微秒的 RFC3339Nano 是否真差异(PG 舍入至多 1µs,容差 2µs)
func timeDiffers(src, dst string) bool {
	t1, err1 := time.Parse(time.RFC3339Nano, src)
	t2, err2 := time.Parse(time.RFC3339Nano, dst)
	if err1 != nil || err2 != nil {
		return true // 解析失败按差异处理
	}
	d := t1.Sub(t2)
	if d < 0 {
		d = -d
	}
	return d > 2*time.Microsecond
}

// isSensitive 敏感字段(明文凭据):key_hash / cookie 列
func isSensitive(col string) bool {
	c := strings.ToLower(col)
	return strings.Contains(c, "key_hash") || strings.Contains(c, "cookie")
}

// mask 敏感值打码:长度 + 前 6 后 4 字符,绝不打印明文
func mask(v string) string {
	if len(v) <= 10 {
		return fmt.Sprintf("%d chars", len(v))
	}
	return fmt.Sprintf("%s…%s (len=%d)", v[:6], v[len(v)-4:], len(v))
}

// sqliteColumns PRAGMA table_info 取列名(表名来自 specs 白名单,安全拼接)
func sqliteColumns(ctx context.Context, db *gorm.DB, table string) ([]string, error) {
	type pragmaCol struct {
		Name string `gorm:"column:name"`
	}
	var cols []pragmaCol
	if err := db.WithContext(ctx).Raw(fmt.Sprintf("PRAGMA table_info(%s)", table)).Scan(&cols).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out, nil
}

// postgresColumns information_schema 取列名
func postgresColumns(ctx context.Context, db *gorm.DB, table string) ([]string, error) {
	type infoCol struct {
		Name string `gorm:"column:column_name"`
	}
	var cols []infoCol
	if err := db.WithContext(ctx).
		Raw(`SELECT column_name FROM information_schema.columns WHERE table_name = ?`, table).
		Scan(&cols).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out, nil
}

// checkSetval 抽查序列已推进到 ≥ MAX(id);无序列的表(如 mimo_quota_cookie)跳过
func checkSetval(ctx context.Context, db *gorm.DB, table string, maxID uint64) (bool, error) {
	var seq string
	if err := db.WithContext(ctx).
		Raw(`SELECT pg_get_serial_sequence(?, 'id')`, table).Scan(&seq).Error; err != nil {
		return false, err
	}
	if seq == "" {
		return true, nil
	}
	var last int64
	// seq 形如 public.<table>_id_seq(schema 限定),来自 pg_get_serial_sequence
	// 系统函数 — 不可加双引号(会把 "public.xxx" 整个当单个标识符)
	if err := db.WithContext(ctx).
		Raw(fmt.Sprintf(`SELECT last_value FROM %s`, seq)).Scan(&last).Error; err != nil {
		return false, err
	}
	return uint64(last) >= maxID, nil
}
