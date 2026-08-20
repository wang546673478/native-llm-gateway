// Package provider — 模型从上游同步
// 对应 Task 5:调 ListModels 拉上游在售模型 → upsert 到 Store
package provider

import (
	"context"
	"fmt"
)

// ModelSyncStore 同步落库所需的最小 DB 接口(由 database 实现注入),
// 放 provider 包定接口 = 依赖倒置,provider 不 import database。
type ModelSyncStore interface {
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
}

// SyncVendorModels 拉某厂商上游在售模型并落库。
// 优先用 vendor 的 OpenAI 面;若无 openai 面则跳过(返回 nil)。
// 单个面 ListModels 返回 ErrListModelsNotSupported 时,换同 vendor 的 openai 面。
func SyncVendorModels(ctx context.Context, m *Manager, vendor string, store ModelSyncStore) ([]string, error) {
	names := m.Names()
	var openaiFace string
	for _, n := range names {
		if m.VendorFor(n) == vendor {
			if p, ok := m.Get(n); ok && p.Protocol() == ProtocolOpenAI {
				openaiFace = n
				break
			}
		}
	}
	if openaiFace == "" {
		// 没有 openai 面 → 无法同步(当前 6 vendor 都有,防御性返回)
		return nil, fmt.Errorf("vendor %q has no openai-compatible face to sync models", vendor)
	}
	p, _ := m.Get(openaiFace)
	ids, err := p.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	if err := store.UpsertModels(ctx, vendor, ids); err != nil {
		return nil, err
	}
	return ids, nil
}
