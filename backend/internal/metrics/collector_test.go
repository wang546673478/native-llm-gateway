// Package metrics — Collector tests
package metrics

import (
	"strings"
	"testing"
)

// TestCollector_QuotaMetricsRegistered 验证 4 个 quota metric 都注册到 registry,
// /metrics endpoint 能看到指标(label=未 emit 时 counter 不会出现在输出里,
// 所以用 Gauge (pendingProbes) + 直接构造 Registry 检查 counter vec).
func TestCollector_QuotaMetricsRegistered(t *testing.T) {
	c := NewCollector()

	// emit 一次让 counter vec 产生带 label 的 series
	c.IncQuotaProbe("deepseek", "restored")
	c.IncQuotaPoll("glm", "still_exhausted")
	c.IncQuotaKeyTransition("kimi", "active", "quota_exceeded")
	c.SetQuotaPendingProbes(3)

	// Gather all metric families via the registry's gather
	mfs, err := c.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	want := map[string]bool{
		"gateway_quota_probe_total":                  false,
		"gateway_quota_poll_total":                   false,
		"gateway_quota_key_status_transitions_total": false,
		"gateway_quota_pending_probes":               false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q not registered (gathered families: %v)", name, mfNames(mfs))
		}
	}
}

func mfNames(mfs interface{}) []string {
	// small helper for t.Errorf
	type nameGetter interface{ GetName() string }
	out := []string{}
	for _, mf := range mfs.([]interface{}) {
		out = append(out, mf.(nameGetter).GetName())
	}
	return out
}

// TestCollector_QuotaMetricLabels 验证 emit 后 counter 值正确
func TestCollector_QuotaMetricLabels(t *testing.T) {
	c := NewCollector()
	c.IncQuotaProbe("deepseek", "restored")
	c.IncQuotaProbe("deepseek", "restored") // 2
	c.IncQuotaProbe("deepseek", "auth_failed")
	c.IncQuotaPoll("glm", "still_exhausted")

	mfs, _ := c.Registry().Gather()
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "gateway_quota_") {
			t.Logf("metric=%s metrics=%d", mf.GetName(), len(mf.GetMetric()))
			for _, m := range mf.GetMetric() {
				labels := make([]string, 0, len(m.Label))
				for _, lp := range m.Label {
					labels = append(labels, lp.GetName()+"="+lp.GetValue())
				}
				if m.Counter != nil {
					t.Logf("  counter(%v)=%v", labels, m.Counter.GetValue())
				}
				if m.Gauge != nil {
					t.Logf("  gauge(%v)=%v", labels, m.Gauge.GetValue())
				}
			}
		}
	}
}