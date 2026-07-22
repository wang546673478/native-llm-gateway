// Package server 负责 Gateway 服务的启动、编排和优雅关停
// 对应规格书 5.x 服务生命周期
package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/api/http/handler"
	"github.com/wang546673478/native-llm-gateway/internal/api/http/middleware"
	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/circuit"
	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/metrics"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/proxy"
	"github.com/wang546673478/native-llm-gateway/internal/router"
	"github.com/wang546673478/native-llm-gateway/internal/usage"
)

// Server 持有所有运行时依赖
type Server struct {
	cfg      *config.Config
	logger   *zap.Logger
	db       *gorm.DB
	manager  *provider.Manager
	router   *router.Router
	engine   *proxy.Engine
	pools    map[string]*keypool.Pool
	cm       *circuit.Manager
	auth     *auth.Authenticator
	usageC   *usage.Collector
	usageR   *usage.Repository
	metricsC *metrics.Collector
	accessR  *accesslog.Recorder // P67: 接入日志 Recorder
	http     *http.Server
}

// New 构造 Server
func New(cfg *config.Config, logger *zap.Logger, db *gorm.DB, manager *provider.Manager) (*Server, error) {
	// P3+P4+P5: 构造 KeyPool map + Router + Proxy
	// P30:从 DB (provider_api_keys 表) 读 key 而不是 config.yaml
	pools := buildKeyPools(cfg, db, logger)
	r := router.NewRouter(logger, manager, pools, router.Config{
		Aliases:         toRouterAliases(cfg.Routing.Aliases, cfg.Routing.Chains),
		DefaultStrategy: cfg.Routing.DefaultStrategy,
		MaxAttempts:     cfg.Retry.MaxAttempts,
	})
	// P6: Circuit Breaker
	cm := circuit.NewManager(r)
	reporter := circuit.NewReporter(cm)
	// 为每个 enabled Provider 创建 Breaker
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		cm.GetOrCreate(name, circuit.Config{
			FailureThreshold: p.CircuitBreaker.FailureThreshold,
			FailureWindow:    p.CircuitBreaker.FailureWindow,
			OpenTimeout:      p.CircuitBreaker.OpenTimeout,
			HalfOpenRequests: p.CircuitBreaker.HalfOpenRequests,
			CountableErrors:  p.CircuitBreaker.CountableErrors,
			ExcludedErrors:   p.CircuitBreaker.ExcludedErrors,
		})
	}

	// P7: Authenticator(从 DB 加载;config keys 在启动时被 seed 到 DB)
	var authn *auth.Authenticator
	if cfg.Auth.Enabled {
		// 把 config 里的 keys seed 到 DB
		gkKeys := make([]auth.GatewayKey, 0, len(cfg.Auth.Keys))
		for _, k := range cfg.Auth.Keys {
			gkKeys = append(gkKeys, auth.GatewayKey{
				Name:          k.Name,
				KeyHash:       k.Key,
				AllowedModels: k.AllowedModels,
				RateLimit:     auth.RateLimitConfig{RPM: k.RateLimit.RPM, TPM: k.RateLimit.TPM},
			})
		}
		if err := auth.SeedFromConfig(context.Background(), db, gkKeys); err != nil {
			logger.Warn("seed keys from config failed", zap.Error(err))
		}
		// 从 DB 加载所有 keys(含 config seed + UI 添加的)
		dbKeys, err := auth.LoadFromDB(context.Background(), db)
		if err != nil {
			logger.Warn("load keys from DB failed", zap.Error(err))
			dbKeys = gkKeys // fallback to config
		}
		authn = auth.New(dbKeys)
		logger.Info("auth enabled", zap.Int("keys", len(dbKeys)))
	}

	// P8: Usage Collector + Metrics Collector
	usageC := usage.NewCollector(db, cfg.Usage.BatchSize, int(cfg.Usage.FlushInterval.Milliseconds()))
	usageRepo := usage.NewRepository(db)
	metricsC := metrics.NewCollector()

	// P67: AccessLog Recorder(接入日志模块)
	//   - enabled=false:返回 no-op Recorder,proxy 所有调用都静默 no-op
	//   - zero value 字段用 default 兜底
	accessCfg := accesslog.RecorderConfig{
		Enabled:       cfg.Server.AccessLog.Enabled,
		BodyDir:       cfg.Server.AccessLog.BodyDir,
		BufferSize:    cfg.Server.AccessLog.BufferSize,
		BatchSize:     cfg.Server.AccessLog.BatchSize,
		FlushInterval: cfg.Server.AccessLog.FlushInterval,
		Retention:     cfg.Server.AccessLog.Retention,
	}
	if accessCfg.BodyDir == "" {
		accessCfg.BodyDir = config.DefaultAccessLogBodyDir
	}
	if accessCfg.BufferSize == 0 {
		accessCfg.BufferSize = config.DefaultAccessLogBufferSize
	}
	if accessCfg.BatchSize == 0 {
		accessCfg.BatchSize = config.DefaultAccessLogBatchSize
	}
	if accessCfg.FlushInterval == 0 {
		accessCfg.FlushInterval = config.DefaultAccessLogFlushInterval
	}
	if accessCfg.Retention == 0 {
		accessCfg.Retention = config.DefaultAccessLogRetention
	}
	accessR, err := accesslog.NewRecorder(accessCfg, db, logger)
	if err != nil {
		return nil, fmt.Errorf("accesslog new: %w", err)
	}

	eng := proxy.NewEngine(proxy.Config{
		Router:        r,
		Logger:        logger,
		Usage:         usage.NewAdapter(usageC),
		Metrics:       metrics.NewAdapter(metricsC),
		Breaker:       reporter,
		TokenRecorder: newAuthTokenRecorder(authn), // P13: TPM 计数(若 auth 启用)
		Authenticator: authn,                        // P19: Provider 绑定检查
		AccessLog:     accessR,                      // P67: 接入日志
		MaxRetry:      cfg.Retry.MaxAttempts,
	})
	// P30:把 DB Pool 注入到每个 Provider(Manager.LoadFromConfig 时 Pool 还是 nil)
	injectPools(manager, pools, logger)

	return &Server{
		cfg:      cfg,
		logger:   logger,
		db:       db,
		manager:  manager,
		router:   r,
		engine:   eng,
		pools:    pools,
		cm:       cm,
		auth:     authn,
		usageC:   usageC,
		usageR:   usageRepo,
		metricsC: metricsC,
		accessR:  accessR,
	}, nil
}

// P30:把 buildKeyPools 读出来的 Pool 注入到每个 Provider
// Manager.LoadFromConfig 时 Pool 还是 nil(那时 DB 还没读),
// 启动后 Server.New 再注入
func injectPools(manager *provider.Manager, pools map[string]*keypool.Pool, logger *zap.Logger) {
	for name, p := range manager.GetAll() {
		pool, ok := pools[name]
		if !ok {
			continue
		}
		// Provider interface 加 SetPool(*keypool.Pool) 方法
		// 各 base 实现透传
		if setter, ok := p.(interface {
			SetPool(*keypool.Pool)
		}); ok {
			setter.SetPool(pool)
			logger.Info("pool injected", zap.String("provider", name))
		}
	}
}

// buildKeyPools 为每个 enabled Provider 构造一个 KeyPool
// P30:key 从 DB (provider_api_keys 表) 读,不用 config.yaml
func buildKeyPools(cfg *config.Config, db *gorm.DB, logger *zap.Logger) map[string]*keypool.Pool {
	out := make(map[string]*keypool.Pool)
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	poolCfg := keypool.Config{
		CoolingDuration: cfg.KeyPool.CoolingDuration,
		MaxCoolingCount: cfg.KeyPool.MaxCoolingCount,
	}
	store := auth.NewProviderKeyStore(db)
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		out[name] = buildOnePool(context.Background(), name, sched, poolCfg, store, logger)
	}
	return out
}

// buildOnePool P35: 给单个 provider 从 DB 构造 Pool
// 启动时全量构造、运行时热更新都用它
func buildOnePool(ctx context.Context, name string, sched keypool.Scheduler, poolCfg keypool.Config, store auth.ProviderKeyStore, logger *zap.Logger) *keypool.Pool {
	rows, err := store.List(ctx, name)
	if err != nil {
		logger.Warn("read provider keys from DB failed",
			zap.String("provider", name),
			zap.Error(err))
		return keypool.NewPool(name, nil, sched, poolCfg)
	}
	if len(rows) == 0 {
		logger.Warn("provider has no API keys in DB",
			zap.String("provider", name))
		return keypool.NewPool(name, nil, sched, poolCfg)
	}
	keys := make([]*keypool.Key, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		bs := row.BillingSource
		if bs == "" {
			bs = "api" // 兜底
		}
		keys = append(keys, &keypool.Key{
			ID:            fmt.Sprintf("%d", row.ID),
			ProviderName:  name,
			Name:          row.Name,
			Key:           row.KeyHash,
			Status:        keypool.KeyStatusActive,
			BillingSource: bs, // P48: 单 key 计费 tier,Pool.Acquire 按此排序
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		})
	}
	logger.Info("keypool built from DB",
		zap.String("provider", name),
		zap.Int("keys", len(keys)),
	)
	return keypool.NewPool(name, keys, sched, poolCfg)
}

// toManagerConfigForReload 把 config 投影成 ManagerConfig(只用于 ReloadPricing,不需要 Pools)
func toManagerConfigForReload(cfg *config.Config, pools map[string]*keypool.Pool) *provider.ManagerConfig {
	mcfg := &provider.ManagerConfig{
		Providers: make(map[string]provider.ManagerProviderConfig, len(cfg.Providers)),
		Pools:     make(map[string]any, len(pools)),
	}
	for name, pool := range pools {
		mcfg.Pools[name] = pool
	}
	for name, p := range cfg.Providers {
		proto, _ := provider.ParseProtocol(p.Protocol)
		modelCosts := make(map[string]provider.ModelCost, len(p.Models))
		for _, m := range p.Models {
			modelCosts[m.ID] = provider.ModelCost{
				CostPer1kInput:         m.CostPer1kInput,
				CostPer1kOutput:        m.CostPer1kOutput,
				CostPer1kCacheRead:     m.CostPer1kCacheRead,
				CostPer1kCacheCreation: m.CostPer1kCacheCreation,
			}
		}
		mcfg.Providers[name] = provider.ManagerProviderConfig{
			Enabled:    p.Enabled,
			Endpoint:   p.Endpoint,
			Protocol:   proto,
			Timeout:    p.Timeout,
			Models:     nil, // ReloadPricing 不需要 models 列表
			ModelCosts: modelCosts,
			APIKeys:    nil,
			// P47: 计费来源 — 热重载时也带上
			BillingSource: defaultBillingSource(p.BillingSource),
		}
	}
	return mcfg
}

// defaultBillingSource 同 defaultStr,但专门给 BillingSource 用(语义清晰)
func defaultBillingSource(s string) string {
	if s == "" {
		return "api"
	}
	return s
}

// toRouterAliases 把 config 风格别名表转 router 风格
// P39: chain_ref → 从 chains map 展开成 providers
// P53: TargetModel → 短格式(留空 Providers,Router 走 auto-discovery)
func toRouterAliases(in map[string]config.AliasRule, chains map[string][]config.AliasRoute) map[string]router.AliasConfig {
	out := make(map[string]router.AliasConfig, len(in))
	for alias, rule := range in {
		// 决定实际使用的 provider 列表
		var src []config.AliasRoute
		switch {
		case rule.ChainRef != "":
			if chain, ok := chains[rule.ChainRef]; ok {
				src = chain
			}
			// chain_ref 找不到 → src 留空,Router 走 auto-discovery
		case rule.TargetModel != "":
			// P53: 短格式 — TargetModel 模式下,src 留空,Router 会从所有
			// 声明该 model 的 provider 中自动发现
			_ = rule.TargetModel // intentionally not expanded here
		default:
			src = rule.Providers
		}

		ps := make([]router.ProviderRoute, 0, len(src))
		for _, p := range src {
			ps = append(ps, router.ProviderRoute{
				Name: p.Name, Model: p.Model, Priority: p.Priority, Weight: p.Weight,
			})
		}
		out[alias] = router.AliasConfig{
			Alias:       alias,
			Strategy:    rule.Strategy,
			Providers:   ps,
			TargetModel: rule.TargetModel, // P53: 短格式标记
		}
	}
	return out
}

// Run 启动 HTTP 服务
func (s *Server) Run(ctx context.Context) error {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	s.registerRoutes(r)

	// P8: 启动 Usage Collector 后台落库协程
	s.usageC.Start(ctx)
	// P67: 启动 AccessLog Recorder(async buffer + retention)
	if s.accessR != nil {
		s.accessR.Start(ctx)
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.http = &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  s.cfg.Server.ReadTimeout,
		WriteTimeout: s.cfg.Server.WriteTimeout,
		IdleTimeout:  s.cfg.Server.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("gateway listening", zap.String("addr", addr))
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received")
		s.usageC.Stop() // flush 剩余记录
		if s.accessR != nil {
			_ = s.accessR.Close() // flush buffer + stop retention
		}
		return s.shutdown()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server error: %w", err)
		}
		return nil
	}
}

// shutdown 优雅关闭
func (s *Server) shutdown() error {
	timeout := s.cfg.Server.ShutdownTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s.logger.Info("graceful shutdown", zap.Duration("timeout", timeout))
	return s.http.Shutdown(ctx)
}

// registerRoutes 注册路由
func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"version": "0.5.0-p5",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	r.GET("/readyz", func(c *gin.Context) {
		if s.db == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_not_initialized"})
			return
		}
		sqlDB, err := s.db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_handle_error", "error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
		defer cancel()
		if err := sqlDB.PingContext(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	r.GET("/admin/providers", func(c *gin.Context) {
		all := s.manager.GetAll()
		out := make([]gin.H, 0, len(all))
		for name, p := range all {
			out = append(out, gin.H{
				"name":     name,
				"protocol": string(p.Protocol()),
				"models":   p.Models(),
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"count":     len(out),
			"providers": out,
		})
	})

	// P8: /metrics(Prometheus 格式)
	r.GET("/metrics", gin.WrapH(s.metricsC.Handler()))

	// P12: 管理 API
	gkInfos := make([]handler.GatewayKeyInfo, 0, len(s.cfg.Auth.Keys))
	for _, k := range s.cfg.Auth.Keys {
		gkInfos = append(gkInfos, handler.GatewayKeyInfo{
			Name:          k.Name,
			AllowedModels: k.AllowedModels,
			RPM:           k.RateLimit.RPM,
			TPM:           k.RateLimit.TPM,
		})
	}
	admin := &handler.Admin{
		Manager:  s.manager,
		Registry: provider.Default(),
		Pools:    s.pools,
		Router:   s.router,
		Breakers: s.cm,
		Usage:    s.usageR,
		Aliases:  toRouterAliases(s.cfg.Routing.Aliases, s.cfg.Routing.Chains),
		Keys:     gkInfos,
	}
	admin.Register(r.Group("/api/v1"))

	// P16: Gateway Keys CRUD handler
	// 注意:CRUD 端点本身不要求 auth.enabled,这样即使没启用 auth 也能管理 keys
	// Authenticator 用一个 noop wrapper,把 Reload 调用变 no-op
	noopReload := func(keys []auth.GatewayKey) {
		if s.auth != nil {
			s.auth.Reload(keys)
		}
	}
	keysHandler := auth.NewKeysHandler(s.db, noopReload)
	keysHandler.Register(r.Group("/api/v1"))

	// P30: Provider API keys 管理(给已插件化的 Provider 加上游 LLM key)
	pkHandler := auth.NewProviderKeysHandler(s.db, s.ReloadProviderPool)
	pkHandler.RegisterOn(r.Group("/api/v1"))

	// P5: 真代理接入
	// 注册具体协议路径 + NoRoute 兜底(覆盖其他 /v1/* 子路径)
	// P7: 当 auth.enabled=true 时,代理端点前挂 Auth + RateLimit 中间件
	proxyHandlers := []gin.HandlerFunc{}
	if s.auth != nil {
		proxyHandlers = append(proxyHandlers,
			middleware.AuthMiddleware(s.auth),
			middleware.RateLimitMiddleware(s.auth),
		)
	}
	proxyHandlers = append(proxyHandlers, s.engine.HandleRequest)

	r.POST("/v1/chat/completions", proxyHandlers...)
	r.POST("/v1/messages", proxyHandlers...)
	r.POST("/v1/completions", proxyHandlers...)
	// 流式请求也走 HandleRequest,Engine 内部从 body.stream 判断
	// 没匹配到的路径兜底(例如 /v1/embeddings 之类)
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && len(c.Request.URL.Path) > 4 && c.Request.URL.Path[:4] == "/v1/" {
			if s.auth != nil {
				middleware.AuthMiddleware(s.auth)(c)
				if c.IsAborted() {
					return
				}
				middleware.RateLimitMiddleware(s.auth)(c)
				if c.IsAborted() {
					return
				}
			}
			s.engine.HandleRequest(c)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"type": "not_found", "message": "no route for " + c.Request.URL.Path,
		}})
	})
}

var _ = database.Provider{} // keep database import alive

// authTokenRecorder 把 auth.Authenticator 适配到 proxy.TokenUsageRecorder
type authTokenRecorder struct {
	a *auth.Authenticator
}

func newAuthTokenRecorder(a *auth.Authenticator) *authTokenRecorder {
	if a == nil {
		return nil
	}
	return &authTokenRecorder{a: a}
}

func (r *authTokenRecorder) RecordUsage(keyID string, tokens int64) {
	r.a.RecordTokens(keyID, tokens)
}

// Reload 热重载 — 替换 Aliases / Auth Keys / Pricing
// 注意:Provider 实例 / KeyPool / Circuit Breaker 不在此函数内重载
// (它们的 Reload 已在 Manager 上,但涉及 HTTP 客户端重建,留给后续阶段)
func (s *Server) Reload(newCfg *config.Config) {
	if newCfg == nil {
		return
	}
	// Router aliases
	s.router.ReloadAliases(toRouterAliases(newCfg.Routing.Aliases, newCfg.Routing.Chains))

	// Manager 定价表(cost) — 不需要重建 Provider 实例,只刷 pricing map
	s.manager.ReloadPricing(toManagerConfigForReload(newCfg, s.pools))

	// Authenticator — P51: 重载时必须从 DB 重新加载,不能只用 config keys
	// 否则通过 API 添加的 key 会在 config 热重载后失效
	if s.auth != nil && newCfg.Auth.Enabled {
		dbKeys, err := auth.LoadFromDB(context.Background(), s.db)
		if err != nil {
			s.logger.Warn("reload keys from DB failed", zap.Error(err))
		} else {
			s.auth.Reload(dbKeys)
		}
	}

	s.logger.Info("config reloaded",
		zap.Int("aliases", len(newCfg.Routing.Aliases)),
		zap.Int("auth_keys", len(newCfg.Auth.Keys)),
	)
}

// ReloadProviderPool P35: 从 DB 重建指定 provider 的 Pool,注入到 Provider
// providerName 为空时全量重建
// ProviderKeysHandler.Create/Delete 后会调这个
func (s *Server) ReloadProviderPool(providerName string) {
	sched := keypool.NewScheduler(s.cfg.KeyPool.KeyRotation)
	poolCfg := keypool.Config{
		CoolingDuration: s.cfg.KeyPool.CoolingDuration,
		MaxCoolingCount: s.cfg.KeyPool.MaxCoolingCount,
	}
	store := auth.NewProviderKeyStore(s.db)
	ctx := context.Background()

	if providerName != "" {
		// 单个 provider 热更新
		pool := buildOnePool(ctx, providerName, sched, poolCfg, store, s.logger)
		s.pools[providerName] = pool
		// 注入到 Provider(让它下次请求用新 Pool)
		if pv, ok := s.manager.Get(providerName); ok {
			if setter, ok := pv.(interface{ SetPool(*keypool.Pool) }); ok {
				setter.SetPool(pool)
				s.logger.Info("provider pool reloaded", zap.String("provider", providerName), zap.Int("keys", pool.Size()))
			}
		}
		// P54: 同时更新 Router 持有的 pool 引用 — 不然 router 用旧 pool
		// (含已 disable 的 key),新加的 key 永远用不上
		if s.router != nil {
			s.router.SetPool(providerName, pool)
		}
		return
	}
	// 全量重建
	for name, p := range s.cfg.Providers {
		if !p.Enabled {
			continue
		}
		pool := buildOnePool(ctx, name, sched, poolCfg, store, s.logger)
		s.pools[name] = pool
		if pv, ok := s.manager.Get(name); ok {
			if setter, ok := pv.(interface{ SetPool(*keypool.Pool) }); ok {
				setter.SetPool(pool)
			}
		}
	}
	s.logger.Info("all provider pools reloaded", zap.Int("providers", len(s.pools)))
}