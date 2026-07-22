package accesslog

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// StatusBucket 是 status 过滤的单个原子条件(spec F9 决议)。
//
// 一个 bucket 要么按 status_code 区间(Min/Max 至少一个非零),要么按 error_type
// 精确匹配(ErrorType 非空)。两种语义互斥 — handler 端构造,store 端 OR 拼装。
//
// 示例:status=4xx,auth_failed →
//   {Min:400, Max:500} OR {ErrorType:"auth_failed"}
type StatusBucket struct {
	Min       int    // status_code >= Min(0 表示无下界)
	Max       int    // status_code <  Max(0 表示无上界)
	ErrorType string // 精确匹配 error_type;非空时忽略 Min/Max
}

// QueryFilter 列表/计数共用过滤条件
//   - StatusMin/StatusMax 提供方便的 status_code 范围过滤(向后兼容的单区间)
//   - StatusBuckets 提供 F9 多 bucket OR:F9 调用时优先使用
//   - 字符串字段精确匹配
type QueryFilter struct {
	StartTime     time.Time
	EndTime       time.Time
	GatewayKey    string
	ProviderName  string
	ModelID       string
	ErrorType     string
	StatusMin     int
	StatusMax     int
	StatusBuckets []StatusBucket // F9:status= 多值时设置,OR 拼接
	Limit         int
	Offset        int
}

// Store AccessLog 的 DB 读写
type Store struct {
	db *gorm.DB
}

// NewStore 构造 Store
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Insert 插入一条记录
func (s *Store) Insert(ctx context.Context, e *AccessEntry) error {
	row := toRow(e)
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return err
	}
	e.ID = row.ID
	return nil
}

// toRow / fromRow 在 AccessEntry (业务结构) 和 dbpkg.AccessLog (GORM) 之间转换
// 字段一一对应;保留两份 struct 是为了让 DB 模型和业务 API 解耦
func toRow(e *AccessEntry) *dbpkg.AccessLog {
	return &dbpkg.AccessLog{
		TraceID:        e.TraceID,
		CreatedAt:      e.CreatedAt,
		GatewayKeyID:   e.GatewayKeyID,
		GatewayKeyName: e.GatewayKeyName,
		Method:         e.Method,
		Path:           e.Path,
		ClientIP:       e.ClientIP,
		UserAgent:      e.UserAgent,
		RequestedModel: e.RequestedModel,
		FinalModel:     e.FinalModel,
		ProviderName:   e.ProviderName,
		Protocol:       e.Protocol,
		IsStream:       e.IsStream,
		StatusCode:     e.StatusCode,
		ErrorType:      e.ErrorType,
		LatencyMs:      e.LatencyMs,
		ReqBodyPath:    e.ReqBodyPath,
		ReqBodySize:    e.ReqBodySize,
		RespBodyPath:   e.RespBodyPath,
		RespBodySize:   e.RespBodySize,
		// truncated marker 写到 filename 后缀,DB 列已移除(F1)
	}
}

func fromRow(r *dbpkg.AccessLog) *AccessEntry {
	return &AccessEntry{
		ID:             r.ID,
		TraceID:        r.TraceID,
		CreatedAt:      r.CreatedAt,
		GatewayKeyID:   r.GatewayKeyID,
		GatewayKeyName: r.GatewayKeyName,
		Method:         r.Method,
		Path:           r.Path,
		ClientIP:       r.ClientIP,
		UserAgent:      r.UserAgent,
		RequestedModel: r.RequestedModel,
		FinalModel:     r.FinalModel,
		ProviderName:   r.ProviderName,
		Protocol:       r.Protocol,
		IsStream:       r.IsStream,
		StatusCode:     r.StatusCode,
		ErrorType:      r.ErrorType,
		LatencyMs:      r.LatencyMs,
		ReqBodyPath:    r.ReqBodyPath,
		ReqBodySize:    r.ReqBodySize,
		RespBodyPath:   r.RespBodyPath,
		RespBodySize:   r.RespBodySize,
		// truncated marker 写到 filename 后缀,DB 列已移除(F1)
	}
}

// List 按 filter 查询,默认按 created_at DESC
func (s *Store) List(ctx context.Context, f QueryFilter) ([]*AccessEntry, error) {
	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	q = q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset)

	var rows []dbpkg.AccessLog
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*AccessEntry, len(rows))
	for i := range rows {
		out[i] = fromRow(&rows[i])
	}
	return out, nil
}

// Count 统计符合条件记录数
func (s *Store) Count(ctx context.Context, f QueryFilter) (int64, error) {
	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// GetByID 单条查询(详情页用)
func (s *Store) GetByID(ctx context.Context, id uint) (*AccessEntry, error) {
	var row dbpkg.AccessLog
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	return fromRow(&row), nil
}

// DeleteOlderThan 删除 created_at < cutoff 的记录,返回删除数
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&dbpkg.AccessLog{})
	return res.RowsAffected, res.Error
}

// DeleteByIDs 仅删除指定主键集合的记录,返回删除数。
//
// 用于 retention 等需要 page-aligned 删除的场景:先取一页 ID,删它们的
// body 文件,再用本方法删对应的 DB 行 — 避免 List 取 1000 行但
// DeleteOlderThan 把所有 expired 行都删了,导致超出首页的 body 文件被
// 永久 orphan。
func (s *Store) DeleteByIDs(ctx context.Context, ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&dbpkg.AccessLog{})
	return res.RowsAffected, res.Error
}

// GroupByCount 返回指定列在 filter 范围内的 distinct 值数量(0 if filter 无匹配)。
//
// F14 决议:admin "active_keys" 需要真正 distinct 的 gateway key 数,不能用
// COUNT(*) 替代 — 不同 key 可能调用 N 次,但只算 1 个 active key。
//
//   - column 走白名单(防止 SQL 注入),只支持 AccessLog 上已有列名
//   - 复用 buildWhere 的过滤(时间/状态/错误类型等)
func (s *Store) GroupByCount(ctx context.Context, f QueryFilter, column string) (int64, error) {
	// 白名单 — 防止 SQL 注入 & 拼写错误
	allowed := map[string]bool{
		"trace_id":         true,
		"gateway_key_id":   true,
		"gateway_key_name": true,
		"method":           true,
		"path":             true,
		"requested_model":  true,
		"final_model":      true,
		"provider_name":    true,
		"protocol":         true,
		"error_type":       true,
	}
	if !allowed[column] {
		return 0, gorm.ErrInvalidField
	}

	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	var n int64
	if err := q.Select("COUNT(DISTINCT " + column + ")").Scan(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// buildWhere 是 List/Count 共用的 where 构造器
func (s *Store) buildWhere(q *gorm.DB, f QueryFilter) *gorm.DB {
	if !f.StartTime.IsZero() {
		q = q.Where("created_at >= ?", f.StartTime)
	}
	if !f.EndTime.IsZero() {
		q = q.Where("created_at <= ?", f.EndTime)
	}
	if f.GatewayKey != "" {
		q = q.Where("gateway_key_name = ?", f.GatewayKey)
	}
	if f.ProviderName != "" {
		q = q.Where("provider_name = ?", f.ProviderName)
	}
	if f.ModelID != "" {
		q = q.Where("(requested_model = ? OR final_model = ?)", f.ModelID, f.ModelID)
	}
	if f.ErrorType != "" {
		q = q.Where("error_type = ?", f.ErrorType)
	}
	// F9 多 bucket 优先:handler 设置了 StatusBuckets,这里 OR 拼接所有 bucket
	// 每个 bucket 是 (status range) 或 (error_type),互斥
	if len(f.StatusBuckets) > 0 {
		q = q.Where(statusBucketClause(f.StatusBuckets), statusBucketArgs(f.StatusBuckets)...)
		return q
	}
	// 向后兼容:单区间 status 过滤(老的 usage 测试 / retention 等)
	if f.StatusMin > 0 {
		q = q.Where("status_code >= ?", f.StatusMin)
	}
	if f.StatusMax > 0 {
		q = q.Where("status_code < ?", f.StatusMax)
	}
	return q
}

// statusBucketClause 与 statusBucketArgs 配套,把 []StatusBucket 转成
// "(bucket1) OR (bucket2) ..." + args slice,供 gorm Where 拼接。
//
// 设计目标:不要手工拼字符串插入值(column whitelist 已保证安全);
// 用占位符 ? + gorm 绑定,避免任何形式的注入风险。
func statusBucketClause(buckets []StatusBucket) string {
	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if b.ErrorType != "" {
			parts = append(parts, "error_type = ?")
			continue
		}
		conds := make([]string, 0, 2)
		if b.Min > 0 {
			conds = append(conds, "status_code >= ?")
		}
		if b.Max > 0 {
			conds = append(conds, "status_code < ?")
		}
		switch len(conds) {
		case 0:
			// 空 bucket,跳过(no-op)
		case 1:
			parts = append(parts, conds[0])
		default:
			parts = append(parts, "("+strings.Join(conds, " AND ")+")")
		}
	}
	return strings.Join(parts, " OR ")
}

func statusBucketArgs(buckets []StatusBucket) []any {
	out := make([]any, 0, len(buckets)*2)
	for _, b := range buckets {
		if b.ErrorType != "" {
			out = append(out, b.ErrorType)
			continue
		}
		if b.Min > 0 {
			out = append(out, b.Min)
		}
		if b.Max > 0 {
			out = append(out, b.Max)
		}
	}
	return out
}