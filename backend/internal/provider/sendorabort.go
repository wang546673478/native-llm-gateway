package provider

import "context"

// SendOrAbort 向流式 chunk channel 发送;若 ctx 已取消(客户端断开/代理停止排空),
// 不阻塞发送而是返回 false 让 producer 退出。
//
// 低耦合修复:三个协议 base 的流 producer 此前直接 `ch <- chunk`,当代理因客户端
// 断开而停止排空 buffer 时,producer 卡在 `ch <-`(buffer 满)永不退出 → goroutine
// 与其 httpResp.Body 永久泄漏。改用 select 感知 ctx.Done,消费者消失即退出 producer。
func SendOrAbort(ctx context.Context, ch chan<- *StreamChunk, c *StreamChunk) bool {
	select {
	case ch <- c:
		return true
	case <-ctx.Done():
		return false
	}
}
