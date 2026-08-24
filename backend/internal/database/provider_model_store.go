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
	// CountVendorModels 查询该厂商已有的模型数量(用于检查手动添加的模型)。
	CountVendorModels(ctx context.Context, vendor string) (int, error)

	// --- P-model-face: 面→模型归属(provider_model_faces 表) ---

	// ReplaceFaceModels 整体替换某**面**的模型归属(先删该面全部旧行再按序插入),
	// sort_order 记该面 ListModels 的返回下标。只在该面 ListModels 成功时调用 ——
	// 失败/NotSupported 时不动已有归属,避免上游临时抖动清空归属。
	ReplaceFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error
	// AddFaceModels 增量添加某**面**的模型归属(只添加新模型,不删除已有模型)。
	// 用于中转站场景:同一 API key 可能分配了不同分组(GPT vs Claude),每次调用只返回当前分组的模型,
	// 多次同步会互相覆盖。增量模式让所有分组的模型都保留。
	AddFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error
	// AllFaces 列出全部面归属行(按 vendor/face/sort_order 有序)。
	AllFaces(ctx context.Context) ([]ProviderModelFace, error)
	// PruneOrphanModels 删除该 vendor 下「在任何面都无归属」的 provider_models 行,
	// 返回删除行数。用于清理上游下架/换 channel 后残留的模型(手工触发,不在同步里自动跑)。
	PruneOrphanModels(ctx context.Context, vendor string) (int64, error)
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

func (s *gormProviderModelStore) CountVendorModels(ctx context.Context, vendor string) (int, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&ProviderModel{}).Where("vendor = ?", vendor).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

// ReplaceFaceModels 整体替换某面的归属:事务内先删该面旧行,再按 modelIDs 顺序插入。
// 整体替换(而非 upsert)才能让上游下架的模型从该面消失 —— 归属是可再生数据
// (每次同步重新拉),没有手工内容需要保护;定价在 provider_models 里,不受影响。
func (s *gormProviderModelStore) ReplaceFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("face = ?", face).Delete(&ProviderModelFace{}).Error; err != nil {
			return err
		}
		rows := make([]ProviderModelFace, 0, len(modelIDs))
		for i, id := range modelIDs {
			if id == "" {
				continue
			}
			rows = append(rows, ProviderModelFace{
				Vendor:    vendor,
				Face:      face,
				ModelID:   id,
				SortOrder: i,
				SyncedAt:  &now,
			})
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
}

// AddFaceModels 增量添加某面的模型归属:只添加新模型,不删除已有模型。
// P-relay-independent: 用于中转站场景 — 同一 API key 可能分配了不同分组(GPT vs Claude),
// 每次 ListModels 只返回当前分组的模型。全量替换会让不同分组互相覆盖,增量模式让所有分组的模型都保留。
// sort_order 从已有最大值+1 开始递增,保证新增模型排在已有模型之后。
func (s *gormProviderModelStore) AddFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error {
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 查询该面已有的模型 ID 和最大 sort_order
		var existing []ProviderModelFace
		if err := tx.Where("face = ?", face).Find(&existing).Error; err != nil {
			return err
		}
		existingMap := make(map[string]bool, len(existing))
		maxOrder := -1
		for _, row := range existing {
			existingMap[row.ModelID] = true
			if row.SortOrder > maxOrder {
				maxOrder = row.SortOrder
			}
		}

		// 只添加新模型(已有的跳过)
		rows := make([]ProviderModelFace, 0, len(modelIDs))
		nextOrder := maxOrder + 1
		for _, id := range modelIDs {
			if id == "" || existingMap[id] {
				continue
			}
			rows = append(rows, ProviderModelFace{
				Vendor:    vendor,
				Face:      face,
				ModelID:   id,
				SortOrder: nextOrder,
				SyncedAt:  &now,
			})
			nextOrder++
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
}

func (s *gormProviderModelStore) AllFaces(ctx context.Context) ([]ProviderModelFace, error) {
	var out []ProviderModelFace
	// 按 (vendor, face, sort_order) 排 —— manager 取每个面的首行作该面默认模型,
	// 依赖这里保持该面上游返回顺序(与 All 的 sort_order 契约同构)。
	if err := s.db.WithContext(ctx).
		Order("vendor ASC, face ASC, sort_order ASC, model_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// PruneOrphanModels 删除该 vendor 下无任何面归属的模型行。
// 注意:会连带删掉这些模型的手工价格 —— 它们已不在任何面的上游清单里,
// 留着也无法路由。手工触发(模型管理页「清理无归属」),不在同步流程里自动跑。
//
// 安全前提:该 vendor 一条归属行都没有时**直接返回 0 不删**。此时 vendor 处于
// fallback 模式(尚未同步过,或所有面都不支持模型列表),所有模型都隐式可用 ——
// 若照常执行 `NOT IN (空集)` 会把该 vendor 的模型全删光。
func (s *gormProviderModelStore) PruneOrphanModels(ctx context.Context, vendor string) (int64, error) {
	var faceRows int64
	if err := s.db.WithContext(ctx).Model(&ProviderModelFace{}).
		Where("vendor = ?", vendor).Count(&faceRows).Error; err != nil {
		return 0, err
	}
	if faceRows == 0 {
		return 0, nil
	}
	res := s.db.WithContext(ctx).
		Where("vendor = ?", vendor).
		Where("model_id NOT IN (?)",
			s.db.Model(&ProviderModelFace{}).Select("model_id").Where("vendor = ?", vendor)).
		Delete(&ProviderModel{})
	return res.RowsAffected, res.Error
}
