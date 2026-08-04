// Package quotacheck — Manager 协调 polling + probe 两条路径
package quotacheck

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/metrics"
)

// providerLookup 窄接口,Server.New 构造一个满足接口的对象传入
// 不直接 import `provider` 包(避免 cycle)
type providerLookup interface {
	EndpointFor(name string) string
}

// StaticProviderLookup 简单实现 — 构造时填入 provider name → endpoint 映射
type StaticProviderLookup struct {
	Endpoints map[string]string
}

func (s *StaticProviderLookup) EndpointFor(name string) string {
	if s == nil {
		return ""
	}
	return s.Endpoints[name]
}

// ManagerConfig 全部配置
type ManagerConfig struct {
	Enabled bool

	// Probe (无 balance API 的 provider 用)
	ProbeInitialDelay time.Duration // 第一次探测延迟(默认 5m)
	ProbeMaxBackoff   time.Duration // 探测间隔上限(默认 30m)
	ProbeJitterPct    int           // ±N% 抖动(默认 20)
	ProbeMaxAttempts  int           // 超过则永久 DISABLED(默认 8)

	// Poll (有 balance API 的 provider 用)
	PollInterval  time.Duration // 主动 poll 周期(默认 60s)
	PollJitterPct int           // ±N% 抖动(默认 10)

	// 通用
	HTTPTimeout time.Duration
	UserAgent   string

	// P-quota-balance: UI 显示余额颜色阈值(同 tier 桶内最大值的百分比);默认 10
	WarnThresholdPct int
}

// DefaultManagerConfig 兜底
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		Enabled:           true,
		ProbeInitialDelay: 5 * time.Minute,
		ProbeMaxBackoff:   30 * time.Minute,
		ProbeJitterPct:    20,
		ProbeMaxAttempts:  8,
		PollInterval:      60 * time.Second,
		PollJitterPct:     10,
		HTTPTimeout:       10 * time.Second,
		UserAgent:         "native-llm-gateway/quota-restore-1.0",
		WarnThresholdPct:  10,
	}
}

// PoolsRef pool map 引用 holder — Server.ReloadProviderPool 替换后 Manager 重新读
type PoolsRef struct {
	mu    sync.RWMutex
	pools map[string]*keypool.Pool
}

func NewPoolsRef(initial map[string]*keypool.Pool) *PoolsRef {
	return &PoolsRef{pools: initial}
}

func (r *PoolsRef) Set(pools map[string]*keypool.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools = pools
}

func (r *PoolsRef) Get() map[string]*keypool.Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*keypool.Pool, len(r.pools))
	for k, v := range r.pools {
		out[k] = v
	}
	return out
}

// Manager 主管理器
type Manager struct {
	logger    *zap.Logger
	cfg       ManagerConfig
	pools     *PoolsRef
	prov      providerLookup
	sched     *Scheduler
	metricsC  *metrics.Collector // 可选,nil 时 skip metrics emit

	// worker 生命周期 — Reload 时 cancel + 重新 Start
	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerMu     sync.Mutex

	// 测试可注入
	now   func() time.Time
	rand  *rand.Rand
	randM sync.Mutex
}

// NewManager 构造 Manager
// prov 用 StaticProviderLookup 包一下传入
// metricsC 可传 nil(测试 / 单测场景),nil 时不 emit
func NewManager(logger *zap.Logger, pools *PoolsRef, prov providerLookup, metricsC *metrics.Collector, cfg ManagerConfig) *Manager {
	if cfg.ProbeInitialDelay <= 0 {
		cfg.ProbeInitialDelay = 5 * time.Minute
	}
	if cfg.ProbeMaxBackoff <= 0 {
		cfg.ProbeMaxBackoff = 30 * time.Minute
	}
	if cfg.ProbeMaxAttempts <= 0 {
		cfg.ProbeMaxAttempts = 8
	}
	if cfg.ProbeJitterPct < 0 {
		cfg.ProbeJitterPct = 0
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.PollJitterPct < 0 {
		cfg.PollJitterPct = 0
	}
	if cfg.WarnThresholdPct <= 0 {
		cfg.WarnThresholdPct = 10
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "native-llm-gateway/quota-restore-1.0"
	}
	m := &Manager{
		logger:   logger,
		cfg:      cfg,
		pools:    pools,
		prov:     prov,
		sched:    NewScheduler(),
		metricsC: metricsC,
		now:      time.Now,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	// P68: 立即注入 callback — 即使 Manager.Start 还没跑(worker ticker 没起),
	// pool 事件(OnQuotaExceeded)也能 fire 并入堆。Start 调 rescanExisting 时
	// 会复用这些状态。修复一个 race:RegisterRoutes 早于 Server.Run,期间
	// mark-quota-exceeded 端点如果被调用,callback 不会丢失事件。
	m.injectCallbacks()
	return m
}

// Start 启动 polling + probe goroutines
// 注意: callback 已经在 NewManager 时注入,这里不再 inject(避免重复 log)
// 支持 Reload 重新调用 — 先 cancel 旧 worker 再启新的
func (m *Manager) Start(ctx context.Context) {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()

	// 停旧 worker
	if m.workerCancel != nil {
		m.logger.Info("quotacheck: stopping previous worker before Start")
		m.workerCancel()
	}

	if !m.cfg.Enabled {
		m.logger.Info("quotacheck.Manager disabled by config")
		return
	}

	// 冷启动:扫描现有 QUOTA_EXCEEDED keys(把已经在 QUOTA_EXCEEDED 状态的入堆)
	m.rescanExisting()

	workerCtx, cancel := context.WithCancel(ctx)
	m.workerCtx = workerCtx
	m.workerCancel = cancel
	go m.pollLoop(workerCtx)
	go m.probeLoop(workerCtx)
	m.logger.Info("quotacheck.Manager started",
		zap.Duration("probe_initial_delay", m.cfg.ProbeInitialDelay),
		zap.Duration("poll_interval", m.cfg.PollInterval),
	)
}

// injectCallbacks 给所有 Pool 设置 OnQuotaExceeded
// 也会在 ReloadProviderPool 后再次调用
func (m *Manager) injectCallbacks() {
	for name, pool := range m.pools.Get() {
		m.injectOneCallback(name, pool)
	}
	m.metricsSetPending(m.sched.pendingCount())
}

// injectOneCallback P68: 给单个 pool 注入 callback + 把 QUOTA_EXCEEDED 状态
// 重新入堆(用于 ReloadProviderPool 后)
// callback 签名:OnQuotaExceeded(*Key, KeyStatus) — 第二参数是状态变之前的 status
// 用来 emit transition metric(from → to)
func (m *Manager) injectOneCallback(providerName string, p *keypool.Pool) {
	p.OnQuotaExceeded = func(k *keypool.Key, fromStatus keypool.KeyStatus) {
		m.handleQuotaExceeded(providerName, k, fromStatus)
	}
	p.OnKeyRestored = func(k *keypool.Key) {
		m.logger.Info("key restored from quota_exceeded",
			zap.String("provider", providerName),
			zap.String("key_id", k.ID),
			zap.String("tier", k.BillingSource),
		)
	}
	p.OnKeyDisabled = func(k *keypool.Key) {
		m.logger.Info("key disabled after quota probe",
			zap.String("provider", providerName),
			zap.String("key_id", k.ID),
			zap.Int("attempts", k.QuotaProbeAttempts),
		)
	}
	// 把已经是 QUOTA_EXCEEDED 的 key 重新入堆(防止 reload 丢失状态)
	for _, k := range p.KeyPtrs() {
		if k.Status == keypool.KeyStatusQuotaExceeded {
			m.logger.Info("reinjecting quota_exceeded key after pool reload",
				zap.String("provider", providerName),
				zap.String("key_id", k.ID),
			)
			// reload 路径下 fromStatus 已经是 QUOTA_EXCEEDED — manager 内部会跳过 metric
			m.handleQuotaExceeded(providerName, k, keypool.KeyStatusQuotaExceeded)
		}
	}
}

// ReinjectCallback P68: Server.ReloadProviderPool 替换 pool 后调这个
// 把 callback 重新注入到新 pool + 重新入堆已有 QUOTA_EXCEEDED 的 key
func (m *Manager) ReinjectCallback(providerName string, p *keypool.Pool) {
	m.injectOneCallback(providerName, p)
	m.metricsSetPending(m.sched.pendingCount())
}

// rescanExisting 冷启动:把已有 QUOTA_EXCEEDED 的 key 立即入堆
func (m *Manager) rescanExisting() {
	for name, pool := range m.pools.Get() {
		for _, k := range pool.KeyPtrs() {
			if k.Status == keypool.KeyStatusQuotaExceeded {
				m.logger.Info("cold-start: scheduling existing quota_exceeded key",
					zap.String("provider", name),
					zap.String("key_id", k.ID),
				)
				// 冷启动: fromStatus 已经是 QUOTA_EXCEEDED — manager 内部会跳过 emit
				m.handleQuotaExceeded(name, k, keypool.KeyStatusQuotaExceeded)
			}
		}
	}
}

// handleQuotaExceeded 回调 — 把 key 入堆,首次延迟 = ProbeInitialDelay
// 同时 emit transition metric(从 fromStatus → QUOTA_EXCEEDED)
// fromStatus 来自 pool callback(状态变之前的值),不是当前 k.Status
func (m *Manager) handleQuotaExceeded(providerName string, k *keypool.Key, fromStatus keypool.KeyStatus) {
	if fromStatus != keypool.KeyStatusQuotaExceeded {
		// 真发生了 transition(从 active/cooling/disabled → quota_exceeded)
		m.metricsTransition(providerName, string(fromStatus), string(keypool.KeyStatusQuotaExceeded))
	}
	// 已经在 QUOTA_EXCEEDED(冷启动 rescan / ReinjectCallback)就不重复 emit
	nextAt := m.now().Add(m.jitter(m.cfg.ProbeInitialDelay, m.cfg.ProbeJitterPct))
	m.sched.scheduleKey(providerName, k.ID, nextAt, m.cfg.ProbeInitialDelay, 0)
	m.metricsSetPending(m.sched.pendingCount())
}

// handleProbeResult 探测完成后的状态机
func (m *Manager) handleProbeResult(providerName, keyID string, result Result) {
	pool, ok := m.pools.Get()[providerName]
	if !ok {
		return
	}
	// 在 pool.keys 里找匹配 key
	var k *keypool.Key
	for _, key := range pool.KeyPtrs() {
		if key.ID == keyID {
			k = key
			break
		}
	}
	if k == nil {
		return
	}
	fromStatus := string(k.Status)

	switch result {
	case ResultRestored:
		m.logger.Info("probe restored",
			zap.String("provider", providerName),
			zap.String("key_id", keyID),
		)
		pool.RestoreQuota(k)
		m.metricsTransition(providerName, fromStatus, string(keypool.KeyStatusActive))
		// 探测成功,从堆里移除
		m.sched.removeKey(providerName, keyID)

	case ResultStillExhausted:
		attempts := pool.IncQuotaProbeAttempts(k)
		if attempts >= m.cfg.ProbeMaxAttempts {
			m.logger.Info("probe exhausted max attempts, marking disabled",
				zap.String("provider", providerName),
				zap.String("key_id", keyID),
				zap.Int("attempts", attempts),
			)
			pool.MarkDisabledAfterQuota(k)
			m.metricsTransition(providerName, fromStatus, string(keypool.KeyStatusDisabled))
			m.sched.removeKey(providerName, keyID)
			return
		}
		// exp backoff, capped at ProbeMaxBackoff
		backoff := m.backoff(k.QuotaProbeAttempts)
		nextAt := m.now().Add(m.jitter(backoff, m.cfg.ProbeJitterPct))
		m.sched.scheduleKey(providerName, keyID, nextAt, backoff, k.QuotaProbeAttempts)

	case ResultAuthFailed:
		m.logger.Info("probe auth failed, marking disabled",
			zap.String("provider", providerName),
			zap.String("key_id", keyID),
		)
		pool.MarkDisabledAfterQuota(k)
		m.metricsTransition(providerName, fromStatus, string(keypool.KeyStatusDisabled))
		m.sched.removeKey(providerName, keyID)

	case ResultTransportError:
		// 网络问题,不消耗 attempt
		backoff := m.backoff(k.QuotaProbeAttempts + 1) // 比 still_exhausted 多 1
		nextAt := m.now().Add(m.jitter(backoff, m.cfg.ProbeJitterPct))
		m.sched.scheduleKey(providerName, keyID, nextAt, backoff, k.QuotaProbeAttempts)
	}
}

// metricsTransition 安全的 metrics 写入(metricsC 可能为 nil)
func (m *Manager) metricsTransition(provider, from, to string) {
	if m.metricsC == nil {
		return
	}
	m.metricsC.IncQuotaKeyTransition(provider, from, to)
}

// metricsProbeInc IncQuotaProbe,m nil 时 no-op
func (m *Manager) metricsProbeInc(provider, result string) {
	if m.metricsC == nil {
		return
	}
	m.metricsC.IncQuotaProbe(provider, result)
}

// metricsPollInc IncQuotaPoll,m nil 时 no-op
func (m *Manager) metricsPollInc(provider, result string) {
	if m.metricsC == nil {
		return
	}
	m.metricsC.IncQuotaPoll(provider, result)
}

// metricsSetPending n 可为 0
func (m *Manager) metricsSetPending(n int) {
	if m.metricsC == nil {
		return
	}
	m.metricsC.SetQuotaPendingProbes(n)
}

// backoff 第 n 次失败的延迟:initial * 2^(n-1),capped at max
func (m *Manager) backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	mult := math.Pow(2, float64(attempts-1))
	d := time.Duration(float64(m.cfg.ProbeInitialDelay) * mult)
	if d > m.cfg.ProbeMaxBackoff {
		d = m.cfg.ProbeMaxBackoff
	}
	if d < m.cfg.ProbeInitialDelay {
		d = m.cfg.ProbeInitialDelay
	}
	return d
}

// jitter 给 d 加 ±pct% 随机抖动
func (m *Manager) jitter(d time.Duration, pct int) time.Duration {
	if pct <= 0 {
		return d
	}
	m.randM.Lock()
	defer m.randM.Unlock()
	if m.rand == nil {
		return d // test 场景可能没初始化 rand
	}
	delta := float64(d) * float64(pct) / 100.0
	offset := (m.rand.Float64()*2 - 1) * delta
	return d + time.Duration(offset)
}

// probeLoop 每秒 pop 一次 due items
func (m *Manager) probeLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("probeLoop panic", zap.Any("err", r))
		}
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.drainDueProbes(ctx)
		}
	}
}

func (m *Manager) drainDueProbes(ctx context.Context) {
	now := m.now()
	items := m.sched.popDueItems(now)
	// 每次 pop 后更新 pending gauge(注意:这里是 pop 后的剩余值)
	defer m.metricsSetPending(m.sched.pendingCount())
	for _, it := range items {
		m.runProbe(ctx, it)
	}
}

func (m *Manager) runProbe(ctx context.Context, it *probeItem) {
	pools := m.pools.Get()
	pool, ok := pools[it.keyProvider]
	if !ok {
		return
	}
	// 在 pool 里找 key
	var k *keypool.Key
	for _, key := range pool.KeyPtrs() {
		if key.ID == it.keyID {
			k = key
			break
		}
	}
	if k == nil {
		return
	}
	// 只探测 QuotaExceeded key(其他状态跳过)
	if k.Status != keypool.KeyStatusQuotaExceeded {
		return
	}

	// 优先 Balancer
	balancer := LookupBalancer(it.keyProvider)
	if balancer != nil {
		baseURL := m.prov.EndpointFor(it.keyProvider)
		bal, err := balancer.FetchBalance(ctx, baseURL, k)
		if err != nil {
			m.logger.Warn("balance fetch err", zap.String("provider", it.keyProvider), zap.Error(err))
			m.metricsPollInc(it.keyProvider, "transport_error")
			m.handleProbeResult(it.keyProvider, it.keyID, ResultTransportError)
			return
		}
		if bal.HasQuota {
			m.metricsPollInc(it.keyProvider, "restored")
			m.handleProbeResult(it.keyProvider, it.keyID, ResultRestored)
		} else {
			m.metricsPollInc(it.keyProvider, "still_exhausted")
			m.handleProbeResult(it.keyProvider, it.keyID, ResultStillExhausted)
		}
		return
	}

	// 退到 Prober
	prober := LookupProber(it.keyProvider)
	if prober == nil {
		// 协议族 fallback
		provLower := it.keyProvider
		if endsWith(provLower, "-anthropic") {
			prober = LookupProber("__anthropic__")
		} else {
			prober = LookupProber("__openai__")
		}
	}
	if prober == nil {
		m.logger.Warn("no prober for provider", zap.String("provider", it.keyProvider))
		return
	}
	baseURL := m.prov.EndpointFor(it.keyProvider)
	result := prober.Probe(ctx, baseURL, k)
	// 记录 probe 结果
	switch result {
	case ResultRestored:
		m.metricsProbeInc(it.keyProvider, "restored")
	case ResultStillExhausted:
		m.metricsProbeInc(it.keyProvider, "still_exhausted")
	case ResultAuthFailed:
		m.metricsProbeInc(it.keyProvider, "auth_failed")
	case ResultTransportError:
		m.metricsProbeInc(it.keyProvider, "transport_error")
	}
	m.handleProbeResult(it.keyProvider, it.keyID, result)
}

func endsWith(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	return s[len(s)-len(sub):] == sub
}

// pollLoop 主动 poll — 每 PollInterval 跑一次所有有 Balancer 的 provider
func (m *Manager) pollLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("pollLoop panic", zap.Any("err", r))
		}
	}()

	// 首次延迟(带 jitter)
	initialDelay := m.jitter(m.cfg.PollInterval, m.cfg.PollJitterPct)
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.pollAllBalancers(ctx)
			interval := m.jitter(m.cfg.PollInterval, m.cfg.PollJitterPct)
			timer.Reset(interval)
		}
	}
}

// pollAllBalancers P68 + P-quota-balance:
//   - 主动轮询所有有 Balancer 的 provider
//   - 同 provider 内分 tier 块跑:先 token_plan,再 api,最后 free
//   - 每把 key 写 Remaining + LastPolledAt
//   - HasQuota=false 且当前 ACTIVE → 走 P68 ReportQuotaExceeded 转移状态
//   - HasQuota=true 且当前 QUOTA_EXCEEDED → 走 P68 RestoreQuota 恢复
//   - DISABLED key 跳过,无关
//   - 每把 key 之间 sleep 1 秒(由 ctx 可中断),不爆上游
func (m *Manager) pollAllBalancers(ctx context.Context) {
	// P-quota-balance:整个 pollAllBalancers 兜 panic recover。
	// 之前没有这一层,某个 key 的 FetchBalance panic 会杀掉整个 pollLoop 静默闭嘴。
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("pollAllBalancers panic", zap.Any("err", r))
		}
	}()
	for providerName, pool := range m.pools.Get() {
		balancer := LookupBalancer(providerName)
		if balancer == nil {
			continue
		}
		baseURL := m.prov.EndpointFor(providerName)
		for _, tier := range []string{"token_plan", "api", "free"} {
			for _, k := range pool.KeyPtrs() {
				effective := k.BillingSource
				if effective == "" {
					effective = "api"
				}
				if effective != tier {
					continue
				}
				if k.Status == keypool.KeyStatusDisabled {
					continue
				}

				bal, err := balancer.FetchBalance(ctx, baseURL, k)
				if err != nil {
					m.logger.Warn("poll balance err",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Error(err))
					m.metricsPollInc(providerName, "transport_error")
					continue
				}

				k.Remaining = bal.Raw
				k.QuotaKind = bal.Kind
				k.LastPolledAt = time.Now()

				switch {
				case !bal.HasQuota && k.Status == keypool.KeyStatusActive:
					m.logger.Info("poll: quota exhausted",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Float64("remaining", bal.Raw))
					pool.ReportQuotaExceeded(k)
					m.metricsPollInc(providerName, "exhausted")
				case bal.HasQuota && k.Status == keypool.KeyStatusQuotaExceeded:
					m.logger.Info("poll: quota restored",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Float64("remaining", bal.Raw))
					pool.RestoreQuota(k)
					m.metricsPollInc(providerName, "restored")
				default:
					m.metricsPollInc(providerName, "ok")
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
}

// Stop — cancel worker goroutines(等 ctx 结束)
func (m *Manager) Stop() {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if m.workerCancel != nil {
		m.workerCancel()
		m.workerCancel = nil
	}
	m.logger.Info("quotacheck.Manager stopped")
}

// WarnThresholdPct P-quota-balance: 返回当前生效的 warn_threshold_pct,供
// admin handler 暴露给前端(避免前端硬编码颜色阈值)。无锁读 — int 读取
// 在 Go 内存模型里是 atomic,容忍偶尔读到旧值。
func (m *Manager) WarnThresholdPct() int {
	return m.cfg.WarnThresholdPct
}

// Reload 接受新 cfg — 翻转 enabled 时 stop + 重新 Start(worker goroutine 重建)
// 其他 config 字段(interval, max attempts, backoff)生效于下次 Start。
// 注意:Start 接受 ctx 来自 Server.Run 顶层,Reload 没法拿 — 所以这里用 background
func (m *Manager) Reload(newCfg ManagerConfig) {
	if newCfg == (ManagerConfig{}) {
		return // zero value,忽略
	}
	prevEnabled := m.cfg.Enabled
	m.cfg = newCfg
	if prevEnabled == newCfg.Enabled {
		// 状态没变,但其他 config 字段(interval/backoff 等)更新 — 现有 worker
		// 用的旧 ticker 不会立即重读,要等下次 Start 才生效
		m.logger.Info("quotacheck.Manager reload: enabled unchanged, other fields updated (effective on next Start)",
			zap.Bool("enabled", newCfg.Enabled),
		)
		return
	}
	if !newCfg.Enabled {
		m.logger.Info("quotacheck.Manager reload: disabling")
		m.Stop()
		return
	}
	m.logger.Info("quotacheck.Manager reload: enabling")
	// Re-Start:context 用 background(因为 Server.Run 顶层 ctx 已经 cancel 不掉)
	m.Start(context.Background())
}
