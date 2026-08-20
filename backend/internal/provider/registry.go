// Package provider — Registry
// 对应规格书 5.3 Provider Registry
package provider

import (
	"fmt"
	"sync"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
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
	Pool             *keypool.Pool // 具体类型 — provider 已 import keypool(SetPool 用),原 interface{} "避免循环" 注释已过时
	FailureThreshold int
	FailureWindow    time.Duration
	OpenTimeout      time.Duration
	// ForceThinkingDisabled P-deepseek-thinking: 上行前强制 thinking=disabled
	// (DeepSeek /anthropic 在 thinking 模式下校验历史 thinking 块,compact 会触发 400)
	ForceThinkingDisabled bool
	// BillingSource P47 计费面(token_plan / api / free)。
	// 用于「按面取 key」:同 vendor 的多个面共享一个 key 池,但 key 与端点是绑定的
	// (mimo 实测:tp- key 只在 token-plan 端点有效、sk- key 只在 api 端点有效,
	// 交叉调用一律 401)。ListModels/HealthCheck 必须按本面的计费源取 key,
	// 不能用 AcquireForProtocol —— 它按 TierOrder 取,永远先给 token_plan 的 key。
	BillingSource string
}

// RegisteredInfo 单个注册名的注册元数据(vendor 用于前端按厂商聚合)
type RegisteredInfo struct {
	Protocol Protocol
	Vendor   string
}

// Registry 维护 name → Factory + Protocol 的映射
// 每个 Provider 包在 init() 时调用 Register 注册自己
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	protocols map[string]Protocol // 用于前端显示绑定选项,即使 provider 未启用
	vendors   map[string]string   // P-provider-vendor: name → vendor(默认 = name)
}

// NewRegistry 构造空 Registry
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]Factory),
		protocols: make(map[string]Protocol),
		vendors:   make(map[string]string),
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

// RegisterGlobalWithProtocolVendor 注册时同时记录 protocol 和 vendor 元数据
func RegisterGlobalWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string) {
	defaultRegistry.RegisterWithProtocolVendor(name, factory, proto, vendor)
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
	r.RegisterWithProtocolVendor(name, factory, proto, name)
}

// RegisterWithProtocolVendor 同 RegisterWithProtocol,但额外记录 vendor 元数据
// vendor 为空时默认 = name(单协议厂商)
func (r *Registry) RegisterWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("provider factory %q already registered", name))
	}
	r.factories[name] = factory
	r.protocols[name] = proto
	if vendor == "" {
		vendor = name
	}
	r.vendors[name] = vendor
}

// VendorFor 查询注册名的 vendor;未注册或未声明时返回 name 本身
func (r *Registry) VendorFor(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if v, ok := r.vendors[name]; ok {
		return v
	}
	return name
}

// ListRegisteredInfo 返回所有已注册 name 的注册元数据
func (r *Registry) ListRegisteredInfo() map[string]RegisteredInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]RegisteredInfo, len(r.factories))
	for n := range r.factories {
		v := r.vendors[n]
		if v == "" {
			v = n // 与 VendorFor 的 fallback 保持一致(plain Register 未记录 vendor)
		}
		out[n] = RegisteredInfo{
			Protocol: r.protocols[n],
			Vendor:   v,
		}
	}
	return out
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
