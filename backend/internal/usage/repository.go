// Package usage — Repository 实现用量的查询与聚合
package usage

import (
	"context"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// QueryFilter 查询过滤条件
type QueryFilter struct {
	StartTime    time.Time
	EndTime      time.Time
	ProviderName string
	ModelID      string
	GatewayKeyID string
	Limit        int
	Offset       int
}

// Repository 用量查询
type Repository struct {
	db *gorm.DB
}

// NewRepository 构造 Repository
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// RecordView 一条明细记录 + 按 P-token-split 规则拆好的输入侧两个数。
//
// 为什么后端算而不让前端算:同一条规则若在 SQL(聚合必需)和前端 TS(明细)
// 各写一份,必然漂移 —— 本项目历史上多次事故都是这个根因。规则单源在
// tokensplit.go,聚合与明细都从那里取,前端只负责显示。
//
// 用匿名嵌入而不是平铺所有字段:平铺意味着 UsageRecord 每加一列都要记得
// 在这里补一遍,漏了就是静默丢字段。嵌入让新列自动可见。
type RecordView struct {
	dbpkg.UsageRecord
	CachedInputTokens   int `json:"cached_input_tokens"`
	UncachedInputTokens int `json:"uncached_input_tokens"`
}

// Query 返回符合过滤条件的 usage 记录(含拆好的输入侧两个数)
func (r *Repository) Query(ctx context.Context, f QueryFilter) ([]RecordView, error) {
	q := r.db.WithContext(ctx).Model(&dbpkg.UsageRecord{})
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.ProviderName != "" {
		q = q.Where("provider_name = ?", f.ProviderName)
	}
	if f.ModelID != "" {
		q = q.Where("model_id = ?", f.ModelID)
	}
	if f.GatewayKeyID != "" {
		q = q.Where("gateway_key_id = ?", f.GatewayKeyID)
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	q = q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset)

	// P-token-split: 明细行也带上拆好的两个数,与聚合共用 tokensplit.go 的片段。
	// 必须显式列出 * —— 只写计算列会让 GORM 丢掉本体字段。
	var out []RecordView
	if err := q.Select(`*,
		` + SQLCachedInput + ` as cached_input_tokens,
		` + SQLUncachedInput + ` as uncached_input_tokens
	`).Scan(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Count P66: 统计符合过滤条件的记录总数(用于分页)
//
// 与 Query 共用同一 filter — 这里不复用 buildUsageWhere(那是私有 helper)
// 因为 Query 已经把 filter 散在 Where 里,这里也照搬一遍保持简单
// 后续可重构为共享 where builder
func (r *Repository) Count(ctx context.Context, f QueryFilter) (int64, error) {
	q := r.db.WithContext(ctx).Model(&dbpkg.UsageRecord{})
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.ProviderName != "" {
		q = q.Where("provider_name = ?", f.ProviderName)
	}
	if f.ModelID != "" {
		q = q.Where("model_id = ?", f.ModelID)
	}
	if f.GatewayKeyID != "" {
		q = q.Where("gateway_key_id = ?", f.GatewayKeyID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// AggregateResult 聚合结果
type AggregateResult struct {
	TotalRequests int64 `json:"total_requests"`
	// TotalInput P-token-split: 「**未缓存**输入」的总量,不是 SUM(input_tokens)。
	//
	// 裸 SUM(input_tokens) 在混口径的库上没有意义 —— 该列含义漂移过三次
	// (见 tokensplit.go),同一个 SUM 会把「含缓存的输入」和「已扣缓存的输入」
	// 加到一起。故这里按 SQLUncachedInput 逐行归一后再 SUM,
	// 与明细页「未缓存输入」列同一口径、可直接对账。
	TotalInput int64 `json:"total_input_tokens"`
	// TotalCachedInput 「缓存输入」总量。此前聚合层完全没有这个数,
	// 缓存量在按 model / 按 billing_source 两张聚合表里都不可见。
	TotalCachedInput int64   `json:"total_cached_input_tokens"`
	TotalOutput      int64   `json:"total_output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalCost        float64 `json:"total_cost"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	// TotalLatencyMs 时间窗内总耗时(SUM(latency_ms)),给前端算聚合 TPS 用。
	// 注意:TPS 聚合必须是「总 token ÷ 总耗时」,不能对每条 TPS 求平均 ——
	// 每条 avg 会因 token 大小差异失真。故这里单独累加出一个总耗时字段。
	TotalLatencyMs int64 `json:"total_latency_ms"`
	// AvgTtftMs 平均首字时间:只对 is_stream=true 且 ttft>0 的记录求平均,
	// 非流式 ttft=0 不参与,避免被 0 稀释。
	AvgTtftMs  float64 `json:"avg_ttft_ms"`
	ErrorCount int64   `json:"error_count"`
}

// Aggregate 按 Model 聚合(P65:去掉 provider 维度,只按 model_id)
//   - 前端按 model 归类卡片
//   - provider 信息由 Usage.vue 表格按需调用 ModelProviders 端点拉
func (r *Repository) Aggregate(ctx context.Context, f QueryFilter) ([]AggregateRow, error) {
	type row struct {
		ModelID      string
		Count        int64
		InputTokens  int64
		CachedTokens int64
		OutputTokens int64
		TotalTokens  int64
		Cost         float64
		AvgLatency   float64
		TotalLatency int64
		AvgTtft      float64
		ErrorCount   int64
	}

	q := r.db.WithContext(ctx).Model(&dbpkg.UsageRecord{})
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.GatewayKeyID != "" {
		q = q.Where("gateway_key_id = ?", f.GatewayKeyID)
	}

	var rows []row
	// P-token-split: input 侧按 tokensplit.go 的规则逐行归一后再 SUM。
	// 不能裸 SUM(input_tokens) —— 该列口径混着三个时代。
	err := q.Select(`
		model_id,
		COUNT(*) as count,
		COALESCE(SUM(` + SQLUncachedInput + `),0) as input_tokens,
		COALESCE(SUM(` + SQLCachedInput + `),0) as cached_tokens,
		COALESCE(SUM(output_tokens),0) as output_tokens,
		COALESCE(SUM(total_tokens),0) as total_tokens,
		COALESCE(SUM(cost),0) as cost,
		COALESCE(AVG(latency_ms),0) as avg_latency,
		COALESCE(SUM(latency_ms),0) as total_latency,
		COALESCE(AVG(CASE WHEN is_stream THEN ttft_ms END),0) as avg_ttft,
		COALESCE(SUM(CASE WHEN status_code >= 400 OR error_type != '' THEN 1 ELSE 0 END),0) as error_count
	`).Group("model_id").Order("count DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]AggregateRow, len(rows))
	for i, r := range rows {
		out[i] = AggregateRow{
			ModelID: r.ModelID,
			AggregateResult: AggregateResult{
				TotalRequests:    r.Count,
				TotalInput:       r.InputTokens,
				TotalCachedInput: r.CachedTokens,
				TotalOutput:      r.OutputTokens,
				TotalTokens:      r.TotalTokens,
				TotalCost:        r.Cost,
				AvgLatencyMs:     r.AvgLatency,
				TotalLatencyMs:   r.TotalLatency,
				AvgTtftMs:        r.AvgTtft,
				ErrorCount:       r.ErrorCount,
			},
		}
	}
	return out, nil
}

// AggregateRow P65: 一行聚合(只按 Model 维度)
//   - 去掉 ProviderName,因为 GROUP BY 不再按 provider 分组
//   - 前端按 model 归类卡片,Provider 信息走 ModelProviders 单独查
type AggregateRow struct {
	ModelID string `json:"model_id"`
	AggregateResult
}

// ModelProviderRow P65: 给定 model,列出哪些 provider 调用过(按请求数排序)
type ModelProviderRow struct {
	ProviderName string `json:"provider_name"`
	RequestCount int64  `json:"request_count"`
}

// ModelProviders 按 model 查 provider 分布
//   - Usage.vue 表格的 Provider 列 click/hover 时调用
//   - 返回该 model 在时间窗内被哪些 provider 调用 + 各 provider 的请求数
func (r *Repository) ModelProviders(ctx context.Context, f QueryFilter, modelID string) ([]ModelProviderRow, error) {
	var rows []ModelProviderRow
	q := r.db.WithContext(ctx).Model(&dbpkg.UsageRecord{}).
		Where("model_id = ?", modelID)
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.GatewayKeyID != "" {
		q = q.Where("gateway_key_id = ?", f.GatewayKeyID)
	}
	err := q.Select("provider_name, COUNT(*) as request_count").
		Group("provider_name").Order("request_count DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// BillingSourceRow P47: 按计费来源聚合
type BillingSourceRow struct {
	BillingSource string `json:"billing_source"`
	AggregateResult
}

// AggregateByBillingSource P47: 按 billing_source 聚合
// 返回每种计费来源的请求数 / token / cost,用于 dashboard
func (r *Repository) AggregateByBillingSource(ctx context.Context, f QueryFilter) ([]BillingSourceRow, error) {
	type row struct {
		BillingSource string
		Count         int64
		InputTokens   int64
		CachedTokens  int64
		OutputTokens  int64
		TotalTokens   int64
		Cost          float64
		AvgLatency    float64
		TotalLatency  int64
		AvgTtft       float64
		ErrorCount    int64
	}

	q := r.db.WithContext(ctx).Model(&dbpkg.UsageRecord{})
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.GatewayKeyID != "" {
		q = q.Where("gateway_key_id = ?", f.GatewayKeyID)
	}

	var rows []row
	// P-token-split: 与 Aggregate 同一片段(单源在 tokensplit.go),口径必须一致 ——
	// 两张聚合表在 dashboard 上并列,一边归一一边不归一会对不上账。
	err := q.Select(`
		billing_source,
		COUNT(*) as count,
		COALESCE(SUM(` + SQLUncachedInput + `),0) as input_tokens,
		COALESCE(SUM(` + SQLCachedInput + `),0) as cached_tokens,
		COALESCE(SUM(output_tokens),0) as output_tokens,
		COALESCE(SUM(total_tokens),0) as total_tokens,
		COALESCE(SUM(cost),0) as cost,
		COALESCE(AVG(latency_ms),0) as avg_latency,
		COALESCE(SUM(latency_ms),0) as total_latency,
		COALESCE(AVG(CASE WHEN is_stream THEN ttft_ms END),0) as avg_ttft,
		COALESCE(SUM(CASE WHEN status_code >= 400 OR error_type != '' THEN 1 ELSE 0 END),0) as error_count
	`).Group("billing_source").Order("count DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]BillingSourceRow, len(rows))
	for i, r := range rows {
		out[i] = BillingSourceRow{
			BillingSource: r.BillingSource,
			AggregateResult: AggregateResult{
				TotalRequests:    r.Count,
				TotalInput:       r.InputTokens,
				TotalCachedInput: r.CachedTokens,
				TotalOutput:      r.OutputTokens,
				TotalTokens:      r.TotalTokens,
				TotalCost:        r.Cost,
				AvgLatencyMs:     r.AvgLatency,
				TotalLatencyMs:   r.TotalLatency,
				AvgTtftMs:        r.AvgTtft,
				ErrorCount:       r.ErrorCount,
			},
		}
	}
	return out, nil
}
