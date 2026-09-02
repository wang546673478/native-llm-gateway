// Package proxy 实现 LLM Gateway 的代理引擎。
package proxy

import (
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/usage"
)

// UsageRecord 用量记录 — type alias 指向 usage.Record
// 单一职责:数据结构归 usage 包所有,proxy 只引用。消除 usage→proxy 反向依赖
// (之前 UsageRecord 定义在 proxy,usage/adapter.go 反向 import proxy,违反低耦合)
type UsageRecord = usage.Record

// UsageRecorder 用量记录钩子
// P5 阶段用 NoopUsageRecorder,P8 接入 usage.Collector
type UsageRecorder interface {
	Record(r *UsageRecord)
}

// MetricsRecorder 指标钩子(简化接口)
type MetricsRecorder interface {
	RecordRequest(provider string, statusCode int, latency time.Duration, isStream bool, errorType string)
}

// StreamTTFTRecorder is optional so alternate/test MetricsRecorder
// implementations do not need to change. Prometheus implements it to expose
// first body, first ping and first data distributions for relay budgeting.
type StreamTTFTRecorder interface {
	RecordStreamTTFT(provider, model, requestSize, phase string, duration time.Duration)
}

// RelayEventRecorder is optional and exposes bounded relay lifecycle events.
// Event and stage values are code-defined enums, never request-derived data.
type RelayEventRecorder interface {
	RecordRelayEvent(provider, event, stage string)
	AddRelayActiveUpstreams(provider string, delta int)
}

// NoopUsageRecorder / NoopMetricsRecorder 默认 no-op 实现
// P-per-key-circuit: CircuitReporter 已移除 — 熔断器下沉到 keypool(per-key),
// 由 Pool.ReportSuccess / ReportError(server_error|timeout|connection)内部上报

type NoopUsageRecorder struct{}

func (NoopUsageRecorder) Record(*UsageRecord) {}

type NoopMetricsRecorder struct{}

func (NoopMetricsRecorder) RecordRequest(string, int, time.Duration, bool, string) {}
func (NoopMetricsRecorder) RecordStreamTTFT(string, string, string, string, time.Duration) {
}
func (NoopMetricsRecorder) RecordRelayEvent(string, string, string) {}
func (NoopMetricsRecorder) AddRelayActiveUpstreams(string, int)     {}

// errorIsRetryable 集中判断错误是否触发 failover
func errorIsRetryable(pe *provider.ProviderError) bool {
	return pe != nil && pe.IsRetryable()
}

// TokenUsageRecorder TPM 计数回调(P13)
// 在拿到 Provider.Usage 后由 Engine 回调,用于客户端 TPM 限流
type TokenUsageRecorder interface {
	RecordUsage(keyID string, tokens int64)
}
