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
		// routeDirectModel 也需要传 opts(支持 ProviderKeyIDs)
		return r.routeDirectModelWithOpts(ctx, req.Model, req, o)
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

	// P52: 全局 tier 排序 — 先穷尽所有 token_plan,再 api,最后 free
	// (同 tier 内保留 chain priority 顺序)
	candidates = sortCandidatesByTier(candidates, r.pools)

	ordered, err := pol.Order(candidates)
	if err != nil {
		return nil, err
	}

	return &RouteIterator{
		alias:           req.Model,
		candidates:      ordered,
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

	// P52: 全局 tier 排序 — 同上,先 token_plan 再 api 再 free
	candidates = sortCandidatesByTier(candidates, r.pools)

	return &RouteIterator{
		alias:          modelID,
		candidates:     candidates,
		pools:          r.pools,
		manager:        r.manager,
		providerKeyIDs: o.ProviderKeyIDs,
	}, nil
}

// sortCandidatesByTier P52: 按 best tier 排序(token_plan > api > free)
// 同 tier 内保留原顺序(由 policy.Order 处理 chain priority)
// 注意:这是 stable sort,所以同 tier 内的相对顺序不变
func sortCandidatesByTier(in []ProviderRoute, pools map[string]*keypool.Pool) []ProviderRoute {
	tierRank := map[string]int{"token_plan": 0, "api": 1, "free": 2}
	items := make([]struct {
		cand ProviderRoute
		rank int
	}, len(in))
	for i, c := range in {
		rank := 3 // unknown tier 排最后
		if pool, ok := pools[c.Name]; ok && pool != nil {
			best := pool.BestTier()
			if best != "" {
				if r, ok := tierRank[best]; ok {
					rank = r
				}
			}
		}
		items[i] = struct {
			cand ProviderRoute
			rank int
		}{cand: c, rank: rank}
	}
	// stable sort by rank
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].rank < items[j-1].rank; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	out := make([]ProviderRoute, len(items))
	for i, it := range items {
		out[i] = it.cand
	}
	return out
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

// RouteIterator 持有排序好的候选,Next() 取下一个可用
type RouteIterator struct {
	alias          string
	candidates     []ProviderRoute
	pools          map[string]*keypool.Pool
	manager        *provider.Manager
	providerKeyIDs []uint // P34: 限定的 ProviderKey ID 子集(空 = 不限)
	current        int
}

// Next 返回下一个可用的 RouteResult
func (it *RouteIterator) Next() (*RouteResult, error) {
	for it.current < len(it.candidates) {
		c := it.candidates[it.current]
		it.current++

		pv, ok := it.manager.Get(c.Name)
		if !ok {
			continue
		}

		var k *keypool.Key
		if pool, ok := it.pools[c.Name]; ok && pool != nil {
			var (
				kk  *keypool.Key
				err error
			)
			if len(it.providerKeyIDs) > 0 {
				// P34: 限定 ProviderKey ID 子集
				kk, err = pool.AcquireFromIDs(it.providerKeyIDs)
			} else {
				kk, err = pool.Acquire()
			}
			if err != nil {
				continue
			}
			k = kk
		}

		return &RouteResult{
			ProviderName: c.Name,
			ModelID:      c.Model,
			Key:          k,
			Protocol:     pv.Protocol(),
		}, nil
	}
	return nil, ErrNoRoute
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
