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

	"github.com/wang546673478/native-llm-gateway/internal/api/http/middleware"
	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/circuit"
	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/proxy"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// Server 持有所有运行时依赖
type Server struct {
	cfg     *config.Config
	logger  *zap.Logger
	db      *gorm.DB
	manager *provider.Manager
	router  *router.Router
	engine  *proxy.Engine
	pools   map[string]*keypool.Pool
	cm      *circuit.Manager
	auth    *auth.Authenticator
	http    *http.Server
}

// New 构造 Server
func New(cfg *config.Config, logger *zap.Logger, db *gorm.DB, manager *provider.Manager) *Server {
	// P3+P4+P5: 构造 KeyPool map + Router + Proxy
	pools := buildKeyPools(cfg, logger)
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

	// P7: Authenticator
	var authn *auth.Authenticator
	if cfg.Auth.Enabled {
		gkKeys := make([]auth.GatewayKey, 0, len(cfg.Auth.Keys))
		for i, k := range cfg.Auth.Keys {
			gkKeys = append(gkKeys, auth.GatewayKey{
				Name:          k.Name,
				KeyHash:       k.Key, // 注:P7 简化,直接用明文比对;生产应该 hash
				AllowedModels: k.AllowedModels,
				RateLimit:     auth.RateLimitConfig{RPM: k.RateLimit.RPM, TPM: k.RateLimit.TPM},
			})
			_ = i
		}
		authn = auth.New(gkKeys)
		logger.Info("auth enabled", zap.Int("keys", len(gkKeys)))
	}

	eng := proxy.NewEngine(proxy.Config{
		Router:   r,
		Logger:   logger,
		Usage:    proxy.NoopUsageRecorder{},
		Metrics:  proxy.NoopMetricsRecorder{},
		Breaker:  reporter,
		MaxRetry: cfg.Retry.MaxAttempts,
	})
	return &Server{
		cfg:     cfg,
		logger:  logger,
		db:      db,
		manager: manager,
		router:  r,
		engine:  eng,
		pools:   pools,
		cm:      cm,
		auth:    authn,
	}
}

// buildKeyPools 为每个 enabled Provider 构造一个 KeyPool
func buildKeyPools(cfg *config.Config, logger *zap.Logger) map[string]*keypool.Pool {
	out := make(map[string]*keypool.Pool)
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		keys := make([]*keypool.Key, 0, len(p.Keys))
		for i, k := range p.Keys {
			now := time.Now().UTC()
			keys = append(keys, &keypool.Key{
				ID:           fmt.Sprintf("%s-%d", name, i),
				ProviderName: name,
				Name:         k.Name,
				Key:          k.Key,
				Status:       keypool.KeyStatusActive,
				CreatedAt:    now,
				UpdatedAt:    now,
			})
		}
		pool := keypool.NewPool(name, keys, sched, keypool.Config{
			CoolingDuration: cfg.KeyPool.CoolingDuration,
			MaxCoolingCount: cfg.KeyPool.MaxCoolingCount,
		})
		out[name] = pool
		logger.Info("keypool built",
			zap.String("provider", name),
			zap.Int("keys", len(keys)),
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
