package database

import (
	"context"
	"testing"
)

// P-relay-cascade 守卫组:DeleteFaceModels 是删站级联清理的执行端。
// face 是普通字符串列而非外键 —— 删 relay_stations 行不会带走归属行,
// 所以必须显式清。清的粒度错了会造成两类事故:
//   - 清太宽(如按 vendor / 前缀)→ 误删活面归属行 → 该面失去全部候选 → 503
//   - 清太窄 / 不清 → 孤儿归属行占着 (face, model_id) 唯一索引,
//     模型管理页看不到,「无归属」判定失真(历史欠账 81 行)

// TestDeleteFaceModels_OnlyTargetFace 精确性:只删目标面,
// 同 vendor 的兄弟面归属行必须一行不动。
func TestDeleteFaceModels_OnlyTargetFace(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	rows := []ProviderModelFace{
		{Vendor: "mimo", Face: "mimo", ModelID: "mimo-v2.5", SortOrder: 0},
		{Vendor: "mimo", Face: "mimo", ModelID: "mimo-v3", SortOrder: 1},
		{Vendor: "mimo", Face: "mimo-token-plan", ModelID: "mimo-v2.5", SortOrder: 0},
		{Vendor: "mimo", Face: "mimo-token-plan", ModelID: "mimo-v3", SortOrder: 1},
		{Vendor: "deepseek", Face: "deepseek", ModelID: "deepseek-chat", SortOrder: 0},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed face row: %v", err)
		}
	}

	n, err := store.DeleteFaceModels(ctx, "mimo-token-plan")
	if err != nil {
		t.Fatalf("DeleteFaceModels: %v", err)
	}
	if n != 2 {
		t.Errorf("删除行数 = %d, want 2", n)
	}

	remaining, err := store.AllFaces(ctx)
	if err != nil {
		t.Fatalf("AllFaces: %v", err)
	}
	byFace := map[string]int{}
	for _, r := range remaining {
		byFace[r.Face]++
	}
	if byFace["mimo-token-plan"] != 0 {
		t.Errorf("mimo-token-plan 残留 %d 行, want 0", byFace["mimo-token-plan"])
	}
	if byFace["mimo"] != 2 {
		t.Errorf("兄弟面 mimo 剩 %d 行, want 2 —— 同 vendor 活面被误删会让该面失去候选(503)", byFace["mimo"])
	}
	if byFace["deepseek"] != 1 {
		t.Errorf("无关面 deepseek 剩 %d 行, want 1", byFace["deepseek"])
	}
}

// TestDeleteFaceModels_LeavesPricingRows 刻意不删 provider_models:
// 定价是 vendor 级唯一真相源,vendor 可能仍有其他活面共享它。
// 一并删会让活面的模型价格归零(计费静默错算)。
func TestDeleteFaceModels_LeavesPricingRows(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := db.Create(&ProviderModel{
		Vendor: "mimo", ModelID: "mimo-v2.5",
		CostPerMillionInput: 0.3, CostPerMillionOutput: 1.2, Source: "manual",
	}).Error; err != nil {
		t.Fatalf("seed pricing: %v", err)
	}
	if err := db.Create(&ProviderModelFace{
		Vendor: "mimo", Face: "mimo-token-plan", ModelID: "mimo-v2.5",
	}).Error; err != nil {
		t.Fatalf("seed face: %v", err)
	}

	if _, err := store.DeleteFaceModels(ctx, "mimo-token-plan"); err != nil {
		t.Fatalf("DeleteFaceModels: %v", err)
	}

	priced, err := store.ListByVendor(ctx, "mimo")
	if err != nil {
		t.Fatalf("ListByVendor: %v", err)
	}
	if len(priced) != 1 {
		t.Fatalf("定价行数 = %d, want 1 —— 删面不该动 provider_models", len(priced))
	}
	if priced[0].CostPerMillionInput != 0.3 || priced[0].CostPerMillionOutput != 1.2 {
		t.Errorf("定价被改动: in=%v out=%v, want in=0.3 out=1.2",
			priced[0].CostPerMillionInput, priced[0].CostPerMillionOutput)
	}
}

// TestDeleteFaceModels_EmptyFaceIsNoOp 空面名必须直接返回 0 行且不删任何东西。
// 没有这道闸门,一个空 face(站名为空 / FaceNames 退化)会变成
// 匹配空串的 WHERE 条件,甚至(若哪天改成拼 SQL)全表删。
func TestDeleteFaceModels_EmptyFaceIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	seed := []ProviderModelFace{
		{Vendor: "a", Face: "a", ModelID: "m1"},
		{Vendor: "b", Face: "b", ModelID: "m2"},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := store.DeleteFaceModels(ctx, "")
	if err != nil {
		t.Fatalf("DeleteFaceModels(\"\"): %v", err)
	}
	if n != 0 {
		t.Errorf("空面名删除行数 = %d, want 0", n)
	}

	all, err := store.AllFaces(ctx)
	if err != nil {
		t.Fatalf("AllFaces: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("空面名调用后剩 %d 行, want 2 —— 空 face 绝不能删到任何行", len(all))
	}
}

// TestDeleteFaceModels_NoPrefixMatching 守卫「前缀匹配」的诱惑:
// 删 tokenmarket 不能牵连 tokenmarket-cc / tokenmarket-codex(独立的另外两个站)。
// 这是最初方案 DeleteByFacePrefix 会踩的坑。
func TestDeleteFaceModels_NoPrefixMatching(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	seed := []ProviderModelFace{
		{Vendor: "tokenmarket", Face: "tokenmarket", ModelID: "claude-opus-5"},
		{Vendor: "tokenmarket-cc", Face: "tokenmarket-cc", ModelID: "claude-opus-5"},
		{Vendor: "tokenmarket-codex", Face: "tokenmarket-codex", ModelID: "gpt-5.4"},
	}
	for i := range seed {
		if err := db.Create(&seed[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := store.DeleteFaceModels(ctx, "tokenmarket")
	if err != nil {
		t.Fatalf("DeleteFaceModels: %v", err)
	}
	if n != 1 {
		t.Errorf("删除行数 = %d, want 1(只有精确面 tokenmarket)", n)
	}

	all, err := store.AllFaces(ctx)
	if err != nil {
		t.Fatalf("AllFaces: %v", err)
	}
	left := map[string]bool{}
	for _, r := range all {
		left[r.Face] = true
	}
	for _, sibling := range []string{"tokenmarket-cc", "tokenmarket-codex"} {
		if !left[sibling] {
			t.Errorf("兄弟站面 %q 被误删 —— 前缀匹配会打掉在跑的站", sibling)
		}
	}
}

// TestDeleteFaceModels_UnknownFaceIsNoOp 不存在的面返回 0 行、无错误。
// 级联清理会对 FaceNames 报出的全部面调用(超集,可能含从未注册成功的面),
// 未命中必须是安静的 no-op 而不是报错中断整个删除流程。
func TestDeleteFaceModels_UnknownFaceIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newProviderModelTestDB(t)
	store := NewProviderModelStore(db)

	if err := db.Create(&ProviderModelFace{
		Vendor: "a", Face: "a", ModelID: "m1",
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := store.DeleteFaceModels(ctx, "never-registered-face")
	if err != nil {
		t.Fatalf("未命中的面应无错误, got %v", err)
	}
	if n != 0 {
		t.Errorf("删除行数 = %d, want 0", n)
	}

	all, _ := store.AllFaces(ctx)
	if len(all) != 1 {
		t.Errorf("剩 %d 行, want 1", len(all))
	}
}
