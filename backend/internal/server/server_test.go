// Package server — Server 单元测试(ReloadProviderPool 共享不变量)
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/wang546673478/native-llm-gateway/internal/config"
	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	// P-provider-vendor: init() 注册 deepseek / deepseek-anthropic(vendor=deepseek)
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"
	// TestVendorHasBalancer 用:glm 有 balancer(官方 monitor 端点)、qwen 没有
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/glm"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/qwen"
)

// fakeProvider 最小 Provider 实现 — Manager.SetForTesting 用
type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string                { return f.name }
func (f *fakeProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (f *fakeProvider) Models() []string            { return nil }
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

// TestWebStatic — P-web-static 方案 B:Go 进程托管前端静态文件。
// 覆盖:文件命中 / SPA fallback / 未配置时让位 / 非 GET 不接管 / 路径穿越拒绝
func TestWebStatic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}

	// newWebStaticServer 构造 static_dir 指向临时目录的 Server
	newWebStaticServer := func(staticDir string) *Server {
		return &Server{cfg: &config.Config{Server: config.ServerConfig{StaticDir: staticDir}}}
	}

	do := func(s *Server, method, p string) (bool, int, string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, p, nil)
		c, _ := gin.CreateTestContext(w)
		c.Request = req
		ok := s.webStatic(c)
		return ok, w.Code, w.Body.String()
	}

	// 未配置 static_dir → 让位(false),调用方维持 404 JSON
	if ok, _, _ := do(newWebStaticServer(""), http.MethodGet, "/"); ok {
		t.Fatal("static_dir 未配置时 webStatic 必须让位(false)")
	}

	cases := []struct {
		name     string
		method   string
		path     string
		wantOK   bool
		wantCode int
		wantBody string
	}{
		{"index", http.MethodGet, "/", true, http.StatusOK, "<html>app</html>"},
		{"asset hit", http.MethodGet, "/assets/app.js", true, http.StatusOK, "console.log(1)"},
		{"spa fallback", http.MethodGet, "/some/vue/route", true, http.StatusOK, "<html>app</html>"},
		{"head", http.MethodHead, "/", true, http.StatusOK, ""},
		{"post let pass", http.MethodPost, "/", false, 0, ""},
		{"traversal rejected", http.MethodGet, "/../etc/passwd", true, http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, code, body := do(newWebStaticServer(dir), tc.method, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("handled = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && code != tc.wantCode {
				t.Errorf("status = %d, want %d", code, tc.wantCode)
			}
			if tc.wantOK && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestVendorHasBalancer — 有余额查询 balancer 的 vendor(deepseek/glm)→
// poll 模式;无 balancer 的(qwen)→ probe 模式,配额耗尽不永久标记
func TestVendorHasBalancer(t *testing.T) {
	if !vendorHasBalancer("deepseek") {
		t.Error("deepseek: want has-balancer=true (both faces register balancers)")
	}
	if !vendorHasBalancer("glm") {
		t.Error("glm: want has-balancer=true (official monitor quota endpoint)")
	}
	if vendorHasBalancer("qwen") {
		t.Error("qwen: want has-balancer=false (no balance API)")
	}
}

func TestKeyStateSnapshotPath(t *testing.T) {
	// SQLite:与 DB 同目录
	if got := keyStateSnapshotPath("/tmp/gateway-data/gateway.db"); got != "/tmp/gateway-data/key-state.json" {
		t.Fatalf("sqlite dsn: got %q", got)
	}
	// PG:URL dsn 落 cwd(修复 2026-08-07 — filepath.Dir 会把 URL 拆出
	// "postgres:/..." 怪路径,快照静默写失败)
	for _, dsn := range []string{
		"postgres://gateway:pw@127.0.0.1:5432/gateway",
		"postgresql://gateway:pw@127.0.0.1:5432/gateway",
	} {
		if got := keyStateSnapshotPath(dsn); got != "key-state.json" {
			t.Fatalf("pg dsn %q: got %q, want key-state.json", dsn, got)
		}
	}
}
