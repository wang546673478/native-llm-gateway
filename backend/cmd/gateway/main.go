// Package main 是 Gateway 的入口
// 负责:
//  1. 解析命令行参数(config 路径)
//  2. 加载配置
//  3. 初始化日志
//  4. 打开数据库 + 迁移
//  5. 构建 Provider Manager 并加载所有 enabled Provider
//  6. 启动 HTTP 服务
//  7. 监听信号,触发优雅关停
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"

	_ "github.com/wang546673478/native-llm-gateway/internal/provider/builtin" // 触发所有内置 Provider init() 注册(deepseek/gemini/glm/mimo/minimax/qwen)

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

	// P9: Provider Pool 由 Server(buildKeyPools)从 DB 构造并 SetPool 注入 —
	// 这里 manager 不带预构造 pools(否则被 DB 路径覆盖,冗余)。
	// 2026-08-09 P30:所有 provider key 以 DB(provider_api_keys)为唯一权威。
	manager := provider.NewManager(registry, logger)
	if err := manager.LoadFromConfig(context.Background(), toManagerConfig(cfg)); err != nil {
		return fmt.Errorf("load providers: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(cfg, logger, db, manager)
	if err != nil {
		return fmt.Errorf("server new: %w", err)
	}

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
// P9: Provider Pool 不从这里传 —— Server(DB)构造后 SetPool 注入,这里
// ManagerConfig.Pools 留空(nil),由 Server.New 的 injectPools 填充。
func toManagerConfig(cfg *config.Config) *provider.ManagerConfig {
	mcfg := &provider.ManagerConfig{
		Providers: make(map[string]provider.ManagerProviderConfig, len(cfg.Providers)),
		Pools:     make(map[string]*keypool.Pool),
		// P-provider-timeout: 全局 provider 请求超时兜底(provider.timeout==0 时用)
		DefaultTimeout: cfg.Timeouts.ProviderDefault,
	}
	for name, p := range cfg.Providers {
		proto, _ := provider.ParseProtocol(p.Protocol) // config.validate() 已确保合法
		mcfg.Providers[name] = provider.ManagerProviderConfig{
			Enabled:  p.Enabled,
			Endpoint: p.Endpoint,
			Protocol: proto,
			Timeout:  p.Timeout,
			// P47: 计费来源,默认 api(没标就当 api)
			BillingSource: defaultStr(p.BillingSource, "api"),
			// P-catch-all: 默认模型(catch_all 自动模式用)
			DefaultModel: p.DefaultModel,
			// P-responses: Responses API 能力(/v1/responses 透传)
			ResponsesAPI: p.ResponsesAPI,
			// P-deepseek-thinking: 强制 thinking=disabled(DeepSeek /anthropic 校验)
			ForceThinkingDisabled: p.ForceThinkingDisabled,
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

// defaultStr 返回 s,如果 s 为空则返回 fallback
func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
