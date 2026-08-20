// Package provider — 模型从上游同步
// 对应 Task 5:调 ListModels 拉上游在售模型 → upsert 到 Store
package provider

import (
	"context"
	"fmt"
	"sort"
)

// ModelSyncStore 同步落库所需的最小 DB 接口(由 database 实现注入),
// 放 provider 包定接口 = 依赖倒置,provider 不 import database。
type ModelSyncStore interface {
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
}

// SyncVendorModels 拉某厂商上游在售模型并落库。
// 遍历该 vendor 的所有注册面,逐个调 ListModels 合并去重;单个面返回
// ErrListModelsNotSupported / 其它错误时继续试其它面(不中断整体)。
// 这样"同一 vendor 多协议面各自有模型端点"的中转站厂商(如 rightapi 的
// codex/grok/claude 三面各有一份 /v1/models)能合并到同一 vendor 名下;
// 而 deepseek/minimax/mimo 的 anthropic 面返回 NotSupported 直接跳过,行为不变。
func SyncVendorModels(ctx context.Context, m *Manager, vendor string, store ModelSyncStore) ([]string, error) {
	var merged []string
	seen := make(map[string]bool)
	for _, n := range m.Names() {
		if m.VendorFor(n) != vendor {
			continue
		}
		p, ok := m.Get(n)
		if !ok {
			continue
		}
		ids, err := p.ListModels(ctx)
		if err != nil {
			// 该面不支持模型列表(NotSupported)或拉取失败 → 跳过,由其它面兜底。
			continue
		}
		for _, id := range ids {
			if id != "" && !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
	}
	if len(merged) == 0 {
		// 所有面都拉不到模型(既无 openai 面,或全部 NotSupported/failed)。
		return nil, fmt.Errorf("vendor %q has no face with a working model list", vendor)
	}
	if err := store.UpsertModels(ctx, vendor, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// VendorSyncResult 单个 vendor 的同步结果(用于"全部同步"逐厂商汇报)。
// Error 空表示成功;单个 vendor 失败不影响其它 vendor 继续同步。
type VendorSyncResult struct {
	Vendor       string `json:"vendor"`
	SyncedModels int    `json:"synced_models"`
	Error        string `json:"error,omitempty"`
}

// SyncAllVendorModels 同步所有已注册 vendor 的上游模型,逐个调 SyncVendorModels。
// 返回按 vendor 名排序的结果列表;单个 vendor 失败不中断整体,失败信息记在
// 对应结果的 Error 字段。整体只在整个遍历无法进行(理论不发生)才返回 error。
func SyncAllVendorModels(ctx context.Context, m *Manager, store ModelSyncStore) ([]VendorSyncResult, error) {
	vendors := m.Vendors()
	results := make([]VendorSyncResult, 0, len(vendors))
	for _, vendor := range vendors {
		ids, err := SyncVendorModels(ctx, m, vendor, store)
		res := VendorSyncResult{Vendor: vendor}
		if err != nil {
			res.Error = err.Error()
		} else {
			res.SyncedModels = len(ids)
		}
		results = append(results, res)
	}
	return results, nil
}

// Vendors 返回 Manager 已知的所有 vendor 去重列表(按名排序)。
// vendor 从已注册面名经 VendorFor 归位得到,加新 provider 自动纳入,无需手工维护。
func (m *Manager) Vendors() []string {
	set := make(map[string]struct{})
	for _, name := range m.Names() {
		v := m.VendorFor(name)
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
