// Package keypool — Pool:管理单个 Provider 下的所有 Key
// 对应规格书 5.4 Key Pool
package keypool

import (
	"fmt"
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

// BuildPoolFromStrings P30 便捷函数:从明文 key 列表直接构造 Pool
// Authenticator 从 DB 读出明文后直接用,跳过 Key struct 包装
func BuildPoolFromStrings(providerName string, plainKeys []string, cfg Config) *Pool {
	now := time.Now().UTC()
	keys := make([]*Key, len(plainKeys))
	for i, pk := range plainKeys {
		keys[i] = &Key{
			ID:            fmt.Sprintf("%s-key-%d", providerName, i+1),
			ProviderName:  providerName,
			Name:          fmt.Sprintf("key-%d", i+1),
			Key:           pk,
			Status:        KeyStatusActive,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	return NewPool(providerName, keys, nil, cfg)
}

// Acquire 获取一个可用的 Key
//   1. 遍历 keys,若 COOLING 且 cooling_until < now,自动恢复为 ACTIVE
//   2. Scheduler 从可用 keys 中选一个
//   3. 若没有可用,返回 ErrNoAvailableKey
func (p *Pool) Acquire() (*Key, error) {
	return p.acquireFiltered(nil)
}

// AcquireFromIDs P34: 从指定 ID 子集里挑 Key(Gateway Key 绑定了 ProviderKeyIDs 时用)
// allowedIDs 为 nil/空 → 等价 Acquire(用全部 keys)
// 非空 → 只从 ID 在这个集合里的 key 里挑
func (p *Pool) AcquireFromIDs(allowedIDs []uint) (*Key, error) {
	if len(allowedIDs) == 0 {
		return p.Acquire()
	}
	// 转成 map 加速 lookup
	set := make(map[uint]struct{}, len(allowedIDs))
	for _, id := range allowedIDs {
		set[id] = struct{}{}
	}
	return p.acquireFiltered(set)
}

// acquireFiltered 内部实现,allowedIDSet 为 nil 表示用所有 keys
// P48: 按 BillingSource 分桶,优先返回 token_plan key,没有再 api,最后 free
// 同 tier 内用 Scheduler(round-robin)
func (p *Pool) acquireFiltered(allowedIDSet map[uint]struct{}) (*Key, error) {
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
		if !k.IsUsable(now) {
			continue
		}
		// P34: 如果 allowedIDSet 不为空,只收 ID 在集合里的 key
		if allowedIDSet != nil {
			id := parseKeyIDUint(k.ID)
			if _, ok := allowedIDSet[id]; !ok {
				continue
			}
		}
		usable = append(usable, k)
	}
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}

	// 3. P48: 按 billing_source 分桶,优先 token_plan > api > free
	tierOrder := []string{"token_plan", "api", "free"}
	for _, tier := range tierOrder {
		bucket := make([]*Key, 0, len(usable))
		for _, k := range usable {
			tierOfKey := k.BillingSource
			if tierOfKey == "" {
				tierOfKey = "api" // 兜底
			}
			if tierOfKey == tier {
				bucket = append(bucket, k)
			}
		}
		if len(bucket) > 0 {
			return p.scheduler.Select(bucket)
		}
	}

	// 所有 bucket 都没有(理论上不会到这)— 兜底用全量
	return p.scheduler.Select(usable)
}

// parseKeyIDUint 把 Key.ID (格式 "<provider>-key-<N>" 或纯数字字符串) 转 uint
// P34: Pool 里的 Key.ID 现在是 DB ProviderAPIKey.ID 的字符串形式(数字)
// 为了向前兼容保留旧的 "<provider>-key-N" 形式(返回 0 表示不在 ID 集合里匹配)
func parseKeyIDUint(id string) uint {
	var n uint
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint(c-'0')
	}
	return n
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

// Tiers 返回 Pool 中可用 key 的 tier 列表(去重,按优先级排序)
// P52: token_plan > api > free
// Router 用这个来排序 chain 候选 — 先穷尽所有 token_plan,再 api
func (p *Pool) Tiers() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	tierOrder := []string{"token_plan", "api", "free"}
	seen := map[string]bool{}
	var out []string
	for _, want := range tierOrder {
		for _, k := range p.keys {
			bs := k.BillingSource
			if bs == "" {
				bs = "api" // 兜底
			}
			if bs == want && !seen[want] {
				seen[want] = true
				out = append(out, want)
			}
		}
	}
	return out
}

// BestTier 返回 Pool 中最高优先级 tier(token_plan > api > free),没有 key 返回 ""
func (p *Pool) BestTier() string {
	tiers := p.Tiers()
	if len(tiers) == 0 {
		return ""
	}
	return tiers[0]
}
