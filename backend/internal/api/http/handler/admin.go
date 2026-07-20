// Package handler — 管理 API handlers
package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

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
	r.GET("/dashboard", a.dashboard)
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
		out = append(out, gin.H{
			"name":     name,
			"protocol": string(protocol),
			"loaded":   loadedOK,
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

	records, err := a.Usage.Query(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query_failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"records": records,
		"count":   len(records),
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

	total := usage.AggregateResult{}
	for _, r := range rows {
		total.TotalRequests += r.TotalRequests
		total.TotalInput += r.TotalInput
		total.TotalOutput += r.TotalOutput
		total.TotalTokens += r.TotalTokens
		total.TotalCost += r.TotalCost
		total.ErrorCount += r.ErrorCount
	}

	c.JSON(http.StatusOK, gin.H{
		"window":             "24h",
		"total":              total,
		"by_provider_model":  rows,
		"providers_count":    len(a.Manager.GetAll()),
		"keypools":           poolStatuses(a.Pools),
	})
}

func poolStatuses(pools map[string]*keypool.Pool) []keypool.PoolStatus {
	out := make([]keypool.PoolStatus, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Status())
	}
	return out
}
