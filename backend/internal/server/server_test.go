// Package server — Server 单元测试(ReloadProviderPool 共享不变量)
package server

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	// P-provider-vendor: init() 注册 deepseek / deepseek-anthropic(vendor=deepseek)
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"
)

// fakeProvider 最小 Provider 实现 — Manager.SetForTesting 用
type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string                            { return f.name }
func (f *fakeProvider) Protocol() provider.Protocol             { return provider.ProtocolOpenAI }
func (f *fakeProvider) Models() []string                        { return nil }
func (f *fakeProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (f *fakeProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, nil
}
func (f *fakeProvider) HealthCheck(context.Context) error { return nil }
func (f *fakeProvider) Close() error                      { return nil }

// newReloadTestServer 构造可测 ReloadProviderPool 的 Server:
// 内存 sqlite(provider_api_keys 表,key 存在 vendor 名下)+ manager 含 deepseek / deepseek-anthropic
func newReloadTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&database.ProviderAPIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	// 迁移后 key 都在 vendor 名下("deepseek")
	if err := db.Create(&database.ProviderAPIKey{
		ProviderName: "deepseek", Name: "k1", KeyHash: "hash1",
		Enabled: true, BillingSource: "api",
	}).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	manager := provider.NewManager(provider.Default(), zap.NewNop())
	manager.SetForTesting("deepseek", &fakeProvider{name: "deepseek"})
	manager.SetForTesting("deepseek-anthropic", &fakeProvider{name: "deepseek-anthropic"})
	return &Server{
		cfg: &config.Config{
			Providers: map[string]config.Provider{
				"deepseek":           {Enabled: true},
				"deepseek-anthropic": {Enabled: true},
			},
		},
		logger:  zap.NewNop(),
		db:      db,
		pools:   map[string]*keypool.Pool{},
		manager: manager,
	}
}

// TestReloadProviderPool_SingleRebindsVendorNames P-provider-vendor:
// 单个注册名 reload 后,同 vendor 的所有注册名必须指向同一个新 pool
func TestReloadProviderPool_SingleRebindsVendorNames(t *testing.T) {
	s := newReloadTestServer(t)
	// 先让两个注册名指向不同 pool(模拟旧行为破坏的不变量)
	s.pools["deepseek"] = keypool.NewPool("old-deepseek", nil, nil, keypool.Config{})
	s.pools["deepseek-anthropic"] = keypool.NewPool("old-anthropic", nil, nil, keypool.Config{})

	s.ReloadProviderPool("deepseek")

	if s.pools["deepseek"] == nil || s.pools["deepseek-anthropic"] == nil {
		t.Fatal("pools not rebuilt")
	}
	if s.pools["deepseek"] != s.pools["deepseek-anthropic"] {
		t.Fatal("P-provider-vendor: reload 后 deepseek-anthropic 必须与 deepseek 指向同一 pool(共享不变量被破坏)")
	}
	if got := s.pools["deepseek"].Size(); got != 1 {
		t.Errorf("rebuilt pool Size = %d, want 1 (key 在 vendor 名下读取)", got)
	}
}

// TestReloadProviderPool_FullRebuildVendorGrouped 全量重建也按 vendor 分组
func TestReloadProviderPool_FullRebuildVendorGrouped(t *testing.T) {
	s := newReloadTestServer(t)
	s.pools["deepseek"] = keypool.NewPool("old-deepseek", nil, nil, keypool.Config{})
	s.pools["deepseek-anthropic"] = keypool.NewPool("old-anthropic", nil, nil, keypool.Config{})

	s.ReloadProviderPool("")

	if s.pools["deepseek"] != s.pools["deepseek-anthropic"] {
		t.Fatal("P-provider-vendor: 全量重建后 deepseek-anthropic 必须与 deepseek 指向同一 pool")
	}
	if got := s.pools["deepseek"].Size(); got != 1 {
		t.Errorf("rebuilt pool Size = %d, want 1", got)
	}
}
