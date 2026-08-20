// Package handler — 管理 API handlers
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/inflight"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
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
	Manager   *provider.Manager
	Registry  *provider.Registry
	Pools     map[string]*keypool.Pool
	Router    *router.Router
	Usage     *usage.Repository
	Aliases   map[string]router.AliasConfig
	Keys      []GatewayKeyInfo
	AccessLog *accesslog.Recorder // P67: 接入日志 Recorder(可能为 no-op)
	QuotaMgr  *quotacheck.Manager // P68/P-quota-balance: quota 恢复 worker(nil 时前端拿到 default)
	// P-mimo-quota: MIMO 控制台 cookie 持久化仓库(可能为 nil — 无 DB 时跳过持久化)
	MimoCookieStore MimoQuotaCookieStore
	// P-mimo-quota 解耦:处理器不 import 具体 vendor 包(provider/mimo),由 server
	// 层注入 vendor 专属的 cookie 校验/设置闭包 — 与 keyStatusLookup/quotaMarkFunc
	// 等函数注入同模式。nil = 该特性不可用(返回明确错误而非 panic)。
	MimoQuotaValidate func(ctx context.Context, cookie string) error // 打一次 usage 端点校验
	MimoQuotaSet      func(cookie string)                            // 热注入到内存(影响路由配额判断)
	// P-route-order: Level 2/3 优先级改写仓库(nil = 改写功能不可用,GET 返回空、PUT 报错)
	RouteOrderStore dbpkg.RouteOrderStore
	// P-route-order: PUT /routing/order(scope=provider)成功后由 server 调用来热更新
	// router 的 ProviderOrder(把 route_order 读回内存生效)。nil = 不热更新。
	ProviderOrderReload func()
	// P-route-order: PUT /routing/order(scope=key)成功后由 server 调用来重载该 provider
	// 的 pool(把 route_order 的 key 序接进 keypool)。nil = 不热更新。
	KeyOrderReload func(provider string)
	// P-fingerprint: 设备指纹归一化的运行时查询/热切换(回调注入,handler 不依赖 server 状态)。
	// FingerprintGet 返回 (enabled, canonical_device_id);FingerprintSet 翻转 enabled。
	FingerprintGet func() (bool, string)
	FingerprintSet func(enabled bool)
	// P-inflight: 活跃请求内存快照的只读查询(闭包注入,handler 不依赖 inflight 包)。
	InflightSnapshot func() []*inflight.Snapshot
}

// MimoQuotaCookieStore P-mimo-quota: MIMO 控制台 cookie 存取(单行,id=1)。
// 接口定义在 handler,由 auth.NewMimoQuotaCookieStore 实现注入。
type MimoQuotaCookieStore interface {
	Get(ctx context.Context) (*dbpkg.MimoQuotaCookie, error)
	Upsert(ctx context.Context, cookie string) error
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
	usageRepo *usage.Repository,
	aliases map[string]router.AliasConfig,
	keys []GatewayKeyInfo,
	accessLogR *accesslog.Recorder,
	quotaMgr *quotacheck.Manager,
	mimoCookieStore MimoQuotaCookieStore,
	mimoValidate func(ctx context.Context, cookie string) error, // 可 nil
	mimoSet func(cookie string), // 可 nil
	routeOrderStore dbpkg.RouteOrderStore, // 可 nil(改写不可用)
	providerOrderReload func(), // 可 nil(PUT provider 顺序后热更新 router)
	keyOrderReload func(provider string), // 可 nil(PUT key 顺序后重载 pool)
	fingerprintGet func() (bool, string), // 可 nil(fingerprint 查询不可用)
	fingerprintSet func(enabled bool), // 可 nil(fingerprint 切换不可用)
	inflightSnapshot func() []*inflight.Snapshot, // 可 nil(inflight 查询不可用)
) *Admin {
	return &Admin{
		Manager:             mgr,
		Registry:            reg,
		Pools:               pools,
		Router:              r,
		Usage:               usageRepo,
		Aliases:             aliases,
		Keys:                keys,
		AccessLog:           accessLogR,
		QuotaMgr:            quotaMgr,
		MimoCookieStore:     mimoCookieStore,
		MimoQuotaValidate:   mimoValidate,
		MimoQuotaSet:        mimoSet,
		RouteOrderStore:     routeOrderStore,
		ProviderOrderReload: providerOrderReload,
		KeyOrderReload:      keyOrderReload,
		FingerprintGet:      fingerprintGet,
		FingerprintSet:      fingerprintSet,
		InflightSnapshot:    inflightSnapshot,
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
	// P-mimo-quota: MIMO 控制台 cookie 查询/更新(cookie 约 1 天过期,过期后
	// 轮询退化保守;POST 一条命令热更新,不用改 config 重启)
	r.GET("/providers/mimo/quota-cookie", a.getMimoQuotaCookie)
	r.POST("/providers/mimo/quota-cookie", a.postMimoQuotaCookie)
	r.GET("/routing", a.listRouting)
	r.GET("/routing/order", a.getRouteOrder)
	r.PUT("/routing/order", a.putRouteOrder)
	r.GET("/usage", a.queryUsage)
	r.GET("/usage/aggregate", a.aggregateUsage)
	r.GET("/usage/by_model/:model_id/providers", a.modelProviders) // P65
	r.GET("/dashboard", a.dashboard)
	// P67: 接入日志管理 API(Task 8)
	r.GET("/access-logs", a.listAccessLogs)
	r.GET("/access-logs/stats", a.accessLogStats)
	r.GET("/access-logs/:id/detail", a.getAccessLogDetail)
	// P-training: JSONL 训练数据导出(过滤条件同 list)
	r.GET("/access-logs/export", a.exportAccessLogs)
	// P68 / P-quota-balance: 暴露 quota runtime config(目前只含 warn_threshold_pct)
	// 给前端 ProviderKeys.vue 用,避免硬编码颜色阈值。
	r.GET("/config/quota", a.getQuotaConfig)
	// P-fingerprint: 设备指纹归一化开关查询/热切换
	r.GET("/fingerprint", a.getFingerprint)
	r.PUT("/fingerprint", a.putFingerprint)
	// P-inflight: 实时活跃请求列表
	r.GET("/inflight", a.listInflight)
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
// P-provider-vendor: 按 vendor 聚合输出 — 同一厂商的多个注册名(deepseek / deepseek-anthropic)
// 归到同一 vendor 条目下,前端按厂商展示。key_pool / circuit_breaker 取该 vendor 第一个注册名的
// (共享 pool 时状态相同)。
func (a *Admin) listProviders(c *gin.Context) {
	// P-per-key-circuit: 熔断器已下沉到 keypool(per-key),不再有 provider 级
	// circuit_breaker 状态 — 每把 key 的熔断状态在 /api-keys 端点返回
	type vendorEntry struct {
		Vendor  string
		Names   []gin.H
		Models  []string
		KeyPool *keypool.PoolStatus
	}
	byVendor := make(map[string]*vendorEntry)
	order := make([]string, 0)
	for name, p := range a.Manager.GetAll() {
		v := a.Registry.VendorFor(name)
		entry, ok := byVendor[v]
		if !ok {
			entry = &vendorEntry{Vendor: v}
			byVendor[v] = entry
			order = append(order, v)
		}
		entry.Names = append(entry.Names, gin.H{"name": name, "protocol": string(p.Protocol())})
		entry.Models = append(entry.Models, p.Models()...)
		if pool, ok := a.Pools[name]; ok && entry.KeyPool == nil {
			st := pool.Status()
			entry.KeyPool = &st
		}
	}

	// P-provider-vendor: 确定性排序 — byVendor/Names 来自 Go map 迭代,顺序随机;
	// 不排序则前端 ProviderKeys.vue 取 names[0] 时 ~50% 概率是变体名(deepseek-anthropic),
	// 导致按变体名创建 key 存到共享 pool 读不到的行(静默失效)。
	sort.Strings(order)
	out := make([]gin.H, 0, len(order))
	for _, vendor := range order {
		v := byVendor[vendor]
		// 每个 entry 的 Names 也确定性排序(vendor 名排最前?不 — 按 name 字符串排序,
		// 保证 names[0] 稳定;vendor 名(deepseek)恒小于变体名(deepseek-anthropic))
		sort.Slice(v.Names, func(i, j int) bool {
			return v.Names[i]["name"].(string) < v.Names[j]["name"].(string)
		})
		// models 并集去重
		seen := make(map[string]bool, len(v.Models))
		models := make([]string, 0, len(v.Models))
		for _, m := range v.Models {
			if !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
		entry := gin.H{
			"vendor": v.Vendor,
			"names":  v.Names,
			"models": models,
		}
		if v.KeyPool != nil {
			entry["key_pool"] = v.KeyPool
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"vendors": out, "count": len(out)})
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
	c.JSON(http.StatusOK, info)
}

// getMimoQuotaCookie GET /api/v1/providers/mimo/quota-cookie
// 返回 cookie 配置状态(不回明文,只给掩码)
func (a *Admin) getMimoQuotaCookie(c *gin.Context) {
	out := gin.H{"configured": false, "updated_at": nil, "cookie_masked": ""}
	if a.MimoCookieStore != nil {
		row, err := a.MimoCookieStore.Get(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query_failed", "detail": err.Error()})
			return
		}
		if row != nil && row.Cookie != "" {
			out["configured"] = true
			out["updated_at"] = row.UpdatedAt
			out["cookie_masked"] = maskMimoCookie(row.Cookie)
		}
	}
	c.JSON(http.StatusOK, out)
}

// postMimoQuotaCookie POST /api/v1/providers/mimo/quota-cookie {"cookie": "..."}
// 验证 → 持久化(DB)→ 热注入(内存)。验证失败不写入。
// 抓取方法:浏览器登录 platform.xiaomimimo.com → F12 → 复制任意请求的完整 Cookie。
func (a *Admin) postMimoQuotaCookie(c *gin.Context) {
	var body struct {
		Cookie string `json:"cookie" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Cookie) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "detail": "body must include non-empty 'cookie'"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	// 先验证候选 cookie(打一次 usage 端点;失败 = 过期/无效,不写入)。
	// 具体 vendor 校验由 server 注入的 MimoQuotaValidate 闭包执行(处理器不 import 厂商包)。
	if a.MimoQuotaValidate == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "cookie_validation_unavailable"})
		return
	}
	if err := a.MimoQuotaValidate(ctx, strings.TrimSpace(body.Cookie)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "cookie_invalid",
			"detail": fmt.Sprintf("cookie rejected by MIMO: %v", err),
		})
		return
	}
	if a.MimoCookieStore != nil {
		if err := a.MimoCookieStore.Upsert(c.Request.Context(), body.Cookie); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "persist_failed", "detail": err.Error()})
			return
		}
	}
	// 热注入到内存(影响路由配额判断),由 server 层注入的 setter 闭包完成。
	if a.MimoQuotaSet != nil {
		a.MimoQuotaSet(body.Cookie)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "mimo quota cookie updated"})
}

// maskMimoCookie 只留 cookie 前 24 字符,回显时防泄露
func maskMimoCookie(cookie string) string {
	if len(cookie) <= 24 {
		return "***"
	}
	return cookie[:24] + "..."
}

// embedJSONBody 把 body 字节嵌入导出样本:合法 JSON 原样内嵌(嵌套对象,
// 训练管线直接用),否则退化为字符串(截断/非 JSON 场景)
func embedJSONBody(b []byte) any {
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}

// exportAccessLogs GET /api/v1/access-logs/export — JSONL 训练数据导出
//
// 过滤条件与 list 一致(start/end/gateway_key/provider/model/status),
// 每行一条请求:metadata + req/resp body(原始 JSON 内嵌)。
// 截断样本用 req_body_trunc / resp_body_trunc 标记,方便下游筛选;
// body 文件可能因 retention 丢失 — 读不到就省略该字段。
func (a *Admin) exportAccessLogs(c *gin.Context) {
	store := a.accessLogStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "access_log_disabled"})
		return
	}
	f := accesslog.QueryFilter{
		GatewayKey:   c.Query("gateway_key"),
		ProviderName: c.Query("provider"),
		ModelID:      c.Query("model"),
		TraceID:      c.Query("trace_id"),
		ErrorType:    c.Query("error_type"),
	}
	if t, ok := parseTime(c.Query("start")); ok {
		f.StartTime = t
	}
	if t, ok := parseTime(c.Query("end")); ok {
		f.EndTime = t
	}
	if v := c.Query("status"); v != "" {
		buckets, unknown, ok := parseStatusBuckets(v)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_status", "unknown": unknown})
			return
		}
		f.StatusBuckets = buckets
	}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if f.Limit <= 0 {
		f.Limit = 10000 // 导出默认 1 万条,上限 5 万(防一次性拖垮网关)
	}
	if f.Limit > 50000 {
		f.Limit = 50000
	}

	rows, err := store.List(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export_failed", "detail": err.Error()})
		return
	}

	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition",
		fmt.Sprintf("attachment; filename=access-logs-%d.ndjson", time.Now().UTC().Unix()))

	enc := json.NewEncoder(c.Writer)
	for _, e := range rows {
		sample := map[string]any{
			"id": e.ID, "trace_id": e.TraceID, "created_at": e.CreatedAt,
			"gateway_key_name": e.GatewayKeyName,
			"method":           e.Method, "path": e.Path,
			"requested_model": e.RequestedModel, "final_model": e.FinalModel,
			"provider_name": e.ProviderName, "protocol": e.Protocol,
			"is_stream": e.IsStream, "status_code": e.StatusCode, "error_type": e.ErrorType,
			"latency_ms":      e.LatencyMs,
			"req_body_trunc":  accesslog.IsTruncated(e.ReqBodyPath),
			"resp_body_trunc": accesslog.IsTruncated(e.RespBodyPath),
		}
		if e.ReqBodyPath != "" {
			if b, err := a.AccessLog.ReadBody(e.ReqBodyPath); err == nil {
				sample["req_body"] = embedJSONBody(b)
			}
		}
		if e.RespBodyPath != "" {
			if b, err := a.AccessLog.ReadBody(e.RespBodyPath); err == nil {
				sample["resp_body"] = embedJSONBody(b)
			}
		}
		if err := enc.Encode(sample); err != nil {
			return // 客户端断开,停止写入
		}
	}
}

// listKeys 移除:P16 起由 auth.KeysHandler 提供 DB-backed 的 GET /api/v1/keys

// listRouting GET /api/v1/routing
func (a *Admin) listRouting(c *gin.Context) {
	aliases := a.Aliases
	if aliases == nil {
		aliases = a.Router.Aliases()
	}
	c.JSON(http.StatusOK, gin.H{
		"aliases":   aliases,
		"count":     len(aliases),
		"catch_all": a.Router.CatchAllConfig(), // P-catch-all: 兜底规则(可能为 null)
	})
}

// getRouteOrder GET /api/v1/routing/order?scope=provider|key&provider=<name>
// 返回某作用域的改写排序(Level 2 层内 provider / Level 3 provider 内 key)。
// 无 RouteOrderStore 或该作用域无改写 → 空列表(前端回退默认 created_at 排序)。
func (a *Admin) getRouteOrder(c *gin.Context) {
	scope := c.Query("scope")
	providerName := c.Query("provider")
	if scope != "provider" && scope != "key" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_scope", "detail": "scope=provider|key"})
		return
	}
	if a.RouteOrderStore == nil {
		c.JSON(http.StatusOK, gin.H{"scope": scope, "provider": providerName, "order": []string{}})
		return
	}
	rows, err := a.RouteOrderStore.ListByScope(c.Request.Context(), scope, providerName, c.Query("billing_source"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list_route_order_failed", "detail": err.Error()})
		return
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	c.JSON(http.StatusOK, gin.H{"scope": scope, "provider": providerName, "order": names})
}

// putRouteOrder PUT /api/v1/routing/order
// 整体替换某作用域的改写排序。body: {"scope":"provider|key","provider":"<name>",
// "billing_source":"<token_plan|api>","order":["a","b","c"]}
// 无 RouteOrderStore → 503(改写未启用)。
func (a *Admin) putRouteOrder(c *gin.Context) {
	if a.RouteOrderStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "route_order_disabled"})
		return
	}
	var req struct {
		Scope         string   `json:"scope"`
		Provider      string   `json:"provider"`
		BillingSource string   `json:"billing_source"`
		Order         []string `json:"order"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "detail": err.Error()})
		return
	}
	if req.Scope != "provider" && req.Scope != "key" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_scope", "detail": "scope=provider|key"})
		return
	}
	if err := a.RouteOrderStore.Replace(c.Request.Context(), req.Scope, req.Provider, req.BillingSource, req.Order); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "put_route_order_failed", "detail": err.Error()})
		return
	}
	// P-route-order: 改写落库后按作用域热更新
	//   scope=provider → 热更新 router 的 Level 2 排序
	//   scope=key       → 重载该 provider 的 pool(接进 keypool 的 Level 3 排序)
	if a.ProviderOrderReload != nil && req.Scope == "provider" {
		a.ProviderOrderReload()
	}
	if a.KeyOrderReload != nil && req.Scope == "key" {
		a.KeyOrderReload(req.Provider)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "scope": req.Scope, "provider": req.Provider, "order": req.Order})
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
		total.TotalLatencyMs += r.TotalLatencyMs
		total.ErrorCount += r.ErrorCount
		// 注意:AvgTtftMs / AvgLatencyMs 是 AVG,跨 model 不能简单相加,
		// dashboard 总卡片不展示这两个粒度,这里故意不合成(留 0)。
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_id_required"})
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
		"model_id":  modelID,
		"providers": rows,
		"count":     len(rows),
	})
}

// poolStatuses 汇总所有 key pool 状态(dashboard keypools)。
// P-provider-vendor: Pools map 按注册名建 key,同 vendor 共享同一 pool 指针 —
// 必须按 pool 去重,否则同一个池输出两次(两个 deepseek + 两个 minimax),
// QuotaKnownSum 翻倍。与 quotacheck pollAllBalancers 的 seen 去重同一不变量。
func poolStatuses(pools map[string]*keypool.Pool) []keypool.PoolStatus {
	seen := make(map[*keypool.Pool]bool, len(pools))
	out := make([]keypool.PoolStatus, 0, len(pools))
	for _, p := range pools {
		if seen[p] {
			continue
		}
		seen[p] = true
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

// validStatusTokens 是 ?status= 允许的合法 token 白名单(F9 enum)。
//
// 必须与 spec §1.2 错误桶枚举保持一致,且只允许这些值;
// 任何不在表内的输入视为"未知",由 caller 决定如何回应(本 handler
// 选择 400 BadRequest,见 listAccessLogs)。
var validStatusTokens = map[string]bool{
	"ok":                    true,
	"4xx":                   true,
	"5xx":                   true,
	"auth_failed":           true,
	"no_route":              true,
	"model_not_allowed":     true,
	"key_provider_mismatch": true,
	"upstream_4xx":          true,
	"upstream_429":          true,
	"upstream_5xx":          true,
	"connection_error":      true,
	"timeout":               true,
	"invalid_request":       true, // proxy.classifyError 回这个(clients request 400)
	"stream_interrupted":    true, // proxy stream mid-error(上游断流),前端过滤 UI 有
	"client_disconnected":   true,
	"unknown":               true,
}

// statusBucketFor 把合法的 status token 翻译成对应的 accesslog.StatusBucket。
//
// 映射规则(spec F9):
//   - "ok"   → status_code < 400
//   - "4xx"  → status_code ∈ [400, 500)
//   - "5xx"  → status_code >= 500
//   - 其它 enum 值(error_type 系列)→ error_type 精确匹配
//
// 调用方必须先用 validStatusTokens 校验 t 合法后再调用本函数。
func statusBucketFor(t string) accesslog.StatusBucket {
	switch t {
	case "ok":
		return accesslog.StatusBucket{Max: 400}
	case "4xx":
		return accesslog.StatusBucket{Min: 400, Max: 500}
	case "5xx":
		return accesslog.StatusBucket{Min: 500}
	default:
		return accesslog.StatusBucket{ErrorType: t}
	}
}

// parseStatusBuckets 把 ?status=4xx,auth_failed,no_route,... 解析成
// []accesslog.StatusBucket(F9 binding)。
//
// 返回值:
//
//	buckets  — 翻译后的 StatusBucket 列表(OR 拼装,store.buildWhere 处理)
//	unknown  — 输入中遇到的未知 token(用于在响应里告诉前端具体哪几个)
//	ok       — 是否全部合法;为 false 时 caller 应 400 拒绝
//
// 多值用 OR 拼装。未知 token 不再"静默忽略"(原实现 bug):一旦出现
// 未知值就 ok=false,由 caller 决定如何回应。
func parseStatusBuckets(s string) (buckets []accesslog.StatusBucket, unknown []string, ok bool) {
	if s == "" {
		return nil, nil, true
	}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !validStatusTokens[t] {
			unknown = append(unknown, t)
			continue
		}
		buckets = append(buckets, statusBucketFor(t))
	}
	if len(unknown) > 0 {
		return buckets, unknown, false
	}
	return buckets, nil, true
}

// listAccessLogs GET /api/v1/access-logs
//
// 支持 query params:
//
//	start        RFC3339 时间下界
//	end          RFC3339 时间上界
//	gateway_key  按名字过滤(子查询现查 gateway_keys 当前名字的 ID 集合)
//	provider     精确匹配 provider_name
//	model        匹配 requested_model 或 final_model
//	trace_id     精确匹配 trace_id
//	error_type   精确匹配 error_type
//	status       F9 多值,逗号分隔;OR 拼接
//	limit        默认 20,上限 200(Store.List 内部夹紧)
//	offset       默认 0
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
		TraceID:      c.Query("trace_id"),
		ErrorType:    c.Query("error_type"),
	}
	if t, ok := parseTime(c.Query("start")); ok {
		f.StartTime = t
	}
	if t, ok := parseTime(c.Query("end")); ok {
		f.EndTime = t
	}
	if v := c.Query("status"); v != "" {
		buckets, unknown, ok := parseStatusBuckets(v)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_status", "unknown": unknown})
			return
		}
		f.StatusBuckets = buckets
	}
	if v := c.Query("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if v := c.Query("offset"); v != "" {
		f.Offset, _ = strconv.Atoi(v)
	}

	// 归一化分页参数,确保响应里 limit/offset 与实际查询一致:
	//   - limit  默认 20,上限 200(Store.List 内部也夹紧,但响应要
	//     反映 effective 值,前端才能正确分页)
	//   - offset 不允许为负
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	if f.Offset < 0 {
		f.Offset = 0
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
		"req_body":        string(reqBody), // F3: 原始 JSON 字符串(非 base64)
		"resp_body":       string(respBody),
		"req_body_trunc":  accesslog.IsTruncated(e.ReqBodyPath),
		"resp_body_trunc": accesslog.IsTruncated(e.RespBodyPath),
	})
}

// accessLogStats GET /api/v1/access-logs/stats
//
// 24h 时间窗聚合:
//
//	total_24h   — 总记录数
//	errors_24h  — status_code >= 400 的记录数
//	active_keys — F14 binding:真正 distinct 的 gateway_key_id 数
//	              (COUNT(DISTINCT ...),不能误用 COUNT(*);
//	              gateway_key_name 不落库,distinct 按 ID 数)
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

	// F14: 用 GroupByCount 真正算 distinct gateway key(按 ID —
	// name 不落库;ID 数才是真正的 key 数,改名不会 inflate)
	activeKeys, _ := store.GroupByCount(ctx, last24h, "gateway_key_id")

	c.JSON(http.StatusOK, gin.H{
		"total_24h":   total,
		"errors_24h":  errs,
		"active_keys": activeKeys,
	})
}

// getQuotaConfig GET /api/v1/config/quota
//
// 返回 quota manager 的 runtime config 中需要前端知道的字段(目前只有
// warn_threshold_pct)。QuotaMgr 为 nil 时返回兜底默认值 10,与
// quotacheck.DefaultManagerConfig / NewManager 的兜底值保持一致。
//
// 设计:只读,不做 hot-reload 入口 — 前端想改阈值改 config.yaml 重启即可。
func (a *Admin) getQuotaConfig(c *gin.Context) {
	pct := 10
	if a.QuotaMgr != nil {
		pct = a.QuotaMgr.WarnThresholdPct()
	}
	c.JSON(http.StatusOK, gin.H{
		"warn_threshold_pct": pct,
	})
}

// getFingerprint GET /api/v1/fingerprint
// 返回设备指纹归一化的运行时状态(enabled + canonical_device_id)。
// FingerprintGet 为 nil(= server 未注入)时返回默认关闭 + 空 device_id。
func (a *Admin) getFingerprint(c *gin.Context) {
	enabled := false
	canonical := ""
	if a.FingerprintGet != nil {
		enabled, canonical = a.FingerprintGet()
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":             enabled,
		"canonical_device_id": canonical,
	})
}

// putFingerprint PUT /api/v1/fingerprint
// 热切换设备指纹归一化开关。body: {"enabled": true|false}。
// 只翻 enabled,不改 canonical_device_id(该值启动时 Capture,改它需重启)。
func (a *Admin) putFingerprint(c *gin.Context) {
	if a.FingerprintSet == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "fingerprint toggle not available"})
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'enabled' is required"})
		return
	}
	a.FingerprintSet(*req.Enabled)
	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}

// listInflight GET /api/v1/inflight — 返回当前活跃请求的内存快照列表。
// elapsed_ms 由 now - StartedAt 现算(快照只存 StartedAt,不存耗时)。
func (a *Admin) listInflight(c *gin.Context) {
	snap := []*inflight.Snapshot{}
	if a.InflightSnapshot != nil {
		snap = a.InflightSnapshot()
	}
	type req struct {
		TraceID        string `json:"trace_id"`
		StartedAt      string `json:"started_at"`
		RequestedModel string `json:"requested_model"`
		FinalModel     string `json:"final_model"`
		ProviderName   string `json:"provider_name"`
		GatewayKeyName string `json:"gateway_key_name"`
		IsStream       bool   `json:"is_stream"`
		ElapsedMs      int64  `json:"elapsed_ms"`
	}
	now := time.Now()
	out := make([]req, 0, len(snap))
	for _, s := range snap {
		out = append(out, req{
			TraceID:        s.TraceID,
			StartedAt:      s.StartedAt.UTC().Format(time.RFC3339),
			RequestedModel: s.RequestedModel,
			FinalModel:     s.FinalModel,
			ProviderName:   s.ProviderName,
			GatewayKeyName: s.GatewayKeyName,
			IsStream:       s.IsStream,
			ElapsedMs:      now.Sub(s.StartedAt).Milliseconds(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"requests": out})
}
