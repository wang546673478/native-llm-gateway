// Package provider — Manager
// 对应规格书 5.2 Provider Manager
package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"go.uber.org/zap"
)

// ManagerConfig Manager 需要的配置视图
type ManagerConfig struct {
	Providers map[string]ManagerProviderConfig
	// Pools 预先构造好的 Pool 映射,LoadFromConfig 会注入到 ProviderConfig.Pool
	Pools map[string]*keypool.Pool // name → *keypool.Pool(具体类型,原 any 已过时)
	// DefaultTimeout 全局 provider 请求超时兜底(来自 config.timeouts.provider_default)。
	// 某 provider 未显式设 timeout(0)时用它;仍为 0 则落到各协议 base 的 60/90s 默认。
	// 修复:config 的 provider_default 字段此前无任何消费方(silent no-op)。
	DefaultTimeout time.Duration
}

// ManagerProviderConfig 单个 Provider 的配置(对应 config.yaml 中 providers.<name>.*)
type ManagerProviderConfig struct {
	Enabled  bool
	Endpoint string
	Protocol Protocol
	Timeout  time.Duration
	Models   []string
	Circuit  ManagerCircuitConfig
	// P37: 模型定价表(对应 config.yaml 中 providers.<name>.models[].cost_per_1k_input/output)
	// 索引:model id → (cost_per_1k_input, cost_per_1k_output),单位 USD
	ModelCosts map[string]ModelCost
	// P47: 计费来源 — token_plan / api / free
	BillingSource string
	// P-catch-all: catch_all 自动模式下该 provider 承接未知模型名的默认模型。
	// 空 = 取 Models 第一个
	DefaultModel string
	// P-responses: 原生支持 OpenAI Responses API(/v1/responses 透传)
	ResponsesAPI bool
	// P-deepseek-thinking: 上行前强制 thinking=disabled(DeepSeek /anthropic
	// 的 thinking 校验会拒绝 compact 后的历史,详见 anthropic_compatible.Config)
	ForceThinkingDisabled bool
}

// ModelCost 单个 model 的定价
// P40: 新增 cache pricing 字段 — 单位 CNY (¥) / 1k tokens
//   - CostPer1kInput:         普通输入(没命中 cache)
//   - CostPer1kOutput:        输出
//   - CostPer1kCacheRead:     cache 命中(读)— 最便宜,DeepSeek v4-flash ¥0.02/M
//   - CostPer1kCacheCreation: 写入 cache(创建新单元)— 中等,通常 = input * 1.25(Anthropic)
//
// P-quota-512k: 长上下文悬崖(MiniMax M3 官方规则 — 输入含缓存 > 阈值,
// 输入/输出/缓存读取全项乘 multiplier;阈值 0 = 不启用)。单位:threshold = tokens,
// multiplier = 倍率(如 512000 / 2)。
type ModelCost struct {
	CostPer1kInput            float64
	CostPer1kOutput           float64
	CostPer1kCacheRead        float64
	CostPer1kCacheCreation    float64
	LongContextInputThreshold int64
	LongContextMultiplier     float64
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
	// P47: billingSource 映射 provider name → "token_plan" / "api" / "free"
	// LoadFromConfig / Reload 时填充
	billingSources map[string]string
	// P-catch-all: provider name → 默认模型(catch_all 自动模式用)。
	// LoadFromConfig / ReloadPricing 时填充;热重载同步
	defaultModels map[string]string
	// P-responses: provider name → 是否原生支持 Responses API(/v1/responses)
	// LoadFromConfig / ReloadPricing 时填充
	responsesAPI map[string]bool
	// P-tier-failover: provider name → 配置的 endpoint(baseURL)
	// LoadFromConfig 时填充,EndpointFor 查用(endpoint 改动需重启,ReloadPricing 不同步)
	endpoints map[string]string
}

// 编译期断言:Manager 满足路由/代理层依赖的窄接口 ProviderLookup
var _ ProviderLookup = (*Manager)(nil)

// NewManager 构造 Manager
func NewManager(registry *Registry, logger *zap.Logger) *Manager {
	return &Manager{
		registry:       registry,
		logger:         logger,
		providers:      make(map[string]Provider),
		pricing:        make(map[string]ModelCost),
		billingSources: make(map[string]string),
		defaultModels:  make(map[string]string),
		responsesAPI:   make(map[string]bool),
		endpoints:      make(map[string]string),
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
			Timeout:          resolveProviderTimeout(pcfg.Timeout, cfg.DefaultTimeout),
			Models:           pcfg.Models,
			Pool:             cfg.Pools[name],
			FailureThreshold: pcfg.Circuit.FailureThreshold,
			FailureWindow:    pcfg.Circuit.FailureWindow,
			OpenTimeout:      pcfg.Circuit.OpenTimeout,
			// P-deepseek-thinking: 透传到 provider 工厂(deepseek-anthropic 用)
			ForceThinkingDisabled: pcfg.ForceThinkingDisabled,
		}

		// P37: 填充定价表
		for modelID, cost := range pcfg.ModelCosts {
			m.pricing[pricingKey(name, modelID)] = cost
		}
		// P47: 填充 billing_source
		bs := pcfg.BillingSource
		if bs == "" {
			bs = "api"
		}
		m.billingSources[name] = bs
		// P-catch-all: 默认模型 — 显式配置优先,否则第一个声明
		dm := pcfg.DefaultModel
		if dm == "" && len(pcfg.Models) > 0 {
			dm = pcfg.Models[0]
		}
		m.defaultModels[name] = dm
		// P-responses: Responses API 能力标记
		m.responsesAPI[name] = pcfg.ResponsesAPI
		// P-tier-failover: endpoint 映射(给 quotacheck.CheckQuota 提供 baseURL)
		m.endpoints[name] = pcfg.Endpoint

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

// BillingSourceFor P47: 查 provider 的计费来源("token_plan" / "api" / "free")
// 未找到返回 "api"(最常见的兜底值)
func (m *Manager) BillingSourceFor(provider string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if bs, ok := m.billingSources[provider]; ok {
		return bs
	}
	return "api"
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

// ReloadPricing 只更新定价表,不动 Provider 实例
// 用于热重载 config.yaml 时同步 cost 改动
func (m *Manager) ReloadPricing(cfg *ManagerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pricing = make(map[string]ModelCost)
	m.billingSources = make(map[string]string)
	m.defaultModels = make(map[string]string)
	for name, pcfg := range cfg.Providers {
		for modelID, cost := range pcfg.ModelCosts {
			m.pricing[pricingKey(name, modelID)] = cost
		}
		bs := pcfg.BillingSource
		if bs == "" {
			bs = "api"
		}
		m.billingSources[name] = bs
		// P-catch-all: 默认模型与 pricing 同频热重载
		dm := pcfg.DefaultModel
		if dm == "" && len(pcfg.Models) > 0 {
			dm = pcfg.Models[0]
		}
		m.defaultModels[name] = dm
		// P-responses: 能力标记同频热重载
		m.responsesAPI[name] = pcfg.ResponsesAPI
	}
	m.logger.Info("pricing reloaded", zap.Int("entries", len(m.pricing)))
}

// SupportsResponsesAPI P-responses: 该 provider 是否原生支持 OpenAI
// Responses API(/v1/responses 透传,Codex 客户端)
func (m *Manager) SupportsResponsesAPI(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.responsesAPI[name]
}

// DefaultModelFor P-catch-all: 返回 provider 承接未知模型名的默认模型
// (显式 default_model 优先,否则第一个声明)。空 = 该 provider 没有可用默认模型
func (m *Manager) DefaultModelFor(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultModels[name]
}

// EndpointFor P-tier-failover: 查 provider 的 endpoint(给 quotacheck.CheckQuota
// 提供 baseURL)。未注册返回空串。
func (m *Manager) EndpointFor(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.endpoints[name]
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
