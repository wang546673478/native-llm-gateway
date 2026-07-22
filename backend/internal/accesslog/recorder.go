package accesslog

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// RecorderConfig 顶层配置
//
// 注:F6 决议用 time.Duration 直读。yaml 用 string 如 "24h";mapstructure
// 会自动 "24h" → time.Duration(与项目其他字段如 Retry.OpenTimeout 同套路)
// 如发现项目用别的方案,请贴本地 loadConfig sample 后调整。
type RecorderConfig struct {
	Enabled       bool          // false → 整体 no-op
	BodyDir       string        // body 文件根目录
	BufferSize    int           // 通道容量
	BatchSize     int           // 批量 flush 行数
	FlushInterval time.Duration // 周期
	Retention     time.Duration // 默认 24h
}

// Recorder 是外部使用的轻量门面
type Recorder struct {
	cfg     RecorderConfig
	logger  *zap.Logger
	bf      *BodyFileWriter
	buf     *Buffer
	store   *Store
	reten   *Retention
	started bool
	mu      sync.Mutex
}

// NewRecorder 构造 Recorder(还不启动)
func NewRecorder(cfg RecorderConfig, db *gorm.DB, logger *zap.Logger) (*Recorder, error) {
	if !cfg.Enabled {
		return &Recorder{cfg: cfg}, nil // no-op recorder
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	bf, err := NewBodyFileWriter(cfg.BodyDir)
	if err != nil {
		return nil, err
	}
	store := NewStore(db)
	buf := NewBuffer(store, BufferConfig{
		Capacity:      cfg.BufferSize,
		BatchSize:     cfg.BatchSize,
		FlushInterval: cfg.FlushInterval, // 直接是 time.Duration
	})
	buf.SetLogger(logger)
	return &Recorder{
		cfg:    cfg,
		logger: logger,
		bf:     bf,
		buf:    buf,
		store:  store,
		reten:  NewRetention(store, bf, cfg.Retention, logger),
	}, nil
}

// Start 启动 worker + retention
func (r *Recorder) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || !r.cfg.Enabled {
		return
	}
	r.buf.Start(ctx)
	r.reten.Start(ctx)
	r.started = true
}

// Close 是 Recorder 的完整 facade shutdown —— 负责 flush buffer worker 并
// 停止 owned retention goroutine。Close 后不再有任何后台活动:
//
//   - r.buf.Close() flush 残余 + 退出 buffer worker
//   - r.reten.Close() cancel retention ctx 并等待其 goroutine 退出
//
// 注意:Close 不可并发调用。Buffer.Close() 内部 Get/Set 是 mutex 保护的,
// 但不是原子的;两次并发 Close 可能都越过 guard 然后对同一 channel close(b.ch)
// 触发 panic。Task 4 review 已明确记录这一点。
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	r.buf.Close()
	if r.reten != nil {
		r.reten.Close()
	}
	return nil
}

// RecordAsync 是热路径 API,zero-blocking
//   - body 文件同步写(失败也继续,只 metadata)
//   - metadata 异步 push 到 buffer
func (r *Recorder) RecordAsync(e *AccessEntry) {
	if !r.cfg.Enabled || e == nil {
		return
	}
	if e.TraceID == "" {
		return
	}
	// F15 决议:删掉那条 `select { case <-r.buf.ch: default: }` 死代码 —
	// 那是非阻塞 drain,既不检查 closed 也不通知,纯噪音。Push 内部已经处理
	// channel 满 → drop,主路径永不阻塞。
	r.buf.Push(e)
}

// WriteBody 同步写 body,返回相对路径 + 写入错误。
//
// canonical contract 与 BodyFileWriter.Write 一致:(string, error)。
// truncated 标记不通过返回值暴露,而是编码在 relPath 的文件名后缀
// (.truncated.json,见 bodyfile.go)。需要判断是否被截断时调用方应使用
// accesslog.IsTruncated(path)。
func (r *Recorder) WriteBody(traceID, kind string, data []byte) (string, error) {
	if !r.cfg.Enabled {
		return "", nil
	}
	path, err := r.bf.Write(traceID, kind, data)
	if err != nil {
		r.logger.Warn("accesslog body write failed",
			zap.String("trace_id", traceID),
			zap.String("kind", kind),
			zap.Error(err))
		return "", err
	}
	return path, nil
}

// ReadBody 暴露给 handler 用(读 body 文件)
func (r *Recorder) ReadBody(relPath string) ([]byte, error) {
	if relPath == "" {
		return nil, nil
	}
	return r.bf.Read(relPath)
}

// BodyFileRoot 给 handler 做权限检查(防止 ../ 越权)
func (r *Recorder) BodyFileRoot() string {
	return r.bf.RootDir()
}

// Store 暴露给 handler 用(查 DB)
func (r *Recorder) Store() *Store { return r.store }