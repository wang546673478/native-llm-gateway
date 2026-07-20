// Package metrics — proxy.MetricsRecorder 适配器
package metrics

import (
	"time"
)

// Adapter 把 metrics.Collector 适配到 proxy.MetricsRecorder 接口
type Adapter struct {
	c *Collector
}

// NewAdapter 构造 Adapter
func NewAdapter(c *Collector) *Adapter { return &Adapter{c: c} }

// RecordRequest 实现 proxy.MetricsRecorder
func (a *Adapter) RecordRequest(provider string, statusCode int, latency time.Duration, isStream bool, errorType string) {
	a.c.RecordRequest(provider, statusCode, latency, isStream, errorType)
}

// RecordTokens 把 token 计入 Prometheus(Proxy 在成功响应后调用)
func (a *Adapter) RecordTokens(provider string, input, output int) {
	a.c.RecordTokens(provider, input, output)
}
