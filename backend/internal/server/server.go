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

	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
)

// Server 持有所有运行时依赖
// 阶段 P0: 只持有 config 和 logger
// 阶段 P1: + db
// 阶段 P2+: + providerManager, router, keypools, circuitBreakers, ...
type Server struct {
	cfg    *config.Config
	logger *zap.Logger
	db     *gorm.DB
	http   *http.Server
}

// New 构造 Server
func New(cfg *config.Config, logger *zap.Logger, db *gorm.DB) *Server {
	return &Server{
		cfg:    cfg,
		logger: logger,
		db:     db,
	}
}

// Run 启动 HTTP 服务,直到收到 shutdown 信号或 server 异常退出
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

// shutdown 优雅关闭,等待 in-flight 请求完成
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
// P0 阶段只暴露健康检查和占位的 v1 代理入口
func (s *Server) registerRoutes(r *gin.Engine) {
	// /healthz — 进程存活,不做任何外部依赖检查
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"version": "0.2.0-p1",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	// /readyz — P1 新增:依赖健康(目前是 DB),用于 k8s readinessProbe
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

	// P5 之前,/v1/* 的代理入口只返回 501 Not Implemented
	v1 := r.Group("/v1")
	v1.Any("/*path", func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error": gin.H{
				"type":    "not_implemented",
				"message": "proxy engine is not wired yet (phase P1 has DB only)",
			},
		})
	})
}

// silence unused for early-phase imports
var _ = database.Provider{}
