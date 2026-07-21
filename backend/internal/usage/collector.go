// Package usage 实现异步用量收集 + 批量落库
// 对应规格书 5.8 Usage Collector
package usage

import (
	"context"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// Record 单条用量记录(来自 Proxy.UsageRecorder)
type Record struct {
	TraceID       string
	GatewayKeyID  string
	ProviderName  string
	ModelID       string
	Protocol      string
	// P47: 计费来源(token_plan / api / free)
	BillingSource string
	InputTokens   int
	OutputTokens  int
	TotalTokens   int
	Cost          float64
	LatencyMs     int64
	IsStream      bool
	StatusCode    int
	ErrorType     string
}

// Collector 异步收集器
type Collector struct {
	db         *gorm.DB
	channel    chan *Record
	batchSize  int
	flushInt   time.Duration
	stopCh     chan struct{}
	doneCh     chan struct{}
	once       sync.Once
}

// NewCollector 构造 Collector
// batchSize: 累积多少条就 flush
// flushInterval: 多久强制 flush 一次
func NewCollector(db *gorm.DB, batchSize, flushIntervalMs int) *Collector {
	if batchSize <= 0 {
		batchSize = 100
	}
	if flushIntervalMs <= 0 {
		flushIntervalMs = 10_000 // 10s
	}
	return &Collector{
		db:        db,
		channel:   make(chan *Record, 1024),
		batchSize: batchSize,
		flushInt:  time.Duration(flushIntervalMs) * time.Millisecond,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Record 非阻塞记录一条用量;channel 满时丢弃(不阻塞 Proxy)
func (c *Collector) Record(r *Record) {
	select {
	case c.channel <- r:
	default:
		// channel 满,丢弃(打印一条 warn 便于排查)
		log.Printf("usage: channel full, dropped record (provider=%s, model=%s)", r.ProviderName, r.ModelID)
	}
}

// Start 启动后台批量写入协程
func (c *Collector) Start(ctx context.Context) {
	go c.run(ctx)
}

// run 后台循环:累积 batch,达到 batchSize 或 flushInterval 就写库
func (c *Collector) run(ctx context.Context) {
	defer close(c.doneCh)

	ticker := time.NewTicker(c.flushInt)
	defer ticker.Stop()

	batch := make([]*Record, 0, c.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// 退出前 flush 一次
			flush()
			return
		case <-c.stopCh:
			flush()
			return
		case r := <-c.channel:
			batch = append(batch, r)
			if len(batch) >= c.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Stop 停止后台 goroutine(flush 剩余记录)
func (c *Collector) Stop() {
	c.once.Do(func() {
		close(c.stopCh)
	})
	<-c.doneCh
}

// flush 把 batch 转换为 GORM 模型并批量写入
func (c *Collector) flush(batch []*Record) {
	models := make([]dbpkg.UsageRecord, len(batch))
	now := time.Now().UTC()
	for i, r := range batch {
		models[i] = dbpkg.UsageRecord{
			TraceID:       r.TraceID,
			GatewayKeyID:  r.GatewayKeyID,
			ProviderName:  r.ProviderName,
			ModelID:       r.ModelID,
			Protocol:      r.Protocol,
			BillingSource: r.BillingSource,
			InputTokens:   r.InputTokens,
			OutputTokens:  r.OutputTokens,
			TotalTokens:   r.TotalTokens,
			Cost:          r.Cost,
			LatencyMs:     int(r.LatencyMs),
			IsStream:      r.IsStream,
			StatusCode:    r.StatusCode,
			ErrorType:     r.ErrorType,
			CreatedAt:     now,
		}
	}
	if err := c.db.CreateInBatches(models, c.batchSize).Error; err != nil {
		log.Printf("usage: flush failed: %v", err)
	}
}

// Pending 返回 channel 中积压的待写入记录数(调试用)
func (c *Collector) Pending() int {
	return len(c.channel)
}
