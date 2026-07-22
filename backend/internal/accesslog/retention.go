package accesslog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Retention 删除过期 access_logs + body 文件
type Retention struct {
	store    *Store
	bf       *BodyFileWriter
	retent   time.Duration
	interval time.Duration
	logger   *zap.Logger

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewRetention 构造 Retention
func NewRetention(store *Store, bf *BodyFileWriter, retent time.Duration, logger *zap.Logger) *Retention {
	if retent <= 0 {
		retent = 24 * time.Hour
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Retention{
		store:    store,
		bf:       bf,
		retent:   retent,
		interval: 5 * time.Minute,
		logger:   logger,
	}
}

// Start 跑 goroutine,每 5 分钟扫一次
func (r *Retention) Start(ctx context.Context) {
	bgCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go r.run(bgCtx)
}

// Close 停止
func (r *Retention) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
}

func (r *Retention) run(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// 启动后立刻跑一次
	r.runOnce(ctx)

	for {
		select {
		case <-ticker.C:
			r.runOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (r *Retention) runOnce(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-r.retent)

	// 先取待删行(单轮 max 1000)
	rows, err := r.store.List(ctx, QueryFilter{
		EndTime: cutoff,
		Limit:   1000,
	})
	if err != nil {
		r.logger.Warn("retention list failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		// 即便没记录,也调 DeleteOlderThan 清理残余
		_, _ = r.store.DeleteOlderThan(ctx, cutoff)
		return
	}

	// 删 body 文件
	for _, e := range rows {
		if e.ReqBodyPath != "" {
			_ = os.Remove(filepath.Join(r.bf.RootDir(), e.ReqBodyPath))
		}
		if e.RespBodyPath != "" {
			_ = os.Remove(filepath.Join(r.bf.RootDir(), e.RespBodyPath))
		}
	}

	// 删 DB 行
	if _, err := r.store.DeleteOlderThan(ctx, cutoff); err != nil {
		r.logger.Warn("retention delete failed", zap.Error(err))
	}
}