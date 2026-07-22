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