// Package router 实现按模型名/别名到 Provider + Key 的路由解析
// 对应规格书 5.5 Router
package router

import (
	"context"
	"errors"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/policy"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// 为兼容 spec 命名,把 policy 包里的类型在 router 重新导出
type (
	ProviderRoute = policy.ProviderRoute
	AliasConfig   = policy.AliasConfig
)

// Config 路由层关心的配置
type Config struct {
	Aliases         map[string]AliasConfig
	DefaultStrategy string
	MaxAttempts     int
}

// RouteResult 路由结果:把一个请求锁定到具体的 Provider + Model + Key
type RouteResult struct {
	ProviderName string
	ModelID      string
	Key          *keypool.Key
	Endpoint     string
	Protocol     provider.Protocol
}

// RouteOption P34: 给 Route() 传可选参数(ProviderKeyIDs 限定)
type RouteOption func(*routeOpts)

type routeOpts struct {
	ProviderKeyIDs []uint
}

// WithProviderKeyIDs 让路由从指定 ProviderKeyIDs 子集里挑凭证
func WithProviderKeyIDs(ids []uint) RouteOption {
	return func(o *routeOpts) {
		o.ProviderKeyIDs = ids
	}
}

// Router 持有所有路由决策所需的状态
type Router struct {
	mu          sync.RWMutex
	logger      *zap.Logger
	manager     *provider.Manager
	pools       map[string]*keypool.Pool
	aliases     map[string]AliasConfig
	policies    map[string]policy.Policy
	cfg         Config
	healthStatus map[string]bool // P6 接 Circuit Breaker
}

// NewRouter 构造 Router
func NewRouter(logger *zap.Logger, manager *provider.Manager, pools map[string]*keypool.Pool, cfg Config) *Router {
	if cfg.DefaultStrategy == "" {
		cfg.DefaultStrategy = "priority"
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	r := &Router{
		logger:       logger,
		manager:      manager,
		pools:        pools,
		aliases:      cfg.Aliases,
		cfg:          cfg,
		healthStatus: make(map[string]bool),
	}
	r.policies = map[string]policy.Policy{
		"priority": policy.NewPriorityPolicy(),
		"weight":   policy.NewWeightPolicy(),
		"cost":     policy.NewCostPolicy(),
		"health":   policy.NewHealthPolicy(),
		"":         policy.NewPriorityPolicy(),
	}
	return r
}

// SetHealthStatus 由 Circuit Breaker 调用
func (r *Router) SetHealthStatus(providerName string, open bool) {
	r.healthStatus[providerName] = open
}

// ErrNoRoute 没有匹配路由
var ErrNoRoute = errors.New("router: no route matches the request")

// Route 把请求解析成一个 RouteIterator(支持 failover)
func (r *Router) Route(ctx context.Context, req *provider.Request, opts ...RouteOption) (*RouteIterator, error) {
	o := &routeOpts{}
	for _, opt := range opts {
		opt(o)
	}
	rule, ok := r.aliases[req.Model]
	if !ok {
		// alias 没注册 → 自动发现:从所有 enabled provider 中找声明该 model 的
		return r.routeDirectModelWithOpts(ctx, req.Model, req, o)
	}

	// P53: alias 注册了但没有显式 providers(chain_ref 解析后为空也算)— 自动发现
	if len(rule.Providers) == 0 {
		r.logger.Debug("alias has no explicit providers, auto-discover",
			zap.String("alias", req.Model))
		// 短格式 TargetModel 优先,否则用 alias 名字本身作为 target model id
		target := rule.TargetModel
		if target == "" {
			target = req.Model
		}
		return r.routeDirectModelWithOpts(ctx, target, req, o)
	}

	strategy := rule.Strategy
	if strategy == "" {
		strategy = r.cfg.DefaultStrategy
	}
	pol, ok := r.policies[strategy]
	if !ok {
		r.logger.Warn("unknown routing strategy, fallback to priority",
			zap.String("alias", req.Model),
			zap.String("strategy", strategy))
		pol = r.policies["priority"]
	}

	candidates := r.filterCandidates(ctx, rule.Providers, req)
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}

	// P64: 先按 policy(priority/weight 等)对 provider 排序,再按 tier 跨 provider 拉平
	ordered, err := pol.Order(candidates)
	if err != nil {
		return nil, err
	}
	keyCandidates := buildKeyCandidates(ordered, r.pools)

	return &RouteIterator{
		alias:           req.Model,
		candidates:      keyCandidates,
		pools:           r.pools,
		manager:         r.manager,
		providerKeyIDs:  o.ProviderKeyIDs,
	}, nil
}

// routeDirectModel 没有别名规则时,按真实 model id 直接查找 Provider
func (r *Router) routeDirectModel(ctx context.Context, modelID string, req *provider.Request) (*RouteIterator, error) {
	return r.routeDirectModelWithOpts(ctx, modelID, req, &routeOpts{})
}

func (r *Router) routeDirectModelWithOpts(ctx context.Context, modelID string, req *provider.Request, o *routeOpts) (*RouteIterator, error) {
	// P36: 当一个 model 在多个 provider 都有声明时(例如 minimax 和 minimax-openai 都声明 MiniMax-M3)
	// 根据客户端请求的 URL 路径推断协议,优先选协议匹配的 provider:
	//   - /v1/chat/completions → OpenAI provider (例如 minimax-openai)
	//   - /v1/messages          → Anthropic provider (例如 minimax)
	//   - generatecontent 路径  → Google provider
	// 这样用户用 OpenAI 客户端发 /v1/chat/completions 时会自动走 OpenAI 兼容端点
	reqProto := detectProtocol(req.Path)
	candidates := make([]ProviderRoute, 0)
	for name, p := range r.manager.GetAll() {
		for _, m := range p.Models() {
			if m != modelID {
				continue
			}
			// 如果请求有明确协议,过滤掉不匹配的
			if reqProto != "" && p.Protocol() != reqProto {
				continue
			}
			candidates = append(candidates, ProviderRoute{Name: name, Model: modelID})
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}

	// P64: auto-discovery 路径也按 tier 跨 provider 拉平
	keyCandidates := buildKeyCandidates(candidates, r.pools)

	return &RouteIterator{
		alias:          modelID,
		candidates:     keyCandidates,
		pools:          r.pools,
		manager:        r.manager,
		providerKeyIDs: o.ProviderKeyIDs,
	}, nil
}

// filterCandidates 协议匹配 + 健康 + 已注册
func (r *Router) filterCandidates(ctx context.Context, providers []ProviderRoute, req *provider.Request) []ProviderRoute {
	reqProto := detectProtocol(req.Path)
	out := make([]ProviderRoute, 0, len(providers))
	for _, p := range providers {
		pv, ok := r.manager.Get(p.Name)
		if !ok {
			continue
		}
		if reqProto != "" && pv.Protocol() != reqProto {
			continue
		}
		if r.healthStatus[p.Name] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// detectProtocol 从 URL 路径推断客户端协议
func detectProtocol(path string) provider.Protocol {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/v1/messages"):
		return provider.ProtocolAnthropic
	case strings.Contains(p, "/chat/completions"):
		return provider.ProtocolOpenAI
	case strings.Contains(p, ":generatecontent") || strings.Contains(p, "/v1beta/models"):
		return provider.ProtocolGoogle
	default:
		return ""
	}
}

// KeyCandidate P64: 把候选从 provider 维度展开到 (provider, tier) 维度
// 嵌入 ProviderRoute 保留 Name/Model/Priority/Weight 字段;Tier 标注该候选
// 来自哪个 billing_source 桶(token_plan / api / free)
type KeyCandidate struct {
	ProviderRoute
	Tier string
}

// RouteIterator 持有排序好的候选,Next() 取下一个可用
// P64: candidates 类型从 []ProviderRoute 改为 []KeyCandidate,
// 跨 provider 拉平为 token_plan → api → free 三层
type RouteIterator struct {
	alias          string
	candidates     []KeyCandidate
	pools          map[string]*keypool.Pool
	manager        *provider.Manager
	providerKeyIDs []uint // P34: 限定的 ProviderKey ID 子集(空 = 不限)
	current        int
}

// Next 返回下一个可用的 RouteResult
// P64: 每个候选指定 tier,Next() 调用 AcquireFromTier(不做 provider 内降级)
// 失败 → 推进到下一 KeyCandidate(可能是同 tier 下一个 provider,或下一 tier)
func (it *RouteIterator) Next() (*RouteResult, error) {
	for it.current < len(it.candidates) {
		c := it.candidates[it.current]
		it.current++

		pv, ok := it.manager.Get(c.Name)
		if !ok {
			continue
		}

		if pool, ok := it.pools[c.Name]; ok && pool != nil {
			var (
				k   *keypool.Key
				err error
			)
			if len(it.providerKeyIDs) > 0 {
				// P34 + P64: 限定 ProviderKey ID 子集,同时指定 tier
				idSet := make(map[uint]struct{}, len(it.providerKeyIDs))
				for _, id := range it.providerKeyIDs {
					idSet[id] = struct{}{}
				}
				k, err = pool.AcquireFromTier(c.Tier, idSet)
			} else {
				k, err = pool.AcquireFromTier(c.Tier, nil)
			}
			if err != nil {
				continue
			}
			return &RouteResult{
				ProviderName: c.Name,
				ModelID:      c.Model,
				Key:          k,
				Protocol:     pv.Protocol(),
			}, nil
		}

		// 没有 pool(测试场景)— 仍返回 RouteResult,Key=nil
		return &RouteResult{
			ProviderName: c.Name,
			ModelID:      c.Model,
			Protocol:     pv.Protocol(),
		}, nil
	}
	return nil, ErrNoRoute
}

// buildKeyCandidates P64: 跨 provider 拉平,先 token_plan 全部 → 再 api → 再 free
// 同 tier 内 stable 保留输入顺序(由 policy.Order 排出的 provider 顺序)
// 每个 provider 按 pool.Tiers() 展开成它声明的所有 tier
//   - provider 没有 pool → 兜底按 "api" 产一个 KeyCandidate
//   - provider 没有声明任何 key → pool.Tiers() 返回 [],同样兜底 "api"
//   (调用方 AcquireFromTier 实际拿不到 key 时会自动 continue)
func buildKeyCandidates(routes []ProviderRoute, pools map[string]*keypool.Pool) []KeyCandidate {
	tierOrder := []string{"token_plan", "api", "free"}
	buckets := make(map[string][]KeyCandidate, 3)
	for _, t := range tierOrder {
		buckets[t] = nil
	}

	for _, r := range routes {
		var tiers []string
		if pool, ok := pools[r.Name]; ok && pool != nil {
			tiers = pool.Tiers()
		}
		if len(tiers) == 0 {
			tiers = []string{"api"} // 兜底
		}
		for _, t := range tiers {
			buckets[t] = append(buckets[t], KeyCandidate{ProviderRoute: r, Tier: t})
		}
	}

	out := make([]KeyCandidate, 0, len(routes)*3)
	for _, t := range tierOrder {
		out = append(out, buckets[t]...)
	}
	return out
}

// Aliases 返回所有已注册的别名
func (r *Router) Aliases() map[string]AliasConfig {
	out := make(map[string]AliasConfig, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// ReloadAliases 原子替换别名表(P14 热重载)
// 注意:这不会改变 underlying Manager / Pools,只更新路由规则
func (r *Router) ReloadAliases(aliases map[string]AliasConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases = aliases
	r.logger.Info("router aliases reloaded", zap.Int("count", len(aliases)))
}

// Manager 返回底层 provider.Manager(Proxy 用来查 Provider 实例)
func (r *Router) Manager() *provider.Manager { return r.manager }

// Pool 返回指定 Provider 的 KeyPool(Proxy 用来 ReportSuccess/ReportRateLimit)
func (r *Router) Pool(providerName string) *keypool.Pool { return r.pools[providerName] }

// SetPool 注入 Pool(由 main.go 在启动时调用,把 cfg 里声明的 KeyPool 绑到 Router)
func (r *Router) SetPool(providerName string, pool *keypool.Pool) {
	if r.pools == nil {
		r.pools = make(map[string]*keypool.Pool)
	}
	r.pools[providerName] = pool
}

// SetProviderHealth 实现 circuit.ProviderHealth 接口
// 由 Circuit Breaker 调用,告诉 Router 某个 Provider 是否 OPEN
func (r *Router) SetProviderHealth(providerName string, open bool) {
	r.healthStatus[providerName] = open
}
