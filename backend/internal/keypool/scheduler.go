// Package keypool — Key 调度策略
package keypool

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
)

// ErrNoAvailableKey 池中无可用 Key
var ErrNoAvailableKey = errors.New("keypool: no available key")

// Scheduler Key 选择策略接口
type Scheduler interface {
	Select(keys []*Key) (*Key, error)
}

// RoundRobinScheduler 轮询
type RoundRobinScheduler struct {
	counter uint64 // atomic
}

// Select 从 keys 中轮询选一个可用的
// 注意:这里只负责"轮询选择",可用性过滤由 Pool.Acquire 完成
func (s *RoundRobinScheduler) Select(keys []*Key) (*Key, error) {
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}
	idx := atomic.AddUint64(&s.counter, 1) - 1
	return keys[int(idx%uint64(len(keys)))], nil
}

// LeastUsedScheduler 选择 TotalRequests 最少的可用 Key
type LeastUsedScheduler struct{}

// Select 返回 TotalRequests 最小的 Key(平局取首个)
func (s *LeastUsedScheduler) Select(keys []*Key) (*Key, error) {
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}
	best := keys[0]
	for _, k := range keys[1:] {
		if k.TotalRequests < best.TotalRequests {
			best = k
		}
	}
	return best, nil
}

// RandomScheduler 随机(Go 1.20+ math/rand 自动 seed,无需手写)
type RandomScheduler struct {
	mu sync.Mutex
}

func (s *RandomScheduler) Select(keys []*Key) (*Key, error) {
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}
	s.mu.Lock()
	idx := rand.Intn(len(keys))
	s.mu.Unlock()
	return keys[idx], nil
}

// StickyScheduler 顺序黏性(2026-08-10,Level 3):固定用「最高优先级可用 key」。
// bucket 已按优先级序(加入时间 / route_order 改写),keys[0] = 当前最高优先级可用 key,
// 始终选 keys[0]:
//   - 高优先级 key 一直健康 → 一直用它(不轮换)。
//   - 高优先级 key 不可用(QE / COOLING / 熔断 OPEN / 耗尽)→ filterBreakers + IsUsable
//     把它剔除,keys[0] 变成下一把 → 自动推进。
//   - 高优先级 key 恢复(额度 poll 回 ACTIVE / 熔断 HALF_OPEN→CLOSED)→ 重新进 bucket,
//     keys[0] 又变回它 → 自动回位。
// 这就是"先用尽高位再切低位、高位恢复自动回位"的状态机 — 无内部可变状态,无需锁,
// 也无需 Rewind()(2026-08-10):旧实现把 current 黏死在末位,key-1 一次 connection
// 错误把 sticky 推进到 weige 后,即使 key-1 恢复也不回位。
type StickyScheduler struct{}

func (s *StickyScheduler) Select(keys []*Key) (*Key, error) {
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}
	return keys[0], nil
}

// NewScheduler 根据 strategy 字符串构造对应 Scheduler
// 默认(空)= sticky 顺序黏性(2026-08-10,Level 3);round_robin/least_used/random 为显式可选
func NewScheduler(strategy string) Scheduler {
	switch strategy {
	case "least_used":
		return &LeastUsedScheduler{}
	case "random":
		return &RandomScheduler{}
	case "round_robin":
		return &RoundRobinScheduler{}
	case "sticky", "":
		return &StickyScheduler{}
	default:
		// 未知 → 回退 sticky(新默认),避免落到轮换
		return &StickyScheduler{}
	}
}
