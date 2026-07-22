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