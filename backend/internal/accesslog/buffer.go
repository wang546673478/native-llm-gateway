package accesslog

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// flushTimeout 单批 INSERT 的超时上限。
//
// 独立于 server.shutdown_timeout(30s):那个管 HTTP 排空,本 Buffer 在它之后才关。
// 排空阶段最多 Capacity/BatchSize 批(默认 10000/100 = 100 批),且 Push 在停机后
// 直接丢条不再入队,所以批数有上界,不会无限循环。
const flushTimeout = 5 * time.Second

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
//
// channel 所有权(2026-08-26 修 panic):数据 channel ch **永不 close**,
// 关停只 close 独立的 stopCh。理由是 ch 有多个 sender —— 每个在飞请求结束时
// 都会经 Recorder.RecordAsync → Push 发一条,而 Go 里"关闭一个还有 sender 的
// channel"必然 panic(send on closed channel),无论加多少 closed 标志位都只是
// 缩小竞态窗口而非消除:检查和发送之间永远存在间隙。
// 隔壁 usage.Collector 一直是这个写法(close(stopCh),数据 channel 不关),
// 本 Buffer 是包内唯一的例外,现已对齐。
type Buffer struct {
	store *Store
	cfg   BufferConfig
	log   *zap.Logger

	ch chan *AccessEntry

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
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
		store:  store,
		cfg:    cfg,
		log:    zap.NewNop(),
		stopCh: make(chan struct{}),
		ch:     make(chan *AccessEntry, cfg.Capacity),
	}
}

// SetLogger 注入 zap logger(主路径需要看到丢条警告)
func (b *Buffer) SetLogger(l *zap.Logger) {
	if l != nil {
		b.log = l
	}
}

// Push 是 Recorder.RecordAsync 的核心;永不阻塞,且 Close 之后调用也安全。
//
// 两种丢条原因分开判、分开报,不合并成一个 select ——
// 若写成 `select { case <-b.stopCh: ...; case b.ch <- e: ...; default: }`,
// 停机后两个 case 同时就绪时 Go 随机挑一个,日志里会随机出现"buffer full",
// 排障时看起来像容量不够(去调 capacity),而真因是已经停机。
func (b *Buffer) Push(e *AccessEntry) {
	if e == nil {
		return
	}
	select {
	case <-b.stopCh:
		// 已停机:worker 不再消费,发进去也没人读 → 丢条并说明真因。
		// 正常关停顺序下这里不该有条目(HTTP 先排空完才关 Buffer);
		// 会落到这里 = 排空超时后仍有在飞请求,属于预期内的兜底。
		b.log.Warn("accesslog buffer closed, dropping entry",
			zap.String("trace_id", e.TraceID),
		)
		return
	default:
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

// Close 停 worker 并 flush 残余;可重入、可并发调用。
//
// stopOnce 保证 close(stopCh) 只执行一次 —— 旧实现用 Get/Set 两步检查,
// 两个并发 Close 可能都越过 guard 然后 double close 同一 channel(panic)。
// wg.Wait() 放在 Once 外面:并发调用者都要等 worker 真正退出才返回,
// 否则第二个 Close 会在残余还没落库时就宣告完成。
func (b *Buffer) Close() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})
	b.wg.Wait()
}

// run 是 flush worker。
//
// ctx 只用于 DB 操作,**不是**停止信号 —— 唯一的停止信号是 stopCh(即 Close())。
// 这点是这个 panic 的真正成因:原实现把 `<-ctx.Done()` 当退出条件,而 Start 拿到的
// 是请求生命周期 ctx,SIGTERM 一到它立刻 fire,worker 在 HTTP 排空**之前**就退出。
// 于是排空窗口里完成的请求 Push 进一个没人读的 channel(旧代码此时还 close 了它 → panic)。
// 现在 worker 活到 Close() 为止,配合 server.go 里"先排空 HTTP 再关 Buffer"的顺序,
// 在飞请求的日志才真正落库。
func (b *Buffer) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	// 落库用的基准 ctx 剥掉 cancel。
	//
	// Start 拿到的 ctx 在 SIGTERM 时立刻 cancel,而 worker 现在要活到 Close()
	// (跨越整个 HTTP 排空窗口)。若直接拿它发 INSERT,GORM 一律判 context canceled,
	// 三次重试全废 —— 那等于把 panic 换成静默丢数据,排空期间每一批日志都写不进去。
	// 只保留 value(trace 等),超时由每批自己的 flushTimeout 兜。
	baseCtx := context.WithoutCancel(ctx)

	// batch buffer
	batch := make([]*AccessEntry, 0, b.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		dbCtx, cancel := context.WithTimeout(baseCtx, flushTimeout)
		defer cancel()

		// M1 修复:best-effort 重试 3 次,但每次从成功偏移量继续——避免已插入的
		// 前 N 条在重试时被再次 Insert(auto-ID)造成重复日志行。
		offset := 0
		for i := 0; i < 3 && offset < len(batch); i++ {
			n, err := b.insertBatch(dbCtx, batch[offset:])
			if err != nil {
				if i == 2 {
					b.log.Error("accesslog batch insert failed",
						zap.Int("rows", len(batch[offset:])),
						zap.Error(err),
					)
				}
				time.Sleep(50 * time.Millisecond)
			}
			offset += n
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-b.ch:
			batch = append(batch, e)
			if len(batch) >= b.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.stopCh:
			b.drain(&batch, flush)
			return
		}
	}
}

// drain 把 channel 里剩下的条目全部收走并落库,然后返回(供 run 退出)。
//
// 关停时 channel 里可能还积着最多 Capacity(默认 10000)条已被接收的条目。
// 旧实现靠 close(ch) 让 range 自然读完;既然不再 close,就必须显式排空,
// 否则这些"已经答应要写"的日志会随进程一起消失。
func (b *Buffer) drain(batch *[]*AccessEntry, flush func()) {
	for {
		select {
		case e := <-b.ch:
			*batch = append(*batch, e)
			if len(*batch) >= b.cfg.BatchSize {
				flush()
			}
		default:
			flush()
			return
		}
	}
}

// insertBatch 调 Store.Insert,逐条插入(简单可靠);
// 如要更高吞吐可改成 GROUP INSERT,但当前 batch=100 已够用。
// 返回成功插入的条数(供 M1 重试从 offset 继续,避免已提交行被再插成重复)。
func (b *Buffer) insertBatch(ctx context.Context, batch []*AccessEntry) (int, error) {
	for i, e := range batch {
		if err := b.store.Insert(ctx, e); err != nil {
			return i, err
		}
	}
	return len(batch), nil
}
