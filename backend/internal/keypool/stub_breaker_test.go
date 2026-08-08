package keypool

import (
	"sync"
	"sync/atomic"
)

// Breaker 测试用实现 — 简化版熔断器,无依赖
//
// 老的测试用 circuit.Config/CircuitBreaker(CircuitConfig 来注入 pool),
// 那个时代 Pool 还直接 import circuit。重构后 Pool 不再 import circuit,
// 测试也用 BreakerFactory 注入。
type stubBreaker struct {
	mu             sync.Mutex
	failures       int64
	threshold      int
	open           atomic.Bool
	successCount   int64
	failureCount   int64
	allowReturn    bool
	recordFailures []string
}

func newStubBreaker(threshold int) *stubBreaker {
	return &stubBreaker{threshold: threshold, allowReturn: true}
}

func (b *stubBreaker) Allow() bool {
	if b.open.Load() {
		return false
	}
	return b.allowReturn
}

func (b *stubBreaker) RecordSuccess() {
	atomic.AddInt64(&b.successCount, 1)
	b.failures = 0
	b.open.Store(false)
}

func (b *stubBreaker) RecordFailure(_ string) {
	atomic.AddInt64(&b.failureCount, 1)
	b.failures++
	if b.failures >= int64(b.threshold) {
		b.open.Store(true)
	}
}

func (b *stubBreaker) State() string {
	if b.open.Load() {
		return "OPEN"
	}
	return "CLOSED"
}

// stubBreakerFactory 测试用工厂 — 每次返回新实例(模拟 per-key 独立熔断)
func stubBreakerFactory(threshold int) BreakerFactory {
	return func(_ string) Breaker {
		return newStubBreaker(threshold)
	}
}
