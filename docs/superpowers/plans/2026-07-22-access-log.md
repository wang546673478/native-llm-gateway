# Access Logs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-request access log (metadata in DB + body in JSONL files + 24h retention) so admins can inspect every Claude Code (or any client) request from the Web UI without SSH-ing to the server.

**Architecture:** New `internal/accesslog` package owns storage (DB + files) and lifecycle. `proxy.handle` adds a `defer`-backed hook that builds an `AccessEntry`, asks the recorder to write the request body synchronously, then `RecordAsync` to flush metadata to DB without blocking the hot path. A retention goroutine removes >24h records and their files every 5 minutes. A new `AccessLogs.vue` page lists, filters, and shows full request/response in a drawer.

**Tech Stack:** Go 1.23, GORM + SQLite (existing), zap, Naive UI 2.44.1 + Vue 3 + TypeScript, Naive UI drawer.

## Global Constraints

- 主路径零阻塞 — `RecordAsync` 必须 `select { case ... default }`,绝不阻塞 proxy
- Body 文件 24h 自动清理(磁盘不增长)
- DB 表 schema 见 spec §1.1(21 字段),所有索引保留
- Go 1.23 风格 — `time.Now().UTC()`, `slices.Contains`, 不引入新依赖
- Frontend — 复用 Usage.vue 的 n-data-table 后端分页模式(不发明新控件)
- 全部 commit 用 `feat(scope):` / `fix(scope):` 前缀并 push 到 origin/main
- YAGNI:不要添加实时 SSE 推送、手动 cleanup 按钮、跨实例同步

---

## File Inventory

| File | Status | Responsibility |
|---|---|---|
| `backend/internal/database/models.go` | Modify | + `AccessLog` GORM struct |
| `backend/internal/database/database.go` | Modify | Register `AccessLog` in `Migrate` |
| `backend/internal/accesslog/entry.go` | Create | `AccessEntry` struct + JSON marshaling |
| `backend/internal/accesslog/bodyfile.go` | Create | Body file read/write + path builder |
| `backend/internal/accesslog/store.go` | Create | DB List/Count/Get |
| `backend/internal/accesslog/buffer.go` | Create | In-memory buffer + flush worker |
| `backend/internal/accesslog/recorder.go` | Create | Public API: `NewRecorder`, `RecordAsync`, `Close` |
| `backend/internal/accesslog/retention.go` | Create | 24h cleanup goroutine |
| `backend/internal/accesslog/bodyfile_test.go` | Create | Unit tests for body file |
| `backend/internal/accesslog/store_test.go` | Create | Unit tests for store (in-memory sqlite) |
| `backend/internal/accesslog/buffer_test.go` | Create | Unit tests for buffer |
| `backend/internal/config/config.go` | Modify | + `AccessLog` config |
| `backend/internal/server/server.go` | Modify | Construct recorder, inject, close |
| `backend/internal/proxy/proxy.go` | Modify | Build entry in `handle()`, defer RecordAsync |
| `backend/internal/auth/authenticator.go` | Modify | `KeyNameFromCtx(ctx)` helper |
| `backend/internal/api/http/handler/admin.go` | Modify | + 3 access_logs endpoints |
| `config.example.yaml` | Modify | + `server.access_log` block |
| `frontend/src/api/client.ts` | Modify | + `accessLogs` API |
| `frontend/src/views/AccessLogs.vue` | Create | Page + drawer |
| `frontend/src/router/index.ts` (or similar) | Modify | + `/access-logs` route |

**Module boundaries:**
- `accesslog` is self-contained. The rest of the codebase depends only on `Recorder.RecordAsync(*AccessEntry)`.
- `bodyfile.go` and `store.go` are package-private utilities — never exported beyond `recorder.go`.

---

## Task 1: Add `AccessLog` GORM model

**Files:**
- Modify: `backend/internal/database/models.go:1-145`
- Modify: `backend/internal/database/database.go:75-93`

**Step 1.1: Add `AccessLog` struct to models.go**

Open `backend/internal/database/models.go` and append after `GatewayKey`:

```go
// AccessLog 每次客户端请求的接入日志(P67: 新增 — 给管理员调试用)
//
// 设计要点:
//   - 只存 metadata;body 落地文件(.jsonl 滚动),DB 不存以防 SQLite 单行过大
//   - trace_id 与 X-Request-Id 一致,跨 usage_records / access_logs 可 join
//   - gateway_key_name 是冗余字段,UI 不用 join 也能展示;auth 关掉时为空
//   - body_path 是相对 body_dir 的路径,不存绝对路径(避免重启后指向失效位置)
type AccessLog struct {
	ID               uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraceID          string    `gorm:"column:trace_id;index;not null" json:"trace_id"`
	CreatedAt        time.Time `gorm:"index;column:created_at" json:"created_at"`
	GatewayKeyID     string    `gorm:"column:gateway_key_id;index" json:"gateway_key_id"`
	GatewayKeyName   string    `gorm:"column:gateway_key_name" json:"gateway_key_name"`
	Method           string    `gorm:"column:method" json:"method"`
	Path             string    `gorm:"column:path" json:"path"`
	ClientIP         string    `gorm:"column:client_ip" json:"client_ip"`
	UserAgent        string    `gorm:"column:user_agent" json:"user_agent"`
	RequestedModel   string    `gorm:"column:requested_model;index" json:"requested_model"`
	FinalModel       string    `gorm:"column:final_model;index" json:"final_model"`
	ProviderName     string    `gorm:"column:provider_name;index" json:"provider_name"`
	Protocol         string    `gorm:"column:protocol" json:"protocol"`
	IsStream         bool      `gorm:"column:is_stream" json:"is_stream"`
	StatusCode       int       `gorm:"column:status_code;index" json:"status_code"`
	ErrorType        string    `gorm:"column:error_type;index" json:"error_type"`
	LatencyMs        int       `gorm:"column:latency_ms" json:"latency_ms"`
	ReqBodyPath      string    `gorm:"column:req_body_path" json:"req_body_path"`
	ReqBodySize      int       `gorm:"column:req_body_size" json:"req_body_size"`
	RespBodyPath     string    `gorm:"column:resp_body_path" json:"resp_body_path"`
	RespBodySize     int       `gorm:"column:resp_body_size" json:"resp_body_size"`
	// 注意:truncated 信息放在文件后缀 `.truncated.json`,不存 DB 列(spec §1.1 锁定 21 字段)
}

// TableName
func (AccessLog) TableName() string { return "access_logs" }
```

**Step 1.2: Register in Migrate**

Open `backend/internal/database/database.go`, change `Migrate` to:

```go
func Migrate(db *gorm.DB) error {
	tables := []interface{}{
		&Provider{},
		&ProviderModel{},
		&ModelAlias{},
		&ProviderAPIKey{},
		&UsageRecord{},
		&RoutingConfig{},
		&GatewayKey{},
		&AccessLog{}, // P67: 接入日志
	}
	if err := db.AutoMigrate(tables...); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	if err := migrateProviderToProviders(db); err != nil {
		return fmt.Errorf("data migrate: %w", err)
	}
	return nil
}
```

**Step 1.3: Verify build**

Run: `cd backend && go build ./...`
Expected: compiles without errors.

**Step 1.4: Manual verify table created**

Run the server briefly (it uses AutoMigrate on startup):

```bash
cd backend && go build -o /tmp/gw-test .
sqlite3 data/gateway.db ".schema access_logs"
rm /tmp/gw-test
```

Expected: `sqlite3` shows columns including `trace_id`, `gateway_key_id`, `req_body_path`, etc.

**Step 1.5: Commit**

```bash
git add backend/internal/database/models.go backend/internal/database/database.go
git commit -m "feat(database): add AccessLog GORM model for access logs"
```

---

## Task 2: Body file read/write package

**Files:**
- Create: `backend/internal/accesslog/bodyfile.go`
- Create: `backend/internal/accesslog/bodyfile_test.go`

**Step 2.1: Write failing test**

Create `backend/internal/accesslog/bodyfile_test.go`:

```go
package accesslog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBodyFile_WriteAndRead(t *testing.T) {
	dir := t.TempDir()
	bw, err := NewBodyFileWriter(dir)
	if err != nil {
		t.Fatalf("NewBodyFileWriter: %v", err)
	}
	defer bw.Close()

	traceID := "test-trace-1"
	data := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`)

	path, err := bw.Write(traceID, "req", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 路径是相对路径,格式 YYYY-MM-DD/{traceID}-req.json
	wantPrefix := filepath.Join(bw.today(), traceID) + "-req"
	if !filepath.HasPrefix(path, wantPrefix) {
		t.Errorf("path = %q, want prefix %q", path, wantPrefix)
	}

	// 文件存在
	full := filepath.Join(dir, path)
	if _, err := os.Stat(full); err != nil {
		t.Errorf("file not found: %v", err)
	}

	// 能读回
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("body mismatch: got %q want %q", got, data)
	}
}

func TestBodyFile_PathFor(t *testing.T) {
	// 不创建文件,只断言路径格式
	got := BodyFilePath("trace-abc", "2026-07-22", "req")
	want := filepath.Join("2026-07-22", "trace-abc-req.json")
	if got != want {
		t.Errorf("BodyFilePath = %q, want %q", got, want)
	}
}
```

**Step 2.2: Run test to verify it fails**

Run: `cd backend && go test ./internal/accesslog/ -run TestBodyFile -v`
Expected: FAIL with `bodyfile.go: no such file or directory`.

**Step 2.3: Implement `bodyfile.go`**

Create `backend/internal/accesslog/bodyfile.go`:

```go
package accesslog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BodyFilePath 构造 body 文件相对路径
// 调用方拿到路径后拼上 rootDir 得到绝对路径
func BodyFilePath(traceID, date, kind string) string {
	return filepath.Join(date, fmt.Sprintf("%s-%s.json", traceID, kind))
}

// BodyFileWriter 管理 body 文件的写入
//   - 按日期分目录(YYYY-MM-DD)
//   - 同 traceID 的 request/response 各一份
//   - 单条 max 8MB(常量),超过打 truncated 标记由调用方在 metadata 里标记
type BodyFileWriter struct {
	rootDir string
	mu      sync.Mutex
}

// NewBodyFileWriter 构造 writer;若 rootDir 不存在则创建
func NewBodyFileWriter(rootDir string) (*BodyFileWriter, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir body dir: %w", err)
	}
	return &BodyFileWriter{rootDir: rootDir}, nil
}

// today 返回 YYYY-MM-DD(UTC)
func (b *BodyFileWriter) today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// Write 写入 body 文件,返回相对路径
// kind = "req" or "resp"
//   - dir 不存在自动建
//   - max 8 MB,超过截断
func (b *BodyFileWriter) Write(traceID, kind string, data []byte) (relPath string, truncated bool, err error) {
	const maxBody = 8 * 1024 * 1024 // 8 MB
	if len(data) > maxBody {
		data = data[:maxBody]
		truncated = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	date := b.today()
	rel := BodyFilePath(traceID, date, kind)
	abs := filepath.Join(b.rootDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return "", false, err
	}
	return rel, truncated, nil
}

// Read 读取 body 文件内容;不存在则返回 os.ErrNotExist
func (b *BodyFileWriter) Read(relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.rootDir, relPath))
}

// RootDir 返回 rootDir
func (b *BodyFileWriter) RootDir() string { return b.rootDir }

// Close 释放资源(预留,目前无内部状态)
func (b *BodyFileWriter) Close() error { return nil }
```

**Step 2.4: Run tests, expect pass**

Run: `cd backend && go test ./internal/accesslog/ -run TestBodyFile -v`
Expected: PASS, 2 tests pass.

**Step 2.5: Commit**

```bash
git add backend/internal/accesslog/bodyfile.go backend/internal/accesslog/bodyfile_test.go
git commit -m "feat(accesslog): body file writer with day-rotation and 8MB cap"
```

---

## Task 3: DB store for AccessLog

**Files:**
- Create: `backend/internal/accesslog/store.go`
- Create: `backend/internal/accesslog/store_test.go`

**Step 3.1: Write failing test**

Create `backend/internal/accesslog/store_test.go`:

```go
package accesslog

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

func newTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&dbpkg.AccessLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestStore_InsertAndGet(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()

	e := &AccessEntry{
		TraceID:        "trace-1",
		CreatedAt:      time.Now().UTC(),
		GatewayKeyName: "prod-a",
		Method:         "POST",
		Path:           "/v1/messages",
		RequestedModel: "MiniMax-M3",
		StatusCode:     200,
		LatencyMs:      123,
	}
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	rows, err := s.List(ctx, QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List len = %d, want 1", len(rows))
	}
	if rows[0].TraceID != "trace-1" {
		t.Errorf("TraceID = %q", rows[0].TraceID)
	}
}

func TestStore_Filter(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []*AccessEntry{
		{TraceID: "t1", CreatedAt: now, GatewayKeyName: "prod-a", StatusCode: 200, ProviderName: "minimax"},
		{TraceID: "t2", CreatedAt: now, GatewayKeyName: "dev-b", StatusCode: 503, ErrorType: "no_route"},
		{TraceID: "t3", CreatedAt: now, GatewayKeyName: "prod-a", StatusCode: 403, ErrorType: "model_not_allowed", ProviderName: "minimax"},
	} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// by gateway key
	rows, err := s.List(ctx, QueryFilter{GatewayKey: "prod-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("by key prod-a: got %d, want 2", len(rows))
	}

	// by status filter (>= 400)
	rows, err = s.List(ctx, QueryFilter{StatusMin: 400})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("status >= 400: got %d, want 2", len(rows))
	}

	// by error_type
	rows, err = s.List(ctx, QueryFilter{ErrorType: "model_not_allowed"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("error_type=model_not_allowed: got %d, want 1", len(rows))
	}
	if rows[0].TraceID != "t3" {
		t.Errorf("TraceID = %q, want t3", rows[0].TraceID)
	}
}

func TestStore_DeleteOlderThan(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()

	old := time.Now().UTC().Add(-48 * time.Hour)
	newer := time.Now().UTC().Add(-1 * time.Hour)

	for _, e := range []*AccessEntry{
		{TraceID: "old", CreatedAt: old},
		{TraceID: "newer", CreatedAt: newer},
	} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	n, err := s.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}

	rows, _ := s.List(ctx, QueryFilter{Limit: 10})
	if len(rows) != 1 || rows[0].TraceID != "newer" {
		t.Errorf("rows = %+v", rows)
	}
}
```

**Step 3.2: Run test to verify it fails**

Run: `cd backend && go test ./internal/accesslog/ -run TestStore -v`
Expected: FAIL, `store.go: no such file or directory`.

**Step 3.3: Implement `entry.go` first (small)**

Create `backend/internal/accesslog/entry.go`:

```go
package accesslog

import "time"

// AccessEntry 是被打包到 DB 和 (可能的)日志里的一条记录
//
// 设计为值类型语义 — Recorder 拿到指针后立即消费,Rec caller 不应修改
type AccessEntry struct {
	ID             uint      `json:"id"`
	TraceID        string    `json:"trace_id"`
	CreatedAt      time.Time `json:"created_at"`
	GatewayKeyID   string    `json:"gateway_key_id"`
	GatewayKeyName string    `json:"gateway_key_name"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	ClientIP       string    `json:"client_ip"`
	UserAgent      string    `json:"user_agent"`
	RequestedModel string    `json:"requested_model"`
	FinalModel     string    `json:"final_model"`
	ProviderName   string    `json:"provider_name"`
	Protocol       string    `json:"protocol"`
	IsStream       bool      `json:"is_stream"`
	StatusCode     int       `json:"status_code"`
	ErrorType      string    `json:"error_type"`
	LatencyMs      int       `json:"latency_ms"`
	ReqBodyPath    string    `json:"req_body_path"`
	ReqBodySize    int       `json:"req_body_size"`
	RespBodyPath   string    `json:"resp_body_path"`
	RespBodySize   int       `json:"resp_body_size"`
	// Truncated 状态由 文件名后缀 .truncated.json 标记,不在业务 struct 里(spec F1 决议)
}
```

**Step 3.4: Implement `store.go`**

Create `backend/internal/accesslog/store.go`:

```go
package accesslog

import (
	"context"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// QueryFilter 列表/计数共用过滤条件
//   - StatusMin/StatusMax 提供方便的 status_code 范围过滤(status=ok → max<400)
//   - 字符串字段精确匹配
type QueryFilter struct {
	StartTime    time.Time
	EndTime      time.Time
	GatewayKey   string
	ProviderName string
	ModelID      string
	ErrorType    string
	StatusMin    int
	StatusMax    int
	Limit        int
	Offset       int
}

// Store AccessLog 的 DB 读写
type Store struct {
	db *gorm.DB
}

// NewStore 构造 Store
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Insert 插入一条记录
func (s *Store) Insert(ctx context.Context, e *AccessEntry) error {
	row := toRow(e)
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return err
	}
	e.ID = row.ID
	return nil
}

// toRow / fromRow 在 AccessEntry (业务结构) 和 dbpkg.AccessLog (GORM) 之间转换
// 字段一一对应;保留两份 struct 是为了让 DB 模型和业务 API 解耦
func toRow(e *AccessEntry) *dbpkg.AccessLog {
	return &dbpkg.AccessLog{
		TraceID:        e.TraceID,
		CreatedAt:      e.CreatedAt,
		GatewayKeyID:   e.GatewayKeyID,
		GatewayKeyName: e.GatewayKeyName,
		Method:         e.Method,
		Path:           e.Path,
		ClientIP:       e.ClientIP,
		UserAgent:      e.UserAgent,
		RequestedModel: e.RequestedModel,
		FinalModel:     e.FinalModel,
		ProviderName:   e.ProviderName,
		Protocol:       e.Protocol,
		IsStream:       e.IsStream,
		StatusCode:     e.StatusCode,
		ErrorType:      e.ErrorType,
		LatencyMs:      e.LatencyMs,
		ReqBodyPath:    e.ReqBodyPath,
		ReqBodySize:    e.ReqBodySize,
		RespBodyPath:   e.RespBodyPath,
		RespBodySize:   e.RespBodySize,
		// truncated marker 写到 filename 后缀,DB 列已移除(F1)
	}
}

func fromRow(r *dbpkg.AccessLog) *AccessEntry {
	return &AccessEntry{
		ID:             r.ID,
		TraceID:        r.TraceID,
		CreatedAt:      r.CreatedAt,
		GatewayKeyID:   r.GatewayKeyID,
		GatewayKeyName: r.GatewayKeyName,
		Method:         r.Method,
		Path:           r.Path,
		ClientIP:       r.ClientIP,
		UserAgent:      r.UserAgent,
		RequestedModel: r.RequestedModel,
		FinalModel:     r.FinalModel,
		ProviderName:   r.ProviderName,
		Protocol:       r.Protocol,
		IsStream:       r.IsStream,
		StatusCode:     r.StatusCode,
		ErrorType:      r.ErrorType,
		LatencyMs:      r.LatencyMs,
		ReqBodyPath:    r.ReqBodyPath,
		ReqBodySize:    r.ReqBodySize,
		RespBodyPath:   r.RespBodyPath,
		RespBodySize:   r.RespBodySize,
		// truncated marker 写到 filename 后缀,DB 列已移除(F1)
	}
}

// List 按 filter 查询,默认按 created_at DESC
func (s *Store) List(ctx context.Context, f QueryFilter) ([]*AccessEntry, error) {
	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	q = q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset)

	var rows []dbpkg.AccessLog
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*AccessEntry, len(rows))
	for i := range rows {
		out[i] = fromRow(&rows[i])
	}
	return out, nil
}

// Count 统计符合条件记录数
func (s *Store) Count(ctx context.Context, f QueryFilter) (int64, error) {
	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// GetByID 单条查询(详情页用)
func (s *Store) GetByID(ctx context.Context, id uint) (*AccessEntry, error) {
	var row dbpkg.AccessLog
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	return fromRow(&row), nil
}

// DeleteOlderThan 删除 created_at < cutoff 的记录,返回删除数
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&dbpkg.AccessLog{})
	return res.RowsAffected, res.Error
}

// buildWhere 是 List/Count 共用的 where 构造器
func (s *Store) buildWhere(q *gorm.DB, f QueryFilter) *gorm.DB {
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.GatewayKey != "" {
		q = q.Where("gateway_key_name = ?", f.GatewayKey)
	}
	if f.ProviderName != "" {
		q = q.Where("provider_name = ?", f.ProviderName)
	}
	if f.ModelID != "" {
		q = q.Where("(requested_model = ? OR final_model = ?)", f.ModelID, f.ModelID)
	}
	if f.ErrorType != "" {
		q = q.Where("error_type = ?", f.ErrorType)
	}
	if f.StatusMin > 0 {
		q = q.Where("status_code >= ?", f.StatusMin)
	}
	if f.StatusMax > 0 {
		q = q.Where("status_code < ?", f.StatusMax)
	}
	return q
}
```

**Step 3.5: Run tests, expect pass**

Run: `cd backend && go test ./internal/accesslog/ -run TestStore -v`
Expected: PASS, 3 tests pass.

**Step 3.6: Commit**

```bash
git add backend/internal/accesslog/entry.go backend/internal/accesslog/store.go backend/internal/accesslog/store_test.go
git commit -m "feat(accesslog): DB store with list/count/get/delete filter"
```

---

## Task 4: Async buffer + batch flush

**Files:**
- Create: `backend/internal/accesslog/buffer.go`
- Create: `backend/internal/accesslog/buffer_test.go`

**Step 4.1: Write failing test**

Create `backend/internal/accesslog/buffer_test.go`:

```go
package accesslog

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

func newBufferWithStore(t *testing.T, capacity int) (*Buffer, *Store) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&dbpkg.AccessLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(db)
	b := NewBuffer(store, BufferConfig{
		Capacity:      capacity,
		BatchSize:     5,
		FlushInterval: 50 * time.Millisecond,
	})
	return b, store
}

func TestBuffer_PushAndFlush(t *testing.T) {
	b, store := newBufferWithStore(t, 100)
	b.Start(context.Background())
	defer b.Close()

	for i := 0; i < 12; i++ {
		b.Push(&AccessEntry{TraceID: "t-batch"})
	}

	// 等够 2 个 batch(5+5)+ 残余
	time.Sleep(400 * time.Millisecond)

	rows, err := store.List(context.Background(), QueryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 12 {
		t.Errorf("got %d rows, want 12", len(rows))
	}
}

func TestBuffer_DropWhenFull(t *testing.T) {
	b, store := newBufferWithStore(t, 2)
	// 不启动 worker,channel 永远不消费 → Push 满则丢
	// 不 panic + Close 不死锁 = 通过
	for i := 0; i < 1000; i++ {
		b.Push(&AccessEntry{TraceID: "x"})
	}
	b.Close()
	_ = store // 不读 store,本次主题只断言不阻塞
}

func TestBuffer_CloseFlushesRemainder(t *testing.T) {
	b, store := newBufferWithStore(t, 100)
	b.Start(context.Background())

	for i := 0; i < 3; i++ {
		b.Push(&AccessEntry{TraceID: "close-flush"})
	}
	// 3 条 < BatchSize,也没到 Interval,但 Close 应该 flush 残余
	b.Close()

	rows, _ := store.List(context.Background(), QueryFilter{Limit: 100})
	if len(rows) != 3 {
		t.Errorf("after Close got %d, want 3", len(rows))
	}
}
```

**Step 4.2: Run test, expect fail**

Run: `cd backend && go test ./internal/accesslog/ -run TestBuffer -v`
Expected: FAIL, `buffer.go: no such file or directory`.

**Step 4.3: Implement `buffer.go`**

Create `backend/internal/accesslog/buffer.go`:

```go
package accesslog

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// BufferConfig buffer 配置
type BufferConfig struct {
	Capacity      int           // 通道容量;默认 10000
	BatchSize     int           // 一次 flush 行数;默认 100
	FlushInterval time.Duration // ticker 周期;默认 1s
}

// Buffer 是 Recorder 用的 in-memory 通道 + 批量 flush worker
//
// 设计目标:
//   - Push 永远不阻塞(channel 满则丢)
//   - 定期批量 INSERT 减少 DB 压力
//   - Close 时强制 flush 残余
type Buffer struct {
	store *Store
	cfg   BufferConfig
	log   *zap.Logger

	ch     chan *AccessEntry
	closed atomicBool
	wg     sync.WaitGroup
}

type atomicBool struct {
	v int32 // 0=false, 1=true;通过 sync.Mutex 保护读写(F7 决议:保留 mutex 实现,改正注释)
	mux sync.Mutex
}

func (a *atomicBool) Set(b bool) {
	a.mux.Lock()
	defer a.mux.Unlock()
	if b {
		a.v = 1
	} else {
		a.v = 0
	}
}

func (a *atomicBool) Get() bool {
	a.mux.Lock()
	defer a.mux.Unlock()
	return a.v == 1
}

// NewBuffer 构造 Buffer(未启动,需调 Start 触发 worker)
func NewBuffer(store *Store, cfg BufferConfig) *Buffer {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 10000
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	return &Buffer{
		store: store,
		cfg:   cfg,
		log:   zap.NewNop(),
		ch:    make(chan *AccessEntry, cfg.Capacity),
	}
}

// SetLogger 注入 zap logger(主路径需要看到丢条警告)
func (b *Buffer) SetLogger(l *zap.Logger) {
	if l != nil {
		b.log = l
	}
}

// Push 是 Recorder.RecordAsync 的核心;永不阻塞
func (b *Buffer) Push(e *AccessEntry) {
	if e == nil {
		return
	}
	select {
	case b.ch <- e:
	default:
		// channel 满 = 丢整条 record(zap Warn,绝不阻塞主路径)
		b.log.Warn("accesslog buffer full, dropping entry",
			zap.String("trace_id", e.TraceID),
		)
	}
}

// Start 启动 worker
func (b *Buffer) Start(ctx context.Context) {
	b.wg.Add(1)
	go b.run(ctx)
}

// Close 关闭 worker;会 flush 残余
func (b *Buffer) Close() {
	if b.closed.Get() {
		return
	}
	b.closed.Set(true)
	close(b.ch)
	b.wg.Wait()
}

func (b *Buffer) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	// batch buffer
	batch := make([]*AccessEntry, 0, b.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// best-effort:重试 3 次
		for i := 0; i < 3; i++ {
			if err := b.insertBatch(ctx, batch); err == nil {
				break
			} else if i == 2 {
				b.log.Error("accesslog batch insert failed",
					zap.Int("rows", len(batch)),
					zap.Error(err),
				)
			}
			time.Sleep(50 * time.Millisecond)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-b.ch:
			if !ok {
				// channel closed → flush 残余退出
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= b.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// insertBatch 调 Store.Insert,逐条插入(简单可靠);
// 如要更高吞吐可改成 GROUP INSERT,但当前 batch=100 已够用
func (b *Buffer) insertBatch(ctx context.Context, batch []*AccessEntry) error {
	for _, e := range batch {
		if err := b.store.Insert(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
```

**Step 4.4: Run tests**

Run: `cd backend && go test ./internal/accesslog/ -run TestBuffer -v`
Expected: PASS, 3 tests pass.

**Step 4.5: Commit**

```bash
git add backend/internal/accesslog/buffer.go backend/internal/accesslog/buffer_test.go
git commit -m "feat(accesslog): async buffer with batch flush and drop-on-full"
```

---

## Task 5: Recorder facade + retention goroutine

**Files:**
- Create: `backend/internal/accesslog/recorder.go`
- Create: `backend/internal/accesslog/retention.go`

**Step 5.1: Implement `recorder.go`**

Create `backend/internal/accesslog/recorder.go`:

```go
package accesslog

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// RecorderConfig 顶层配置
//
// 注:F6 决议用 time.Duration 直读。yaml 用 string 如 "24h";mapstructure
// 会自动 "24h" → time.Duration(与项目其他字段如 Retry.OpenTimeout 同套路)
// 如发现项目用别的方案,请贴本地 loadConfig sample 后调整。
type RecorderConfig struct {
	Enabled       bool          // false → 整体 no-op
	BodyDir       string        // body 文件根目录
	BufferSize    int           // 通道容量
	BatchSize     int           // 批量 flush 行数
	FlushInterval time.Duration // 周期
	Retention     time.Duration // 默认 24h
}

// Recorder 是外部使用的轻量门面
type Recorder struct {
	cfg     RecorderConfig
	logger  *zap.Logger
	bf      *BodyFileWriter
	buf     *Buffer
	store   *Store
	reten   *Retention
	started bool
	mu      sync.Mutex
}

// NewRecorder 构造 Recorder(还不启动)
func NewRecorder(cfg RecorderConfig, db *gorm.DB, logger *zap.Logger) (*Recorder, error) {
	if !cfg.Enabled {
		return &Recorder{cfg: cfg}, nil // no-op recorder
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	bf, err := NewBodyFileWriter(cfg.BodyDir)
	if err != nil {
		return nil, err
	}
	store := NewStore(db)
	buf := NewBuffer(store, BufferConfig{
		Capacity:      cfg.BufferSize,
		BatchSize:     cfg.BatchSize,
		FlushInterval: cfg.FlushInterval, // 直接是 time.Duration
	})
	buf.SetLogger(logger)
	return &Recorder{
		cfg:    cfg,
		logger: logger,
		bf:     bf,
		buf:     buf,
		store:   store,
		reten:   NewRetention(store, bf, cfg.Retention, logger),
	}, nil
}

// Start 启动 worker + retention
func (r *Recorder) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || !r.cfg.Enabled {
		return
	}
	r.buf.Start(ctx)
	r.reten.Start(ctx)
	r.started = true
}

// Close flush + 关闭
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	r.buf.Close()
	return nil
}

// RecordAsync 是热路径 API,zero-blocking
//   - body 文件同步写(失败也继续,只 metadata)
//   - metadata 异步 push 到 buffer
func (r *Recorder) RecordAsync(e *AccessEntry) {
	if !r.cfg.Enabled || e == nil {
		return
	}
	if e.TraceID == "" {
		return
	}
	// F15 决议:删掉那条 `select { case <-r.buf.ch: default: }` 死代码 —
	// 那是非阻塞 drain,既不检查 closed 也不通知,纯噪音。Push 内部已经处理
	// channel 满 → drop,主路径永不阻塞。
	r.buf.Push(e)
}

// WriteBody 同步写 body,返回相对路径 + 是否 truncated
func (r *Recorder) WriteBody(traceID, kind string, data []byte) (string, bool) {
	if !r.cfg.Enabled {
		return "", false
	}
	path, trunc, err := r.bf.Write(traceID, kind, data)
	if err != nil {
		r.logger.Warn("accesslog body write failed",
			zap.String("trace_id", traceID),
			zap.String("kind", kind),
			zap.Error(err))
		return "", false
	}
	return path, trunc
}

// ReadBody 暴露给 handler 用(读 body 文件)
func (r *Recorder) ReadBody(relPath string) ([]byte, error) {
	if relPath == "" {
		return nil, nil
	}
	return r.bf.Read(relPath)
}

// BodyFileRoot 给 handler 做权限检查(防止 ../ 越权)
func (r *Recorder) BodyFileRoot() string {
	return r.bf.RootDir()
}

// Store 暴露给 handler 用(查 DB)
func (r *Recorder) Store() *Store { return r.store }
```

**Step 5.2: Skip durationAlias entirely**

F6 决议:`RecorderConfig` 用 `time.Duration` 直读。无需任何 wrapper / alias。
如果实施时发现项目 config loader 不能把 yaml 的 "24h" 自动转成 `time.Duration`,按以下两种兜底:
- (a) 在 config.go 加 `parseDuration(s string) time.Duration` helper,用 `time.ParseDuration`
- (b) 改 yaml 把 retention 写成 `86400000000000` 纳秒

两条路总有一条行得通,**不要**新增 wrapper 类型。

**Step 5.3: Implement `retention.go`**

Create `backend/internal/accesslog/retention.go`:

```go
package accesslog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Retention 删除过期 access_logs + body 文件
type Retention struct {
	store    *Store
	bf       *BodyFileWriter
	retent   time.Duration
	interval time.Duration
	logger   *zap.Logger

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewRetention 构造 Retention
func NewRetention(store *Store, bf *BodyFileWriter, retent time.Duration, logger *zap.Logger) *Retention {
	if retent <= 0 {
		retent = 24 * time.Hour
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Retention{
		store:    store,
		bf:       bf,
		retent:   retent,
		interval: 5 * time.Minute,
		logger:   logger,
	}
}

// Start 跑 goroutine,每 5 分钟扫一次
func (r *Retention) Start(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go r.run(bgCtx)
}

// Close 停止
func (r *Retention) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

func (r *Retention) run(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// 启动后立刻跑一次
	r.runOnce(ctx)

	for {
		select {
		case <-ticker.C:
			r.runOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Retention) runOnce(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-r.retent)

	// 先取待删行(单轮 max 1000)
	rows, err := r.store.List(ctx, QueryFilter{
		EndTime: cutoff,
		Limit:   1000,
	})
	if err != nil {
		r.logger.Warn("retention list failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		// 即便没记录,也调 DeleteOlderThan 清理残余
		_, _ = r.store.DeleteOlderThan(ctx, cutoff)
		return
	}

	// 删 body 文件
	for _, e := range rows {
		if e.ReqBodyPath != "" {
			_ = os.Remove(filepath.Join(r.bf.RootDir(), e.ReqBodyPath))
		}
		if e.RespBodyPath != "" {
			_ = os.Remove(filepath.Join(r.bf.RootDir(), e.RespBodyPath))
		}
	}

	// 删 DB 行
	if _, err := r.store.DeleteOlderThan(ctx, cutoff); err != nil {
		r.logger.Warn("retention delete failed", zap.Error(err))
	}
}
```

**Step 5.4: Add basic smoke tests for Recorder (F8 renumber)**

Append to `recorder_test.go` (create if absent):

```go
package accesslog

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

func TestRecorder_DisabledIsNoop(t *testing.T) {
	r, _ := NewRecorder(RecorderConfig{Enabled: false}, nil, nil)
	r.Start(context.Background())
	r.RecordAsync(&AccessEntry{TraceID: "x"})
	r.Close()
	// 不 panic 即过
}

func TestRecorder_RecordAsyncStoresAfterFlush(t *testing.T) {
	db, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	db.AutoMigrate(&dbpkg.AccessLog{})

	r, err := NewRecorder(RecorderConfig{
		Enabled:       true,
		BodyDir:       t.TempDir(),
		BufferSize:    100,
		BatchSize:     10,
		FlushInterval: 50 * time.Millisecond,
		Retention:     24 * time.Hour,
	}, db, nil)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	r.Start(context.Background())
	defer r.Close()

	// 写 body
	reqPath, _ := r.WriteBody("trace-1", "req", []byte("{}"))
	if reqPath == "" {
		t.Errorf("WriteBody returned empty path")
	}

	r.RecordAsync(&AccessEntry{
		TraceID:      "trace-1",
		CreatedAt:    time.Now().UTC(),
		StatusCode:   200,
		ReqBodyPath:  reqPath,
		ReqBodySize:  2,
	})

	time.Sleep(300 * time.Millisecond)

	rows, _ := r.Store().List(context.Background(), QueryFilter{Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].TraceID != "trace-1" {
		t.Errorf("TraceID = %q", rows[0].TraceID)
	}
}
```

**Step 5.5: Run tests**

Run: `cd backend && go test ./internal/accesslog/ -v`
Expected: PASS for all accesslog tests.

**Step 5.6: Commit**

```bash
git add backend/internal/accesslog/recorder.go backend/internal/accesslog/retention.go backend/internal/accesslog/recorder_test.go
git commit -m "feat(accesslog): recorder facade + 24h retention goroutine"
```

---

## Task 6: Config + Server wiring + proxy hook

**Files:**
- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/server/server.go`
- Modify: `backend/internal/proxy/proxy.go`
- Modify: `backend/internal/auth/authenticator.go`

**Step 6.1: Add config struct**

Open `backend/internal/config/config.go`. Find the `Server` struct (or wherever server-level config lives). Add:

```go
// AccessLogConfig 接入日志模块配置(§3.4 spec)
type AccessLogConfig struct {
	Enabled       bool          `yaml:"enabled"`
	BodyDir       string        `yaml:"body_dir"`
	BufferSize    int           `yaml:"buffer_size"`
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	Retention     time.Duration `yaml:"retention"`
}

// 在顶层的 ServerConfig 内加:
//   AccessLog AccessLogConfig `yaml:"access_log"`
//
// 默认值由以下 if-zero 兜底:
const (
	defaultAccessLogBodyDir       = "./data/access"
	defaultAccessLogBufferSize    = 10000
	defaultAccessLogBatchSize     = 100
	defaultAccessLogFlushInterval = time.Second
	defaultAccessLogRetention     = 24 * time.Hour
)
```

**Step 6.2: Update `config.example.yaml`**

Append:

```yaml
server:
  access_log:
    enabled: true
    body_dir: ./data/access
    buffer_size: 10000
    batch_size: 100
    flush_interval: 1s
    retention: 24h
```

**Step 6.3: Add `KeyNameFromCtx` helper**

In `backend/internal/auth/authenticator.go`, add:

```go
// KeyNameFromCtx 从 gin.Context 拿出当前 gateway key 的 name
// 给 accesslog 等不需要拿到完整 *GatewayKey 的调用方用
func KeyNameFromCtx(c *gin.Context) string {
	if v, ok := c.Get("gateway_key"); ok {
		if gk, ok := v.(*GatewayKey); ok {
			return gk.Name
		}
	}
	return ""
}
```

(Adjust if existing `gateway_key_name` is already in context — match local naming.)

**Step 6.4: Wire Recorder in server.New**

In `backend/internal/server/server.go`:

1. Import `accesslog` package.
2. After constructing `usageR`, add:

```go
accessCfg := accesslog.RecorderConfig{
	Enabled:       cfg.AccessLog.Enabled,
	BodyDir:       cfg.AccessLog.BodyDir,
	BufferSize:    cfg.AccessLog.BufferSize,
	BatchSize:     cfg.AccessLog.BatchSize,
	FlushInterval: cfg.AccessLog.FlushInterval,
	Retention:     cfg.AccessLog.Retention,
}
if accessCfg.BodyDir == "" {
    accessCfg.BodyDir = defaultAccessLogBodyDir
}
// ... (其他 zero-value 兜底)
accessR, err := accesslog.NewRecorder(accessCfg, db, logger)
if err != nil {
    return nil, fmt.Errorf("accesslog new: %w", err)
}
accessR.Start(context.Background())
```

3. Pass `accessR` into `proxy.NewEngine`:

```go
eng := proxy.NewEngine(proxy.Config{
	Router:      r,
	Logger:      logger,
	Usage:       usageC,
	Metrics:     metricsC,
	Breaker:     reporter,
	Authn:       authn,
	AccessLog:   accessR, // P67: 接入日志
	MaxRetry:    cfg.Retry.MaxAttempts,
})
```

4. Save `accessR` in `Server` struct, close in `Close` / shutdown:

```go
type Server struct {
	// ... existing fields
	accessR *accesslog.Recorder
}

// in shutdown:
if s.accessR != nil { _ = s.accessR.Close() }
```

**Step 6.5: Modify Engine.Config to accept Recorder**

In `backend/internal/proxy/proxy.go`:

proxy 直接 import accesslog(无 cycle — accesslog 包不依赖 proxy)。Engine 持有一个指向 `*accesslog.Recorder` 的字段(可为 nil 表示未启用),通过 `engine.accessLog.RecordAsync(entry)` 和 `engine.accessLog.WriteBody(traceID, kind, data)` 调用。

```go
import (
    "github.com/wang546673478/native-llm-gateway/internal/accesslog"
)

type Config struct {
    Router      *router.Router
    Logger      *zap.Logger
    Usage       UsageRecorder
    Metrics     MetricsRecorder
    Breaker     CircuitReporter
    Authn       *auth.Authenticator
    AccessLog   *accesslog.Recorder // P67: 可为 nil(没启用),nil-safety 调用
    MaxRetry    int
}

type Engine struct {
    // ... existing fields
    accessLog *accesslog.Recorder
}

// NewEngine:cfg.AccessLog 透传到 e.accessLog;为 nil 直接保留零值,所有调用点 nil-check 后跳过。
```

调用约定(在 `e.accessLog != nil` 时):

```go
e.accessLog.WriteBody(traceID, "req", body)   // 同步阻塞(写小文件),只在 handle 入口 1 次
e.accessLog.WriteBody(traceID, "resp", resp)  // 同步阻塞,非流式 1 次
// 流式: streamBuffer.Write(chunk);defer 在 doStream 末尾一次性 WriteBody 整个 buffer
e.accessLog.RecordAsync(entry)                // 异步 push,绝不阻塞
```

**Step 6.6: Add full access-log hook to `proxy.handle` + classifyError**

Open `backend/internal/proxy/proxy.go`, do the following (F2/F5 决议:handle 入口建 entry + defer RecordAsync + classifyError 一并归属到 Task 6;Task 7 只做 body write):

1. 加 import:
```go
import (
    "github.com/wang546673478/native-llm-gateway/internal/accesslog"
    ...
)
```

2. 在文件里加 `classifyError` helper(整段复制):
```go
// classifyError 把 HTTP status + 上游错误翻译成 error_type 枚举(spec §1.2)
// Pure function — 不依赖 Engine 实例,方便单元测试。
//   - statusCode 来自 c.Writer.Status()
//   - providerEmpty 表示没成功路由到任何 provider (== no_route 场景)
//   - upstreamErrType: 若最后出错有 provider.ProviderError,传它;否则传 provider.ErrorType("")
func classifyError(statusCode int, providerEmpty bool, upstreamErrType provider.ErrorType) string {
    if statusCode == 0 {
        return "unknown"
    }
    if statusCode < 400 {
        return "ok"
    }
    switch statusCode {
    case http.StatusUnauthorized, http.StatusForbidden:
        return "auth_failed"
    case http.StatusServiceUnavailable:
        if providerEmpty {
            return "no_route"
        }
        return "upstream_5xx"
    case http.StatusTooManyRequests:
        return "upstream_429"
    }
    if statusCode >= 500 {
        return "upstream_5xx"
    }
    if upstreamErrType == provider.ErrorTypeTimeout {
        return "timeout"
    }
    if upstreamErrType == provider.ErrorTypeConnection {
        return "connection_error"
    }
    return "upstream_4xx"
}
```

3. Modify `handle()` — 加 entry + defer。整段改写如下(注意:不要留 `// existing, unchanged` 注释,完整把 handle 重写):
```go
func (e *Engine) handle(c *gin.Context, isStream bool) {
    ctx := c.Request.Context()
    traceID := extractOrGenTraceID(c)

    // P67: 接入日志 — 入口建 entry,defer 统一 RecordAsync
    var entry *accesslog.AccessEntry
    if e.accessLog != nil {
        entry = &accesslog.AccessEntry{
            TraceID:        traceID,
            CreatedAt:      time.Now().UTC(),
            Method:         c.Request.Method,
            Path:           c.Request.URL.Path, // 不含 query string(spec F1)
            ClientIP:       c.ClientIP(),
            UserAgent:      c.Request.UserAgent(),
            GatewayKeyID:   c.GetString("gateway_key_id"),
            GatewayKeyName: auth.KeyNameFromCtx(c),
            IsStream:       isStream,
        }
    }
    // 持有供 defer 使用 — entry / providerName / lastErr
    var (
        lastProviderName string
        lastErr          *provider.ProviderError
    )
    defer func() {
        if entry == nil || e.accessLog == nil {
            return
        }
        entry.StatusCode = c.Writer.Status()
        entry.ErrorType = classifyError(entry.StatusCode, lastProviderName == "", lastErr)
        if lastErr != nil && lastProviderName != "" {
            entry.ProviderName = lastProviderName
        }
        entry.LatencyMs = int(time.Since(entry.CreatedAt) / time.Millisecond)
        if entry.FinalModel == "" {
            entry.FinalModel = entry.RequestedModel
        }
        e.accessLog.RecordAsync(entry)
    }()

    // 1. 读取 body — 不变
    body, err := io.ReadAll(c.Request.Body)
    if err != nil {
        e.logger.Error("read body", zap.Error(err), zap.String("trace_id", traceID))
        writeJSONError(c, http.StatusBadRequest, "invalid_request", "failed to read request body")
        return
    }
    if entry != nil {
        if p, _ := e.accessLog.WriteBody(traceID, "req", body); p != "" {
            entry.ReqBodyPath = p
            entry.ReqBodySize = len(body)
        }
    }

    // 2. extract model — 不变
    model, bodyStream, err := extractModelAndStream(body)
    if err != nil || model == "" {
        writeJSONError(c, http.StatusBadRequest, "invalid_request", "request body must include non-empty 'model' field")
        return
    }
    isStream = bodyStream
    if entry != nil {
        entry.RequestedModel = model
    }

    // 2.4 alias 解析 — 不变,更新 entry.FinalModel
    if e.router != nil {
        if target, ok := e.router.ResolveAlias(model); ok && target != model {
            if newBody, ok2 := rewriteModelField(body, target); ok2 {
                body = newBody
            }
            model = target
        }
    }
    if entry != nil {
        entry.FinalModel = model
    }

    // 3. 构造 Provider.Request — 不变
    req := &provider.Request{ /* 与现状一致 */ }
    if entry != nil {
        entry.Protocol = req.Headers.Get("anthropic-version") // best-effort
    }

    // 4. 路由 — 不变,但 iter.Next() 成功时记 lastProviderName
    iter, err := e.router.Route(ctx, req, routeOpts...)
    if err != nil {
        // ... existing fallback path
        writeJSONError(c, http.StatusServiceUnavailable, "no_route", ...)
        return
    }

    // 4.5-4.6: 白名单 / Provider binding — 不变

    // 5. failover 循环 — 每个成功/失败候选记 lastProviderName 和 lastErr
    var lastErrTyped *provider.ProviderError
    attempts := 0
    for {
        // ... existing
        lastProviderName = result.ProviderName
        // 成功后:providerName,ProviderError nil
        lastErrTyped = pe // 失败时
    }

    // 注:runWithFirstResult / tryOneCandidate / handleAllFailed 等也保持不变,
    // 配合上面 defer 捕获 status 自动写入。
}
```

**重要:** 不要把 `entry.ProviderName = lastProviderName` 写在 attempt 循环内 — 让 defer 统一基于 `lastProviderName` 写,避免在多个 placeholder provider 路由间错位。

**Step 6.7: Build**

Run: `cd backend && go build ./...`
Expected: compiles.

**Step 6.8: Commit**

```bash
git add backend/internal/config/config.go backend/internal/server/server.go backend/internal/proxy/proxy.go backend/internal/auth/authenticator.go config.example.yaml
git commit -m "feat(observability): wire accesslog recorder into proxy engine with classifyError"
```

---

## Task 7: Proxy body write — non-stream + stream (per-trace + global 1000 cap)

F2/F5/F4 决议:
- `classifyError` 已并入 Task 6.6,Task 7 不再重复。
- handle() entry / defer RecordAsync 已并入 Task 6.6,本任务只做 **body write**。
- F4:全局并发流式响应 1000 上限,在 Task 7 实现。

**Files:**
- Modify: `backend/internal/proxy/proxy.go` (doRequest / writeNonStreamResponse / doStream / engine 新字段)
- Modify: `backend/internal/accesslog/bodyfile.go` (暴露 `IsTruncated` helper + `MaxBodyBytes` const — F12)

**Step 7.1: Expose `MaxBodyBytes` const + `IsTruncated` helper (F12)**

Add to `backend/internal/accesslog/bodyfile.go`:

```go
// MaxBodyBytes 单条 body 文件 8MB 上限(spec §3.3)
const MaxBodyBytes = 8 * 1024 * 1024

// IsTruncated 通过文件名后缀判断是否被截断
// 命名约定:截断的 body 文件后缀是 `.truncated.json`,正常是 `.json`
func IsTruncated(relPath string) bool {
    return strings.HasSuffix(relPath, ".truncated.json")
}
```

并在现有的 `Write(traceID, kind, data)` 里改:
- 超 MaxBodyBytes → 把文件名后缀从 `.json` 改为 `.truncated.json`
- `BodyFilePath` 加一份 `BodyFilePathTruncated(traceID, date, kind)` 或让 `Write` 内部根据 truncated 标志选路径

实现示例:
```go
func (b *BodyFileWriter) Write(traceID, kind string, data []byte) (relPath string, err error) {
    trunc := len(data) > MaxBodyBytes
    if trunc {
        data = data[:MaxBodyBytes]
    }
    // ... 写文件逻辑不变
    if trunc {
        rel = strings.TrimSuffix(rel, ".json") + ".truncated.json"
    }
    return rel, nil
}
```

**Step 7.2: Non-stream body write — in `doRequest` / `writeNonStreamResponse`**

In `writeNonStreamResponse`(非流式路径),after capturing the upstream body:

```go
if e.accessLog != nil && !isStream {
    respPath, _ := e.accessLog.WriteBody(req.TraceID, "resp", respBody)
    entry.RespBodyPath = respPath
    entry.RespBodySize = len(respBody)
}
```

**重要:** 调用 writeNonStreamResponse 时,函数签名需要接受 `entry` 参数或在 Engine 用一个 `gin.Context`-keyed 映射。简单做法:把 `entry` 作为 `Engine.writeNonStreamResponse(c, respBody, respHeader, entry)` 的最后一个参数(选这个方案)。

**Step 7.3: Stream accumulator with **global 1000 cap** (F4)**

Add fields to Engine:
```go
type Engine struct {
    // ... existing
    accessLog *accesslog.Recorder
    streamBuf sync.Map // map[string]*streamAcc
    streamCnt int64     // atomic counter for global cap
}
```

定义 stream buffer 类型(在 proxy 包内):
```go
type streamAcc struct {
    sync.Mutex
    buf  bytes.Buffer
    truncated bool
}

const maxConcurrentStreams = 1000 // spec §3.3
```

Helper methods on Engine:
```go
func (e *Engine) acquireStreamSlot(traceID string) (*streamAcc, bool) {
    // atomic add;>= 1000 → 返回 false 且不登记 buf
    n := atomic.AddInt64(&e.streamCnt, 1)
    if n > maxConcurrentStreams {
        atomic.AddInt64(&e.streamCnt, -1)
        return nil, false
    }
    newAcc := &streamAcc{}
    actual, _ := e.streamBuf.LoadOrStore(traceID, newAcc)
    return actual.(*streamAcc), true
}

func (e *Engine) appendStreamChunk(traceID string, chunk []byte, entry *accesslog.AccessEntry) {
    acc, ok := e.acquireStreamSlot(traceID)
    if !ok {
        // 超 1000 → 不累积 body;truncated 状态由 accesslog 层在 finalize 写 .truncated.json 后缀
        // (F1 决议:不存 DB 列,F12 决议:文件名后缀标记)
        return
    }
    acc.Lock()
    if acc.buf.Len() < accesslog.MaxBodyBytes {
        acc.buf.Write(chunk)
        if acc.buf.Len() >= accesslog.MaxBodyBytes {
            acc.truncated = true
        }
    } else {
        acc.truncated = true
    }
    acc.Unlock()
}

func (e *Engine) finalizeStream(traceID string, entry *accesslog.AccessEntry) {
    accAny, ok := e.streamBuf.LoadAndDelete(traceID)
    if !ok {
        atomic.AddInt64(&e.streamCnt, -1)
        return
    }
    acc := accAny.(*streamAcc)
    if e.accessLog != nil {
        path, _ := e.accessLog.WriteBody(traceID, "resp", acc.buf.Bytes())
        entry.RespBodyPath = path
        entry.RespBodySize = acc.buf.Len()
    }
    atomic.AddInt64(&e.streamCnt, -1)
}
```

**Step 7.4: Wire into doStream SSE forwarding**

In `doStream`,每转发一个 chunk 给 client 时,也调 `e.appendStreamChunk(traceID, chunk, entry)`。
`message_stop` / `io.EOF` / `client disconnect` 时调 `e.finalizeStream(traceID, entry)` 一次性写入。
失败路径(defer 处)同样调。

**Step 7.5: Extend proxy_test**

Append to `backend/internal/proxy/proxy_test.go`:

```go
func TestStreamBuffer_GlobalCap(t *testing.T) {
    e := &Engine{}
    var wg sync.WaitGroup
    for i := 0; i < 1500; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _, ok := e.acquireStreamSlot(fmt.Sprintf("t-%d", i))
            if i < 1000 && !ok { t.Errorf("early alloc should succeed") }
            if i >= 1000 && ok { t.Errorf("late alloc should fail") }
        }()
    }
    wg.Wait()
}
```

(只测全局上限,集成测试留给 Step 7.6。)

**Step 7.6: Manual E2E**

Run the gateway with mock LLM upstream on 18080, hit `/v1/messages` with a fake token. Confirm:

```bash
# 1. 启动 mock + gateway
cd backend && go run . --config ../config.example.yaml
# 另开 terminal:
curl -s -X POST http://localhost:8080/v1/messages \
  -H "x-api-key: gw-key-dev-please-change-me" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}' | head -100

# 2. 检查文件
ls data/access/2026-07-22/
# 应看到 {traceID}-req.json 和 {traceID}-resp.json

# 3. SQLite 查行
sqlite3 data/gateway.db "SELECT trace_id,status_code,error_type,provider_name,req_body_path FROM access_logs ORDER BY created_at DESC LIMIT 5;"
```

期望:
- 200 + 流式响应 → access_logs 行 status_code=200,error_type=ok
- body 文件存在且 raw(不是 base64)
- proxy.go go build 通过

**Step 7.7: Commit**

```bash
git add backend/internal/proxy/proxy.go
git commit -m "feat(proxy): write request/response bodies for access logs"
```

---

## Task 8: HTTP endpoints for AccessLogs UI

**Files:**
- Modify: `backend/internal/api/http/handler/admin.go`

**Step 8.1: Inject Recorder into Admin handler**

In the Admin struct, add `AccessLog *accesslog.Recorder`.

In the NewAdmin constructor signature, add the param.

In server.New where Admin is constructed, pass `s.accessR`.

**Step 8.2: Add routes**

In `admin.go`, find where routes are registered (`r.GET("/usage", ...)` etc), add:

```go
func (a *Admin) listAccessLogs(c *gin.Context) {
    f := accesslog.QueryFilter{
        StartTime:    parseTime(c.Query("start")),
        EndTime:      parseTime(c.Query("end")),
        GatewayKey:   c.Query("gateway_key"),
        ProviderName: c.Query("provider"),
        ModelID:      c.Query("model"),
        ErrorType:    c.Query("error_type"),
    }
    if v := c.Query("status"); v != "" {
        switch v {
        case "ok":
            f.StatusMax = 400
        case "4xx":
            f.StatusMin, f.StatusMax = 400, 500
        case "5xx":
            f.StatusMin = 500
        }
    }
    if v := c.Query("limit"); v != "" {
        f.Limit, _ = strconv.Atoi(v)
    }
    if v := c.Query("offset"); v != "" {
        f.Offset, _ = strconv.Atoi(v)
    }

    store := a.AccessLog.Store()
    total, err := store.Count(c.Request.Context(), f)
    if err != nil {
        c.JSON(500, gin.H{"error": "count_failed", "detail": err.Error()})
        return
    }
    rows, err := store.List(c.Request.Context(), f)
    if err != nil {
        c.JSON(500, gin.H{"error": "list_failed", "detail": err.Error()})
        return
    }
    c.JSON(200, gin.H{
        "records": rows,
        "total":   total,
        "limit":   f.Limit,
        "offset":  f.Offset,
    })
}

func (a *Admin) getAccessLogDetail(c *gin.Context) {
    idStr := c.Param("id")
    id, err := strconv.ParseUint(idStr, 10, 64)
    if err != nil {
        c.JSON(400, gin.H{"error": "invalid_id"})
        return
    }
    e, err := a.AccessLog.Store().GetByID(c.Request.Context(), uint(id))
    if err != nil {
        c.JSON(404, gin.H{"error": "not_found"})
        return
    }

    // 加 body(可能因为 retention 而丢失)
    var reqBody, respBody []byte
    if e.ReqBodyPath != "" {
        if b, err := a.AccessLog.ReadBody(e.ReqBodyPath); err == nil {
            reqBody = b
        }
    }
    if e.RespBodyPath != "" {
        if b, err := a.AccessLog.ReadBody(e.RespBodyPath); err == nil {
            respBody = b
        }
    }

    c.JSON(200, gin.H{
        "metadata":        e,
        "req_body":        string(reqBody), // 原始内容,JSON-safe 字符串(spec §4.2 raw body 决议 F3)
        "resp_body":       string(respBody),
        "req_body_trunc":  accesslog.IsTruncated(e.ReqBodyPath),
        "resp_body_trunc": accesslog.IsTruncated(e.RespBodyPath),
    })
}

func (a *Admin) accessLogStats(c *gin.Context) {
    store := a.AccessLog.Store()
    ctx := c.Request.Context()

    total, _ := store.Count(ctx, accesslog.QueryFilter{
        StartTime: time.Now().UTC().Add(-24 * time.Hour),
    })
    errs, _ := store.Count(ctx, accesslog.QueryFilter{
        StartTime: time.Now().UTC().Add(-24 * time.Hour),
        StatusMin: 400,
    })

    // last24hFilter 复用,避免 3 处 StartTime + Add(-24h) 重复(F12)
    last24h := accesslog.QueryFilter{StartTime: time.Now().UTC().Add(-24 * time.Hour)}
    total, _ := store.Count(ctx, last24h)
    last24hErr := last24h
    last24hErr.StatusMin = 400
    errs, _ := store.Count(ctx, last24hErr)
    // F14 决议:用 GroupByCount 真正算 distinct gateway key
    activeKeys, _ := store.GroupByCount(ctx, last24h, "gateway_key_name")

    c.JSON(200, gin.H{
        "total_24h":   total,
        "errors_24h":  errs,
        "active_keys": activeKeys,
    })
}
```

**Step 8.3: Add `GroupByCount` to Store**

```go
// GroupByCount returns count of distinct values for a column within filter window
func (s *Store) GroupByCount(ctx context.Context, f QueryFilter, column string) (int64, error) {
    q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
    var n int64
    err := q.Select("COUNT(DISTINCT " + column + ")").Scan(&n).Error
    return n, err
}
```

**Step 8.4: Register routes**

In admin handler's Register function:

```go
r.GET("/access-logs", a.listAccessLogs)
r.GET("/access-logs/stats", a.accessLogStats)
r.GET("/access-logs/:id/detail", a.getAccessLogDetail)
```

**Step 8.5: Build + manual test**

Run: `go build ./...`

```bash
# 验证 list
curl -s "http://127.0.0.1:8080/api/v1/access-logs?limit=5" \
    -H "Authorization: Bearer gw-key-dev-..." | python3 -m json.tool | head -50
```

Expect: `{"records": [...], "total": N, ...}`

**Step 8.6: Commit**

```bash
git add backend/internal/api/http/handler/admin.go backend/internal/accesslog/store.go
git commit -m "feat(api): admin endpoints for access-logs list/detail/stats"
```

---

## Task 9: AccessLogs.vue page + drawer

**Files:**
- Modify: `frontend/src/api/client.ts`
- Create: `frontend/src/views/AccessLogs.vue`
- Modify: `frontend/src/router/index.ts` (or wherever routes are)
- Modify: `frontend/src/components/MainLayout.vue` (or menu component)

**Step 9.1: Add API client**

In `frontend/src/api/client.ts`:

```ts
export interface AccessLog {
  id: number
  trace_id: string
  created_at: string
  gateway_key_id: string
  gateway_key_name: string
  method: string
  path: string
  client_ip: string
  user_agent: string
  requested_model: string
  final_model: string
  provider_name: string
  protocol: string
  is_stream: boolean
  status_code: number
  error_type: string
  latency_ms: number
  req_body_path: string
  req_body_size: number
  resp_body_path: string
  resp_body_size: number
  // body_truncated 不存在 — 由 detail 接口的 req_body_trunc/resp_body_trunc 推断
}

export interface AccessLogListResp {
  records: AccessLog[]
  total: number
  limit: number
  offset: number
}

export interface AccessLogDetailResp {
  metadata: AccessLog
  req_body: string        // 原始内容(F3)
  resp_body: string
  req_body_trunc: boolean   // 由 req_body_path 文件名后缀 .truncated.json 推断
  resp_body_trunc: boolean
}

// In the default export object:
accessLogs: {
  list: (params?: Record<string, string | number>) =>
    client.get<AccessLogListResp>('/access-logs', { params }).then(r => r.data),
  detail: (id: number) =>
    client.get<AccessLogDetailResp>(`/access-logs/${id}/detail`).then(r => r.data),
  stats: () =>
    client.get<{ total_24h: number; errors_24h: number; active_keys: number }>(
      '/access-logs/stats'
    ).then(r => r.data),
}
```

**Step 9.2: Create AccessLogs.vue**

Create `frontend/src/views/AccessLogs.vue`:

```vue
<template>
  <n-spin :show="loading">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">
          Access Logs (24h)
          <n-tag style="margin-left: 8px" type="info">总 {{ stats.total_24h ?? '…' }}</n-tag>
          <n-tag style="margin-left: 4px" type="error">错 {{ stats.errors_24h ?? '…' }}</n-tag>
          <n-tag style="margin-left: 4px">{{ stats.active_keys ?? '…' }} 活跃 key</n-tag>
        </n-h3>
        <n-space>
          <n-button @click="load">刷新</n-button>
        </n-space>
      </n-space>

      <n-space style="margin-bottom: 12px" :wrap="false">
        <n-input v-model:value="filterTraceId" placeholder="Trace ID" clearable style="width: 220px" />
        <n-select v-model:value="filterKey" :options="keyOptions" placeholder="Gateway Key" clearable style="width: 160px" @update:value="resetAndLoad" />
        <n-select v-model:value="filterStatus" :options="statusOptions" placeholder="状态" clearable style="width: 140px" @update:value="resetAndLoad" />
        <n-button type="primary" @click="resetAndLoad">查询</n-button>
      </n-space>

      <n-data-table
        :columns="columns"
        :data="records"
        :pagination="pagination"
        :bordered="false"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
        @row-click="openDetail"
        style="cursor: pointer"
      />
    </n-card>

    <!-- 详情抽屉 -->
    <n-drawer v-model:show="drawerVisible" :width="700" placement="right">
      <n-drawer-content :title="`Trace ${detail?.metadata.trace_id ?? ''}`" closable>
        <div v-if="detail">
          <n-descriptions :column="1" bordered size="small" style="margin-bottom: 16px">
            <n-descriptions-item label="时间">{{ detail.metadata.created_at }}</n-descriptions-item>
            <n-descriptions-item label="状态">
              <n-tag :type="statusTagType(detail.metadata.status_code)">
                {{ detail.metadata.status_code }} {{ detail.metadata.error_type }}
              </n-tag>
            </n-descriptions-item>
            <n-descriptions-item label="Gateway Key">{{ detail.metadata.gateway_key_name || '—' }}</n-descriptions-item>
            <n-descriptions-item label="Provider">{{ detail.metadata.provider_name || '—' }}</n-descriptions-item>
            <n-descriptions-item label="Model">{{ detail.metadata.requested_model }} → {{ detail.metadata.final_model }}</n-descriptions-item>
            <n-descriptions-item label="延迟">{{ detail.metadata.latency_ms }} ms</n-descriptions-item>
            <n-descriptions-item label="Client IP">{{ detail.metadata.client_ip }}</n-descriptions-item>
          </n-descriptions>

          <n-h4>请求体 ({{ formatSize(detail.metadata.req_body_size) }})</n-h4>
          <n-card embedded>
            <n-input
              type="textarea"
              :value="detail.req_body || '— 不可用 —'"
              :autosize="{ minRows: 4, maxRows: 16 }"
              readonly
            />
            <n-button size="tiny" @click="copy(detail.req_body)" style="margin-top: 4px">复制</n-button>
          </n-card>

          <n-h4>响应体 ({{ formatSize(detail.metadata.resp_body_size) }})</n-h4>
          <n-card embedded>
            <n-input
              type="textarea"
              :value="detail.resp_body || '— 不可用 —'"
              :autosize="{ minRows: 4, maxRows: 16 }"
              readonly
            />
            <n-button size="tiny" @click="copy(detail.resp_body)" style="margin-top: 4px">复制</n-button>
          </n-card>
        </div>
      </n-drawer-content>
    </n-drawer>
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, ref, computed } from 'vue'
import {
  NCard, NSpace, NButton, NDataTable, NSelect, NInput,
  NDrawer, NDrawerContent, NTag, NDescriptions, NDescriptionsItem,
  NH3, NH4, NSpin, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import api from '@/api/client'
import type { AccessLog, AccessLogDetailResp } from '@/api/client'

const message = useMessage()
const records = ref<AccessLog[]>([])
const stats = ref({ total_24h: 0, errors_24h: 0, active_keys: 0 })
const loading = ref(false)
const pagination = ref({ page: 1, pageSize: 20, itemCount: 0, showSizePicker: true, pageSizes: [20, 50, 100, 200] })

const filterTraceId = ref('')
const filterKey = ref<string | null>(null)
const filterStatus = ref<string | null>(null)
const keyOptions = ref<{ label: string; value: string }[]>([])

const statusOptions = [
  { label: '成功 (2xx/3xx)', value: 'ok' },
  { label: '4xx', value: '4xx' },
  { label: '5xx', value: '5xx' },
  { label: '认证失败', value: 'auth_failed' },
  { label: '无路由', value: 'no_route' },
  { label: '模型未授权', value: 'model_not_allowed' },
]

const drawerVisible = ref(false)
const detail = ref<AccessLogDetailResp | null>(null)

function formatSize(b: number) {
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  return `${(b / 1024 / 1024).toFixed(2)} MB`
}

function statusTagType(code: number) {
  if (code >= 500) return 'error'
  if (code >= 400) return 'warning'
  return 'success'
}

const columns: DataTableColumns<AccessLog> = [
  {
    title: '时间',
    key: 'created_at',
    width: 180,
    render: (row) => row.created_at.substring(11, 19),
  },
  {
    title: '状态',
    key: 'status_code',
    width: 90,
    render: (row) =>
      h(NTag, { type: statusTagType(row.status_code), size: 'small' }, () => `${row.status_code}`),
  },
  { title: 'Key', key: 'gateway_key_name', width: 120 },
  {
    title: 'Model',
    key: 'requested_model',
    width: 180,
    render: (row) => {
      if (row.requested_model === row.final_model) return row.requested_model
      return h('span', {}, [
        h('span', { style: 'color: #999' }, row.requested_model),
        h('span', { style: 'margin: 0 4px' }, '→'),
        h('span', { style: 'color: #2080f0' }, row.final_model),
      ])
    },
  },
  { title: 'Provider', key: 'provider_name', width: 120 },
  { title: '延迟', key: 'latency_ms', width: 70 },
  { title: 'Trace', key: 'trace_id', render: (row) => row.trace_id.substring(0, 8) },
]

async function load() {
  loading.value = true
  try {
    const params: Record<string, string | number> = {
      limit: pagination.value.pageSize,
      offset: (pagination.value.page - 1) * pagination.value.pageSize,
    }
    if (filterTraceId.value) params.trace_id = filterTraceId.value // map if backend supports
    if (filterKey.value) params.gateway_key = filterKey.value
    if (filterStatus.value) params.status = filterStatus.value

    const [listResp, statsResp] = await Promise.all([
      api.accessLogs.list(params),
      api.accessLogs.stats(),
    ])
    records.value = listResp.records
    pagination.value.itemCount = listResp.total
    stats.value = statsResp
  } catch (e: any) {
    message.error('加载失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    loading.value = false
  }
}

async function loadKeyOptions() {
  // 复用 Keys.vue 的方式: GET /api/v1/keys
  const r = await api.keys.list().catch(() => ({ keys: [] }))
  keyOptions.value = (r.keys ?? []).map((k: any) => ({ label: k.name, value: k.name }))
}

function onPageChange(page: number) { pagination.value.page = page; load() }
function onPageSizeChange(size: number) { pagination.value.pageSize = size; pagination.value.page = 1; load() }

function resetAndLoad() {
  pagination.value.page = 1
  load()
}

async function openDetail(row: AccessLog) {
  try {
    const d = await api.accessLogs.detail(row.id)
    detail.value = d
    drawerVisible.value = true
  } catch (e: any) {
    message.error('加载详情失败: ' + (e.response?.data?.error ?? e.message))
  }
}

async function copy(text: string) {
  if (!text) return
  try { await navigator.clipboard.writeText(text); message.success('已复制') } catch {}
}

onMounted(() => { load(); loadKeyOptions() })
</script>
```

**Step 9.3: Register route + menu**

In frontend router (e.g. `src/router/index.ts` or wherever routes are declared):

```ts
{ path: '/access-logs', name: 'access-logs', component: () => import('@/views/AccessLogs.vue') }
```

In `MainLayout.vue` (or wherever menu items live), add:

```vue
<n-menu-item key="access-logs">📋 Access Logs</n-menu-item>
```

(Wire to the existing router push handler.)

**Step 9.4: Build**

```bash
cd frontend && npm run build
```

Expected: no TS errors.

**Step 9.5: Manual E2E**

1. Start backend + frontend
2. Open http://localhost:5173/access-logs
3. Verify table renders, header stats show
4. Filter by status=5xx → only errors show
5. Click a row → drawer opens with raw bodies
6. Click 复制 on body → clipboard updated

**Step 9.6: Commit**

```bash
git add frontend/src/views/AccessLogs.vue frontend/src/api/client.ts frontend/src/router/* frontend/src/components/MainLayout.vue
git commit -m "feat(frontend): AccessLogs page with list, filter, detail drawer"
```

---

## Self-Review Checklist

After writing this plan, the following checks pass:

1. **Spec coverage:**
   - §1 DB schema → Task 1 ✓
   - §2 Components → Tasks 2-5 ✓
   - §3 Async write + streamBuffer cap → Tasks 4, 5, 7 ✓
   - §4 API endpoints (list/detail/stats) → Task 8 ✓
   - §5 UI page → Task 9 ✓
   - §6 Retention → Task 5 (Retention goroutine) ✓
   - §7 No existing code changes (P67 additive) → Tasks 6-7 ✓
   - §9 Implementation order followed → Tasks 1-9 sequential ✓

2. **Placeholder scan:** No TBD/TODO in code; all signatures concrete.

3. **Type consistency:**
   - `AccessEntry` is defined in `internal/accesslog/entry.go` (Task 3.3) and used everywhere ✓
   - `BodyFileWriter` constructor `NewBodyFileWriter(rootDir)` ✓
   - `Store.List/Count/Get/Insert/DeleteOlderThan` signatures consistent in Tasks 3, 8 ✓
   - `Recorder.RecordAsync(*AccessEntry)`, `WriteBody(string, string, []byte) (string, bool)` ✓
   - Frontend types `AccessLog / AccessLogListResp / AccessLogDetailResp` match backend JSON ✓

4. **Edge cases handled:**
   - Channel full → drop + log (Task 4.3)
   - Body file write fail → metadata-only entry (Task 5.3)
   - Retention tick failure → retry next tick (Task 5.3)
   - Stream chunk accumulator overflow → truncate (Task 7.2)
   - AccessLogs list page empty → table shows "no data"
   - Body file deleted by retention → detail shows "不可用" (Task 9.2)

---

## Risks and Open Questions

- **Import direction:** `proxy` imports `accesslog` is acceptable since `accesslog` doesn't import `proxy`. Confirm by inspecting `accesslog/` — should not reference proxy types.
- **YAML Duration parsing:** If existing config uses `time.Duration` directly with mapstructure, mirror that. If it uses a special type, match it.
- **Test isolation:** Some tests use raw `dbpkg.AccessLog` migration. Confirm that `db.AutoMigrate(&dbpkg.AccessLog{})` works in in-memory SQLite (it should).
- **Stream chunk accumulator lock:** `sync.Map` per trace ID — fine for expected concurrent streams < 1000. If more, switch to per-engine map + mutex.

---

## Execution Choice

After this plan is reviewed and approved, choose:

1. **Subagent-driven:** I dispatch a fresh subagent per task, review between, faster iteration.
2. **Inline execution:** Tasks executed in this session with checkpoints.
