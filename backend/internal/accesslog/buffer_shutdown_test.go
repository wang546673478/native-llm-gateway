package accesslog

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBuffer_PushAfterCloseDoesNotPanic 守卫:Close 之后 Push 不许 panic。
//
// 这是 2026-08-26 线上实录的崩溃(gin Recovery 兜住,但那条请求返 500 且日志丢失):
// 旧实现 Close() 里 close(b.ch),而 ch 有多个 sender —— 每个在飞请求结束时都会
// 经 Recorder.RecordAsync → Push 发一条。关停排空窗口里完成的请求撞上已关的
// channel,直接 send on closed channel。
//
// 加 closed 标志位**不能**修这个:检查和发送之间永远有间隙。唯一可靠的做法是
// 数据 channel 永不 close(只 close 独立的 stopCh),所以这个守卫盯的是
// "ch 不被 close"这个不变式,而不是某个标志位的存在。
func TestBuffer_PushAfterCloseDoesNotPanic(t *testing.T) {
	b, _ := newBufferWithStore(t, 100)
	b.Start(context.Background())
	b.Close()

	// 关停后仍有在飞请求投递(排空超时的正常情形)。不 panic 即通过。
	for i := 0; i < 100; i++ {
		b.Push(&AccessEntry{TraceID: "after-close"})
	}
}

// TestBuffer_ConcurrentPushDuringCloseDoesNotPanic 守卫:Push 与 Close 并发不许 panic。
//
// 比上一个更接近真实时序 —— 线上不是"先关完再 Push",而是关停与在飞请求收尾**同时**
// 发生。这个用例专门制造那个交叠窗口;-race 下跑能同时暴露数据竞争。
func TestBuffer_ConcurrentPushDuringCloseDoesNotPanic(t *testing.T) {
	b, _ := newBufferWithStore(t, 10)
	b.Start(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Push(&AccessEntry{TraceID: "racing"})
			}
		}()
	}

	time.Sleep(5 * time.Millisecond) // 让 Push 先跑起来,确保与 Close 交叠
	b.Close()
	wg.Wait()
}

// TestBuffer_ConcurrentCloseDoesNotPanic 守卫:并发 Close 不许 double close。
//
// 旧实现用 Get/Set 两步检查做 guard,两个并发 Close 可能都越过它然后对同一
// channel close 两次(panic)。recorder.go 的注释当年明确记录了这个已知隐患;
// 现在改用 sync.Once,这个守卫防止有人改回两步检查。
func TestBuffer_ConcurrentCloseDoesNotPanic(t *testing.T) {
	b, _ := newBufferWithStore(t, 100)
	b.Start(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Close()
		}()
	}
	wg.Wait()
}

// TestBuffer_WorkerSurvivesContextCancel 守卫:ctx cancel 不是停止信号。
//
// 这条是整个修复的核心。Start(ctx) 拿到的是请求生命周期 ctx,SIGTERM 一到就 cancel;
// 旧实现拿 `<-ctx.Done()` 当退出条件,于是 worker 在 HTTP 排空**之前**就退出,
// 排空窗口(最长 shutdown_timeout=30s)里完成的所有请求都没人消费。
//
// 断言:ctx cancel 后 Push 进来的条目照样落库。若有人把 ctx.Done() 加回 select,
// 这里会退化成 0 行。
func TestBuffer_WorkerSurvivesContextCancel(t *testing.T) {
	b, store := newBufferWithStore(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx)

	cancel()                          // 模拟 SIGTERM
	time.Sleep(20 * time.Millisecond) // 给"错误实现"足够时间退出

	// 排空窗口里完成的在飞请求
	for i := 0; i < 3; i++ {
		b.Push(&AccessEntry{TraceID: "in-flight-during-drain"})
	}
	b.Close() // 关停顺序:HTTP 排空完才走到这里

	rows, err := store.List(context.Background(), QueryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("ctx cancel 后落库 %d 行, want 3;"+
			"worker 不该把 ctx.Done() 当停止信号(否则排空窗口的日志全丢)", len(rows))
	}
}

// TestBuffer_FlushSucceedsAfterContextCancel 守卫:落库 ctx 必须剥离 cancel。
//
// 与上一个用例是两个独立的失效点,别合并:worker 活下来了,但如果 INSERT 还拿
// 那个已 cancel 的 ctx,GORM 一律判 context canceled,三次重试全废 —— 表现为
// "worker 在跑、日志却一条都不落",比 panic 更难查(没有任何报错线索)。
//
// 断言用 ticker 触发的普通 flush 路径(不是 Close 的 drain 路径):
// 排空窗口里 ticker 每 FlushInterval 就 flush 一次,这才是主要落库路径。
func TestBuffer_FlushSucceedsAfterContextCancel(t *testing.T) {
	b, store := newBufferWithStore(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx)
	defer b.Close()

	cancel()

	b.Push(&AccessEntry{TraceID: "flushed-by-ticker"})
	// FlushInterval=50ms(见 newBufferWithStore),等够 ticker 至少一轮。
	// 不调 Close —— 那会走 drain,测不到 ticker 路径。
	time.Sleep(300 * time.Millisecond)

	rows, err := store.List(context.Background(), QueryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ctx cancel 后 ticker flush 落库 %d 行, want 1;"+
			"落库 ctx 必须 WithoutCancel(否则 INSERT 全被判 context canceled)", len(rows))
	}
}

// TestBuffer_CloseDrainsBacklog 守卫:Close 必须排空 channel 里的积压。
//
// 旧实现靠 close(ch) 让 worker 把剩余读完;既然改成不 close,就必须显式 drain,
// 否则"已经收下但还没落库"的条目(最多 Capacity 条)会随进程消失。
// 这里故意积压到超过 BatchSize(=5),覆盖 drain 内部的分批 flush 分支。
func TestBuffer_CloseDrainsBacklog(t *testing.T) {
	b, store := newBufferWithStore(t, 100)

	// 先塞满再启动 worker,保证条目确实积压在 channel 里而非被即时消费
	const n = 12
	for i := 0; i < n; i++ {
		b.Push(&AccessEntry{TraceID: "backlog"})
	}
	b.Start(context.Background())
	b.Close()

	rows, err := store.List(context.Background(), QueryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != n {
		t.Errorf("Close 后落库 %d 行, want %d;drain 必须把 channel 积压读完", len(rows), n)
	}
}
