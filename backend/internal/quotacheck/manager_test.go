// Package quotacheck — Manager unit tests
package quotacheck

import (
	"context"
	"math/rand"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/metrics"
)

// fakeProviderLookup 测试用,无外部依赖
type fakeProviderLookup struct {
	endpoints map[string]string
}

func (f *fakeProviderLookup) EndpointFor(name string) string {
	return f.endpoints[name]
}

// stubBalancer 简单 stub,可配置返回
type stubBalancer struct {
	mu      sync.Mutex
	calls   int32
	balance Balance
	err     error
}

func (s *stubBalancer) FetchBalance(_ context.Context, _ string, _ *keypool.Key) (Balance, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balance, s.err
}

// stubProber 简单 stub
type stubProber struct {
	mu     sync.Mutex
	calls  int32
	result Result
}

func (s *stubProber) Probe(_ context.Context, _ string, _ *keypool.Key) Result {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

func newTestPool(t *testing.T) *keypool.Pool {
	t.Helper()
	now := time.Now()
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now, BillingSource: "api"},
	}
	return keypool.NewPool("test", keys, nil, keypool.Config{})
}

func TestManagerConfig_WarnThresholdPctDefault(t *testing.T) {
	c := DefaultManagerConfig()
	if c.WarnThresholdPct != 10 {
		t.Errorf("Default WarnThresholdPct = %d, want 10", c.WarnThresholdPct)
	}
}

// Test 1: 探测结果 Restored → pool.RestoreQuota 被调
func TestManager_ProbeRestoredCallsRestoreQuota(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")
	if keys[0].Status != keypool.KeyStatusQuotaExceeded {
		t.Fatalf("setup: want QUOTA_EXCEEDED, got %s", keys[0].Status)
	}

	// Manually trigger the result handler — same as probeLoop would
	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools:  NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.handleProbeResult("test", "1", ResultRestored)

	if keys[0].Status != keypool.KeyStatusActive {
		t.Errorf("after restored: status = %s, want ACTIVE", keys[0].Status)
	}
}

// Test 2: P-no-disabled: StillExhausted → 保持 QE + backoff 重调度,永不 DISABLED
func TestManager_StillExhaustedStaysQE(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	cfg := DefaultManagerConfig()
	cfg.ProbeInitialDelay = 100 * time.Millisecond
	cfg.ProbeMaxBackoff = 500 * time.Millisecond
	cfg.ProbeJitterPct = 0

	m := &Manager{
		logger: zap.NewNop(),
		cfg:    cfg,
		pools:  NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.rand = rand.New(rand.NewSource(42))

	// 反复 still_exhausted(远超旧的 max attempts=8)→ 仍保持 QE,不 DISABLED
	for i := 0; i < 10; i++ {
		m.handleProbeResult("test", "1", ResultStillExhausted)
	}
	if keys[0].Status != keypool.KeyStatusQuotaExceeded {
		t.Errorf("after repeated still_exhausted: status = %s, want QUOTA_EXCEEDED (no DISABLED)", keys[0].Status)
	}
	if m.sched.pendingCount() != 1 {
		t.Errorf("pending = %d, want 1 (still rescheduled)", m.sched.pendingCount())
	}
}

// Test 3: P-no-disabled: AuthFailed → 保持 QE + 重调度,不 DISABLED(换 key 后自动恢复)
func TestManager_AuthFailedStaysQE(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools:  NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.handleProbeResult("test", "1", ResultAuthFailed)

	if keys[0].Status != keypool.KeyStatusQuotaExceeded {
		t.Errorf("after auth_failed: status = %s, want QUOTA_EXCEEDED (no DISABLED)", keys[0].Status)
	}
	if m.sched.pendingCount() != 1 {
		t.Errorf("pending = %d, want 1 (still rescheduled)", m.sched.pendingCount())
	}
}

// Test 4: TransportError → 不消耗 attempt,继续入堆
func TestManager_TransportErrorNoAttemptsConsumed(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools:  NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.handleProbeResult("test", "1", ResultTransportError)
	m.handleProbeResult("test", "1", ResultTransportError)
	m.handleProbeResult("test", "1", ResultTransportError)

	// 状态应保持 QUOTA_EXCEEDED(没到 max attempts → 没 DISABLED)
	if keys[0].Status != keypool.KeyStatusQuotaExceeded {
		t.Errorf("after transport errs: status = %s, want QUOTA_EXCEEDED", keys[0].Status)
	}
	// QuotaProbeAttempts 应保持 0
	if keys[0].QuotaProbeAttempts != 0 {
		t.Errorf("QuotaProbeAttempts = %d, want 0 (transport err 不消耗)", keys[0].QuotaProbeAttempts)
	}
}

// Test 5: Backoff 公式 — 10ms initial,attempts=1/2/3 → 10/20/40ms,capped at max
func TestManager_BackoffExponential(t *testing.T) {
	cfg := DefaultManagerConfig()
	cfg.ProbeInitialDelay = 10 * time.Millisecond
	cfg.ProbeMaxBackoff = 80 * time.Millisecond
	cfg.ProbeJitterPct = 0
	m := &Manager{cfg: cfg}

	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},  // capped
		{10, 80 * time.Millisecond}, // capped
	}
	for _, c := range cases {
		got := m.backoff(c.attempts)
		if got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// Test 6: cold-start rescanExisting 把已有 QUOTA_EXCEEDED key 入堆
func TestManager_RescanExistingSchedulesQuotaExceeded(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
		{ID: "2", Name: "k2", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded") // k1 标 quota_exceeded

	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools:  NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.rescanExisting()

	if m.sched.pendingCount() != 1 {
		t.Errorf("sched.pendingCount = %d, want 1", m.sched.pendingCount())
	}
}

// helpers

var _ sync.Mutex // keep sync import used even if not directly referenced

// TestManager_MetricsEmittedOnRestore 验证 metricsC 收到 IncQuotaKeyTransition
func TestManager_MetricsEmittedOnRestore(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	mc := metrics.NewCollector()
	m := &Manager{
		logger:   zap.NewNop(),
		cfg:      DefaultManagerConfig(),
		pools:    NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:     &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:    NewScheduler(),
		now:      time.Now,
		metricsC: mc,
	}

	m.handleProbeResult("test", "1", ResultRestored)

	// gather metrics,看 transition counter
	mfs, _ := mc.Registry().Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "gateway_quota_key_status_transitions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labelStr := ""
			for _, lp := range m.Label {
				labelStr += lp.GetName() + "=" + lp.GetValue() + " "
			}
			if m.Counter != nil && m.Counter.GetValue() > 0 {
				found = true
				t.Logf("transition: %s -> %v", labelStr, m.Counter.GetValue())
			}
		}
	}
	if !found {
		t.Errorf("expected a quota transition counter to fire on Restored")
	}
}

// TestManager_MetricsNilSafe 验证 metricsC=nil 时所有路径不 panic
func TestManager_MetricsNilSafe(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("test", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	m := &Manager{
		logger:   zap.NewNop(),
		cfg:      DefaultManagerConfig(),
		pools:    NewPoolsRef(map[string]*keypool.Pool{"test": pool}),
		prov:     &fakeProviderLookup{endpoints: map[string]string{"test": "http://x"}},
		sched:    NewScheduler(),
		now:      time.Now,
		metricsC: nil, // 显式 nil
	}

	// 不应 panic
	m.handleQuotaExceeded("test", keys[0], keypool.KeyStatusActive)
	m.handleProbeResult("test", "1", ResultRestored)
	m.handleProbeResult("test", "1", ResultStillExhausted)
}

// fakeBalancer records the call order of FetchBalance
type fakeBalancer struct {
	calls []string // 记录被调用的 key ID
	bal   Balance  // 所有 key 都返同一个 Balance
	err   error
}

func (f *fakeBalancer) FetchBalance(_ context.Context, _ string, k *keypool.Key) (Balance, error) {
	f.calls = append(f.calls, k.ID)
	return f.bal, f.err
}

func TestPollAllBalancers_TierBlocked(t *testing.T) {
	// pool: 2 token_plan + 1 api + 1 free = 4 key
	// 期望调用顺序:[token_plan × 2, api × 1, free × 1]
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "free-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "free"},
		{ID: "tp-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "tp-2", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "api-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "api"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: Balance{HasQuota: true, Raw: 1.0, Kind: "percent"}}
	originalReg := balancerRegistry["p"]
	RegisterBalancer("p", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["p"] = originalReg
		} else {
			delete(balancerRegistry, "p")
		}
	})

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())

	m.pollAllBalancers(context.Background())

	want := []string{"tp-1", "tp-2", "api-1", "free-1"}
	if !reflect.DeepEqual(b.calls, want) {
		t.Errorf("calls = %v, want %v (tier-blocked order)", b.calls, want)
	}
	// Remaining 应被填入
	for _, k := range pool.KeyPtrs() {
		if k.Remaining != 1.0 {
			t.Errorf("%s Remaining = %v, want 1.0", k.ID, k.Remaining)
		}
		if k.LastPolledAt.IsZero() {
			t.Errorf("%s LastPolledAt not set", k.ID)
		}
		// P-quota-display: QuotaKind 应随 poll 写入(pipeline: balancer → Key)
		if k.QuotaKind != "percent" {
			t.Errorf("%s QuotaKind = %q, want %q", k.ID, k.QuotaKind, "percent")
		}
	}
}

func TestPollAllBalancers_PolledAllStatusesNotJustQuotaExceeded(t *testing.T) {
	// 现在 ACTIVE key 也被轮询
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "active-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "quota-1", ProviderName: "p", Status: keypool.KeyStatusQuotaExceeded, BillingSource: "token_plan"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: Balance{HasQuota: true, Raw: 7.7}}
	originalReg := balancerRegistry["p"]
	RegisterBalancer("p", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["p"] = originalReg
		} else {
			delete(balancerRegistry, "p")
		}
	})

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())
	m.pollAllBalancers(context.Background())

	if len(b.calls) != 2 {
		t.Errorf("calls = %d, want 2 (both ACTIVE and QUOTA_EXCEEDED should be polled)", len(b.calls))
	}
	// P-quota-display: balancer 未上报 Kind → QuotaKind 写 ""(兼容路径,前端按 currency)
	for _, k := range pool.KeyPtrs() {
		if k.QuotaKind != "" {
			t.Errorf("%s QuotaKind = %q, want empty (no Kind reported)", k.ID, k.QuotaKind)
		}
	}
}

func TestPollAllBalancers_HasQuotaFalseOnActivePushedToQuotaExceeded(t *testing.T) {
	// HasQuota=false 且 Status=ACTIVE → 自动 ReportQuotaExceeded
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "active-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: Balance{HasQuota: false, Raw: 0}}
	originalReg := balancerRegistry["p"]
	RegisterBalancer("p", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["p"] = originalReg
		} else {
			delete(balancerRegistry, "p")
		}
	})

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())
	m.pollAllBalancers(context.Background())

	got := pool.KeyPtrs()[0].Status
	if got != keypool.KeyStatusQuotaExceeded {
		t.Errorf("Status after poll = %s, want QUOTA_EXCEEDED", got)
	}
}

func TestPollAllBalancers_DedupSharedPool(t *testing.T) {
	// P-provider-vendor: deepseek / deepseek-anthropic 共享同一 pool →
	// 轮询按 pool 指针去重,balance 查询每轮只发生一次
	pool := newTestPool(t) // 1 个 api tier key
	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools: NewPoolsRef(map[string]*keypool.Pool{
			"deepseek":           pool,
			"deepseek-anthropic": pool,
		}),
		prov: &fakeProviderLookup{endpoints: map[string]string{
			"deepseek":           "http://x",
			"deepseek-anthropic": "http://y",
		}},
		sched: NewScheduler(),
		now:   time.Now,
	}

	b := &stubBalancer{balance: Balance{HasQuota: true, Raw: 50, Kind: "currency"}}
	originalReg := balancerRegistry["deepseek"]
	RegisterBalancer("deepseek", b)
	RegisterBalancer("deepseek-anthropic", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["deepseek"] = originalReg
		} else {
			delete(balancerRegistry, "deepseek")
		}
		delete(balancerRegistry, "deepseek-anthropic")
	})

	m.pollAllBalancers(context.Background())

	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Fatalf("balancer calls = %d, want 1 (shared pool dedup)", got)
	}
}

// P-quota-probe-disable: 有 balancer 的 provider 的 QE key 不能被探测次数禁掉 —
// poll(60s) 才是恢复通道,充值/续费后自动 ACTIVE;DISABLED 没有恢复路径
// (2026-08-05 实测:weige 0% 余额被 probe 8 次 → 永久 DISABLED,续费也救不回)
func TestManager_StillExhaustedWithBalancerNotDisabled(t *testing.T) {
	keys := []*keypool.Key{
		{ID: "1", Name: "k1", Status: keypool.KeyStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(), BillingSource: "api"},
	}
	pool := keypool.NewPool("baltest", keys, nil, keypool.Config{})
	pool.ReportError(keys[0], "quota_exceeded")

	b := &fakeBalancer{bal: Balance{HasQuota: false, Raw: 0}}
	originalReg := balancerRegistry["baltest"]
	RegisterBalancer("baltest", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["baltest"] = originalReg
		} else {
			delete(balancerRegistry, "baltest")
		}
	})

	cfg := DefaultManagerConfig()
	cfg.ProbeInitialDelay = 100 * time.Millisecond
	cfg.ProbeMaxBackoff = 500 * time.Millisecond
	cfg.ProbeJitterPct = 0

	m := &Manager{
		logger: zap.NewNop(),
		cfg:    cfg,
		pools:  NewPoolsRef(map[string]*keypool.Pool{"baltest": pool}),
		prov:   &fakeProviderLookup{endpoints: map[string]string{"baltest": "http://x"}},
		sched:  NewScheduler(),
		now:    time.Now,
	}
	m.rand = rand.New(rand.NewSource(42))

	// 超过 max attempts 多次仍不 DISABLED
	for i := 0; i < 5; i++ {
		m.handleProbeResult("baltest", "1", ResultStillExhausted)
	}
	if keys[0].Status != keypool.KeyStatusQuotaExceeded {
		t.Errorf("after repeated still_exhausted with balancer: status = %s, want QUOTA_EXCEEDED (not DISABLED)", keys[0].Status)
	}
}
