// Package handler — 管理 API handlers
package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/circuit"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
	"github.com/wang546673478/native-llm-gateway/internal/usage"
)

// type alias 避免和 router.Router 同名
type Protocol = provider.Protocol

const (
	ProtocolOpenAI    = provider.ProtocolOpenAI
	ProtocolAnthropic = provider.ProtocolAnthropic
	ProtocolGoogle    = provider.ProtocolGoogle
)

// Admin 持有管理 API 所需的依赖
type Admin struct {
	Manager     *provider.Manager
	Registry    *provider.Registry
	Pools       map[string]*keypool.Pool
	Router      *router.Router
	Breakers    *circuit.Manager
	Usage       *usage.Repository
	Aliases     map[string]router.AliasConfig
	Keys        []GatewayKeyInfo
	AccessLog   *accesslog.Recorder // P67: 接入日志 Recorder(可能为 no-op)
}

// NewAdmin 构造 Admin(caller 端负责注入依赖)。
//
// 显式构造器的好处:字段增减时只在 signature 上反映,server.go 里 struct
// literal 用得越多越容易漏字段;有 caller 只在 server.go 一处,影响小。
func NewAdmin(
	mgr *provider.Manager,
	reg *provider.Registry,
	pools map[string]*keypool.Pool,
	r *router.Router,
	cb *circuit.Manager,
	usageRepo *usage.Repository,
	aliases map[string]router.AliasConfig,
	keys []GatewayKeyInfo,
	accessLogR *accesslog.Recorder,
) *Admin {
	return &Admin{
		Manager:   mgr,
		Registry:  reg,
		Pools:     pools,
		Router:    r,
		Breakers:  cb,
		Usage:     usageRepo,
		Aliases:   aliases,
		Keys:      keys,
		AccessLog: accessLogR,
	}
}

// GatewayKeyInfo 用于管理 API 返回的 Gateway Key 信息(不含密钥明文)
type GatewayKeyInfo struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
}

// Register 把所有管理 API 路由注册到 r
// 注意:GET /keys 由 auth.KeysHandler 提供(P16,DB-backed CRUD),
// Admin 不再重复注册
func (a *Admin) Register(r *gin.RouterGroup) {
	r.GET("/providers", a.listProviders)
	r.GET("/providers/registered", a.listRegisteredProviders)
	r.GET("/providers/:name", a.getProvider)
	r.GET("/routing", a.listRouting)
	r.GET("/usage", a.queryUsage)
	r.GET("/usage/aggregate", a.aggregateUsage)
	r.GET("/usage/by_model/:model_id/providers", a.modelProviders) // P65
	r.GET("/dashboard", a.dashboard)
	// P67: 接入日志管理 API(Task 8)
	r.GET("/access-logs", a.listAccessLogs)
	r.GET("/access-logs/stats", a.accessLogStats)
	r.GET("/access-logs/:id/detail", a.getAccessLogDetail)
}

// listRegisteredProviders GET /api/v1/providers/registered
// 返回所有已注册到 Registry 的 Provider(不管 config 里 enabled 与否)
// 用于前端"绑定 Provider"下拉 — 用户应能选任何协议上支持的 Provider
func (a *Admin) listRegisteredProviders(c *gin.Context) {
	if a.Registry == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []string{}})
		return
	}
	names := a.Registry.ListRegistered()
	protocols := a.Registry.ListRegisteredProtocols()
	out := make([]gin.H, 0, len(names))
	loaded := a.Manager.GetAll()
	for _, name := range names {
		// 优先用 Registry 记录的 protocol 元数据(由 init() 时声明),
		// 其次 fallback 到已加载实例的 protocol
		protocol, ok := protocols[name]
		if !ok {
			protocol = ProtocolOpenAI // 老注册方式没记录,默认 openai
		}
		var loadedOK bool
		if p, ok := loaded[name]; ok {
			protocol = Protocol(p.Protocol())
			loadedOK = true
		}
		// P27: 也带上 models,前端可用来做"允许模型"下拉
		models := []string{}
		if p, ok := loaded[name]; ok {
			models = p.Models()
		}
		out = append(out, gin.H{
			"name":     name,
			"protocol": string(protocol),
			"loaded":   loadedOK,
			"models":   models,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"providers": out,
		"count":     len(out),
	})
}

// listProviders GET /api/v1/providers
// 列出所有 Provider + 状态(KeyPool + Circuit Breaker)
func (a *Admin) listProviders(c *gin.Context) {
	out := make([]gin.H, 0)
	for name, p := range a.Manager.GetAll() {
		info := gin.H{
			"name":     name,
			"protocol": string(p.Protocol()),
			"models":   p.Models(),
		}
		if pool, ok := a.Pools[name]; ok {
			info["key_pool"] = pool.Status()
		}
		if a.Breakers != nil {
			info["circuit_breaker"] = a.Breakers.AllStats()
			for _, s := range a.Breakers.AllStats() {
				if s.Name == name {
					info["circuit_breaker"] = s
					break
				}
			}
		}
		out = append(out, info)
	}
	c.JSON(http.StatusOK, gin.H{"providers": out, "count": len(out)})
}

// getProvider GET /api/v1/providers/:name
func (a *Admin) getProvider(c *gin.Context) {
	name := c.Param("name")
	p, ok := a.Manager.Get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
		return
	}
	info := gin.H{
		"name":     name,
		"protocol": string(p.Protocol()),
		"models":   p.Models(),
	}
	if pool, ok := a.Pools[name]; ok {
		info["key_pool"] = pool.Status()
	}
	if a.Breakers != nil {
		for _, s := range a.Breakers.AllStats() {
			if s.Name == name {
				info["circuit_breaker"] = s
				break
			}
		}
	}
	c.JSON(http.StatusOK, info)
}

// listKeys 移除:P16 起由 auth.KeysHandler 提供 DB-backed 的 GET /api/v1/keys

// listRouting GET /api/v1/routing
func (a *Admin) listRouting(c *gin.Context) {
	aliases := a.Aliases
	if aliases == nil {
		aliases = a.Router.Aliases()
	}
	c.JSON(http.StatusOK, gin.H{"aliases": aliases, "count": len(aliases)})
}

// queryUsage GET /api/v1/usage?start=&end=&provider=&model=&gateway_key=&limit=&offset=
func (a *Admin) queryUsage(c *gin.Context) {
	f := usage.QueryFilter{
		ProviderName: c.Query("provider"),
		ModelID:      c.Query("model"),
		GatewayKeyID: c.Query("gateway_key"),
	}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if v := c.Query("offset"); v != "" {
		f.Offset, _ = strconv.Atoi(v)
	}
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.StartTime = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.EndTime = t
		}
	}

	// P66: 先 Count 拿总量,再 Query 拉当前页 — 让前端做分页
	total, err := a.Usage.Count(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count_failed", "detail": err.Error()})
		return
	}
	records, err := a.Usage.Query(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"records": records,
		"total":   total, // P66: 总量(用于分页)
		"limit":   f.Limit,
		"offset":  f.Offset,
	})
}

// aggregateUsage GET /api/v1/usage/aggregate
func (a *Admin) aggregateUsage(c *gin.Context) {
	f := usage.QueryFilter{
		ProviderName: c.Query("provider"),
		GatewayKeyID: c.Query("gateway_key"),
	}
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.StartTime = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.EndTime = t
		}
	}
	rows, err := a.Usage.Aggregate(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "aggregate_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows, "count": len(rows)})
}

// dashboard GET /api/v1/dashboard
func (a *Admin) dashboard(c *gin.Context) {
	// 最近 24h 聚合
	f := usage.QueryFilter{
		StartTime: time.Now().Add(-24 * time.Hour),
		EndTime:   time.Now(),
	}
	rows, _ := a.Usage.Aggregate(c.Request.Context(), f)

	// P65: total 是独立 AggregateResult 类型(只含聚合列,无 provider/model)
	var total usage.AggregateResult
	for _, r := range rows {
		total.TotalRequests += r.TotalRequests
		total.TotalInput += r.TotalInput
		total.TotalOutput += r.TotalOutput
		total.TotalTokens += r.TotalTokens
		total.TotalCost += r.TotalCost
		total.ErrorCount += r.ErrorCount
	}

	// P47: 按 billing_source 聚合 — dashboard 显示 token_plan / api / free 三组
	byBilling, _ := a.Usage.AggregateByBillingSource(c.Request.Context(), f)

	c.JSON(http.StatusOK, gin.H{
		"window":            "24h",
		"total":             total,
		"by_model":          rows, // P65: 重命名 by_provider_model → by_model
		"by_billing_source": byBilling,
		"providers_count":   len(a.Manager.GetAll()),
		"keypools":          poolStatuses(a.Pools),
	})
}

// modelProviders P65: GET /api/v1/usage/by_model/:model_id/providers
// 返回该 model 在时间窗内被哪些 provider 调用过 + 各 provider 的请求数
// Usage.vue 表格的 Provider 列渲染时按需调用
func (a *Admin) modelProviders(c *gin.Context) {
	modelID := c.Param("model_id")
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_id required"})
		return
	}
	f := usage.QueryFilter{
		StartTime: time.Now().Add(-24 * time.Hour),
		EndTime:   time.Now(),
	}
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.StartTime = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.EndTime = t
		}
	}
	if v := c.Query("gateway_key"); v != "" {
		f.GatewayKeyID = v
	}
	rows, err := a.Usage.ModelProviders(c.Request.Context(), f, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "model_providers_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"model_id": modelID,
		"providers": rows,
		"count":    len(rows),
	})
}

func poolStatuses(pools map[string]*keypool.Pool) []keypool.PoolStatus {
	out := make([]keypool.PoolStatus, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Status())
	}
	return out
}

// ---------------------------------------------------------------------------
// AccessLogs (P67 / Task 8)
// ---------------------------------------------------------------------------

// accessLogStore 取出 *accesslog.Store,处理 Recorder 为 no-op 的情况。
//
// accesslog 配置 Enabled=false 时,Recorder 返回的 Store() 是 nil(P67 决议
// no-op 模式不连 DB)。handler 在 nil 时统一返回 503 — 前端可借此区分
// "禁用" 与 "空集合"。
func (a *Admin) accessLogStore() *accesslog.Store {
	if a.AccessLog == nil {
		return nil
	}
	return a.AccessLog.Store()
}

// parseTime 解析 RFC3339 时间字符串(F11 binding)。
//
// tolerant 设计:空串或解析失败一律返回 zero time + false,caller 用
// `if t, ok := parseTime(...); ok { f.StartTime = t }` 一行搞定;
// 失败不报错 — 时间参数错误不应该让整个 list 接口 500,直接当未设置。
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// last24hFilter 是 accessLogStats 复用 helper(F12 binding)。
//
// 提取出来避免 3 处 time.Now().UTC().Add(-24*time.Hour) 重复,
// 同时把 UTC 语义固定在一处 — 后续若要改时间窗口径,只动这里。
func last24hFilter() accesslog.QueryFilter {
	return accesslog.QueryFilter{
		StartTime: time.Now().UTC().Add(-24 * time.Hour),
	}
}

// parseStatusBuckets 把 ?status=4xx,auth_failed,no_route,... 解析成
// []accesslog.StatusBucket(F9 binding)。
//
// 映射规则(spec F9):
//   - "ok"   → status_code < 400
//   - "4xx"  → status_code ∈ [400, 500)
//   - "5xx"  → status_code >= 500
//   - "auth_failed" / "no_route" / "model_not_allowed" /
//     "key_provider_mismatch" / "upstream_4xx" / "upstream_429" /
//     "upstream_5xx" / "connection_error" / "timeout" / "unknown"
//     → error_type = 该值(精确匹配)
//
// 多值用 OR 拼装(store.buildWhere 处理)。未知 token 静默忽略 —
// 前端若传错不至于让接口 500,只是结果少几行。
func parseStatusBuckets(s string) []accesslog.StatusBucket {
	if s == "" {
		return nil
	}
	tokens := strings.Split(s, ",")
	out := make([]accesslog.StatusBucket, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		switch t {
		case "ok":
			out = append(out, accesslog.StatusBucket{Max: 400})
		case "4xx":
			out = append(out, accesslog.StatusBucket{Min: 400, Max: 500})
		case "5xx":
			out = append(out, accesslog.StatusBucket{Min: 500})
		default:
			// 其他 enum 值都按 error_type 精确匹配
			out = append(out, accesslog.StatusBucket{ErrorType: t})
		}
	}
	return out
}

// listAccessLogs GET /api/v1/access-logs
//
// 支持 query params:
//   start        RFC3339 时间下界
//   end          RFC3339 时间上界
//   gateway_key  精确匹配 gateway_key_name
//   provider     精确匹配 provider_name
//   model        匹配 requested_model 或 final_model
//   error_type   精确匹配 error_type
//   status       F9 多值,逗号分隔;OR 拼接
//   limit        默认 20,上限 200(Store.List 内部夹紧)
//   offset       默认 0
func (a *Admin) listAccessLogs(c *gin.Context) {
	store := a.accessLogStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "access_log_disabled"})
		return
	}
	f := accesslog.QueryFilter{
		GatewayKey:   c.Query("gateway_key"),
		ProviderName: c.Query("provider"),
		ModelID:      c.Query("model"),
		ErrorType:    c.Query("error_type"),
	}
	if t, ok := parseTime(c.Query("start")); ok {
		f.StartTime = t
	}
	if t, ok := parseTime(c.Query("end")); ok {
		f.EndTime = t
	}
	if v := c.Query("status"); v != "" {
		f.StatusBuckets = parseStatusBuckets(v)
	}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if v := c.Query("offset"); v != "" {
		f.Offset, _ = strconv.Atoi(v)
	}

	ctx := c.Request.Context()
	total, err := store.Count(ctx, f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count_failed", "detail": err.Error()})
		return
	}
	rows, err := store.List(ctx, f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"records": rows,
		"total":   total,
		"limit":   f.Limit,
		"offset":  f.Offset,
	})
}

// getAccessLogDetail GET /api/v1/access-logs/:id/detail
//
// 响应包含 metadata + 原始 body(字符串,F3 binding)+ truncated 标记(F1)。
//
// body 文件可能因 retention(24h GC)而丢失 — ReadBody 在文件不存在时返回
// error,这里用 err == nil 才赋值,避免 nil body 字段和 missing 文件混淆。
func (a *Admin) getAccessLogDetail(c *gin.Context) {
	store := a.accessLogStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "access_log_disabled"})
		return
	}
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	e, err := store.GetByID(c.Request.Context(), uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}

	// 加 body(可能因为 retention 而丢失)
	var reqBody, respBody []byte
	if e.ReqBodyPath != "" {
		if b, err := a.AccessLog.ReadBody(e.ReqBodyPath); err == nil {
			reqBody = b
		}
	}
	if e.RespBodyPath != "" {
		if b, err := a.AccessLog.ReadBody(e.RespBodyPath); err == nil {
			respBody = b
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"metadata":        e,
		"req_body":        string(reqBody),  // F3: 原始 JSON 字符串(非 base64)
		"resp_body":       string(respBody),
		"req_body_trunc":  accesslog.IsTruncated(e.ReqBodyPath),
		"resp_body_trunc": accesslog.IsTruncated(e.RespBodyPath),
	})
}

// accessLogStats GET /api/v1/access-logs/stats
//
// 24h 时间窗聚合:
//   total_24h   — 总记录数
//   errors_24h  — status_code >= 400 的记录数
//   active_keys — F14 binding:真正 distinct 的 gateway_key_name 数
//                 (COUNT(DISTINCT ...),不能误用 COUNT(*))
func (a *Admin) accessLogStats(c *gin.Context) {
	store := a.accessLogStore()
	if store == nil {
		// accesslog 整体禁用时返回零值,而不是 503 —
		// dashboard 前端不应因此报错,只是数字为 0
		c.JSON(http.StatusOK, gin.H{
			"total_24h":   int64(0),
			"errors_24h":  int64(0),
			"active_keys": int64(0),
		})
		return
	}
	ctx := c.Request.Context()

	// F12: last24hFilter 复用,避免 3 处 StartTime + Add(-24h) 重复
	last24h := last24hFilter()
	total, _ := store.Count(ctx, last24h)

	errFilter := last24h
	errFilter.StatusMin = 400
	errs, _ := store.Count(ctx, errFilter)

	// F14: 用 GroupByCount 真正算 distinct gateway key
	activeKeys, _ := store.GroupByCount(ctx, last24h, "gateway_key_name")

	c.JSON(http.StatusOK, gin.H{
		"total_24h":   total,
		"errors_24h":  errs,
		"active_keys": activeKeys,
	})
}
