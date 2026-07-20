// Package circuit — Manager + Reporter
// 实现 proxy.CircuitReporter 接口,把 proxy 的 success/failure 反馈到 Breaker
// 同时把 Breaker 状态推回 Router(让 RouteIterator 跳过 OPEN 的 Provider)
package circuit

import (
	"sync"
)

// ProviderHealth Provider 健康查询接口(Router 实现这个接口)
// Breaker 通过回调告诉 Router 某个 Provider 是否 OPEN
type ProviderHealth interface {
	SetProviderHealth(name string, open bool)
}

// Manager 持有所有 Provider 的 Breaker
type Manager struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	health   ProviderHealth
}

// NewManager 构造 Manager
func NewManager(health ProviderHealth) *Manager {
	return &Manager{
		breakers: make(map[string]*Breaker),
		health:   health,
	}
}

// GetOrCreate 获取或创建指定 Provider 的 Breaker
func (m *Manager) GetOrCreate(name string, cfg Config) *Breaker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.breakers[name]; ok {
		return b
	}
	b := New(name, cfg)
	m.breakers[name] = b
	return b
}

// Allow 包装 Breaker.Allow,同步状态给 health
func (m *Manager) Allow(name string) bool {
	m.mu.RLock()
	b, ok := m.breakers[name]
	m.mu.RUnlock()
	if !ok {
		return true // 没有 breaker 就放行
	}
	allowed := b.Allow()
	// 把状态推给 Router(失败时推 OPEN=true)
	if !allowed && m.health != nil {
		m.health.SetProviderHealth(name, true)
	}
	return allowed
}

// IsOpen 查询 Provider 是否处于 OPEN 状态(纯查询)
func (m *Manager) IsOpen(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.breakers[name]
	if !ok {
		return false
	}
	return b.State() == StateOpen
}

// Reporter 实现 proxy.CircuitReporter 接口
// 内部把 success/failure 同步给对应 Breaker 和 Router 健康状态
type Reporter struct {
	mgr    *Manager
	health ProviderHealth
}

// NewReporter 构造 Reporter
func NewReporter(mgr *Manager) *Reporter {
	return &Reporter{mgr: mgr, health: mgr.health}
}

// RecordSuccess 实现 proxy.CircuitReporter
func (r *Reporter) RecordSuccess(providerName string) {
	mgr := r.mgr
	mgr.mu.RLock()
	b, ok := mgr.breakers[providerName]
	mgr.mu.RUnlock()
	if !ok {
		return
	}
	b.RecordSuccess()
	// 成功后,告诉 Router 这个 Provider 健康了(若之前 OPEN)
	if b.State() == StateClosed && r.health != nil {
		r.health.SetProviderHealth(providerName, false)
	}
}

// RecordFailure 实现 proxy.CircuitReporter
func (r *Reporter) RecordFailure(providerName string, errorType string) {
	mgr := r.mgr
	mgr.mu.RLock()
	b, ok := mgr.breakers[providerName]
	mgr.mu.RUnlock()
	if !ok {
		return
	}
	b.RecordFailure(errorType)
	if b.State() == StateOpen && r.health != nil {
		r.health.SetProviderHealth(providerName, true)
	}
}

// AllStats 调试用:所有 Breaker 的状态
func (m *Manager) AllStats() []Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Stats, 0, len(m.breakers))
	for _, b := range m.breakers {
		out = append(out, b.Stats())
	}
	return out
}
