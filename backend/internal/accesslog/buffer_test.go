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