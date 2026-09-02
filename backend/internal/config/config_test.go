// Package config — 配置解析单元测试
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoad_RelayFirstByteTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := testYamlHead + "retry:\n  relay_first_byte_timeout: 180s\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retry.RelayFirstByteTimeout != 180*time.Second {
		t.Fatalf("relay_first_byte_timeout = %v, want 180s", cfg.Retry.RelayFirstByteTimeout)
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

// TestFingerprintDefaults 钉住指纹归一化的「默认开 / 显式关」语义。
// 用 *bool 区分「未配置」(→默认开) 与「显式 false」(→关)。
func TestFingerprintDefaults(t *testing.T) {
	// 1. 未配置 fingerprint → 默认开
	c1 := &Config{}
	if !c1.Fingerprint.FingerprintEnabled() {
		t.Error("unset fingerprint should default to enabled")
	}

	// 2. 显式 false → 关
	f := false
	c2 := &Config{Fingerprint: FingerprintConfig{Enabled: &f}}
	if c2.Fingerprint.FingerprintEnabled() {
		t.Error("explicit enabled=false should be disabled")
	}

	// 3. 显式 true → 开
	tr := true
	c3 := &Config{Fingerprint: FingerprintConfig{Enabled: &tr, CanonicalDeviceID: "abc"}}
	if !c3.Fingerprint.FingerprintEnabled() {
		t.Error("explicit enabled=true should be enabled")
	}
	if c3.Fingerprint.CanonicalDeviceID != "abc" {
		t.Errorf("canonical_device_id = %q, want abc", c3.Fingerprint.CanonicalDeviceID)
	}
}
