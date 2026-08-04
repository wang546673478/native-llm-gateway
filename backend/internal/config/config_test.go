// Package config — 配置解析单元测试
package config

import (
	"os"
	"path/filepath"
	"testing"
)

const testYamlHead = `
server:
  port: 8080
database:
  driver: sqlite
  dsn: "./test.db"
providers:
  deepseek:
    enabled: true
    endpoint: "https://api.deepseek.com"
    protocol: "openai"
    models:
      - id: "deepseek-v4-flash"
`

// TestLoad_CatchAllEmptyRule P-catch-all:
// `catch_all: {}`(空规则 = 自动模式)必须解析成非 nil 空 AliasRule,
// 与「没写 catch_all」(nil = 不兜底)区分开
func TestLoad_CatchAllEmptyRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := testYamlHead + "routing:\n  catch_all: {}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routing.CatchAll == nil {
		t.Fatal("catch_all: {} should parse as non-nil empty rule(自动模式), got nil")
	}
	if len(cfg.Routing.CatchAll.Providers) != 0 || cfg.Routing.CatchAll.TargetModel != "" {
		t.Errorf("catch_all = %+v, want empty rule", cfg.Routing.CatchAll)
	}
}

// TestLoad_NoCatchAll P-catch-all: 不写 catch_all → nil(不兜底)
func TestLoad_NoCatchAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testYamlHead), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routing.CatchAll != nil {
		t.Errorf("catch_all = %+v, want nil(未配置不兜底)", cfg.Routing.CatchAll)
	}
}
