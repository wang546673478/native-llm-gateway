// Package usage — usage.Collector 的一个薄适配
package usage

// Adapter 把 usage.Collector 暴露成可注入 proxy 的 Record 方法
// 单一职责:与 proxy 解耦 — 不再 import proxy 包,直接收 *Record(与 proxy.UsageRecord 是同一类型)
type Adapter struct {
	c *Collector
}

// NewAdapter 构造 Adapter
func NewAdapter(c *Collector) *Adapter { return &Adapter{c: c} }

// Record 转发到 Collector(proxy.UsageRecorder 接口用 type alias 指向 *Record)
func (a *Adapter) Record(r *Record) {
	a.c.Record(r)
}
