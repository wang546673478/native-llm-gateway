// Package inflight 维护「trace_id → 请求状态」的并发安全内存快照,
// 供实时「活跃请求」视图读取。纯内存态、结束即消、不留历史。
//
// 窄接口约定:Registry 只暴露 Put / SetProvider / SetFinalModel / Delete /
// Snapshot 五个方法。
// 未来多实例上 Redis 时,只需替换本包内部 map 为 redis.Client,proxy 层不改一行。
package inflight

import (
	"sort"
	"sync"
	"time"
)

// Snapshot 一条活跃请求的只读快照。
// 全字段为值类型 + string,Snapshot 返回的结构体拷贝即与后续写入隔离。
type Snapshot struct {
	TraceID   string
	StartedAt time.Time // 请求开始,elapsed_ms 由调用方现算(now - StartedAt)
	// RequestedModel 客户端原始请求名(alias 解析前),如 opus。
	RequestedModel string
	// FinalModel 路由实际使用的上游候选模型(result.ModelID),如 MiniMax-M3。
	// 首次候选选定前为零值(空)。
	FinalModel     string
	ProviderName   string // 当前正在打的 vendor,随 failover 实时更新
	GatewayKeyName string
	IsStream       bool
}

// Registry 并发安全的内存快照表。
type Registry struct {
	mu sync.RWMutex
	m  map[string]*Snapshot
}

// NewRegistry 构造一个空的 Registry。
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]*Snapshot)}
}

// Put 记录一条最早开始的活跃请求。若同 TraceID 已存在则覆盖。
func (r *Registry) Put(s *Snapshot) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s.TraceID] = s
}

// SetProvider 更新一条活跃请求当前正在打的 provider。未知 TraceID 是 no-op。
func (r *Registry) SetProvider(traceID, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.m[traceID]; ok {
		s.ProviderName = provider
	}
}

// SetFinalModel 更新一条活跃请求实际使用的上游模型。未知 TraceID 是 no-op。
func (r *Registry) SetFinalModel(traceID, modelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.m[traceID]; ok {
		s.FinalModel = modelID
	}
}

// Delete 移除一条已结束的请求。未知 TraceID 是 no-op。
func (r *Registry) Delete(traceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, traceID)
}

// Snapshot 返回当前所有活跃请求的只读列表,按 StartedAt 升序。
func (r *Registry) Snapshot() []*Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Snapshot, 0, len(r.m))
	for _, s := range r.m {
		cp := *s // 拷贝结构体,与后续 SetProvider 写入隔离
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
