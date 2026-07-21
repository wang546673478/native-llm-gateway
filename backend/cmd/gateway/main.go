// Package main 是 Gateway 的入口
// 负责:
//   1. 解析命令行参数(config 路径)
//   2. 加载配置
//   3. 初始化日志
//   4. 打开数据库 + 迁移
//   5. 构建 Provider Manager 并加载所有 enabled Provider
//   6. 启动 HTTP 服务
//   7. 监听信号,触发优雅关停
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"

	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"               // 触发 init() 注册
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek_anthropic"    // DeepSeek 的 Anthropic 兼容端点
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/glm"                   // 智谱
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/qwen"                  // 通义千问
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/kimi"                  // 月之暗面
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/minimax"               // Anthropic 兼容
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/minimax_openai"        // OpenAI 兼容
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/gemini"                // Google 协议

	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/server"
)

var (
	cfgPath string
	logJSON bool
)

func main() {
	root := &cobra.Command{
		Use:   "gateway",
		Short: "Native LLM Gateway — protocol-aware, pluggable LLM proxy",
		Long: `Native LLM Gateway is a protocol-aware transparent proxy that
sits between AI Agents (Claude Code, Codex, Cline, Continue) and
multiple LLM Providers. It handles multi-provider routing, API Key
pooling, usage metering, and automatic failover — without ever
rewriting request/response bodies.`,
		SilenceUsage: true,
		RunE:         run,
	}

	root.Flags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to config.yaml")
	root.Flags().BoolVar(&logJSON, "log-json", false, "force JSON log format (overrides config)")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := newLogger(cfg.Logging.Level, logJSON || cfg.Logging.Format == "json")
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck

	logger.Info("config loaded",
		zap.String("path", cfgPath),
		zap.Int("providers", len(cfg.Providers)),
		zap.Int("port", cfg.Server.Port),
	)

	// P1: 数据库 + 迁移
	db, err := database.Open(&cfg.Database)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("migrate db: %w", err)
	}
	logger.Info("database ready", zap.String("driver", cfg.Database.Driver))

	// P2: Provider Manager
	// 每个 Provider 包通过 init() 已注册到 provider.Default()
	registry := provider.Default()
	logger.Info("provider registry",
		zap.Strings("registered", registry.ListRegistered()),
	)

	// P9: 先构造 Pools,再喂给 Manager(让工厂能拿到 pool)
	pools := buildPools(cfg)
	for name, pool := range pools {
		logger.Info("keypool built",
			zap.String("provider", name),
			zap.Int("keys", pool.Size()))
	}

	manager := provider.NewManager(registry, logger)
	if err := manager.LoadFromConfig(context.Background(), toManagerConfig(cfg, pools)); err != nil {
		return fmt.Errorf("load providers: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, logger, db, manager)

	// P14: 配置热重载
	if err := config.Watch(ctx, cfgPath, logger, func(newCfg *config.Config) {
		srv.Reload(newCfg)
	}); err != nil {
		logger.Warn("config watch disabled", zap.Error(err))
	}

	if err := srv.Run(ctx); err != nil {
		return err
	}

	// 清理资源
	_ = manager.Close()
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	return nil
}

// toManagerConfig 把完整 cfg 投影成 Manager 关心的子集
// P9: 这里需要先把 pools 构造好,再传给 ManagerConfig.Pools
func toManagerConfig(cfg *config.Config, pools map[string]*keypool.Pool) *provider.ManagerConfig {
	mcfg := &provider.ManagerConfig{
		Providers: make(map[string]provider.ManagerProviderConfig, len(cfg.Providers)),
		Pools:     make(map[string]any, len(pools)),
	}
	for name, pool := range pools {
		mcfg.Pools[name] = pool
	}
	for name, p := range cfg.Providers {
		proto, _ := provider.ParseProtocol(p.Protocol) // config.validate() 已确保合法
		keys := make([]string, 0, len(p.Keys))
		for _, k := range p.Keys {
			keys = append(keys, k.Key)
		}
		models := make([]string, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, m.ID)
		}
		mcfg.Providers[name] = provider.ManagerProviderConfig{
			Enabled:  p.Enabled,
			Endpoint: p.Endpoint,
			Protocol: proto,
			Timeout:  p.Timeout,
			Models:   models,
			APIKeys:  keys,
			Circuit: provider.ManagerCircuitConfig{
				FailureThreshold: p.CircuitBreaker.FailureThreshold,
				FailureWindow:    p.CircuitBreaker.FailureWindow,
				OpenTimeout:      p.CircuitBreaker.OpenTimeout,
				HalfOpenRequests: p.CircuitBreaker.HalfOpenRequests,
			},
		}
	}
	return mcfg
}

// buildPools 提前构造 Pool map,用于注入到 Manager 和 Router
func buildPools(cfg *config.Config) map[string]*keypool.Pool {
	out := make(map[string]*keypool.Pool)
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		keys := make([]*keypool.Key, 0, len(p.Keys))
		now := time.Now().UTC()
		for i, k := range p.Keys {
			keys = append(keys, &keypool.Key{
				ID: fmt.Sprintf("%s-%d", name, i), ProviderName: name, Name: k.Name,
				Key: k.Key, Status: keypool.KeyStatusActive,
				CreatedAt: now, UpdatedAt: now,
			})
		}
		out[name] = keypool.NewPool(name, keys, sched, keypool.Config{
			CoolingDuration: cfg.KeyPool.CoolingDuration,
			MaxCoolingCount: cfg.KeyPool.MaxCoolingCount,
		})
	}
	return out
}

// newLogger 根据 level 构造 zap logger
func newLogger(level string, json bool) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", level, err)
	}

	var cfg zap.Config
	if json {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	return cfg.Build()
}

// silence unused warning when gorm import isn't used in early phases
var _ = gorm.ErrRecordNotFound
