// Package metrics 实现 Prometheus 指标收集
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector 持有所有 Prometheus 指标
type Collector struct {
	requestsTotal  *prometheus.CounterVec
	tokensTotal    *prometheus.CounterVec
	latencySecs    *prometheus.HistogramVec
	streamTTFTSecs *prometheus.HistogramVec
	relayEvents    *prometheus.CounterVec
	relayActive    *prometheus.GaugeVec
	// P68: quota restore metrics
	quotaProbeTotal     *prometheus.CounterVec // probe 调用结果
	quotaPollTotal      *prometheus.CounterVec // poll 调用结果
	quotaKeyTransitions *prometheus.CounterVec // 状态转换计数
	quotaPendingProbes  prometheus.Gauge       // 当前待探测 key 数
	registry            *prometheus.Registry
}

// NewCollector 构造 Collector
func NewCollector() *Collector {
	reg := prometheus.NewRegistry()
	c := &Collector{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total number of proxy requests, labeled by provider, status, is_stream, error_type.",
		}, []string{"provider", "status", "is_stream", "error_type"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tokens_total",
			Help: "Total number of tokens, labeled by provider and type (input/output).",
		}, []string{"provider", "type"}),
		latencySecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Request latency distribution.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"provider", "is_stream"}),
		streamTTFTSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_stream_ttft_seconds",
			Help:    "Streaming time-to-first response phase by provider, model, request size bucket and phase (body|ping|data).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90, 120, 150, 180, 240, 300},
		}, []string{"provider", "model", "request_size", "phase"}),
		relayEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_relay_events_total",
			Help: "Relay lifecycle events by provider, event and bounded stage.",
		}, []string{"provider", "event", "stage"}),
		relayActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_relay_active_upstreams",
			Help: "Current relay upstream attempts, including active response streams.",
		}, []string{"provider"}),
		// P68
		quotaProbeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_quota_probe_total",
			Help: "Quota probe attempts by provider and result (restored|still_exhausted|auth_failed|transport_error).",
		}, []string{"provider", "result"}),
		quotaPollTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_quota_poll_total",
			Help: "Quota poll attempts (balance API) by provider and result.",
		}, []string{"provider", "result"}),
		quotaKeyTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_quota_key_status_transitions_total",
			Help: "Quota-related key status transitions.",
		}, []string{"provider", "from", "to"}),
		quotaPendingProbes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gateway_quota_pending_probes",
			Help: "Number of keys currently waiting in the probe min-heap (PendingProbe scheduler size).",
		}),
		registry: reg,
	}
	reg.MustRegister(
		c.requestsTotal, c.tokensTotal, c.latencySecs, c.streamTTFTSecs, c.relayEvents, c.relayActive,
		c.quotaProbeTotal, c.quotaPollTotal, c.quotaKeyTransitions, c.quotaPendingProbes,
	)
	return c
}

func (c *Collector) RecordRelayEvent(provider, event, stage string) {
	c.relayEvents.WithLabelValues(provider, event, stage).Inc()
}

func (c *Collector) AddRelayActiveUpstreams(provider string, delta int) {
	c.relayActive.WithLabelValues(provider).Add(float64(delta))
}

func (c *Collector) RecordStreamTTFT(provider, model, requestSize, phase string, duration time.Duration) {
	c.streamTTFTSecs.With(prometheus.Labels{
		"provider": provider, "model": model, "request_size": requestSize, "phase": phase,
	}).Observe(duration.Seconds())
}

// RecordRequest 记录一次请求
// 注意:这个方法要极快,不能阻塞 Proxy 主流程
func (c *Collector) RecordRequest(provider string, statusCode int, latency time.Duration, isStream bool, errorType string) {
	labels := prometheus.Labels{
		"provider":   provider,
		"status":     strconv.Itoa(statusCode),
		"is_stream":  strconv.FormatBool(isStream),
		"error_type": errorType,
	}
	c.requestsTotal.With(labels).Inc()
	c.latencySecs.With(prometheus.Labels{
		"provider":  provider,
		"is_stream": strconv.FormatBool(isStream),
	}).Observe(latency.Seconds())
}

// RecordTokens 记录 token 用量(在 SendRequest 拿到 Usage 时调用)
func (c *Collector) RecordTokens(provider string, input, output int) {
	if input > 0 {
		c.tokensTotal.With(prometheus.Labels{"provider": provider, "type": "input"}).Add(float64(input))
	}
	if output > 0 {
		c.tokensTotal.With(prometheus.Labels{"provider": provider, "type": "output"}).Add(float64(output))
	}
}

// P68: 记录一次 probe 结果
// result ∈ {"restored", "still_exhausted", "auth_failed", "transport_error"}
func (c *Collector) IncQuotaProbe(provider, result string) {
	c.quotaProbeTotal.With(prometheus.Labels{"provider": provider, "result": result}).Inc()
}

// P68: 记录一次 poll 结果
func (c *Collector) IncQuotaPoll(provider, result string) {
	c.quotaPollTotal.With(prometheus.Labels{"provider": provider, "result": result}).Inc()
}

// P68: 记录一次 quota-related 状态转换
// from/to ∈ {"active", "quota_exceeded", "cooling"} (P-no-disabled: 无 DISABLED 状态)
func (c *Collector) IncQuotaKeyTransition(provider, from, to string) {
	c.quotaKeyTransitions.With(prometheus.Labels{
		"provider": provider,
		"from":     from,
		"to":       to,
	}).Inc()
}

// P68: 设置当前待探测 key 数
func (c *Collector) SetQuotaPendingProbes(n int) {
	c.quotaPendingProbes.Set(float64(n))
}

// Handler 返回 /metrics HTTP handler
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}

// Registry 返回底层 registry(供测试或合并到全局)
func (c *Collector) Registry() *prometheus.Registry { return c.registry }

// silence unused
var _ = sync.Mutex{}
