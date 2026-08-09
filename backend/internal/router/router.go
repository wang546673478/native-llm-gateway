// Package router 实现按模型名/别名到 Provider + Key 的路由解析
// 对应规格书 5.5 Router
package router

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

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
	// P-catch-all: 兜底路由规则 — 客户端发任何 alias 表外且无 provider 声明的
	// model 名(如 gpt-5 / 任意新探测名)时按此规则路由。nil = 不兜底。
	// 任意 agent 任意模型名都能用,仍按 tier 计费(token_plan → api → free)
	CatchAll *AliasConfig
}

// RouteResult 路由结果:把一个请求锁定到具体的 Provider + Model + Key
// P64: Tier 标注该结果来自哪个 billing_source 桶(token_plan / api / free),
// 层切换判定层(Task 5)据此做计费面切换
type RouteResult struct {
	ProviderName string
	ModelID      string
	Key          *keypool.Key
	Endpoint     string
	Protocol     provider.Protocol
	Tier         string
}

// RouteOption P34: 给 Route() 传可选参数(ProviderKeyIDs 限定)
type RouteOption func(*routeOpts)

type routeOpts struct {
	ProviderKeyIDs []uint
	// P-catch-all: gateway key 白名单 — catch_all 自动模式用它选择候选模型
	AllowedModels []string
}

// WithProviderKeyIDs 让路由从指定 ProviderKeyIDs 子集里挑凭证
func WithProviderKeyIDs(ids []uint) RouteOption {
	return func(o *routeOpts) {
		o.ProviderKeyIDs = ids
	}
}

// WithAllowedModels P-catch-all: 把 gateway key 的白名单传给路由。
// catch_all 自动模式据此选择候选模型:provider 声明过白名单里的模型就用
// 白名单模型,否则该 provider 不参与。空/nil = 不参与选择(用默认模型)
func WithAllowedModels(models []string) RouteOption {
	return func(o *routeOpts) {
		o.AllowedModels = models
	}
}

// Router 持有所有路由决策所需的状态
// 注:Router.manager 用窄接口 provider.ProviderLookup,而非 *provider.Manager 具体类型。
// 之前持具体 Manager 违反 CLAUDE.md「Router 只接收窄接口」;改成只持接口(不持具体)
// 避免旧注释担心的「同时持接口+具体两个 manager 的 Method 冲突」—— Router 现在只
// 有一个字段,类型是接口。通用协调/查询无需 Manager 具体方法。
type Router struct {
	mu       sync.RWMutex
	logger   *zap.Logger
	manager  provider.ProviderLookup
	pools    map[string]*keypool.Pool
	aliases  map[string]AliasConfig
	catchAll *AliasConfig // P-catch-all: 未知 model 名兜底(nil = 不兜底)
	policies map[string]policy.Policy
	cfg      Config
}

// NewRouter 构造 Router
func NewRouter(logger *zap.Logger, manager provider.ProviderLookup, pools map[string]*keypool.Pool, cfg Config) *Router {
	if cfg.DefaultStrategy == "" {
		cfg.DefaultStrategy = "priority"
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	r := &Router{
		logger:   logger,
		manager:  manager,
		pools:    pools,
		aliases:  cfg.Aliases,
		catchAll: cfg.CatchAll,
		cfg:      cfg,
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
		// P-catch-all: 配了 catch_all → 一律走兜底链,客户端模型名只是标签,
		// 不参与路由决策 — 真实模型名也不直连声明它的 provider。
		// 路由只按「请求路径选协议面 + tier 计费(token_plan → api → free)」,
		// 链上能用哪些模型由 gateway key 白名单细化。空规则 = 自动模式
		if r.catchAll != nil {
			r.logger.Debug("catch_all chain (client model name ignored)",
				zap.String("model", req.Model))
			if len(r.catchAll.Providers) == 0 && r.catchAll.TargetModel == "" {
				return r.routeCatchAllAuto(ctx, req.Model, req, o)
			}
			return r.routeAliasRule(ctx, *r.catchAll, req.Model, req, o)
		}
		// 无 catch_all(旧行为):自动发现真实 model 名
		return r.routeDirectModelWithOpts(ctx, req.Model, req, o)
	}
	return r.routeAliasRule(ctx, rule, req.Model, req, o)
}

// routeCatchAllAuto P-catch-all 自动模式(catch_all: {}):
// 所有 enabled provider 都参与,按请求路径协议过滤 + 健康过滤,
// 每个 provider 用它的默认模型(显式 default_model 或第一个声明)承接请求。
// P-whitelist-select: 带 key 白名单时,白名单同时参与候选模型选择 —
// provider 声明过白名单里的模型就用白名单模型(按白名单顺序),
// 声明里没有白名单模型的 provider 不参与。这样 key 允许的模型就是
// 链上实际服务的模型,而不是「白名单只排除、路由目标对不上」。
// tier 计费自动(token_plan → api → free);没 key / 全不可用的 provider 由
// AcquireFromTier 自然跳过。加新 provider + key 即自动进链 — 无路由表可维护
func (r *Router) routeCatchAllAuto(ctx context.Context, aliasName string, req *provider.Request, o *routeOpts) (*RouteIterator, error) {
	reqProto := detectProtocol(req.Path)
	isResponses := reqProto == provider.ProtocolOpenAI && strings.HasSuffix(strings.ToLower(req.Path), "/responses")
	var routes []ProviderRoute
	for name, p := range r.manager.GetAll() {
		if reqProto != "" && p.Protocol() != reqProto {
			continue
		}
		// P-per-key-circuit: 熔断器已下沉到 keypool(per-key),provider 级
		// healthStatus 已移除 — 单把 key 5xx 不再连坐整 provider 的 healthy key
		// P-responses: /responses 透传只走原生支持 Responses API 的 provider
		// (DeepSeek / MiniMax;Qwen / Gemini 不支持,硬发会 400/404 且
		// 404 归类 model_not_found 非 retryable,会中断 failover)
		if isResponses && !r.manager.SupportsResponsesAPI(name) {
			continue
		}
		model := r.manager.DefaultModelFor(name)
		// P-whitelist-select: 白名单参与选择(非空且非通配)
		if len(o.AllowedModels) > 0 && !sliceContains(o.AllowedModels, "*") {
			picked := pickAllowedModel(p.Models(), o.AllowedModels)
			if picked == "" {
				continue // 该 provider 声明的模型都不在白名单 → 不参与
			}
			model = picked
		}
		if model == "" {
			continue
		}
		routes = append(routes, ProviderRoute{
			Name: name, Model: model,
			BillingSource: r.manager.BillingSourceFor(name), // P-mixed-tier-pool
		})
	}
	if len(routes) == 0 {
		return nil, ErrNoRoute
	}
	// P-catch-all-order / Level 2(2026-08-10):manager.GetAll() 是 Go map,迭代顺序随机 —
	// 不排序则每次请求候选顺序都不同(2026-08-07 实测:相邻两条相同请求一条先打 mimo、
	// 一条先打 minimax)。候选名单天然化后,层内 provider 顺序默认 = 该 provider 最早加入
	// key 的时间(先来的优先);无 pool/无 key 的 provider 按 name 兜底,保证确定性。
	pools := r.poolsSnapshot() // F4: RLock 快照,与 SetPools 写同步
	earliest := func(name string) time.Time {
		if pool, ok := pools[name]; ok && pool != nil {
			return pool.EarliestKeyTime()
		}
		return time.Time{}
	}
	sort.Slice(routes, func(i, j int) bool {
		ti, tj := earliest(routes[i].Name), earliest(routes[j].Name)
		if !ti.Equal(tj) {
			// 有最早时间者优先;零值(无 key)排最后
			if ti.IsZero() {
				return false
			}
			if tj.IsZero() {
				return true
			}
			return ti.Before(tj)
		}
		return routes[i].Name < routes[j].Name // 同时间按 name 兜底确定性
	})
	keyCandidates := buildKeyCandidates(routes, pools)
	return &RouteIterator{
		alias:          aliasName,
		candidates:     keyCandidates,
		pools:          pools,
		manager:        r.manager,
		providerKeyIDs: o.ProviderKeyIDs,
	}, nil
}
// sliceContains 简单 contains helper(避免引入 slices 依赖的版本问题)
func sliceContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// pickAllowedModel 在 provider 声明的模型里按白名单顺序挑第一个命中的。
// 单一职责:routeCatchAllAuto 与 filterCandidates 两处都实现了同一段
// "按白名单拍序的候选选中" 循环(filter 语义:白名单里第一个 provider 声明过的
// 模型胜出)。抽出单源,消除复制粘贴型耦合 —— 改选择规则(如"精确匹配优先")
// 只需改这一处。
func pickAllowedModel(models, allowed []string) string {
	for _, am := range allowed {
		if sliceContains(models, am) {
			return am
		}
	}
	return ""
}

// routeAliasRule 走一条 alias 规则(长格式显式 providers 或短格式 TargetModel)。
// catch_all 复用同一路径 — 它的规则结构和 alias 完全一样。
// aliasName 用于迭代器的归属标注(客户端请求的 model 名)
func (r *Router) routeAliasRule(ctx context.Context, rule AliasConfig, aliasName string, req *provider.Request, o *routeOpts) (*RouteIterator, error) {
	// P53: alias 注册了但没有显式 providers(chain_ref 解析后为空也算)— 自动发现
	if len(rule.Providers) == 0 {
		r.logger.Debug("alias has no explicit providers, auto-discover",
			zap.String("alias", aliasName))
		// 短格式 TargetModel 优先,否则用 alias 名字本身作为 target model id
		target := rule.TargetModel
		if target == "" {
			target = aliasName
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
			zap.String("alias", aliasName),
			zap.String("strategy", strategy))
		pol = r.policies["priority"]
	}

	candidates := r.filterCandidates(ctx, rule.Providers, req, o)
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}

	// P64: 先按 policy(priority/weight 等)对 provider 排序,再按 tier 跨 provider 拉平
	ordered, err := pol.Order(candidates)
	if err != nil {
		return nil, err
	}
	keyCandidates := buildKeyCandidates(ordered, r.poolsSnapshot())

	return &RouteIterator{
		alias:          aliasName,
		candidates:     keyCandidates,
		pools:          r.poolsSnapshot(),
		manager:        r.manager,
		providerKeyIDs: o.ProviderKeyIDs,
	}, nil
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
			candidates = append(candidates, ProviderRoute{
				Name: name, Model: modelID,
				BillingSource: r.manager.BillingSourceFor(name), // P-mixed-tier-pool
			})
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoRoute
	}

	// P64: auto-discovery 路径也按 tier 跨 provider 拉平
	keyCandidates := buildKeyCandidates(candidates, r.poolsSnapshot())

	return &RouteIterator{
		alias:          modelID,
		candidates:     keyCandidates,
		pools:          r.poolsSnapshot(),
		manager:        r.manager,
		providerKeyIDs: o.ProviderKeyIDs,
	}, nil
}

// filterCandidates 协议匹配 + 健康 + 已注册 + 白名单模型选择
// P-whitelist-select (Bug fix 2026-08-09): 显式 alias/catch_all 也遵循白名单语义 —
// 白名单非空时,优先从 provider 声明的模型里挑白名单命中的,覆盖显式 model 字段。
// 之前只有自动模式(routeCatchAllAuto)应用白名单,显式列表用 config 写死的 model
// (如 mimo-v2.5),导致 key 白名单配 mimo-v2.5-pro 时 result.ModelID=mimo-v2.5
// → 白名单校验 403 "does not allow model mimo-v2.5"。现统一两种模式。
func (r *Router) filterCandidates(ctx context.Context, providers []ProviderRoute, req *provider.Request, o *routeOpts) []ProviderRoute {
	reqProto := detectProtocol(req.Path)
	out := make([]ProviderRoute, 0, len(providers))
	// 白名单参与选择(与 routeCatchAllAuto 同逻辑)
	whitelistSelect := len(o.AllowedModels) > 0 && !sliceContains(o.AllowedModels, "*")
	for _, p := range providers {
		pv, ok := r.manager.Get(p.Name)
		if !ok {
			continue
		}
		if reqProto != "" && pv.Protocol() != reqProto {
			continue
		}
		// P-per-key-circuit: provider 级健康过滤已移除 — 熔断器在 keypool(per-key)
		// P-mixed-tier-pool: 补块级计费来源(显式 alias providers 列表里没写这个字段)
		p.BillingSource = r.manager.BillingSourceFor(p.Name)
		// P-whitelist-select: 白名单里有这个 provider 声明的模型 → 用白名单模型
		// 覆盖显式 model 字段(用户配白名单 = 声明能用的模型,应优先)。
		// 注意:不 continue 删除不匹配的 provider — 候选保留,由 proxy 的 tryCandidate
		// 白名单校验逐个跳过,全部跳过时 handleAllFailed 返 403 model_not_allowed
		// (删候选会让 proxy 无候选 → 503 no_route,语义错误)
		if whitelistSelect {
			if picked := pickAllowedModel(pv.Models(), o.AllowedModels); picked != "" {
				p.Model = picked
			}
		}
		// P-model-default: 显式 alias 列表里没填 model 时,fallback 到 provider 的 default_model
		// 这样用户配路由可以省略 model 字段,让 default_model 决定
		// (单一职责:model 字段只在用户主动精确控制时填,避免冗余)
		if p.Model == "" {
			p.Model = r.manager.DefaultModelFor(p.Name)
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
	case strings.Contains(p, "/responses"): // P-responses: OpenAI Responses API(Codex)
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
	manager        provider.ProviderLookup
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
				// P-provider-vendor: 按请求协议过滤 key(Protocols 为空 = 不过滤)
				k, err = pool.AcquireFromTier(c.Tier, idSet, string(pv.Protocol()))
			} else {
				k, err = pool.AcquireFromTier(c.Tier, nil, string(pv.Protocol()))
			}
			if err != nil {
				// 已知缝隙:整层熔断 OPEN 时所有候选都在此静默跳过、直落 api 层 —
				// 这是分层降档不变式的 30s 窗口缝隙,勿当作 bug 改出可用性问题
				continue
			}
			return &RouteResult{
				ProviderName: c.Name,
				ModelID:      c.Model,
				Key:          k,
				Protocol:     pv.Protocol(),
				Tier:         c.Tier,
			}, nil
		}

		// 没有 pool(测试场景)— 仍返回 RouteResult,Key=nil
		return &RouteResult{
			ProviderName: c.Name,
			ModelID:      c.Model,
			Protocol:     pv.Protocol(),
			Tier:         c.Tier,
		}, nil
	}
	return nil, ErrNoRoute
}

// buildKeyCandidates P64: 跨 provider 拉平,先 token_plan 全部 → 再 api → 再 free
// 同 tier 内 stable 保留输入顺序(由 policy.Order 排出的 provider 顺序)
// 每个 provider 按 pool.Tiers() 展开成它声明的所有 tier
//   - provider 没有 pool → 兜底按 "api" 产一个 KeyCandidate
//   - provider 没有声明任何 key → pool.Tiers() 返回 [],同样兜底 "api"
//     (调用方 AcquireFromTier 实际拿不到 key 时会自动 continue)
func buildKeyCandidates(routes []ProviderRoute, pools map[string]*keypool.Pool) []KeyCandidate {
	tierOrder := keypool.TierOrder
	buckets := make(map[string][]KeyCandidate, 3)
	for _, t := range tierOrder {
		buckets[t] = nil
	}

	for _, r := range routes {
		// P-mixed-tier-pool: 候选 tier = 池 tiers ∩ 块声明 billing_source。
		// 同一 vendor 共享 pool 混层时(mimo sk-/tp- 两套 key 一个池),pool.Tiers()
		// 是并集 [token_plan, api],直接用会把 api 块的候选错放进 token_plan 桶、
		// tp- key 发到 api 端点(401,2026-08-07 实测)。交集为空(池里没有该块
		// 声明的层 — 块未声明 billing 默认 api 但池全 token_plan)→ 回退池并集,
		// 保持旧行为。空 route(没填 BillingSource)→ 也走池并集
		var tiers []string
		if pool, ok := pools[r.Name]; ok && pool != nil {
			tiers = pool.Tiers()
		}
		if len(tiers) == 0 {
			tiers = []string{"api"} // 兜底
		}
		if r.BillingSource != "" {
			inter := make([]string, 0, len(tiers))
			for _, t := range tiers {
				if t == r.BillingSource {
					inter = append(inter, t)
				}
			}
			if len(inter) > 0 {
				tiers = inter
			}
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

// ResolveAlias 把请求中的 model 名解析成最终要路由的真实 model 名
//
// 三种返回情形:
//
//	A. 不是 alias(用户在白名单里直接写了一个真实 model 名):
//	   → ok=false,proxy 用原 model 名继续(走 router.Route 时的 direct-model 路径)
//
//	B. 是 alias,且有 TargetModel(短格式,auto-discovery):
//	   → ok=true, 返回 TargetModel(真实 model)
//	   proxy 把 model 重写成 TargetModel,然后:
//	   - CheckAllowed 用 TargetModel 走白名单
//	   - body 里 model 字段也改成 TargetModel(让上游收到正确 model)
//
//	C. 是 alias,且只有 providers(长格式,显式路由):
//	   → ok=true, 返回 alias 名 本身 + target_model="" 标记
//	   proxy 不能简单重写 model(因为路由会按 provider 列表 failover),
//	   所以白名单检查应该跳过 — 用 alias 名作为 "白名单检查的凭据":
//	   如果 alias 名在白名单里(用户显式列了 claude-sonnet-4-5 等),通过;
//	   如果 alias 名不在白名单,CheckAllowed 仍走 — 因为 alias 名 == 用户
//	   明确想要的目标,跟直接发真实 model 名语义一致。
//
// 关键收益:Claude Code 发 claude-sonnet-4-5 → 命中 alias →
//   - alias 在白名单 → 放行(用户显式允许)
//   - alias 不在白名单 → 403(用户没有显式允许这个 alias)
//
// 这避免了用户被迫列出 claude-3-5-sonnet-* / claude-sonnet-4-5 等所有探测名。
func (r *Router) ResolveAlias(model string) (target string, isAlias bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, exists := r.aliases[model]
	if !exists {
		return "", false
	}
	if rule.TargetModel != "" {
		return rule.TargetModel, true
	}
	// 长格式 alias(只有 providers):alias 名本身就是用户意图的目标
	return model, true
}

// ReloadAliases 原子替换别名表(P14 热重载)
// 注意:这不会改变 underlying Manager / Pools,只更新路由规则
func (r *Router) ReloadAliases(aliases map[string]AliasConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases = aliases
	r.logger.Info("router aliases reloaded", zap.Int("count", len(aliases)))
}

// ReloadStrategy P14: 热重载 default_strategy / max_attempts。
// 此前这俩只在 NewRouter 构造时固化,Server.Reload 只刷 aliases —— 热改配置里
// 的 routing.default_strategy / retry.max_attempts 会静默保留旧值(路由/调度行为
// 半新半旧)。这里补上,让热重载对路由策略也生效。
func (r *Router) ReloadStrategy(defaultStrategy string, maxAttempts int) {
	if defaultStrategy == "" {
		defaultStrategy = "priority"
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.DefaultStrategy = defaultStrategy
	r.cfg.MaxAttempts = maxAttempts
	// policies 固定集合(priority/weight/cost/health),strategy 名只需落在已知集,
	// 未知名已在 Route() 里 fallback priority,无需重建
	r.logger.Info("router strategy reloaded",
		zap.String("default_strategy", defaultStrategy), zap.Int("max_attempts", maxAttempts))
}

// ReloadCatchAll P-catch-all: 原子替换兜底路由规则(与 ReloadAliases 同频)
func (r *Router) ReloadCatchAll(c *AliasConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.catchAll = c
	r.logger.Info("router catch_all reloaded", zap.Bool("enabled", c != nil))
}

// CatchAllConfig 返回当前 catch_all 规则的拷贝(供 /routing 端点展示)
// Providers 保证非 nil(空规则也返回 [] 而非 null,前端可直接 .length)
func (r *Router) CatchAllConfig() *AliasConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catchAll == nil {
		return nil
	}
	c := *r.catchAll
	c.Providers = append([]ProviderRoute{}, r.catchAll.Providers...)
	return &c
}

// Manager 返回窄接口 provider.ProviderLookup(Proxy 用其查 Provider/Cost)
func (r *Router) Manager() provider.ProviderLookup { return r.manager }

// Pool 返回指定 Provider 的 KeyPool(Proxy 用来 ReportSuccess/ReportRateLimit)
// F4 竞态修复:读 r.pools 字段取 RLock(RWMutex),与 SetPool/SetPools 写同步。
func (r *Router) Pool(providerName string) *keypool.Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pools[providerName]
}

// poolsSnapshot 返回当前 pool map 的引用(RLock 读),供 routing 路径一次性快照。
func (r *Router) poolsSnapshot() map[string]*keypool.Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pools
}

// SetPool 注入单 provider 的 Pool(仅供启动时/测试;热重载请用 SetPools 整表替换,
// 避免就地写共享 map 造成与 quotacheck 轮询的并发 map 崩溃)
func (r *Router) SetPool(providerName string, pool *keypool.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pools == nil {
		r.pools = make(map[string]*keypool.Pool)
	}
	r.pools[providerName] = pool
}

// SetPools 整表替换 Router 持有的 pool map(引用替换,不就地写)。
// ReloadProviderPool 热重载时用它把整张新 map 指给 Router —— 旧 map 永不就地变,
// 并发读方持旧 map 快照也安全,消除了就地写共享 map 的并发崩溃。
func (r *Router) SetPools(newMap map[string]*keypool.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools = newMap
}
