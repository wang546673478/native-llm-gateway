package accesslog

import (
	"context"
	"time"

	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// QueryFilter 列表/计数共用过滤条件
//   - StatusMin/StatusMax 提供方便的 status_code 范围过滤(status=ok → max<400)
//   - 字符串字段精确匹配
type QueryFilter struct {
	StartTime    time.Time
	EndTime      time.Time
	GatewayKey   string
	ProviderName string
	ModelID      string
	ErrorType    string
	StatusMin    int
	StatusMax    int
	Limit        int
	Offset       int
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

// GroupByCount 按指定列分组,返回每个不同值的出现次数
//   - column 必须是 dbpkg.AccessLog 上存在的列名(用白名单校验)
//   - 用于 handler 统计(例如按 gateway_key_name 分组计数)
//   - 返回 map[column_value]count,顺序不保证
func (s *Store) GroupByCount(ctx context.Context, f QueryFilter, column string) (map[string]int64, error) {
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
		return nil, gorm.ErrInvalidField
	}

	q := s.buildWhere(s.db.WithContext(ctx).Model(&dbpkg.AccessLog{}), f)
	var rows []struct {
		Val   string
		Count int64
	}
	if err := q.Select(column + " AS val, COUNT(*) AS count").
		Group("val").
		Order("count DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Val] = r.Count
	}
	return out, nil
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
	if f.StatusMin > 0 {
		q = q.Where("status_code >= ?", f.StatusMin)
	}
	if f.StatusMax > 0 {
		q = q.Where("status_code < ?", f.StatusMax)
	}
	return q
}