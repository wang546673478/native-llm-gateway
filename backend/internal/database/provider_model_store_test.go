package database

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newProviderModelTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProviderModel{}, &ProviderModelFace{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func TestProviderModelStore_UpsertPreservesPricing(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	// 先有一笔手工价
	if err := db.Create(&ProviderModel{
		Vendor:                  "deepseek",
		ModelID:                 "deepseek-chat",
		CostPerMillionInput:     0.27,
		CostPerMillionCacheRead: 0.07,
		CostPerMillionOutput:    1.10,
		Source:                  "manual",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// upsert 上游同步,deepseek-chat 已存在(应保留价),deepseek-reasoner 新增(价 0)
	if err := store.UpsertModels(ctx, "deepseek", []string{"deepseek-chat", "deepseek-reasoner"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.ListByVendor(ctx, "deepseek")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got), got)
	}

	// 顺序按 model_id ASC:deepseek-chat, deepseek-reasoner
	if got[0].ModelID != "deepseek-chat" || got[1].ModelID != "deepseek-reasoner" {
		t.Fatalf("unexpected order: %+v", got)
	}

	// 已有模型保留手工价,source=upstream,synced_at 非空
	chat := got[0]
	if chat.CostPerMillionInput != 0.27 || chat.CostPerMillionCacheRead != 0.07 || chat.CostPerMillionOutput != 1.10 {
		t.Errorf("deepseek-chat price not preserved: %+v", chat)
	}
	if chat.Source != "upstream" {
		t.Errorf("deepseek-chat source = %q, want upstream", chat.Source)
	}
	if chat.SyncedAt == nil {
		t.Errorf("deepseek-chat synced_at is nil")
	}

	// 新模型价格为 0,source=upstream
	reasoner := got[1]
	if reasoner.CostPerMillionInput != 0 || reasoner.CostPerMillionCacheRead != 0 || reasoner.CostPerMillionOutput != 0 {
		t.Errorf("deepseek-reasoner price should be 0: %+v", reasoner)
	}
	if reasoner.Source != "upstream" {
		t.Errorf("deepseek-reasoner source = %q, want upstream", reasoner.Source)
	}
}

func TestProviderModelStore_UpsertKeepsStaleModels(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	// 旧模型已存在
	if err := db.Create(&ProviderModel{
		Vendor:  "openai",
		ModelID: "gpt-4o",
		Source:  "manual",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 上游只同步 gpt-4o-mini,不代表 gpt-4o 被删(旧模型保留)
	if err := store.UpsertModels(ctx, "openai", []string{"gpt-4o-mini"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.ListByVendor(ctx, "openai")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2 (stale gpt-4o preserved): %+v", len(got), got)
	}
}

func TestProviderModelStore_SavePricing(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := db.Create(&ProviderModel{Vendor: "minimax", ModelID: "abab-6.5"}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := store.SavePricing(ctx, "minimax", "abab-6.5", 1.0, 0.5, 5.0); err != nil {
		t.Fatalf("save pricing: %v", err)
	}

	got, err := store.ListByVendor(ctx, "minimax")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d models, want 1", len(got))
	}
	m := got[0]
	if m.CostPerMillionInput != 1.0 || m.CostPerMillionCacheRead != 0.5 || m.CostPerMillionOutput != 5.0 {
		t.Errorf("pricing not saved: %+v", m)
	}
	if m.Source != "manual" {
		t.Errorf("source = %q, want manual", m.Source)
	}
}

func TestProviderModelStore_AllSorted(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	// 乱序插入两个厂商
	rows := []ProviderModel{
		{Vendor: "openai", ModelID: "gpt-4o"},
		{Vendor: "deepseek", ModelID: "deepseek-reasoner"},
		{Vendor: "openai", ModelID: "gpt-4o-mini"},
		{Vendor: "deepseek", ModelID: "deepseek-chat"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	got, err := store.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d, want 4", len(got))
	}
	want := []struct{ vendor, model string }{
		{"deepseek", "deepseek-chat"},
		{"deepseek", "deepseek-reasoner"},
		{"openai", "gpt-4o"},
		{"openai", "gpt-4o-mini"},
	}
	for i, w := range want {
		if got[i].Vendor != w.vendor || got[i].ModelID != w.model {
			t.Errorf("row %d = (%s, %s), want (%s, %s)",
				i, got[i].Vendor, got[i].ModelID, w.vendor, w.model)
		}
	}
}

func TestProviderModelStore_UpsertSkipsEmptyModelID(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := store.UpsertModels(ctx, "deepseek", []string{"", "deepseek-chat"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.ListByVendor(ctx, "deepseek")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ModelID != "deepseek-chat" {
		t.Fatalf("expected only deepseek-chat, got %+v", got)
	}
}

// TestProviderModelStore_PreservesUpstreamOrder 守卫 2026-08-20 根因:
// 默认模型 = 每个 vendor 的首行,必须保持上游 ListModels 的返回顺序
// (上游把旗舰款排最前),不能退回 model_id 字典序 —— 那会让 MiniMax-M3
// 排到 MiniMax-M2 之后,catch_all 模式下主力模型静默降级为基础款。
func TestProviderModelStore_PreservesUpstreamOrder(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	// 真实的 minimax /v1/models 返回顺序:M3 在最前,字典序则相反
	upstream := []string{
		"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed",
		"MiniMax-M2.5", "MiniMax-M2.5-highspeed", "MiniMax-M2",
	}
	if err := store.UpsertModels(ctx, "minimax", upstream); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.ListByVendor(ctx, "minimax")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(upstream) {
		t.Fatalf("got %d models, want %d", len(got), len(upstream))
	}
	for i, want := range upstream {
		if got[i].ModelID != want {
			t.Fatalf("第 %d 个 = %q, want %q(上游顺序被打乱了,默认模型会选错)",
				i, got[i].ModelID, want)
		}
	}
	// 首行即默认模型,必须是旗舰 M3 而不是字典序最小的 MiniMax-M2
	if got[0].ModelID != "MiniMax-M3" {
		t.Errorf("默认模型 = %q, want MiniMax-M3", got[0].ModelID)
	}

	// All() 同样要保序(manager.LoadModelsFromStore 取每 vendor 首行作默认)
	all, err := store.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	var firstMinimax string
	for _, m := range all {
		if m.Vendor == "minimax" {
			firstMinimax = m.ModelID
			break
		}
	}
	if firstMinimax != "MiniMax-M3" {
		t.Errorf("All() 中 minimax 首行 = %q, want MiniMax-M3", firstMinimax)
	}
}

// TestProviderModelStore_ReplaceFaceModels P-model-face:归属按面整体替换 ——
// 同 vendor 多个面各存一份、互不干扰,面内保持上游返回顺序。
func TestProviderModelStore_ReplaceFaceModels(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := store.ReplaceFaceModels(ctx, "rightapi", "rightapi-codex",
		[]string{"gpt-5.4", "gpt-5.5"}); err != nil {
		t.Fatalf("replace codex: %v", err)
	}
	if err := store.ReplaceFaceModels(ctx, "rightapi", "rightapi-claude",
		[]string{"claude-haiku-4-5", "claude-opus-5"}); err != nil {
		t.Fatalf("replace claude: %v", err)
	}

	faces, err := store.AllFaces(ctx)
	if err != nil {
		t.Fatalf("all faces: %v", err)
	}
	if len(faces) != 4 {
		t.Fatalf("got %d face rows, want 4", len(faces))
	}
	byFace := map[string][]string{}
	for _, f := range faces {
		byFace[f.Face] = append(byFace[f.Face], f.ModelID)
	}
	// 面内保序(AllFaces 按 sort_order 排;面内默认模型取首个)
	if got := byFace["rightapi-codex"]; len(got) != 2 || got[0] != "gpt-5.4" {
		t.Errorf("codex 面 = %v, want [gpt-5.4 gpt-5.5](保上游序)", got)
	}
	if got := byFace["rightapi-claude"]; len(got) != 2 || got[0] != "claude-haiku-4-5" {
		t.Errorf("claude 面 = %v, want [claude-haiku-4-5 claude-opus-5]", got)
	}

	// 整体替换:codex 面换掉后旧行必须消失(上游下架的模型从该面移除),
	// 且不影响 claude 面
	if err := store.ReplaceFaceModels(ctx, "rightapi", "rightapi-codex",
		[]string{"gpt-5.6-sol"}); err != nil {
		t.Fatalf("re-replace codex: %v", err)
	}
	faces, err = store.AllFaces(ctx)
	if err != nil {
		t.Fatalf("all faces 2: %v", err)
	}
	byFace = map[string][]string{}
	for _, f := range faces {
		byFace[f.Face] = append(byFace[f.Face], f.ModelID)
	}
	if got := byFace["rightapi-codex"]; len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Errorf("替换后 codex 面 = %v, want [gpt-5.6-sol](旧行须删除)", got)
	}
	if got := byFace["rightapi-claude"]; len(got) != 2 {
		t.Errorf("claude 面 = %v, want 2 行(替换 codex 不该影响它)", got)
	}
}

// TestProviderModelStore_PruneOrphanModels P-model-face:清理在任何面都无归属的模型。
// 场景是真实的:rightapi 从 /claude 官渠换到 /claude-aws 后,官渠独有的
// claude-fable-5 不再出现在任何面的上游清单里,但 UpsertModels 只增不删会留下它。
func TestProviderModelStore_PruneOrphanModels(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := store.UpsertModels(ctx, "rightapi",
		[]string{"claude-opus-5", "claude-fable-5", "gpt-5.4"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 给 claude-fable-5 填一笔手工价 —— prune 会连带删掉它(已无法路由)
	if err := store.SavePricing(ctx, "rightapi", "claude-fable-5", 1, 2, 3); err != nil {
		t.Fatalf("save pricing: %v", err)
	}
	// 换 channel 后的归属:claude-fable-5 不在任何面里
	if err := store.ReplaceFaceModels(ctx, "rightapi", "rightapi-claude",
		[]string{"claude-opus-5"}); err != nil {
		t.Fatalf("replace claude: %v", err)
	}
	if err := store.ReplaceFaceModels(ctx, "rightapi", "rightapi-codex",
		[]string{"gpt-5.4"}); err != nil {
		t.Fatalf("replace codex: %v", err)
	}

	deleted, err := store.PruneOrphanModels(ctx, "rightapi")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1(只有 claude-fable-5 无归属)", deleted)
	}
	got, err := store.ListByVendor(ctx, "rightapi")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("剩余 %d 行, want 2", len(got))
	}
	for _, m := range got {
		if m.ModelID == "claude-fable-5" {
			t.Error("claude-fable-5 应被删除(手工价也一并删,它已无法路由)")
		}
	}
}

// TestProviderModelStore_PruneSkipsVendorWithoutFaces P-model-face 安全前提:
// 某 vendor 一条归属行都没有时(fallback 模式:未同步过 / 所有面都无模型列表端点),
// prune 必须不删任何行 —— 否则 `NOT IN (空集)` 会把该 vendor 的模型全删光,
// 该厂商立刻失去全部候选(2026-08-20 全部 503 的事故形态)。
func TestProviderModelStore_PruneSkipsVendorWithoutFaces(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := store.UpsertModels(ctx, "minimax",
		[]string{"MiniMax-M3", "MiniMax-M2.5"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 不写任何归属行 → fallback 模式
	deleted, err := store.PruneOrphanModels(ctx, "minimax")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0(无归属数据的 vendor 必须整体跳过)", deleted)
	}
	got, err := store.ListByVendor(ctx, "minimax")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("剩余 %d 行, want 2(一行都不能删)", len(got))
	}
}
