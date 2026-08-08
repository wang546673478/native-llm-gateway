// Package keypool — Key 选择过滤器链
//
// 单一职责:一个 Filter 只做一件事,delete 一个 Filter 只删 1 处。
// 之前 acquireFromTierLocked 是 9 件事的大函数,改任何一处都要重新读整段。
// 现在每个 Filter 是独立函数,可单独测、单独替换。
package keypool

import (
	"sort"
	"time"
)

// filterCtx 一次 acquire 的过滤上下文
// 所有 Filter 共享这个结构,新增过滤维度只需加字段
type filterCtx struct {
	tier        string
	allowedSet  map[uint]struct{}
	excludeID   string
	proto       string
	stickyKeyID uint // 0 = 无 sticky
	hasSticky   bool
	pool        *Pool // 用于 filterCircuit 查 breaker
}

// Filter 函数式过滤器:返回 true 保留,false 丢弃
type Filter func(*Key, *filterCtx) bool

// filterRecoverExpiredCooling 恢复过期的 COOLING key 到 ACTIVE
// 副作用:必须先把状态恢复,后续过滤才能看到正确的 Status
func (p *Pool) filterRecoverExpiredCooling(usable []*Key, now time.Time) []*Key {
	for _, k := range usable {
		if k.Status == KeyStatusCooling && now.After(k.CoolingUntil) {
			k.Status = KeyStatusActive
			k.UpdatedAt = now
		}
	}
	return usable
}

// filterIsUsable 过滤掉不可调度的 key
func filterIsUsable(k *Key, ctx *filterCtx) bool {
	return k.IsUsable(time.Now())
}

// filterAllowedIDs 按 PoolKeyIDs 过滤(P34)
func filterAllowedIDs(k *Key, ctx *filterCtx) bool {
	if ctx.allowedSet == nil {
		return true
	}
	id := parseKeyIDUint(k.ID)
	_, ok := ctx.allowedSet[id]
	return ok
}

// filterExcludeID 排除刚失败的那把 key(swapToOtherKey 重试用)
func filterExcludeID(k *Key, ctx *filterCtx) bool {
	if ctx.excludeID == "" {
		return true
	}
	return parseKeyIDUint(k.ID) != parseKeyIDUint(ctx.excludeID)
}

// filterProtocol 按 Key.Protocols 字段过滤(provider vendor 共享池用)
func filterProtocol(k *Key, ctx *filterCtx) bool {
	if ctx.proto == "" {
		return true
	}
	if k.Protocols == "" {
		return true
	}
	return containsProtocol(k.Protocols, ctx.proto)
}

// filterCircuit 过滤掉熔断中的 key
// 拿到 Pool 来检查 breaker(Filter 没有 pool 引用,需要从 ctx 传)
func filterCircuit(k *Key, ctx *filterCtx) bool {
	br := ctx.pool.breakerFor(k)
	return br == nil || br.Allow()
}

// filterTier 仅保留指定 tier 的 key
func filterTier(k *Key, ctx *filterCtx) bool {
	bs := k.BillingSource
	if bs == "" {
		bs = "api"
	}
	return bs == ctx.tier
}

// filterIsPolledAndExhausted 跳过 poll 确认耗尽的 key
func filterIsPolledAndExhausted(k *Key, ctx *filterCtx) bool {
	return !k.IsPolledAndExhausted()
}

// filterSticky 优先返回 sticky 命中的 key
// 这一步不在 chain 里(它要重写 Select 逻辑),通过单独方法处理
func filterSticky(_ *Key, _ *filterCtx) bool {
	// sticky 由 acquireSticky 单独处理,这里永远是 true
	// 此函数只为完整性标记,实际不在 chain 调用
	return true
}

// acquireSticky 尝试 sticky 命中
// 跳过 IsUsable 时间检查,直接看 Status: COOLING/QUOTA_EXCEEDED 都不命中
// (sticky 命中失败时 fallback 调度即可)
func (p *Pool) acquireSticky(usable []*Key, ctx *filterCtx) *Key {
	if !ctx.hasSticky || ctx.stickyKeyID == 0 {
		return nil
	}
	for _, k := range usable {
		if parseKeyIDUint(k.ID) != ctx.stickyKeyID {
			continue
		}
		// sticky 命中检查:Status 必须是 ACTIVE(跳过 COOLING / QE)
		if k.Status == KeyStatusActive {
			return k
		}
		// COOLING 但过期 → 视为可用
		if k.Status == KeyStatusCooling && time.Now().After(k.CoolingUntil) {
			return k
		}
		// 其他状态(QUOTA_EXCEEDED / DISABLED)→ 不命中
	}
	return nil
}

// AcquireOption 链式配置
type AcquireOption func(*filterCtx)

// WithTier 设置 tier
func WithTier(tier string) AcquireOption {
	return func(c *filterCtx) { c.tier = tier }
}

// WithAllowedIDs 限定 PoolKey IDs
func WithAllowedIDs(ids []uint) AcquireOption {
	return func(c *filterCtx) {
		if len(ids) == 0 {
			return
		}
		c.allowedSet = make(map[uint]struct{}, len(ids))
		for _, id := range ids {
			c.allowedSet[id] = struct{}{}
		}
	}
}

// WithExcludeKey 排除某把 key
func WithExcludeKey(keyID string) AcquireOption {
	return func(c *filterCtx) { c.excludeID = keyID }
}

// WithProtocol 按协议过滤
func WithProtocol(proto string) AcquireOption {
	return func(c *filterCtx) { c.proto = proto }
}

// WithStickyKey 启用 sticky session
func WithStickyKey(keyID uint) AcquireOption {
	return func(c *filterCtx) {
		c.stickyKeyID = keyID
		c.hasSticky = true
	}
}

// AcquireWithFilter 新的统一入口:filter chain + sticky 优先
// 语义等价于 acquireFromTierLocked,但每个步骤独立、可测、可替换
// 旧 AcquireFromTier / AcquireFromTierExcluding 等保留,内部委托到这里
func (p *Pool) AcquireWithFilter(opts ...AcquireOption) (*Key, error) {
	ctx := &filterCtx{pool: p}
	for _, opt := range opts {
		opt(ctx)
	}
	if ctx.tier == "" {
		ctx.tier = "api"
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 1. 状态预处理:恢复过期 COOLING(副作用,必须在过滤前)
	usable := p.filterRecoverExpiredCooling(p.keys, now)

	// 2. 过滤链
	chain := []Filter{
		filterIsUsable,
		filterAllowedIDs,
		filterExcludeID,
		filterProtocol,
		filterCircuit,
		filterTier,
		filterIsPolledAndExhausted,
	}
	filtered := usable[:0]
	for _, k := range usable {
		keep := true
		for _, f := range chain {
			if !f(k, ctx) {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, k)
		}
	}
	usable = filtered
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}

	// 3. 排序:token_plan tier 按 Remaining 降序,稳定排序保留 RoundRobin 顺序
	if ctx.tier == "token_plan" {
		sort.SliceStable(usable, func(i, j int) bool {
			return usable[i].Remaining > usable[j].Remaining
		})
	}

	// 4. sticky 优先(sticky 命中后不再调 Select)
	if k := p.acquireSticky(usable, ctx); k != nil {
		return k, nil
	}

	// 5. 调度
	return p.scheduler.Select(usable)
}
