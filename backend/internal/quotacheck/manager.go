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
	logger *zap.Logger
	cfg    ManagerConfig
	pools  *PoolsRef
	prov   providerLookup
	sched  *Scheduler

	// 测试可注入
	now   func() time.Time
	rand  *rand.Rand
	randM sync.Mutex
}

// NewManager 构造 Manager
// prov 用 StaticProviderLookup 包一下传入
func NewManager(logger *zap.Logger, pools *PoolsRef, prov providerLookup, cfg ManagerConfig) *Manager {
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
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "native-llm-gateway/quota-restore-1.0"
	}
	return &Manager{
		logger: logger,
		cfg:    cfg,
		pools:  pools,
		prov:   prov,
		sched:  NewScheduler(),
		now:    time.Now,
		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Start 启动 polling + probe goroutines
// 注入回调到所有 Pool,扫描已有的 QUOTA_EXCEEDED key
func (m *Manager) Start(ctx context.Context) {
	if !m.cfg.Enabled {
		m.logger.Info("quotacheck.Manager disabled by config")
		return
	}

	// 注入回调
	m.injectCallbacks()
	// 冷启动:扫描现有 QUOTA_EXCEEDED keys
	m.rescanExisting()

	go m.pollLoop(ctx)
	go m.probeLoop(ctx)
	m.logger.Info("quotacheck.Manager started",
		zap.Duration("probe_initial_delay", m.cfg.ProbeInitialDelay),
		zap.Duration("poll_interval", m.cfg.PollInterval),
	)
}

// injectCallbacks 给所有 Pool 设置 OnQuotaExceeded
// 也会在 ReloadProviderPool 后再次调用
func (m *Manager) injectCallbacks() {
	for name, pool := range m.pools.Get() {
		providerName := name
		p := pool
		p.OnQuotaExceeded = func(k *keypool.Key) {
			m.handleQuotaExceeded(providerName, k)
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
	}
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
				m.handleQuotaExceeded(name, k)
			}
		}
	}
}

// handleQuotaExceeded 回调 — 把 key 入堆,首次延迟 = ProbeInitialDelay
func (m *Manager) handleQuotaExceeded(providerName string, k *keypool.Key) {
	nextAt := m.now().Add(m.jitter(m.cfg.ProbeInitialDelay, m.cfg.ProbeJitterPct))
	m.sched.scheduleKey(providerName, k.ID, nextAt, m.cfg.ProbeInitialDelay, 0)
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

	switch result {
	case ResultRestored:
		m.logger.Info("probe restored",
			zap.String("provider", providerName),
			zap.String("key_id", keyID),
		)
		pool.RestoreQuota(k)
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
		m.sched.removeKey(providerName, keyID)

	case ResultTransportError:
		// 网络问题,不消耗 attempt
		backoff := m.backoff(k.QuotaProbeAttempts + 1) // 比 still_exhausted 多 1
		nextAt := m.now().Add(m.jitter(backoff, m.cfg.ProbeJitterPct))
		m.sched.scheduleKey(providerName, keyID, nextAt, backoff, k.QuotaProbeAttempts)
	}
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
			m.logger.Debug("balance fetch err", zap.String("provider", it.keyProvider), zap.Error(err))
			m.handleProbeResult(it.keyProvider, it.keyID, ResultTransportError)
			return
		}
		if bal.HasQuota {
			m.handleProbeResult(it.keyProvider, it.keyID, ResultRestored)
		} else {
			// balance == 0 → still exhausted
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

func (m *Manager) pollAllBalancers(ctx context.Context) {
	for providerName, pool := range m.pools.Get() {
		balancer := LookupBalancer(providerName)
		if balancer == nil {
			continue // 没 Balancer 的不主动 poll
		}
		baseURL := m.prov.EndpointFor(providerName)
		for _, k := range pool.KeyPtrs() {
			if k.Status != keypool.KeyStatusQuotaExceeded {
				continue
			}
			bal, err := balancer.FetchBalance(ctx, baseURL, k)
			if err != nil {
				m.logger.Debug("poll balance err", zap.String("provider", providerName), zap.Error(err))
				continue
			}
			if bal.HasQuota {
				m.logger.Info("poll restored",
					zap.String("provider", providerName),
					zap.String("key_id", k.ID),
					zap.Float64("balance", bal.Raw),
				)
				pool.RestoreQuota(k)
			}
		}
	}
}

// Stop — 暂时不需要(cancel 通过 ctx 走),保留供未来 graceful shutdown
func (m *Manager) Stop() {
	m.logger.Info("quotacheck.Manager stopped")
}
