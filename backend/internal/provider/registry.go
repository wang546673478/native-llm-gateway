// Package provider — Registry
// 对应规格书 5.3 Provider Registry
package provider

import (
	"fmt"
	"sync"
	"time"
)

// Factory 是 Provider 工厂函数
// 每个 Provider 包(DeepSeek / GLM / Qwen / Kimi / MiniMax / Gemini 等)
// 负责注册一个 Factory,用于从配置动态创建实例
type Factory func(config ProviderConfig) (Provider, error)

// ProviderConfig 是创建 Provider 实例所需的最小配置
// 对应 config.yaml 中的 providers.<name>.*
type ProviderConfig struct {
	Name             string
	Endpoint         string
	Protocol         Protocol
	Timeout          time.Duration
	Models           []string
	APIKeys          []string // 明文,内部会加密传给 KeyPool
	Pool             interface{} // *keypool.Pool — 用 interface{} 避免循环依赖
	FailureThreshold int
	FailureWindow    time.Duration
	OpenTimeout      time.Duration
}

// Registry 维护 name → Factory + Protocol 的映射
// 每个 Provider 包在 init() 时调用 Register 注册自己
type Registry struct {
	mu          sync.RWMutex
	factories   map[string]Factory
	protocols   map[string]Protocol // 用于前端显示绑定选项,即使 provider 未启用
}

// NewRegistry 构造空 Registry
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]Factory),
		protocols: make(map[string]Protocol),
	}
}

// 默认全局 Registry(供 init() 风格的自动注册使用)
// 业务代码可以自己 NewRegistry() 构造独立实例,但通常用这个就够了
var defaultRegistry = NewRegistry()

// Default 返回全局默认 Registry
func Default() *Registry { return defaultRegistry }

// RegisterGlobal 把 factory 注册到全局 Registry
// 每个 Provider 包在 init() 时调用一次
func RegisterGlobal(name string, factory Factory) {
	defaultRegistry.Register(name, factory)
}

// RegisterGlobalWithProtocol 注册时同时记录 protocol 元数据,
// 让 /providers/registered 接口在 provider 未加载时也能返回正确的 protocol
func RegisterGlobalWithProtocol(name string, factory Factory, proto Protocol) {
	defaultRegistry.RegisterWithProtocol(name, factory, proto)
}

// Register 注册一个 Provider factory
// name 必须唯一;重复注册会 panic,因为这是编程错误
func (r *Registry) Register(name string, factory Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("provider factory %q already registered", name))
	}
	r.factories[name] = factory
}

// RegisterWithProtocol 同 Register,但额外记录 protocol 元数据
func (r *Registry) RegisterWithProtocol(name string, factory Factory, proto Protocol) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("provider factory %q already registered", name))
	}
	r.factories[name] = factory
	r.protocols[name] = proto
}

// ListRegisteredProtocols 返回所有已注册 provider 的 protocol
func (r *Registry) ListRegisteredProtocols() map[string]Protocol {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Protocol, len(r.protocols))
	for k, v := range r.protocols {
		out[k] = v
	}
	return out
}

// Create 用已注册的 factory 创建 Provider 实例
func (r *Registry) Create(name string, cfg ProviderConfig) (Provider, error) {
	r.mu.RLock()
	factory, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider %q is not registered (available: %v)", name, r.ListRegistered())
	}
	return factory(cfg)
}

// ListRegistered 返回所有已注册的 Provider name
func (r *Registry) ListRegistered() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	return names
}
