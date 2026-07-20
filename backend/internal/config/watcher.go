// Package config — 热重载监听
package config

import (
	"context"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// OnChangeFunc 配置变更回调
type OnChangeFunc func(newCfg *Config)

// Watch 用 viper 监听配置文件,变更时调用 fn
// fn 不应阻塞
func Watch(ctx context.Context, path string, logger *zap.Logger, fn OnChangeFunc) error {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return err
	}

	v.WatchConfig()
	v.OnConfigChange(func(e fsnotify.Event) {
		if e.Op&(fsnotify.Write|fsnotify.Create) == 0 {
			return
		}
		logger.Info("config file changed", zap.String("path", e.Name), zap.String("op", e.Op.String()))

		newCfg, err := Load(path)
		if err != nil {
			logger.Warn("reload config failed, keeping old", zap.Error(err))
			return
		}
		fn(newCfg)
	})

	// 后台 goroutine 持续运行;ctx cancel 关闭
	go func() {
		<-ctx.Done()
	}()
	return nil
}
