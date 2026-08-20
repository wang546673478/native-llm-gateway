package database

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// ProviderModelStore 访问 provider_models(厂商在售模型 + 定价)。
type ProviderModelStore interface {
	// ListByVendor 取某厂商的全部模型(按 sort_order 即上游顺序排,model_id 兜底)。
	ListByVendor(ctx context.Context, vendor string) ([]ProviderModel, error)
	// UpsertModels 用上游同步结果整体替换某厂商的模型清单:
	// 保留已有手工价格(按 model_id 匹配),新增模型价格 0、source=upstream;
	// 不再存在于上游列表的旧模型保留(不删除,避免误删手工价格)。
	// sort_order 记上游返回下标,默认模型据此选(见 ProviderModel.SortOrder)。
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
	// SavePricing 手工更新某厂商某模型的三档每百万价格。
	SavePricing(ctx context.Context, vendor, modelID string, input, cacheRead, output float64) error
	// All 列出全部分组后的厂商模型(供模型管理页一次性渲染)。
	All(ctx context.Context) ([]ProviderModel, error)
}

type gormProviderModelStore struct{ db *gorm.DB }

func NewProviderModelStore(db *gorm.DB) ProviderModelStore {
	return &gormProviderModelStore{db: db}
}

func (s *gormProviderModelStore) ListByVendor(ctx context.Context, vendor string) ([]ProviderModel, error) {
	var out []ProviderModel
	if err := s.db.WithContext(ctx).Where("vendor = ?", vendor).
		Order("sort_order ASC, model_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormProviderModelStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	// 读现有(带手工价),映射 model_id → 价格;再逐条 upsert。
	existing, err := s.ListByVendor(ctx, vendor)
	if err != nil {
		return err
	}
	keep := make(map[string]ProviderModel, len(existing))
	for _, m := range existing {
		keep[m.ModelID] = m
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, id := range modelIDs {
			if id == "" {
				continue
			}
			now := time.Now()
			assign := map[string]interface{}{
				"vendor": vendor,
				// sort_order 记上游返回下标 —— 上游把旗舰款排最前,默认模型据此选,
				// 不能用 model_id 字典序(会把 MiniMax-M3 排到 MiniMax-M2 之后)
				"sort_order": i,
				"model_id":   id,
				"source":     "upstream",
				"synced_at":  now,
			}
			// 如果已有手工价,保留(不覆盖价格字段)
			if prev, ok := keep[id]; ok {
				assign["cost_per_million_input"] = prev.CostPerMillionInput
				assign["cost_per_million_cache_read"] = prev.CostPerMillionCacheRead
				assign["cost_per_million_output"] = prev.CostPerMillionOutput
			}
			// FirstOrCreate:按 (vendor, model_id) 找;命中则 Assign 覆盖,未命中则插入。
			// Assign 只写业务字段,不含 ID → 主键由 GORM 自动分配,避免命中行带非零 ID 冲突。
			if err := tx.Where("vendor = ? AND model_id = ?", vendor, id).
				Assign(assign).
				FirstOrCreate(&ProviderModel{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *gormProviderModelStore) SavePricing(ctx context.Context, vendor, modelID string, input, cacheRead, output float64) error {
	return s.db.WithContext(ctx).Model(&ProviderModel{}).
		Where("vendor = ? AND model_id = ?", vendor, modelID).
		Updates(map[string]interface{}{
			"cost_per_million_input":      input,
			"cost_per_million_cache_read": cacheRead,
			"cost_per_million_output":     output,
			"source":                      "manual",
		}).Error
}

func (s *gormProviderModelStore) All(ctx context.Context) ([]ProviderModel, error) {
	var out []ProviderModel
	// 按 (vendor, sort_order) 排 —— manager.LoadModelsFromStore 取每个 vendor 的
	// 首行作默认模型,依赖这里保持上游顺序。model_id 仅作同 sort_order 的兜底 tie-break。
	if err := s.db.WithContext(ctx).Order("vendor ASC, sort_order ASC, model_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
