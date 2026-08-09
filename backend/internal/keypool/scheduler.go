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

// StickyScheduler 顺序黏性(2026-08-10,Level 3):固定用「第一个可用 key」并黏住不换,
// 除非它不可用(被 usable 过滤剔除)→ 推进到当前列表第一个可用。
// 默认顺序 = 进池顺序(key 加入时间 / route_order 改写),第一个 = 最高优先级。
// Rewind() 置空 current,让下一次 Select 重新从列表头扫 — 用于「高位 key 恢复后回位」。
type StickyScheduler struct {
	mu      sync.Mutex
	current *Key
}

func (s *StickyScheduler) Select(keys []*Key) (*Key, error) {
	if len(keys) == 0 {
		return nil, ErrNoAvailableKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 当前 key 仍在可用列表里(未被 QE/COOLING/熔断/耗尽剔除)→ 黏住它(不扫)
	if s.current != nil {
		for _, k := range keys {
			if k == s.current {
				return k, nil
			}
		}
	}
	// 当前 key 不可用 → 推进到自然序第一把可用 key
	s.current = keys[0]
	return s.current, nil
}

// Rewind 置空当前黏住的 key — 额度恢复等「高位 key 回位」事件调用,让下次 Select 重新从头扫
func (s *StickyScheduler) Rewind() {
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
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
