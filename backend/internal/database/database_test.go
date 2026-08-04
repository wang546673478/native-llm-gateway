package database

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMigrateProviderVendorKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProviderAPIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	// 迁移前状态:四个注册名各一行(模拟现状)
	rows := []ProviderAPIKey{
		{ProviderName: "deepseek", Name: "a", KeyHash: "k1", BillingSource: "api"},
		{ProviderName: "deepseek-anthropic", Name: "b", KeyHash: "k2", BillingSource: "api"},
		{ProviderName: "minimax", Name: "c", KeyHash: "k3", BillingSource: "token_plan"},
		{ProviderName: "minimax-openai", Name: "d", KeyHash: "k4", BillingSource: "token_plan"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrateProviderVendorKeys(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 第一次迁移后
	var all []ProviderAPIKey
	if err := db.Order("id").Find(&all).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	want := []struct{ provider, protocols string }{
		{"deepseek", ""}, // 主条目不标 = 全部协议(用户 2026-08-04 裁决)
		{"deepseek", "anthropic"},
		{"minimax", ""},
		{"minimax", "openai"},
	}
	if len(all) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(all), len(want), all)
	}
	for i, w := range want {
		if all[i].ProviderName != w.provider || all[i].Protocols != w.protocols {
			t.Errorf("row %d = (%s, %q), want (%s, %q)",
				i, all[i].ProviderName, all[i].Protocols, w.provider, w.protocols)
		}
	}

	// 幂等:再跑一遍,结果不变
	if err := migrateProviderVendorKeys(db); err != nil {
		t.Fatalf("migrate second run: %v", err)
	}
	var all2 []ProviderAPIKey
	if err := db.Order("id").Find(&all2).Error; err != nil {
		t.Fatalf("query2: %v", err)
	}
	for i, w := range want {
		if all2[i].ProviderName != w.provider || all2[i].Protocols != w.protocols {
			t.Errorf("idempotent row %d = (%s, %q), want (%s, %q)",
				i, all2[i].ProviderName, all2[i].Protocols, w.provider, w.protocols)
		}
	}
}

func TestMigrateProviderVendorKeys_LeavesOthersUntouched(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProviderAPIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	// 迁移不应触碰的行:glm/kimi(已删包,保留无害)、已标非空 protocols 的主条目行
	rows := []ProviderAPIKey{
		{ProviderName: "glm", Name: "a", KeyHash: "k1", BillingSource: "api"},
		{ProviderName: "glm-anthropic", Name: "b", KeyHash: "k2", BillingSource: "api"},
		{ProviderName: "kimi", Name: "c", KeyHash: "k3", BillingSource: "api"},
		{ProviderName: "kimi-anthropic", Name: "d", KeyHash: "k4", BillingSource: "api"},
		// 用户已在 UI 把 deepseek key 改成全协议(空)— 迁移绝不能覆盖它
		{ProviderName: "deepseek", Name: "e", KeyHash: "k5", BillingSource: "api", Protocols: ""},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrateProviderVendorKeys(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var all []ProviderAPIKey
	if err := db.Order("id").Find(&all).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	want := []struct{ provider, protocols string }{
		{"glm", ""},
		{"glm-anthropic", ""},
		{"kimi", ""},
		{"kimi-anthropic", ""},
		{"deepseek", ""}, // 空 = 全部协议,保持原样
	}
	if len(all) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(all), len(want), all)
	}
	for i, w := range want {
		if all[i].ProviderName != w.provider || all[i].Protocols != w.protocols {
			t.Errorf("row %d = (%s, %q), want (%s, %q)",
				i, all[i].ProviderName, all[i].Protocols, w.provider, w.protocols)
		}
	}
}
