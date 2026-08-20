package database

import (
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
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
		if all[i].ProviderName != w.provider || all[i].Protocols != w.protocols {
			t.Errorf("row %d = (%s, %q), want (%s, %q)",
				i, all[i].ProviderName, all[i].Protocols, w.provider, w.protocols)
		}
	}
}

func TestProviderModelSchema(t *testing.T) {
	// 表名
	if got := (ProviderModel{}).TableName(); got != "provider_models" {
		t.Errorf("TableName() = %q, want %q", got, "provider_models")
	}

	// GORM tag:vendor + model_id 复合唯一 idx_vendor_model,字段含 cost_per_million_input
	s, err := schema.Parse(&ProviderModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	var vendorField, modelField *schema.Field
	for _, f := range s.Fields {
		if f.DBName == "vendor" {
			vendorField = f
		}
		if f.DBName == "model_id" {
			modelField = f
		}
	}
	if vendorField == nil || modelField == nil {
		t.Fatalf("missing vendor/model_id fields: vendor=%v model=%v", vendorField != nil, modelField != nil)
	}
	for _, idx := range s.ParseIndexes() {
		if idx.Name == "idx_vendor_model" {
			var cols []string
			for _, c := range idx.Fields {
				cols = append(cols, c.DBName)
			}
			if len(cols) != 2 || cols[0] != "vendor" || cols[1] != "model_id" {
				t.Errorf("idx_vendor_model columns = %v, want [vendor model_id]", cols)
			}
			if idx.Class != "UNIQUE" {
				t.Errorf("idx_vendor_model should be unique, got class %q", idx.Class)
			}
		}
	}

	foundMillion := false
	for _, f := range s.Fields {
		if f.DBName == "cost_per_million_input" {
			foundMillion = true
		}
	}
	if !foundMillion {
		t.Errorf("missing cost_per_million_input column")
	}

	// 功能性验证:唯一约束生效,同 vendor+model_id 二次插入报错
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProviderModel{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	row := ProviderModel{
		Vendor:                 "deepseek",
		ModelID:                "deepseek-chat",
		CostPerMillionInput:    0.27,
		CostPerMillionOutput:   1.10,
		CostPerMillionCacheRead: 0.07,
		Source:                 "upstream",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create row: %v", err)
	}

	var out ProviderModel
	if err := db.Where("vendor = ? AND model_id = ?", "deepseek", "deepseek-chat").First(&out).Error; err != nil {
		t.Fatalf("query row: %v", err)
	}
	if out.CostPerMillionInput != 0.27 || out.CostPerMillionOutput != 1.10 || out.Source != "upstream" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}

	// duplicate vendor+model_id should violate unique index
	dup := ProviderModel{Vendor: "deepseek", ModelID: "deepseek-chat"}
	if err := db.Create(&dup).Error; err == nil {
		t.Errorf("expected unique constraint violation on duplicate (vendor, model_id)")
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
