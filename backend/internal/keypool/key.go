// Package keypool 实现 Provider API Key 的池化管理。
package keypool

import (
	"time"
)

// KeyStatus Key 的运行时状态
type KeyStatus string

const (
	KeyStatusActive  KeyStatus = "ACTIVE"  // 正常可用
	KeyStatusCooling KeyStatus = "COOLING" // 429 后冷却中
	KeyStatusLimited KeyStatus = "LIMITED" // 配额受限(预留)
	// P-no-disabled: 没有 DISABLED 状态 — 终端状态没有恢复路径,瞬时限流/误判
	// 会永久杀掉 healthy key。所有失败都映射到可恢复状态(COOLING / QUOTA_EXCEEDED),
	// 由冷却到期或 balancer poll 自动恢复
	// P68: 配额耗尽(quota_exceeded) — 区别于 DISABLED,worker 可恢复
	KeyStatusQuotaExceeded KeyStatus = "QUOTA_EXCEEDED"
)

// Key 是 Provider 的单个 API Key。
// Key 字段在运行时是明文；provider_api_keys.key_hash 当前同样保存明文。
// P48: 加 BillingSource — Pool.Acquire 按 token_plan > api > free 优先级返回 key
type Key struct {
	ID            string
	ProviderName  string
	Name          string
	Key           string
	Status        KeyStatus
	CoolingUntil  time.Time
	CoolingCount  int
	TotalRequests int64
	TotalTokens   int64
	ErrorCount    int
	// P48: 计费来源 tier(token_plan / api / free),影响 Pool.Acquire 优先级
	BillingSource string
	LastUsedAt    time.Time
	LastErrorAt   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// P68: quota restore worker 用 — 第一次被标记 QUOTA_EXCEEDED 的时间
	QuotaExceededSince time.Time
	// P68: 探测次数(成功 Restore 时 reset 为 0)
	QuotaProbeAttempts int
	// P-quota-poll-guard: poll 连续读到无额度的次数(读到有额度时 reset 为 0)。
	// 连续 >=2 轮才确认耗尽标 QE — 单次瞬态 0 读不误杀 healthy key
	// (2026-08-05 实测:MiniMax 余额 API 瞬态 0 → key-1 被标 QE 一晚上趴着)
	QuotaZeroStreak int
	// P-quota-balance: 上游 quota polling 写入的余额快照与时间戳(runtime, 不落 DB)
	Remaining    float64
	LastPolledAt time.Time
	// P-quota-display: 上次 poll 的数值类型("percent"/"currency"/"")— 前端按此渲染单位
	QuotaKind string
	// P-provider-vendor: 该 key 可用的协议列表(逗号分隔,空 = 全部);从 DB ProviderAPIKey.Protocols 读入
	Protocols string
	// P-per-key-circuit: 熔断快照(API 展示用)。实时状态在 pool 的 per-key breaker 里,
	// Keys() 返回快照时刷新这两个字段。空 CircuitState = 该 provider 未配置熔断
	CircuitOpen  bool
	CircuitState string // CLOSED / OPEN / HALF_OPEN
}

// IsPolledAndExhausted P-quota-prefer: 已轮询确认余额耗尽的 key —
// 任意单位 Remaining <= 0,或 percent 单位 <= 1(MiniMax 自己的 chat API
// 对 1% 就报 2056 用量上限,实测 2026-08-06)。未轮询过的 key(Remaining=0
// 是默认值不是真余额,启动窗口)不算 — 交给请求路径和 poll 确认。
// 窗口刷新/充值后 Remaining 回升 → 自动恢复参与调度
func (k *Key) IsPolledAndExhausted() bool {
	if k.LastPolledAt.IsZero() {
		return false
	}
	if k.Remaining <= 0 {
		return true
	}
	if k.QuotaKind == "percent" && k.Remaining <= 1 {
		return true
	}
	return false
}

// IsUsable 在给定时间点判断 Key 是否可用于调度
func (k *Key) IsUsable(now time.Time) bool {
	switch k.Status {
	case KeyStatusActive, KeyStatusLimited:
		return true
	case KeyStatusCooling:
		return now.After(k.CoolingUntil)
	case KeyStatusQuotaExceeded:
		// P68: 配额耗尽期间不可用,等 worker 探测到恢复才回 ACTIVE
		return false
	default:
		return false
	}
}
