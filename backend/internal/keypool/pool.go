// Package keypool — Pool:管理单个 Provider 下的所有 Key
// 对应规格书 5.4 Key Pool
package keypool

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// authErrorCooling auth 错误(401/403)时 key 进入 COOLING 的时长。
// 独立于 rate-limit 的 CoolingDuration 配置 — auth 说明 key 本身有问题,冷却更久
// 等换 key/修 key,不给 config 表面(业务固定值)。单命名源,消除裸字面量。
const authErrorCooling = 5 * time.Minute

// QuotaRecoveryMode 配额耗尽的恢复策略
type QuotaRecoveryMode string

const (
	// QuotaRecoveryPoll 有 balancer:标 QUOTA_EXCEEDED,quotacheck 轮询恢复
	// (deepseek / minimax — 有余额查询接口)
	QuotaRecoveryPoll QuotaRecoveryMode = "poll"
	// QuotaRecoveryProbe 无 balancer(api 计费,如 glm / qwen / gemini):
	// 没有轮询恢复通道,标了 QUOTA_EXCEEDED 就没有恢复路径(充值也救不回来)。
	// 不永久标记 — 每次请求重新探测,余额不足时 failover 到其他 provider,
	// 充值后自动恢复
	QuotaRecoveryProbe QuotaRecoveryMode = "probe"
)

// BreakerFactory 抽象熔断器工厂 — Pool 不再直接 import circuit 包
// 由调用方(server.buildOnePool)注入,既保持 per-key 熔断语义,又解耦 keypool ↔ circuit
type BreakerFactory func(keyID string) Breaker

// Breaker 熔断器接口 — Pool 只需要 Allow/RecordSuccess/RecordFailure 三个方法
type Breaker interface {
	Allow() bool
	RecordSuccess()
	RecordFailure(errType string)
	// State 返回熔断器状态(CLOSED/OPEN/HALF_OPEN),用于 Keys() 快照
	State() string
}

// Config Pool 配置
type Config struct {
	CoolingDuration time.Duration // 默认冷却时长
	// QuotaRecovery 配额耗尽标记策略;空 = poll(保持现有行为)。
	// 由 server.buildKeyPools 按「该 vendor 是否有 balancer」设置
	QuotaRecovery QuotaRecoveryMode
	// P-per-key-circuit: per-key 熔断器配置(2026-08-06)。
	// 之前熔断器是 per-provider 的 — 一把 key 5 个 5xx 连坐整 provider,
	// healthy key 一起被跳过(2026-08-06 实测:weige 出问题,key-1 也被跳过)。
	// 现在每把 key 独立熔断;FailureThreshold <= 0 = 不启用(测试场景)
	// 通过 BreakerFactory 注入,Pool 不再直接 import circuit 包
	BreakerFactory BreakerFactory
}

// Pool 管理一个 Provider 下的所有 Key
type Pool struct {
	ProviderName string
	cfg          Config
	mu           sync.RWMutex
	keys         []*Key
	scheduler    Scheduler
	// P-per-key-circuit: per-key 熔断器(key.ID → breaker,懒创建)。
	// 5 个 5xx/timeout/connection 在窗口内只熔断这一把 key,
	// 同 provider 的 healthy key 照常参与调度(2026-08-06 之前是 provider 级连坐)
	breakers map[string]Breaker
	// P68: quota restore 回调槽(默认 nil = no-op)。
	// 注入方是 quotacheck.Manager,Pool 不感知 quotacheck 包,避免 import cycle。
	// 签名带 fromStatus:让 callback 知道状态变之前的值,用于 emit transition metric。
	OnQuotaExceeded func(*Key, KeyStatus) // 第二参数:状态变之前的 status
	OnKeyRestored   func(*Key)
}

// NewPool 构造 Pool
func NewPool(providerName string, keys []*Key, scheduler Scheduler, cfg Config) *Pool {
	if cfg.CoolingDuration <= 0 {
		cfg.CoolingDuration = 60 * time.Second
	}
	if cfg.QuotaRecovery == "" {
		cfg.QuotaRecovery = QuotaRecoveryPoll // 默认 poll,保持现有行为
	}
	if scheduler == nil {
		scheduler = &RoundRobinScheduler{}
	}
	return &Pool{
		ProviderName: providerName,
		cfg:          cfg,
		keys:         keys,
		scheduler:    scheduler,
		breakers:     make(map[string]Breaker),
	}
}

// breakerFor P-per-key-circuit: 取(或懒创建)指定 key 的熔断器。
// 调用方必须已持 p.mu(写锁)— 懒创建会写 breakers map。
// 无 BreakerFactory 注入 → nil(测试场景)
func (p *Pool) breakerFor(k *Key) Breaker {
	if p.cfg.BreakerFactory == nil {
		return nil
	}
	if br, ok := p.breakers[k.ID]; ok {
		return br
	}
	br := p.cfg.BreakerFactory(k.ID)
	p.breakers[k.ID] = br
	return br
}

// filterBreakers P-per-key-circuit: 从 usable 里滤掉熔断中的 key。
// 每个 key 调一次 Allow() — 同时处理三种状态:
//   - CLOSED → 放行(无副作用)
//   - OPEN 未超时 → 跳过;OPEN 超时 → 转 HALF_OPEN 放行首个试探请求
//   - HALF_OPEN 有试探位 → 放行;已满 → 跳过
//
// 熔断只影响这一把 key — 同 provider 其他 key 不受牵连(2026-08-06 之前是
// provider 级 healthStatus 连坐,现在 healthStatus 已移除)。
func (p *Pool) filterBreakers(usable []*Key) []*Key {
	if len(usable) == 0 {
		return usable
	}
	out := usable[:0]
	for _, k := range usable {
		br := p.breakerFor(k)
		if br == nil || br.Allow() {
			out = append(out, k)
		}
	}
	return out
}

// Acquire 获取一个可用的 Key,按 tier 降级顺序尝试 token_plan → api → free
// P64: 这里保留 tier 降级是作为"无 tier 信息的旧 caller"的兼容入口
// 新调用方应明确用 AcquireFromTier;P-provider-vendor: 等价 AcquireForProtocol("")
func (p *Pool) Acquire() (*Key, error) {
	return p.AcquireForProtocol("")
}

// AcquireForProtocol P-provider-vendor: 按请求协议取 key(带 tier 降级)
// proto 为空 = 不过滤;非空时只取 Protocols 为空或包含该协议的 key
func (p *Pool) AcquireForProtocol(proto string) (*Key, error) {
	for _, tier := range TierOrder {
		k, err := p.AcquireFromTier(tier, nil, proto)
		if err == nil {
			return k, nil
		}
	}
	return nil, ErrNoAvailableKey
}

// AcquireFromIDs P34: 从指定 ID 子集里挑 Key(Gateway Key 绑定了 ProviderKeyIDs 时用)
// allowedIDs 为 nil/空 → 等价 Acquire(用全部 keys)
// 非空 → 只从 ID 在这个集合里的 key 里挑
// P-provider-vendor: proto 透传给 AcquireFromTier(协议过滤;空 = 不过滤)
func (p *Pool) AcquireFromIDs(allowedIDs []uint, proto string) (*Key, error) {
	if len(allowedIDs) == 0 {
		return p.AcquireForProtocol(proto)
	}
	// 转成 map 加速 lookup
	set := make(map[uint]struct{}, len(allowedIDs))
	for _, id := range allowedIDs {
		set[id] = struct{}{}
	}
	for _, tier := range TierOrder {
		k, err := p.AcquireFromTier(tier, set, proto)
		if err == nil {
			return k, nil
		}
	}
	return nil, ErrNoAvailableKey
}

// AcquireFromTier P64: 只从指定 tier 桶里挑 key,不做 tier 间降级
// tier ∈ {"token_plan", "api", "free"};空字符串按 "api" 兜底
// allowedIDSet nil = 不限 ID;非 nil = 只从 ID 在集合里的 key 里挑
// P-provider-vendor: proto 非空时只取 Protocols 为空或包含该协议的 key;空 = 不过滤
// 该 tier 桶为空时直接返回 ErrNoAvailableKey,让 Router 推进到下一档候选
func (p *Pool) AcquireFromTier(tier string, allowedIDSet map[uint]struct{}, proto string) (*Key, error) {
	return p.acquireFromTierLocked(tier, allowedIDSet, "", proto)
}

// AcquireFromTierExcluding 从指定 tier 桶挑 key,排除指定 ID(换 key 重试用)
// excludeID 为空 = 与 AcquireFromTier 等价
func (p *Pool) AcquireFromTierExcluding(tier, excludeID string, proto string) (*Key, error) {
	return p.acquireFromTierLocked(tier, nil, excludeID, proto)
}

// AcquireFromTierExcludingIDs 同 AcquireFromTierExcluding,额外限定 allowedIDSet
// (P34:换 key 重试也不能跨出 GatewayKey 绑定的 ProviderKey ID 子集 —
// 与 Router RouteIterator.Next 的 idSet 过滤语义一致)
func (p *Pool) AcquireFromTierExcludingIDs(tier, excludeID string, allowedIDSet map[uint]struct{}, proto string) (*Key, error) {
	return p.acquireFromTierLocked(tier, allowedIDSet, excludeID, proto)
}

// acquireFromTierLocked AcquireFromTier / AcquireFromTierExcluding 的公共实现。
// 在 allowedIDSet 过滤后追加 exclude 检查:excludeID 非空时排除 ID 等于它的 key
// (parseKeyIDUint 转换比较,与 allowedIDSet 过滤一致;excludeID 是 DB 数字 ID 字符串)
func (p *Pool) acquireFromTierLocked(tier string, allowedIDSet map[uint]struct{}, excludeID string, proto string) (*Key, error) {
	if tier == "" {
		tier = string(BillingSourceDefault) // 兜底
	}
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
		// Task3: 换 key 重试 — 排除刚失败的那把 key(excludeID 非空时)
		if excludeID != "" && parseKeyIDUint(k.ID) == parseKeyIDUint(excludeID) {
			continue
		}
		usable = append(usable, k)
	}

	// P-provider-vendor: 协议过滤 — Key.Protocols 为空 = 所有协议可用;非空 = 仅列出的协议
	if proto != "" {
		filtered := usable[:0]
		for _, k := range usable {
			if k.Protocols == "" || containsProtocol(k.Protocols, proto) {
				filtered = append(filtered, k)
			}
		}
		usable = filtered
	}
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}

	// P-per-key-circuit: 熔断过滤 — 只跳过熔断中的 key,同 provider 其他 key 不受影响
	usable = p.filterBreakers(usable)
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}

	// P-quota-balance: token_plan tier 在进入 tier 过滤前按 Remaining 降序稳定排序
	// 稳定排序保证 Remaining 相等时仍维持 RoundRobin 原始顺序
	if tier == string(BillingSourceTokenPlan) {
		sort.SliceStable(usable, func(i, j int) bool {
			return usable[i].Remaining > usable[j].Remaining
		})
	}

	// 3. P64: 只从指定 tier 桶里挑,不再做 tier 降级
	bucket := make([]*Key, 0, len(usable))
	for _, k := range usable {
		bs := k.BillingSource
		if bs == "" {
			bs = string(BillingSourceDefault) // 兜底
		}
		if bs == tier {
			// P-quota-prefer: 跳过「已轮询且余额耗尽」的 key — 否则 round-robin
			// 会把请求轮流分给已死的 key(如 MiniMax weige 1%),每轮 429 →
			// failover deepseek,而 healthy key 在旁边空转(2026-08-06 实测)。
			// 未轮询过的不跳过(启动窗口);余额回升后自动恢复参与
			if k.IsPolledAndExhausted() {
				continue
			}
			bucket = append(bucket, k)
		}
	}
	if len(bucket) == 0 {
		return nil, ErrNoAvailableKey
	}
	return p.scheduler.Select(bucket)
}

// containsProtocol 判断逗号分隔的协议列表是否包含指定协议
func containsProtocol(list, proto string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == proto {
			return true
		}
	}
	return false
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

	// P-per-key-circuit: 成功信号 → 熔断器(HALF_OPEN 试探成功 → CLOSED;CLOSED 清窗口)
	if br := p.breakerFor(k); br != nil {
		br.RecordSuccess()
	}

	// Bug fix (2026-08-08): 上游实际请求成功 → 从 QE 状态恢复
	// poll 读到 100% 不直接 restore(避免 MiniMax 状态机滞后)— 必须等真实请求成功
	// 余额信息被 poll 持续更新,这里只做状态切换
	if k.Status == KeyStatusQuotaExceeded {
		k.Status = KeyStatusActive
		k.QuotaProbeAttempts = 0
		k.QuotaExceededSince = time.Time{}
		k.UpdatedAt = time.Now()
		k.CoolingCount = 0 // reset 冷却计数,新窗口
	}

	// 如果是 LIMITED(配额受限但仍可用),成功不改变状态
	// 如果之前错误状态是 COOLING 但已恢复,这里就保持 ACTIVE
}

// ReportRateLimit 上报 429,触发冷却
// retryAfter 来自 Retry-After header;为 0 时用默认 CoolingDuration
// Bug fix (2026-08-08): token_plan key 连续冷却 3 次 → 升级为 QUOTA_EXCEEDED
// 之前没有退出机制,key 反复 429 → 反复刷新 60s COOLING → 永远卡在冷却中
// 但 remaining 早已恢复。升级 QE 后由 quotacheck poll 接管恢复决策
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
	// P-no-disabled: 冷却次数不设上限 — 反复限流只会反复冷却(COOLING 期间不参与
	// 调度,天然自限),不会永久禁用。终端状态没有恢复路径

	// Bug fix: 连续冷却升级 QE — 仅 token_plan 适用(api 层冷却本就是正常限流)
	if k.CoolingCount >= 3 && k.BillingSource == string(BillingSourceTokenPlan) {
		k.Status = KeyStatusQuotaExceeded
		k.QuotaExceededSince = now
		k.QuotaProbeAttempts = 0
		k.UpdatedAt = now
	}
}

// ReportError 上报非 429 错误
//   - auth → 冷却 5 分钟(换 key 后自动恢复;P-no-disabled 不设终端状态)
//   - invalid_request → 仅计数(上游 400 通常是请求内容问题,不是 key 的问题)
//   - quota_exceeded → P68: 走 quota restore 路径(QUOTA_EXCEEDED + worker 探测)
//   - 其他 → 仅累计计数
func (p *Pool) ReportError(k *Key, errType string) {
	p.mu.Lock()
	now := time.Now()
	k.ErrorCount++
	k.LastErrorAt = now
	k.UpdatedAt = now

	// P-per-key-circuit: server_error/timeout/connection → 该 key 熔断计数。
	// 只熔断这一把 key,不连坐同 provider 其他 key(2026-08-06 之前 provider 级连坐)。
	// 429(rate_limit)/quota/auth/invalid_request 不计数(与 circuit.shouldCount 一致)。
	// 哪些 errType 触发熔断由 keypool.ErrorType 单一集合决定(见 errtype.go),
	// 不再散落裸字符串字面量。
	if TripsBreaker(errType) {
		if br := p.breakerFor(k); br != nil {
			br.RecordFailure(errType)
		}
	}

	var quotaCB func(*Key, KeyStatus)
	var fromStatus KeyStatus
	switch ErrorType(errType) {
	case ErrorTypeAuth:
		// P-no-disabled: 上游 401/403-auth:key 本身有问题 → 冷却 5 分钟而非禁用。
		// 冷却期间不参与调度,到期自动重试 — 换 key/修 key 后自动恢复
		k.Status = KeyStatusCooling
		k.CoolingUntil = now.Add(authErrorCooling)
	case ErrorTypeInvalidRequest:
		// P-invalid-req: 上游 400 通常是「这个请求内容它不支持」(agent 回带的
		// 其他厂商 thinking 块、tool 格式差异等),不是 key 有问题 —
		// 只计数,不禁用。禁用会把整条链打死且无恢复路径
	case ErrorTypeQuotaExceeded:
		if p.cfg.QuotaRecovery == QuotaRecoveryProbe {
			// B-probe-quota: 无 balancer 的 api 厂商(glm/qwen/gemini)没有轮询
			// 恢复通道 — 标 QUOTA_EXCEEDED 就是永久死 key。只计数不标记,
			// 每次请求重新探测:余额不足期间 failover 到其他 provider,
			// 充值后自动恢复(代价:每次请求先打一次上游拿错误,毫秒级)
			break
		}
		// P68: 配额耗尽(402 / 429 quota / 403 quota) — 标 QUOTA_EXCEEDED
		// 让 quotacheck.Manager 探测恢复后调 RestoreQuota 回到 ACTIVE
		fromStatus = k.Status
		p.markQuotaExceededLocked(k, now)
		quotaCB = p.OnQuotaExceeded
	}
	p.mu.Unlock() // 提前释放锁,回调里再调 Pool 不会死锁

	if quotaCB != nil {
		quotaCB(k, fromStatus)
	}
}

// markQuotaExceededLocked ReportError quota 分支内部调,要求已持锁
// 不在 defer Unlock 前 fire 回调,避免回调里再调 Pool 死锁
func (p *Pool) markQuotaExceededLocked(k *Key, now time.Time) {
	k.Status = KeyStatusQuotaExceeded
	k.QuotaExceededSince = now
	k.QuotaProbeAttempts = 0
	k.UpdatedAt = now
}

// ReportQuotaExceeded 公开方法 — 报告配额耗尽(供非 ReportError 路径调用)
// 调用者应确保已分类 errType==quota_exceeded
func (p *Pool) ReportQuotaExceeded(k *Key) {
	p.mu.Lock()
	now := time.Now()
	fromStatus := k.Status
	p.markQuotaExceededLocked(k, now)
	cb := p.OnQuotaExceeded
	p.mu.Unlock()

	if cb != nil {
		cb(k, fromStatus)
	}
}

// RestoreQuota 配额恢复 — Manager 探测到余额 > 0 时调
func (p *Pool) RestoreQuota(k *Key) {
	p.mu.Lock()
	k.Status = KeyStatusActive
	k.QuotaProbeAttempts = 0
	k.QuotaExceededSince = time.Time{}
	k.UpdatedAt = time.Now()
	cb := p.OnKeyRestored
	p.mu.Unlock()

	if cb != nil {
		go cb(k) // 异步,避免阻塞调用方
	}
}

// ResetCooling 重置 key 的冷却计数(CoolingCount)
// 用途:poll 读到余额恢复(QE → ACTIVE)后调用,防止"充值 → 立即又被 429 连续升级 QE"的无意义计数累积
func (p *Pool) ResetCooling(k *Key) {
	p.mu.Lock()
	k.CoolingCount = 0
	k.UpdatedAt = time.Now()
	p.mu.Unlock()
}

// IncQuotaProbeAttempts 探测次数 +1(RESTORE 时由 Manager reset 为 0)
// 返回新的 attempts 数
func (p *Pool) IncQuotaProbeAttempts(k *Key) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.QuotaProbeAttempts++
	return k.QuotaProbeAttempts
}

// Status 返回池当前状态摘要
type PoolStatus struct {
	ProviderName      string `json:"provider_name"`
	TotalKeys         int    `json:"total_keys"`
	ActiveKeys        int    `json:"active_keys"`
	CoolingKeys       int    `json:"cooling_keys"`
	QuotaExceededKeys int    `json:"quota_exceeded_keys"` // P68
	// P-quota-balance: 上游 quota polling 的聚合指标
	QuotaPolledKeys int     `json:"quota_polled_keys"` // 至少 poll 过一次的 key 数
	QuotaKnownSum   float64 `json:"quota_known_sum"`   // 已 poll 的 key 的 Remaining 之和
	// P-quota-display: polled keys 的类型 — 全部 percent → "percent";否则 "currency"
	// (空 Kind 如 GLM 按 currency,前端维持 ¥ 渲染)
	QuotaKind string `json:"quota_kind"`
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
		case KeyStatusQuotaExceeded:
			s.QuotaExceededKeys++
		}
	}
	allPercent := true
	for _, k := range p.keys {
		if !k.LastPolledAt.IsZero() {
			s.QuotaPolledKeys++
			s.QuotaKnownSum += k.Remaining
			if k.QuotaKind != "percent" {
				allPercent = false
			}
		}
	}
	// P-quota-display: dominant kind — 全部 percent → "percent",否则 "currency"
	if s.QuotaPolledKeys > 0 {
		if allPercent {
			s.QuotaKind = "percent"
		} else {
			s.QuotaKind = "currency"
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
		// P-per-key-circuit: 刷新熔断快照(只读已有 breaker,不懒创建 —
		// RLock 下写 map 是 race;breaker 内部自带锁)
		if br, ok := p.breakers[k.ID]; ok {
			st := br.State()
			out[i].CircuitState = st
			out[i].CircuitOpen = st != "CLOSED"
		}
	}
	return out
}

// KeyPtrs P68: 返 []*Key 让 caller 能直接调 Report* 方法
// 调用者必须自己保证生命周期(指针指向 pool 内部 key,期间 key 不能被 Reload 替换)
func (p *Pool) KeyPtrs() []*Key {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Key, len(p.keys))
	copy(out, p.keys)
	return out
}

// MutateKey 在持有 p.mu(写锁)下安全改单把 key 的字段。
// 低耦合修复:quotacheck 轮询此前直接写 k.Status/k.Remaining/k.QuotaZeroStreak
// 等(不经锁),与请求路径 ReportSuccess/ReportError(在 p.mu 下写同一批字段)
// 形成数据竞争。把「外部要改 key 字段」统一收敛到 keypool 的锁内变更——
// 外部(quotacheck)不再也不应该知道内部加锁细节(高内聚)。
// k 必须是本 pool 的 KeyPtrs() 里的指针。
func (p *Pool) MutateKey(k *Key, fn func(*Key)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(k)
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

	tierOrder := TierOrder
	seen := map[string]bool{}
	var out []string
	for _, want := range tierOrder {
		for _, k := range p.keys {
			bs := k.BillingSource
			if bs == "" {
				bs = string(BillingSourceDefault) // 兜底
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

// KeyState 单把 key 的运行时状态快照(JSON 序列化友好)。
// P-state-persist: 优雅关停时落盘,重启时恢复 —
// 否则每次 reload 后 QE/COOLING/余额全丢,耗尽的 key 要等 poll 连续 2 轮
// 重新确认(~2 分钟),期间请求反复打它(429 → COOLING 60s 循环)。
type KeyState struct {
	ProviderName string    `json:"provider_name"`
	KeyID        string    `json:"key_id"`
	Status       KeyStatus `json:"status"`
	CoolingUntil time.Time `json:"cooling_until"`
	// P68: 配额耗尽时间(恢复后 quotacheck 需要它决定探测节奏)
	QuotaExceededSince time.Time `json:"quota_exceeded_since"`
	// P-quota-balance: poll 快照(恢复后 balanceGuardHealthy 立即有数据,
	// 不再把耗尽 key 的 2056 误判成限流冷却)
	Remaining       float64   `json:"remaining"`
	LastPolledAt    time.Time `json:"last_polled_at"`
	QuotaKind       string    `json:"quota_kind"`
	QuotaZeroStreak int       `json:"quota_zero_streak"`
	CoolingCount    int       `json:"cooling_count"`
}

// Snapshot 导出所有 key 的运行时状态(供优雅关停落盘)。
// 注意:只导状态,不含明文 key — 快照文件无敏感数据
func (p *Pool) Snapshot() []KeyState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]KeyState, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, KeyState{
			ProviderName:       k.ProviderName,
			KeyID:              k.ID,
			Status:             k.Status,
			CoolingUntil:       k.CoolingUntil,
			QuotaExceededSince: k.QuotaExceededSince,
			Remaining:          k.Remaining,
			LastPolledAt:       k.LastPolledAt,
			QuotaKind:          k.QuotaKind,
			QuotaZeroStreak:    k.QuotaZeroStreak,
			CoolingCount:       k.CoolingCount,
		})
	}
	return out
}

// ApplySnapshot P-state-persist: 按 keyID 恢复快照(重启后调用)。
// 规则:
//   - 已过期的 COOLING 不恢复(acquire 会按 ACTIVE 处理,恢复反而拖住)
//   - QUOTA_EXCEEDED 恢复后,quotacheck 的冷启动 rescan 会自动入堆恢复
//   - Remaining/LastPolledAt 恢复 → balanceGuardHealthy 立即生效,
//     耗尽 key 的 2056 直接标 QE,不再被降级成 60s 冷却循环
func (p *Pool) ApplySnapshot(states []KeyState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	byID := make(map[string]KeyState, len(states))
	for _, s := range states {
		byID[s.KeyID] = s
	}
	for _, k := range p.keys {
		s, ok := byID[k.ID]
		if !ok {
			continue
		}
		// 只恢复「需要时间判断的状态」;ACTIVE 无状态可恢复
		if s.Status == KeyStatusCooling && now.Before(s.CoolingUntil) {
			k.Status = KeyStatusCooling
			k.CoolingUntil = s.CoolingUntil
			k.CoolingCount = s.CoolingCount
		}
		if s.Status == KeyStatusQuotaExceeded {
			k.Status = KeyStatusQuotaExceeded
			k.QuotaExceededSince = s.QuotaExceededSince
		}
		// 余额快照无条件恢复(poll 下一轮会刷新;恢复前 balanceGuard 有数据可用)
		if !s.LastPolledAt.IsZero() {
			k.Remaining = s.Remaining
			k.LastPolledAt = s.LastPolledAt
			k.QuotaKind = s.QuotaKind
			k.QuotaZeroStreak = s.QuotaZeroStreak
		}
	}
}
