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

// NewScheduler 根据 strategy 字符串构造对应 Scheduler
func NewScheduler(strategy string) Scheduler {
	switch strategy {
	case "least_used":
		return &LeastUsedScheduler{}
	case "random":
		return &RandomScheduler{}
	case "round_robin", "":
		return &RoundRobinScheduler{}
	default:
		return &RoundRobinScheduler{}
	}
}

