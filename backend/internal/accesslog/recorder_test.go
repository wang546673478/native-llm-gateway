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
		TraceID:     "trace-1",
		CreatedAt:   time.Now().UTC(),
		StatusCode:  200,
		ReqBodyPath: reqPath,
		ReqBodySize: 2,
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