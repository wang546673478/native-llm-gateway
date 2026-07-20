// Package usage — proxy.UsageRecorder 适配器
package usage

import (
	"github.com/wang546673478/native-llm-gateway/internal/proxy"
)

// Adapter 把 usage.Collector 适配到 proxy.UsageRecorder 接口
type Adapter struct {
	c *Collector
}

// NewAdapter 构造 Adapter
func NewAdapter(c *Collector) *Adapter { return &Adapter{c: c} }

// Record 实现 proxy.UsageRecorder
func (a *Adapter) Record(r *proxy.UsageRecord) {
	a.c.Record(&Record{
		TraceID:      r.TraceID,
		GatewayKeyID: r.GatewayKeyID,
		ProviderName: r.ProviderName,
		ModelID:      r.ModelID,
		Protocol:     r.Protocol,
		LatencyMs:    r.LatencyMs,
		StatusCode:   r.StatusCode,
		ErrorType:    r.ErrorType,
		IsStream:     r.IsStream,
		// InputTokens / OutputTokens / Cost 由 Proxy 在拿到 resp.Usage 后补
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		TotalTokens:  r.TotalTokens,
		Cost:         r.Cost,
	})
}
