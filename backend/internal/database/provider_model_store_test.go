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
	if err := db.AutoMigrate(&ProviderModel{}); err != nil {
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
