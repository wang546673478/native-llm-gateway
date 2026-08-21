// Package provider — Manager
// 对应规格书 5.2 Provider Manager
package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	Circuit  ManagerCircuitConfig
	// P47: 计费来源 — token_plan / api / free
	BillingSource string
	// P-responses: 原生支持 OpenAI Responses API(/v1/responses 透传)
	ResponsesAPI bool
	// P-deepseek-thinking: 上行前强制 thinking=disabled(DeepSeek /anthropic
	// 的 thinking 校验会拒绝 compact 后的历史,详见 anthropic_compatible.Config)
	ForceThinkingDisabled bool
}

// ModelCost 单个 model 的定价
// 三档每百万 token 定价,单位 CNY (¥) / 1M tokens:
//   - CostPerMillionInput:     输入(未缓存)每百万 token
//   - CostPerMillionCacheRead: 缓存命中输入每百万 token;无此概念 = 0
//   - CostPerMillionOutput:    输出每百万 token
type ModelCost struct {
	CostPerMillionInput     float64
	CostPerMillionCacheRead float64
	CostPerMillionOutput    float64
}

// DBModelRow provider 包侧对 database.ProviderModel 的投影结构体。
// 为免 provider 包 import database 的具体类型,由 server 装配层的适配器把
// database.ProviderModel 投影成 DBModelRow。粒度 = vendor(厂商),非注册面。
type DBModelRow struct {
	Vendor                  string
	ModelID                 string
	CostPerMillionInput     float64
	CostPerMillionCacheRead float64
	CostPerMillionOutput    float64
}

// DBFaceRow provider 包侧对 database.ProviderModelFace 的投影结构体。
// 粒度 = 注册面(face),记「该面提供哪些模型」+ 面内顺序。
type DBFaceRow struct {
	Vendor    string
	Face      string
	ModelID   string
	SortOrder int
}

// ModelStore provider 包定义的模型读取窄接口(依赖倒置,provider 不 import database)。
// 由 server 装配层用适配器把 database.ProviderModelStore 适配成本接口。
type ModelStore interface {
	// All 返回全部厂商模型行(按 vendor/model_id 有序)。
	All(ctx context.Context) ([]DBModelRow, error)
	// ListByVendor 返回某厂商的全部模型行。
	ListByVendor(ctx context.Context, vendor string) ([]DBModelRow, error)
	// AllFaces 返回全部面归属行(按 vendor/face/sort_order 有序)。
	AllFaces(ctx context.Context) ([]DBFaceRow, error)
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
	// P-model-face: 注册面名 → 该面提供的模型(按面内 sort_order 有序)。
	// 空 map / 某面缺失 = 该面无归属数据 → ModelsFor 退回 vendor 级全量清单
	// (向后兼容:未同步过的厂商、或 anthropic 面这类无模型列表端点的面)。
	faceModels map[string][]string
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
		faceModels:     make(map[string][]string),
		responsesAPI:   make(map[string]bool),
		endpoints:      make(map[string]string),
	}
}

// LoadFromConfig 从配置加载所有 enabled 的 Provider
func (m *Manager) LoadFromConfig(ctx context.Context, cfg *ManagerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	loaded := 0
	for name, pcfg := range cfg.Providers {
		if !pcfg.Enabled {
			m.logger.Info("provider disabled, skipping", zap.String("provider", name))
			continue
		}

		// P47: 计费面 — 既登记到 m.billingSources,也透传进工厂配置
		// (provider 层按面取 key 要用,见 ProviderConfig.BillingSource)
		bs := pcfg.BillingSource
		if bs == "" {
			bs = "api"
		}

		factoryCfg := ProviderConfig{
			Name:             name,
			Endpoint:         pcfg.Endpoint,
			Protocol:         pcfg.Protocol,
			Timeout:          resolveProviderTimeout(pcfg.Timeout, cfg.DefaultTimeout),
			Pool:             cfg.Pools[name],
			FailureThreshold: pcfg.Circuit.FailureThreshold,
			FailureWindow:    pcfg.Circuit.FailureWindow,
			OpenTimeout:      pcfg.Circuit.OpenTimeout,
			// P-deepseek-thinking: 透传到 provider 工厂(deepseek-anthropic 用)
			ForceThinkingDisabled: pcfg.ForceThinkingDisabled,
			BillingSource:         bs,
		}

		m.billingSources[name] = bs
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
				zap.String("protocol", string(p.Protocol())))
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
// provider 是注册面名,先经 VendorFor 归到厂商(vendor)再查 —— pricing 键是
// vendor:model(LoadModelsFromStore 存的是 vendor)。未找到返回 zero value(cost=0)。
func (m *Manager) CostFor(provider, model string) ModelCost {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.VendorFor(provider)
	if c, ok := m.pricing[pricingKey(v, model)]; ok {
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

// VendorFor P-route-order: 查注册名所属厂商(vendor)。
// 路由 Level 2 排序用 vendor 键(route_order 的 provider 作用域存厂商名),
// 而候选是注册面名(如 mimo-token-plan-anthropic / minimax),需先归到 vendor
// 再查改写,否则改写落在「无人使用的裸面」上导致排序失效(2026-08-10 根因)。
// vendor 未声明时 registry 返回 name 本身(单协议厂商,天然一致)。
func (m *Manager) VendorFor(name string) string {
	return m.registry.VendorFor(name)
}

// ModelsFor 返回某注册面的可用模型 id。
//
// 语义(P-model-face,2026-08-21 起):模型归属的维度是**注册面**,不是 vendor ——
// 中转站厂商(rightapi)每个后缀端点是不同上游,模型互不相通:codex 面只有 gpt-*、
// grok 面只有 grok-*、claude 面只有 claude-*。按 vendor 合并会让 codex 面拿到
// claude 模型并发给上游 → 404 model not found(实测),该面永远轮不到干活。
//
// 核心不变式(fallback):**该面无归属行时,退回该 vendor 的全量清单**。
// 这一条同时覆盖两种正当情形,不能省:
//   - 尚未同步过的厂商(迁移后首次启动)→ 否则候选全空 → 全部 503
//   - anthropic 面这类无模型列表端点的面(deepseek-anthropic)→ 它与 openai 面
//     确实共享同一批模型,vendor 全量正是正确答案
//
// 未同步/无数据 → 空切片。排序确定(按 model 名字典序,与调用方历史行为一致)。
func (m *Manager) ModelsFor(name string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 该面有归属行 → 只用它自己的(面间隔离)
	if models, ok := m.faceModels[name]; ok && len(models) > 0 {
		out := make([]string, len(models))
		copy(out, models)
		sort.Strings(out)
		return out
	}
	// fallback:退回 vendor 级全量清单
	v := m.VendorFor(name)
	out := make([]string, 0)
	seen := map[string]bool{}
	for k := range m.pricing {
		if strings.HasPrefix(k, v+":") { // pricingKey = vendor:model
			model := strings.TrimPrefix(k, v+":")
			if !seen[model] {
				seen[model] = true
				out = append(out, model)
			}
		}
	}
	sort.Strings(out)
	return out
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

// ReloadPricing 只更新 billingSource + responsesAPI(仍来自 config),不动 Provider 实例。
// 定价(pricing)与默认模型(defaultModels)已改由 DB provider_models 提供 —— 由
// LoadModelsFromStore 负责刷新,此处不再读 pcfg.ModelCosts/pcfg.Models。
// 用于热重载 config.yaml 时同步 billing_source / responses_api 改动。
func (m *Manager) ReloadPricing(cfg *ManagerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.billingSources = make(map[string]string)
	for name, pcfg := range cfg.Providers {
		bs := pcfg.BillingSource
		if bs == "" {
			bs = "api"
		}
		m.billingSources[name] = bs
		// P-responses: 能力标记同频热重载
		m.responsesAPI[name] = pcfg.ResponsesAPI
	}
	m.logger.Info("billing/responses reloaded", zap.Int("providers", len(m.billingSources)))
}

// LoadModelsFromStore 从 DB provider_models + provider_model_faces 读入
// pricing / defaultModels / faceModels。
//
// pricing 键统一为 "<vendor>:<model_id>"(经 VendorFor 归位,不是注册面名)——
// 定价是厂商级属性,同一模型在同厂商各面价格相同,face 不进 pricingKey。
//
// defaultModels 按**面**取:该面在 provider_model_faces 里 sort_order 最小的模型;
// 该面无归属行时落回 vendor 级首行(All 按 (vendor, sort_order) 排)。
// sort_order 是上游 ListModels 的返回下标,上游把旗舰款排最前(minimax=M3、
// mimo=mimo-v2.5、deepseek=v4-flash),与改动前的默认模型一致。
// 不能改回 model_id 字典序:那会把 MiniMax-M3 排到 MiniMax-M2 之后,
// catch_all 模式下主力模型静默降级(2026-08-20 根因)。
// 按面取默认模型是 2026-08-21 修的第二个同类根因:vendor 级首行对中转站
// 意味着 codex/grok 面拿到 claude 模型 → 404,该面 0 条成功记录。
//
// 首次启动(server.New,LoadFromConfig 之后)与热重载(Reload)都走这里。
// DB 为空时只打警告,不 panic —— 计费全 0、候选为空,等首次同步 Task 5 拉取填表。
func (m *Manager) LoadModelsFromStore(ctx context.Context, store ModelStore) error {
	rows, err := store.All(ctx)
	if err != nil {
		return err
	}
	faceRows, err := store.AllFaces(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pricing = make(map[string]ModelCost)
	m.defaultModels = make(map[string]string)
	m.faceModels = make(map[string][]string)
	// vendor → 首个 model_id(排序确定)作 fallback 默认模型
	first := make(map[string]string)
	for _, r := range rows {
		c := ModelCost{
			CostPerMillionInput:     r.CostPerMillionInput,
			CostPerMillionCacheRead: r.CostPerMillionCacheRead,
			CostPerMillionOutput:    r.CostPerMillionOutput,
		}
		m.pricing[pricingKey(r.Vendor, r.ModelID)] = c
		if _, ok := first[r.Vendor]; !ok {
			first[r.Vendor] = r.ModelID
		}
	}
	// 面归属:AllFaces 已按 (vendor, face, sort_order) 排序,append 即保持面内上游顺序
	faceFirst := make(map[string]string)
	for _, fr := range faceRows {
		m.faceModels[fr.Face] = append(m.faceModels[fr.Face], fr.ModelID)
		if _, ok := faceFirst[fr.Face]; !ok {
			faceFirst[fr.Face] = fr.ModelID
		}
	}
	// 默认模型:该面有归属 → 用面内首个;否则落回该 vendor 首行(向后兼容)
	for name := range m.providers {
		if dm, ok := faceFirst[name]; ok {
			m.defaultModels[name] = dm
			continue
		}
		if dm, ok := first[m.VendorFor(name)]; ok {
			m.defaultModels[name] = dm
		}
	}
	if len(rows) == 0 {
		m.logger.Warn("provider_models empty — pricing/defaultModels empty, no candidates until first sync")
		return nil
	}
	m.logger.Info("models loaded from store",
		zap.Int("rows", len(rows)), zap.Int("face_rows", len(faceRows)))
	return nil
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
