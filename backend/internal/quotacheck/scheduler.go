// Package quotacheck — min-heap scheduler for pending probes
package quotacheck

import (
	"container/heap"
	"sync"
	"time"
)

// probeItem min-heap 元素
type probeItem struct {
	keyProvider string // "minimax"
	keyID       string // provider 内的 key id
	attempts    int
	nextAt      time.Time
	backoff     time.Duration
}

type probeHeap []*probeItem

func (h probeHeap) Len() int { return len(h) }
func (h probeHeap) Less(i, j int) bool { return h[i].nextAt.Before(h[j].nextAt) }
func (h probeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *probeHeap) Push(x any) { *h = append(*h, x.(*probeItem)) }
func (h *probeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// Scheduler 持堆 + 索引
type Scheduler struct {
	mu    sync.Mutex
	heap  *probeHeap
	index map[string]*probeItem // "minimax:1" → item,用于 dedup / re-schedule
	now   func() time.Time       // 可注入用于 test
}

func NewScheduler() *Scheduler {
	h := &probeHeap{}
	heap.Init(h)
	return &Scheduler{
		heap:  h,
		index: make(map[string]*probeItem),
		now:   time.Now,
	}
}

// scheduleKey 把 key 入堆(已经入过则更新 nextAt + backoff + attempts)
func (s *Scheduler) scheduleKey(providerName, keyID string, nextAt time.Time, backoff time.Duration, attempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := providerName + ":" + keyID
	if existing, ok := s.index[id]; ok {
		existing.nextAt = nextAt
		existing.backoff = backoff
		existing.attempts = attempts
		heap.Fix(s.heap, s.findIndex(id))
		return
	}
	item := &probeItem{
		keyProvider: providerName,
		keyID:       keyID,
		attempts:    attempts,
		nextAt:      nextAt,
		backoff:     backoff,
	}
	heap.Push(s.heap, item)
	s.index[id] = item
}

func (s *Scheduler) findIndex(id string) int {
	for i, it := range *s.heap {
		if s.index[id] == it {
			return i
		}
	}
	return -1
}

// popDueItems 弹出所有 nextAt <= now 的 items
func (s *Scheduler) popDueItems(now time.Time) []*probeItem {
	s.mu.Lock()
	defer s.mu.Unlock()

	var due []*probeItem
	for s.heap.Len() > 0 {
		top := (*s.heap)[0]
		if top.nextAt.After(now) {
			break
		}
		heap.Pop(s.heap)
		delete(s.index, top.keyProvider+":"+top.keyID)
		due = append(due, top)
	}
	return due
}

// removeKey 从堆里移除一个 key
func (s *Scheduler) removeKey(providerName, keyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := providerName + ":" + keyID
	if _, ok := s.index[id]; !ok {
		return
	}
	// 重建堆(skip removed item)
	newHeap := &probeHeap{}
	heap.Init(newHeap)
	for _, it := range *s.heap {
		if it.keyProvider+":"+it.keyID == id {
			continue
		}
		heap.Push(newHeap, it)
	}
	s.heap = newHeap
	delete(s.index, id)
}

// pendingCount 调试用:返回堆里待处理 item 数
func (s *Scheduler) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heap.Len()
}
