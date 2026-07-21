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
	http     *http.Server
}

// New 构造 Server
func New(cfg *config.Config, logger *zap.Logger, db *gorm.DB, manager *provider.Manager) *Server {
	// P3+P4+P5: 构造 KeyPool map + Router + Proxy
	// P30:从 DB (provider_api_keys 表) 读 key 而不是 config.yaml
	pools := buildKeyPools(cfg, db, logger)
	r := router.NewRouter(logger, manager, pools, router.Config{
		Aliases:         toRouterAliases(cfg.Routing.Aliases),
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

	eng := proxy.NewEngine(proxy.Config{
		Router:        r,
		Logger:        logger,
		Usage:         usage.NewAdapter(usageC),
		Metrics:       metrics.NewAdapter(metricsC),
		Breaker:       reporter,
		TokenRecorder: newAuthTokenRecorder(authn), // P13: TPM 计数(若 auth 启用)
		Authenticator: authn,                        // P19: Provider 绑定检查
		MaxRetry:      cfg.Retry.MaxAttempts,
	})
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
	ctx := context.Background()
	// 对每个 enabled provider,从 DB 读它的明文 key,构造 Pool
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		store := auth.NewProviderKeyStore(db)
		plainKeys, err := store.GetPlainKeys(ctx, name)
		if err != nil {
			logger.Warn("read provider keys from DB failed",
				zap.String("provider", name),
				zap.Error(err))
			continue
		}
		if len(plainKeys) == 0 {
			// 没 key 也不报错,让 provider 还是 loaded;调它会返回 'no available key'
			out[name] = keypool.NewPool(name, nil, sched, poolCfg)
			logger.Warn("provider has no API keys in DB",
				zap.String("provider", name))
			continue
		}
		out[name] = keypool.BuildPoolFromStrings(name, plainKeys, poolCfg)
		// BuildPoolFromStrings 用了 nil scheduler,改用带 scheduler 的 NewPool
		keys := make([]*keypool.Key, 0, len(plainKeys))
		for i, pk := range plainKeys {
			keys = append(keys, &keypool.Key{
				ID:           fmt.Sprintf("%s-key-%d", name, i+1),
				ProviderName: name,
				Name:         fmt.Sprintf("key-%d", i+1),
				Key:          pk,
				Status:       keypool.KeyStatusActive,
				CreatedAt:    time.Now().UTC(),
				UpdatedAt:    time.Now().UTC(),
			})
		}
		out[name] = keypool.NewPool(name, keys, sched, poolCfg)
		logger.Info("keypool built from DB",
			zap.String("provider", name),
			zap.Int("keys", len(plainKeys)),
			zap.String("rotation", cfg.KeyPool.KeyRotation),
		)
	}
	return out
}

// toRouterAliases 把 config 风格别名表转 router 风格
func toRouterAliases(in map[string]config.AliasRule) map[string]router.AliasConfig {
	out := make(map[string]router.AliasConfig, len(in))
	for alias, rule := range in {
		ps := make([]router.ProviderRoute, 0, len(rule.Providers))
		for _, p := range rule.Providers {
			ps = append(ps, router.ProviderRoute{
				Name: p.Name, Model: p.Model, Priority: p.Priority, Weight: p.Weight,
			})
		}
		out[alias] = router.AliasConfig{
			Alias:     alias,
			Strategy:  rule.Strategy,
			Providers: ps,
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
		Aliases:  toRouterAliases(s.cfg.Routing.Aliases),
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
	pkHandler := auth.NewProviderKeysHandler(s.db)
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

// Reload 热重载 — 替换 Aliases 和 Auth Keys
// 注意:Provider Manager / KeyPool / Circuit Breaker 不在此函数内重载
// (它们的 Reload 已在 Manager 上,但涉及 HTTP 客户端重建,留给后续阶段)
func (s *Server) Reload(newCfg *config.Config) {
	if newCfg == nil {
		return
	}
	// Router aliases
	s.router.ReloadAliases(toRouterAliases(newCfg.Routing.Aliases))

	// Authenticator
	if s.auth != nil && newCfg.Auth.Enabled {
		gkKeys := make([]auth.GatewayKey, 0, len(newCfg.Auth.Keys))
		for _, k := range newCfg.Auth.Keys {
			gkKeys = append(gkKeys, auth.GatewayKey{
				Name:          k.Name,
				KeyHash:       k.Key,
				AllowedModels: k.AllowedModels,
				RateLimit:     auth.RateLimitConfig{RPM: k.RateLimit.RPM, TPM: k.RateLimit.TPM},
			})
		}
		s.auth.Reload(gkKeys)
	}

	s.logger.Info("config reloaded",
		zap.Int("aliases", len(newCfg.Routing.Aliases)),
		zap.Int("auth_keys", len(newCfg.Auth.Keys)),
	)
}