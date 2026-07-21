// Package provider — Manager
// 对应规格书 5.2 Provider Manager
package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ManagerConfig Manager 需要的配置视图
type ManagerConfig struct {
	Providers map[string]ManagerProviderConfig
	// Pools 预先构造好的 Pool 映射,LoadFromConfig 会注入到 ProviderConfig.Pool
	Pools map[string]any // name → *keypool.Pool(用 any 避免循环依赖)
}

// ManagerProviderConfig 单个 Provider 的配置(对应 config.yaml 中 providers.<name>.*)
type ManagerProviderConfig struct {
	Enabled  bool
	Endpoint string
	Protocol Protocol
	Timeout  time.Duration
	Models   []string
	APIKeys  []string
	Circuit  ManagerCircuitConfig
	// P37: 模型定价表(对应 config.yaml 中 providers.<name>.models[].cost_per_1k_input/output)
	// 索引:model id → (cost_per_1k_input, cost_per_1k_output),单位 USD
	ModelCosts map[string]ModelCost
}

// ModelCost 单个 model 的定价
type ModelCost struct {
	CostPer1kInput  float64
	CostPer1kOutput float64
}

// ManagerCircuitConfig Circuit Breaker 配置
type ManagerCircuitConfig struct {
	FailureThreshold int
	FailureWindow    time.Duration
	OpenTimeout      time.Duration
	HalfOpenRequests int
}

// Manager 持有所有活跃 Provider 实例
type Manager struct {
	registry *Registry
	logger   *zap.Logger

	mu        sync.RWMutex
	providers map[string]Provider
	// P37: 定价表 key = "<provider>:<model_id>",value = ModelCost
	// 在 LoadFromConfig / Reload 时填充
	pricing map[string]ModelCost
}

// NewManager 构造 Manager
func NewManager(registry *Registry, logger *zap.Logger) *Manager {
	return &Manager{
		registry:  registry,
		logger:    logger,
		providers: make(map[string]Provider),
		pricing:   make(map[string]ModelCost),
	}
}

// LoadFromConfig 从配置加载所有 enabled 的 Provider
func (m *Manager) LoadFromConfig(ctx context.Context, cfg *ManagerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pricing = make(map[string]ModelCost)

	loaded := 0
	for name, pcfg := range cfg.Providers {
		if !pcfg.Enabled {
			m.logger.Info("provider disabled, skipping", zap.String("provider", name))
			continue
		}

		factoryCfg := ProviderConfig{
			Name:             name,
			Endpoint:         pcfg.Endpoint,
			Protocol:         pcfg.Protocol,
			Timeout:          pcfg.Timeout,
			Models:           pcfg.Models,
			APIKeys:          pcfg.APIKeys,
			Pool:             cfg.Pools[name],
			FailureThreshold: pcfg.Circuit.FailureThreshold,
			FailureWindow:    pcfg.Circuit.FailureWindow,
			OpenTimeout:      pcfg.Circuit.OpenTimeout,
		}

		// P37: 填充定价表
		for modelID, cost := range pcfg.ModelCosts {
			m.pricing[pricingKey(name, modelID)] = cost
		}

		p, err := m.registry.Create(name, factoryCfg)
		if err != nil {
			m.logger.Warn("create provider failed, skipping",
				zap.String("provider", name),
				zap.Error(err))
			continue
		}

		// 健康检查(短超时,不阻塞启动)
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := p.HealthCheck(hctx); err != nil {
			m.logger.Warn("provider health check failed, still loaded",
				zap.String("provider", name),
				zap.Error(err))
		} else {
			m.logger.Info("provider loaded",
				zap.String("provider", name),
				zap.String("protocol", string(p.Protocol())),
				zap.Int("models", len(p.Models())))
		}
		cancel()

		m.providers[name] = p
		loaded++
	}

	if loaded == 0 {
		return fmt.Errorf("no providers loaded from config (registry has: %v)", m.registry.ListRegistered())
	}
	m.logger.Info("providers loaded", zap.Int("count", loaded))
	return nil
}

// Get 按名字获取 Provider
func (m *Manager) Get(name string) (Provider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[name]
	return p, ok
}

// GetAll 返回所有 Provider 的快照
func (m *Manager) GetAll() map[string]Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Provider, len(m.providers))
	for k, v := range m.providers {
		out[k] = v
	}
	return out
}

// GetByProtocol 返回所有声明该协议的 Provider
func (m *Manager) GetByProtocol(proto Protocol) []Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Provider, 0)
	for _, p := range m.providers {
		if p.Protocol() == proto {
			out = append(out, p)
		}
	}
	return out
}

// Names 返回所有活跃 Provider 名字
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.providers))
	for n := range m.providers {
		out = append(out, n)
	}
	return out
}

// CostFor P37: 查 (provider, model) 的定价
// 未找到返回 zero value(cost=0)— Proxy 会用 0 兜底
func (m *Manager) CostFor(provider, model string) ModelCost {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.pricing[pricingKey(provider, model)]; ok {
		return c
	}
	return ModelCost{}
}

// pricingKey 内部 hash key
func pricingKey(provider, model string) string {
	return provider + ":" + model
}

// Reload 重新加载(简化:关掉旧的再 Load,后续可加 diff)
func (m *Manager) Reload(ctx context.Context, cfg *ManagerConfig) error {
	m.mu.Lock()
	for name, p := range m.providers {
		if err := p.Close(); err != nil {
			m.logger.Warn("close provider on reload", zap.String("provider", name), zap.Error(err))
		}
	}
	m.providers = make(map[string]Provider)
	m.mu.Unlock()

	return m.LoadFromConfig(ctx, cfg)
}

// Close 关闭所有 Provider
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for name, p := range m.providers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", name, err)
		}
	}
	m.providers = make(map[string]Provider)
	return firstErr
}

// SetForTesting 直接塞入一个已构造的 Provider(仅测试用)
func (m *Manager) SetForTesting(name string, p Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[name] = p
}
