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
	// UpsertModels 落厂商级模型清单(定价表,只增不删,保留手工价)。
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
	// ReplaceFaceModels 整体替换某注册面的模型归属(P-model-face)。
	ReplaceFaceModels(ctx context.Context, vendor, face string, modelIDs []string) error
}

// SyncVendorModels 拉某厂商上游在售模型并落库。
//
// 遍历该 vendor 的所有注册面,逐个调 ListModels:
//   - 该面成功 → 立即 ReplaceFaceModels 记下**这个面**的归属(P-model-face),
//     并把 id 合并进 vendor 级清单
//   - 该面返回 ErrListModelsNotSupported / 其它错误 → 跳过,**不动它已有的归属**
//     (上游临时抖动不该清空归属;anthropic 面这类天生无模型端点的面永远无归属,
//     由 ModelsFor 的 fallback 退回 vendor 全量)
//
// 面归属让中转站厂商(rightapi 的 codex/grok/claude 三面各有一份 /v1/models、
// 模型互不相通)不再把 claude 模型发给 codex 端点(404 model not found);
// 而 deepseek/minimax/mimo 的 anthropic 面无归属 → fallback 共享 openai 面的清单,
// 行为不变。
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
			// 不碰该面已有归属:失败是瞬态的,清空会让该面掉进 fallback 拿到全厂商模型。
			continue
		}
		// 记下该面自己的归属(整体替换 → 上游下架的模型从该面消失)
		if err := store.ReplaceFaceModels(ctx, vendor, n, ids); err != nil {
			return nil, err
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
