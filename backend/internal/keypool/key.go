// Package keypool 实现 Provider API Key 的池化管理
// 对应规格书 5.4 Key Pool
package keypool

import (
	"time"
)

// KeyStatus Key 的运行时状态
type KeyStatus string

const (
	KeyStatusActive        KeyStatus = "ACTIVE"          // 正常可用
	KeyStatusCooling       KeyStatus = "COOLING"         // 429 后冷却中
	KeyStatusLimited       KeyStatus = "LIMITED"         // 配额受限(预留)
	KeyStatusDisabled      KeyStatus = "DISABLED"        // 累计冷却超阈值,永久禁用
	// P68: 配额耗尽(quota_exceeded) — 区别于 DISABLED,worker 可恢复
	KeyStatusQuotaExceeded KeyStatus = "QUOTA_EXCEEDED"
)

// Key 是 Provider 的单个 API Key
// Key 字段在运行时是明文,落库时由 Encryptor 加密
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
	// P68: 探测次数(成功 Restore 时 reset 为 0;max attempts 触发 DISABLED)
	QuotaProbeAttempts int
	// P-quota-balance: 上游 quota polling 写入的余额快照与时间戳(runtime, 不落 DB)
	Remaining    float64
	LastPolledAt time.Time
	// P-quota-display: 上次 poll 的数值类型("percent"/"currency"/"")— 前端按此渲染单位
	QuotaKind string
}

// IsUsable 在给定时间点判断 Key 是否可用于调度
func (k *Key) IsUsable(now time.Time) bool {
	switch k.Status {
	case KeyStatusActive, KeyStatusLimited:
		return true
	case KeyStatusCooling:
		return now.After(k.CoolingUntil)
	case KeyStatusDisabled:
		return false
	case KeyStatusQuotaExceeded:
		// P68: 配额耗尽期间不可用,等 worker 探测到恢复才回 ACTIVE
		return false
	default:
		return false
	}
}
