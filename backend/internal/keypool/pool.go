// Package keypool — Pool:管理单个 Provider 下的所有 Key
// 对应规格书 5.4 Key Pool
package keypool

import (
	"sync"
	"time"
)

// Config Pool 配置
type Config struct {
	CoolingDuration time.Duration // 默认冷却时长
	MaxCoolingCount int           // 累计超过此值则 DISABLED
}

// Pool 管理一个 Provider 下的所有 Key
type Pool struct {
	ProviderName string
	cfg          Config
	mu           sync.RWMutex
	keys         []*Key
	scheduler    Scheduler
}

// NewPool 构造 Pool
func NewPool(providerName string, keys []*Key, scheduler Scheduler, cfg Config) *Pool {
	if cfg.CoolingDuration <= 0 {
		cfg.CoolingDuration = 60 * time.Second
	}
	if cfg.MaxCoolingCount <= 0 {
		cfg.MaxCoolingCount = 5
	}
	if scheduler == nil {
		scheduler = &RoundRobinScheduler{}
	}
	return &Pool{
		ProviderName: providerName,
		cfg:          cfg,
		keys:         keys,
		scheduler:    scheduler,
	}
}

// Acquire 获取一个可用的 Key
//   1. 遍历 keys,若 COOLING 且 cooling_until < now,自动恢复为 ACTIVE
//   2. Scheduler 从可用 keys 中选一个
//   3. 若没有可用,返回 ErrNoAvailableKey
func (p *Pool) Acquire() (*Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 1. 恢复过期的 COOLING
	for _, k := range p.keys {
		if k.Status == KeyStatusCooling && now.After(k.CoolingUntil) {
			k.Status = KeyStatusActive
			k.UpdatedAt = now
		}
	}

	// 2. 收集可用 Key(过滤)
	usable := make([]*Key, 0, len(p.keys))
	for _, k := range p.keys {
		if k.IsUsable(now) {
			usable = append(usable, k)
		}
	}
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}

	// 3. Scheduler 选一个
	return p.scheduler.Select(usable)
}

// ReportSuccess 上报成功
func (p *Pool) ReportSuccess(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k.TotalRequests++
	k.LastUsedAt = time.Now()
	k.UpdatedAt = k.LastUsedAt

	// 如果是 LIMITED(配额受限但仍可用),成功不改变状态
	// 如果之前错误状态是 COOLING 但已恢复,这里就保持 ACTIVE
}

// ReportRateLimit 上报 429,触发冷却
// retryAfter 来自 Retry-After header;为 0 时用默认 CoolingDuration
func (p *Pool) ReportRateLimit(k *Key, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	cooling := retryAfter
	if cooling <= 0 {
		cooling = p.cfg.CoolingDuration
	}

	k.Status = KeyStatusCooling
	k.CoolingUntil = now.Add(cooling)
	k.CoolingCount++
	k.LastErrorAt = now
	k.ErrorCount++
	k.UpdatedAt = now

	if k.CoolingCount > p.cfg.MaxCoolingCount {
		k.Status = KeyStatusDisabled
	}
}

// ReportError 上报非 429 错误
// 当前实现:auth / invalid_request 直接 DISABLED;
// 其他错误仅累计计数,不立即禁用
func (p *Pool) ReportError(k *Key, errType string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	k.ErrorCount++
	k.LastErrorAt = now
	k.UpdatedAt = now

	switch errType {
	case "auth", "invalid_request":
		// 401/403/400 通常说明 Key 本身有问题,直接禁用
		k.Status = KeyStatusDisabled
	}
}

// Status 返回池当前状态摘要
type PoolStatus struct {
	ProviderName string `json:"provider_name"`
	TotalKeys    int    `json:"total_keys"`
	ActiveKeys   int    `json:"active_keys"`
	CoolingKeys  int    `json:"cooling_keys"`
	DisabledKeys int    `json:"disabled_keys"`
}

// Status 池状态摘要
func (p *Pool) Status() PoolStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	s := PoolStatus{ProviderName: p.ProviderName, TotalKeys: len(p.keys)}
	for _, k := range p.keys {
		switch k.Status {
		case KeyStatusActive, KeyStatusLimited:
			s.ActiveKeys++
		case KeyStatusCooling:
			s.CoolingKeys++
		case KeyStatusDisabled:
			s.DisabledKeys++
		}
	}
	return s
}

// Keys 返回池内所有 Key 的快照(只读)
func (p *Pool) Keys() []Key {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Key, len(p.keys))
	for i, k := range p.keys {
		out[i] = *k
	}
	return out
}

// Size Key 总数
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.keys)
}
