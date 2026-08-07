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
	if err := db.AutoMigrate(&dbpkg.AccessLog{}, &dbpkg.GatewayKey{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedGatewayKeys 造 gateway_keys 行(名字现查填充/过滤依赖它)
func seedGatewayKeys(t *testing.T, db *gorm.DB, names ...string) {
	t.Helper()
	for i, n := range names {
		gk := &dbpkg.GatewayKey{
			Name:    n,
			KeyHash: "hash-" + n,
		}
		if err := db.Create(gk).Error; err != nil {
			t.Fatalf("create gateway key %q: %v", n, err)
		}
		if i == 0 && gk.ID != 1 {
			// 断言 ID 从 1 开始,测试里硬编码 1/2 才可靠
			t.Fatalf("first gateway key ID = %d, want 1", gk.ID)
		}
	}
}

func TestStore_InsertAndGet(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	seedGatewayKeys(t, db, "prod-a")

	e := &AccessEntry{
		TraceID:        "trace-1",
		CreatedAt:      time.Now().UTC(),
		GatewayKeyID:   "1",
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
	// 名字不落库 — List 按 ID 现查 gateway_keys 填充
	if rows[0].GatewayKeyName != "prod-a" {
		t.Errorf("GatewayKeyName = %q, want prod-a", rows[0].GatewayKeyName)
	}
}

// TestStore_Filter_GatewayKeyName: 按名字过滤走子查询(现查 gateway_keys 的 ID
// 集合再匹配),gateway_key_name 列已移除
func TestStore_Filter_GatewayKeyName(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	seedGatewayKeys(t, db, "prod-a", "dev-b")

	for _, e := range []*AccessEntry{
		{TraceID: "t1", CreatedAt: time.Now().UTC(), GatewayKeyID: "1", StatusCode: 200, ProviderName: "minimax"},
		{TraceID: "t2", CreatedAt: time.Now().UTC(), GatewayKeyID: "2", StatusCode: 503, ErrorType: "no_route"},
		{TraceID: "t3", CreatedAt: time.Now().UTC(), GatewayKeyID: "1", StatusCode: 403, ErrorType: "model_not_allowed", ProviderName: "minimax"},
	} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// by gateway key name(现查 ID 匹配)
	rows, err := s.List(ctx, QueryFilter{GatewayKey: "prod-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("by key prod-a: got %d, want 2", len(rows))
	}
	// 展示名来自现查
	for _, r := range rows {
		if r.GatewayKeyName != "prod-a" {
			t.Errorf("GatewayKeyName = %q, want prod-a", r.GatewayKeyName)
		}
	}
}

// TestStore_GatewayKeyRename: 改名后历史记录跟随新名字(名字不落库的收益)
func TestStore_GatewayKeyRename(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	seedGatewayKeys(t, db, "prod-a")

	if err := s.Insert(ctx, &AccessEntry{TraceID: "t1", CreatedAt: time.Now().UTC(), GatewayKeyID: "1", StatusCode: 200}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	db.Model(&dbpkg.GatewayKey{}).Where("id = 1").Update("name", "prod-b")

	rows, err := s.List(ctx, QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].GatewayKeyName != "prod-b" {
		t.Errorf("after rename: name = %q, want prod-b", rows[0].GatewayKeyName)
	}
}

func TestStore_Filter(t *testing.T) {
	db := newTestDB(t)
	s := NewStore(db)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []*AccessEntry{
		{TraceID: "t1", CreatedAt: now, StatusCode: 200, ProviderName: "minimax"},
		{TraceID: "t2", CreatedAt: now, StatusCode: 503, ErrorType: "no_route"},
		{TraceID: "t3", CreatedAt: now, StatusCode: 403, ErrorType: "model_not_allowed", ProviderName: "minimax"},
	} {
		if err := s.Insert(ctx, e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// by status filter (>= 400)
	rows, err := s.List(ctx, QueryFilter{StatusMin: 400})
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
