// Package router 实现按模型名/别名到 Provider + Key 的路由解析
// 对应规格书 5.5 Router
package router

import (
	"context"
	"errors"
	"strings"

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

// Router 持有所有路由决策所需的状态
type Router struct {
	logger       *zap.Logger
	manager      *provider.Manager
	pools        map[string]*keypool.Pool
	aliases      map[string]AliasConfig
	policies     map[string]policy.Policy
	cfg          Config
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
func (r *Router) Route(ctx context.Context, req *provider.Request) (*RouteIterator, error) {
	rule, ok := r.aliases[req.Model]
	if !ok {
		return r.routeDirectModel(ctx, req.Model, req)
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

	ordered, err := pol.Order(candidates)
	if err != nil {
		return nil, err
	}

	return &RouteIterator{
		alias:      req.Model,
		candidates: ordered,
		pools:      r.pools,
		manager:    r.manager,
	}, nil
}

// routeDirectModel 没有别名规则时,按真实 model id 直接查找 Provider
func (r *Router) routeDirectModel(ctx context.Context, modelID string, req *provider.Request) (*RouteIterator, error) {
	candidates := make([]ProviderRoute, 0)
	for name, p := range r.manager.GetAll() {
		for _, m := range p.Models() {
			if m == modelID {
				candidates = append(candidates, ProviderRoute{Name: name, Model: modelID})
			}
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}
	return &RouteIterator{
		alias:      modelID,
		candidates: candidates,
		pools:      r.pools,
		manager:    r.manager,
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

// RouteIterator 持有排序好的候选,Next() 取下一个可用
type RouteIterator struct {
	alias      string
	candidates []ProviderRoute
	pools      map[string]*keypool.Pool
	manager    *provider.Manager
	current    int
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
			kk, err := pool.Acquire()
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
