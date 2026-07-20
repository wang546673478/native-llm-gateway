// Package circuit 实现 Circuit Breaker(熔断器)
// 对应规格书 5.6
//
// 状态机:
//   CLOSED   → 正常处理请求;失败计数超出阈值 → OPEN
//   OPEN     → 直接拒绝请求;OpenTimeout 后 → HALF_OPEN
//   HALF_OPEN→ 放行最多 HalfOpenRequests 个试探请求;
//              全部成功 → CLOSED;任一失败 → 重新 OPEN
package circuit

import (
	"sync"
	"time"
)

// State 熔断器状态
type State string

const (
	StateClosed   State = "CLOSED"
	StateOpen     State = "OPEN"
	StateHalfOpen State = "HALF_OPEN"
)

// 默认错误类型(与 provider.ErrorType 对齐)
var defaultCountableErrors = map[string]bool{
	"server_error": true,
	"timeout":      true,
	"connection":   true,
}
var defaultExcludedErrors = map[string]bool{
	"rate_limit": true,
}

// Config 熔断器配置
type Config struct {
	FailureThreshold int
	FailureWindow    time.Duration
	OpenTimeout      time.Duration
	HalfOpenRequests int
	CountableErrors  []string
	ExcludedErrors   []string
}

// Breaker 单个 Provider 的熔断器
type Breaker struct {
	name   string
	config Config

	mu           sync.Mutex
	state        State
	failures     []time.Time    // 滑动窗口内的失败时间戳
	successCount int            // HALF_OPEN 期间累计成功数
	openedAt     time.Time
	halfOpenInFlight int        // HALF_OPEN 期间已发出去的请求数
}

// New 构造 Breaker
func New(name string, cfg Config) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = 60 * time.Second
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 30 * time.Second
	}
	if cfg.HalfOpenRequests <= 0 {
		cfg.HalfOpenRequests = 1
	}

	countable := defaultCountableErrors
	if len(cfg.CountableErrors) > 0 {
		countable = make(map[string]bool, len(cfg.CountableErrors))
		for _, e := range cfg.CountableErrors {
			countable[e] = true
		}
	}
	excluded := defaultExcludedErrors
	if len(cfg.ExcludedErrors) > 0 {
		excluded = make(map[string]bool, len(cfg.ExcludedErrors))
		for _, e := range cfg.ExcludedErrors {
			excluded[e] = true
		}
	}

	return &Breaker{
		name:   name,
		config: cfg,
		state:  StateClosed,
	}
}

// shouldCount 判断给定错误类型是否计入熔断统计
func (b *Breaker) shouldCount(errType string) bool {
	if errType == "" {
		return false
	}
	if _, excluded := defaultExcludedErrors[errType]; excluded {
		return false
	}
	// 未配置 countable_errors 时,server_error / timeout / connection 都计入
	return defaultCountableErrors[errType]
}

// Allow 检查是否允许请求通过
// 返回 false 时表示熔断器 OPEN,调用方应跳过该 Provider
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	switch b.state {
	case StateClosed:
		return true

	case StateOpen:
		if now.Sub(b.openedAt) >= b.config.OpenTimeout {
			// 超时,转入 HALF_OPEN
			b.state = StateHalfOpen
			b.halfOpenInFlight = 0
			b.successCount = 0
			b.failures = nil
			// 允许第一个试探请求
			b.halfOpenInFlight++
			return true
		}
		return false

	case StateHalfOpen:
		if b.halfOpenInFlight < b.config.HalfOpenRequests {
			b.halfOpenInFlight++
			return true
		}
		return false
	}
	return true
}

// RecordSuccess 记录成功
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		b.successCount++
		if b.successCount >= b.config.HalfOpenRequests {
			// 试探全部成功 → 关闭熔断
			b.state = StateClosed
			b.failures = nil
			b.successCount = 0
			b.halfOpenInFlight = 0
		}
	case StateClosed:
		// 清掉一些旧失败记录(成功也算一种"修复信号")
		now := time.Now()
		cutoff := now.Add(-b.config.FailureWindow)
		newFails := b.failures[:0]
		for _, t := range b.failures {
			if t.After(cutoff) {
				newFails = append(newFails, t)
			}
		}
		b.failures = newFails
	}
}

// RecordFailure 记录失败
// errType 与 provider.ErrorType 对齐:"server_error"/"timeout"/"connection" 等
func (b *Breaker) RecordFailure(errType string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	// 不计入的错误类型(如 429)直接忽略
	if !b.shouldCount(errType) {
		return
	}

	switch b.state {
	case StateClosed:
		// 滑动窗口:清掉窗口外的,加新失败
		cutoff := now.Add(-b.config.FailureWindow)
		newFails := b.failures[:0]
		for _, t := range b.failures {
			if t.After(cutoff) {
				newFails = append(newFails, t)
			}
		}
		newFails = append(newFails, now)
		b.failures = newFails

		if len(b.failures) >= b.config.FailureThreshold {
			b.state = StateOpen
			b.openedAt = now
		}

	case StateHalfOpen:
		// 试探期失败 → 重新 OPEN
		b.state = StateOpen
		b.openedAt = now
		b.halfOpenInFlight = 0
		b.successCount = 0
	}
}

// State 返回当前状态
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Reset 手动恢复(运维用)
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.failures = nil
	b.successCount = 0
	b.halfOpenInFlight = 0
}

// Stats 返回调试信息
type Stats struct {
	Name             string `json:"name"`
	State            State  `json:"state"`
	FailuresInWindow int    `json:"failures_in_window"`
	OpenedAt         string `json:"opened_at,omitempty"`
}

// Stats 状态摘要
func (b *Breaker) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Stats{Name: b.name, State: b.state, FailuresInWindow: len(b.failures)}
	if b.state == StateOpen {
		s.OpenedAt = b.openedAt.Format(time.RFC3339)
	}
	return s
}

// Name 返回 Provider 名字
func (b *Breaker) Name() string { return b.name }
