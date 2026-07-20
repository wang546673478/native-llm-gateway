// Package main 是 Gateway 的入口
// 负责:
//   1. 解析命令行参数(config 路径)
//   2. 加载配置
//   3. 初始化日志
//   4. 启动 HTTP 服务
//   5. 监听信号,触发优雅关停
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

	"github.com/wang546673478/native-llm-gateway/internal/config"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, logger)
	return srv.Run(ctx)
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
