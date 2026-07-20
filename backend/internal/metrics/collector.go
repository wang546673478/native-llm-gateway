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
	requestsTotal *prometheus.CounterVec
	tokensTotal   *prometheus.CounterVec
	latencySecs   *prometheus.HistogramVec
	registry      *prometheus.Registry
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
		registry: reg,
	}
	reg.MustRegister(c.requestsTotal, c.tokensTotal, c.latencySecs)
	return c
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

// Handler 返回 /metrics HTTP handler
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}

// Registry 返回底层 registry(供测试或合并到全局)
func (c *Collector) Registry() *prometheus.Registry { return c.registry }

// silence unused
var _ = sync.Mutex{}
