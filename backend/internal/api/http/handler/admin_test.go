// Package handler — Admin API unit tests
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/inflight"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// fakeProvider 最小 Provider 实现(测试用)
type fakeProvider struct {
	name     string
	protocol provider.Protocol
	models   []string
}

func (f *fakeProvider) Name() string                { return f.name }
func (f *fakeProvider) Protocol() provider.Protocol { return f.protocol }
func (f *fakeProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (f *fakeProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, nil
}
func (f *fakeProvider) HealthCheck(context.Context) error { return nil }
func (f *fakeProvider) ListModels(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeProvider) SetPool(*keypool.Pool) {}
func (f *fakeProvider) Close() error          { return nil }

// fakeModelStore 最小 provider.ModelStore 实现(listProviders 喂 models 用)。
type fakeModelStore struct {
	rows []provider.DBModelRow
}

func (s fakeModelStore) All(ctx context.Context) ([]provider.DBModelRow, error) {
	return s.rows, nil
}
func (s fakeModelStore) ListByVendor(ctx context.Context, vendor string) ([]provider.DBModelRow, error) {
	var out []provider.DBModelRow
	for _, r := range s.rows {
		if r.Vendor == vendor {
			out = append(out, r)
		}
	}
	return out, nil
}

// TestListProviders_VendorAggregation P-provider-vendor:
// GET /providers 按 vendor 聚合 — deepseek 双注册名归一个 vendor,names 排序确定,
// models 并集去重,key_pool 来自共享 pool(vendor 名)
func TestListProviders_VendorAggregation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 共享 pool(deepseek 与 deepseek-anthropic 同一指针)
	now := time.Now()
	mkKey := func(id, name string) *keypool.Key {
		return &keypool.Key{
			ID: id, ProviderName: "deepseek", Name: name,
			Status: keypool.KeyStatusActive, BillingSource: "api",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	sharedPool := keypool.NewPool("deepseek", []*keypool.Key{
		mkKey("1", "k1"), mkKey("2", "k2"),
	}, nil, keypool.Config{})
	qwenPool := keypool.NewPool("qwen", nil, nil, keypool.Config{})

	// Registry:vendor 元数据(deepseek 与 deepseek-anthropic 同 vendor)
	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendor("deepseek", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("deepseek-anthropic", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolAnthropic, "deepseek")
	reg.RegisterWithProtocolVendor("qwen", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolOpenAI, "qwen")

	// Manager:SetForTesting 塞 3 个 fake provider(用带 vendor 的 registry,ModelsFor 才能归位)
	mgr := provider.NewManager(reg, zap.NewNop())
	mgr.SetForTesting("deepseek", &fakeProvider{name: "deepseek", protocol: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}})
	mgr.SetForTesting("deepseek-anthropic", &fakeProvider{name: "deepseek-anthropic", protocol: provider.ProtocolAnthropic, models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}})
	mgr.SetForTesting("qwen", &fakeProvider{name: "qwen", protocol: provider.ProtocolOpenAI, models: []string{"qwen-plus"}})
	// Task 6:models 改 DB 来源,须显式喂 store(否则 listProviders 的 ModelsFor 返回空)。
	_ = mgr.LoadModelsFromStore(context.Background(), fakeModelStore{rows: []provider.DBModelRow{
		{Vendor: "deepseek", ModelID: "deepseek-v4-flash"},
		{Vendor: "deepseek", ModelID: "deepseek-v4-pro"},
		{Vendor: "qwen", ModelID: "qwen-plus"},
	}})

	a := &Admin{
		Manager:  mgr,
		Registry: reg,
		Pools: map[string]*keypool.Pool{
			"deepseek":           sharedPool,
			"deepseek-anthropic": sharedPool,
			"qwen":               qwenPool,
		},
		// Breakers 留 nil — 输出不含 circuit_breaker,断言范围收窄到聚合本身
	}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
	ginCtx.Request = req
	a.listProviders(ginCtx)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Vendors []struct {
			Vendor string `json:"vendor"`
			Names  []struct {
				Name     string `json:"name"`
				Protocol string `json:"protocol"`
			} `json:"names"`
			Models  []string            `json:"models"`
			KeyPool *keypool.PoolStatus `json:"key_pool"`
		} `json:"vendors"`
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Count != 2 {
		t.Fatalf("count = %d, want 2 (deepseek + qwen)", body.Count)
	}

	// deepseek vendor:两个注册名、排序确定(names[0] = "deepseek")、models 并集去重、共享 pool
	var ds *struct {
		Vendor string `json:"vendor"`
		Names  []struct {
			Name     string `json:"name"`
			Protocol string `json:"protocol"`
		} `json:"names"`
		Models  []string            `json:"models"`
		KeyPool *keypool.PoolStatus `json:"key_pool"`
	}
	for i := range body.Vendors {
		if body.Vendors[i].Vendor == "deepseek" {
			ds = &body.Vendors[i]
			break
		}
	}
	if ds == nil {
		t.Fatal("deepseek vendor missing")
	}
	if len(ds.Names) != 2 {
		t.Fatalf("deepseek names = %d, want 2", len(ds.Names))
	}
	if ds.Names[0].Name != "deepseek" || ds.Names[0].Protocol != "openai" {
		t.Errorf("names[0] = %+v, want {deepseek openai}(排序确定)", ds.Names[0])
	}
	if ds.Names[1].Name != "deepseek-anthropic" || ds.Names[1].Protocol != "anthropic" {
		t.Errorf("names[1] = %+v, want {deepseek-anthropic anthropic}", ds.Names[1])
	}
	if len(ds.Models) != 2 {
		t.Errorf("models = %v, want 2 个去重", ds.Models)
	}
	if ds.KeyPool == nil || ds.KeyPool.ProviderName != "deepseek" || ds.KeyPool.TotalKeys != 2 {
		t.Errorf("key_pool = %+v, want 共享 pool(vendor=deepseek, 2 keys)", ds.KeyPool)
	}
}

// TestPoolStatuses_DedupSharedPools P-provider-vendor:
// Pools map 按注册名建 key,同 vendor 共享同一 pool 指针 — poolStatuses 必须按
// pool 去重,否则 dashboard 的 keypools 同一个池出现两次(两个 deepseek + 两个
// minimax),QuotaKnownSum 翻倍统计
func TestPoolStatuses_DedupSharedPools(t *testing.T) {
	now := time.Now()
	mkKey := func(id, name string) *keypool.Key {
		return &keypool.Key{
			ID: id, ProviderName: "deepseek", Name: name,
			Status: keypool.KeyStatusActive, BillingSource: "api",
			CreatedAt: now, UpdatedAt: now,
		}
	}
	shared := keypool.NewPool("deepseek", []*keypool.Key{
		mkKey("1", "k1"), mkKey("2", "k2"),
	}, nil, keypool.Config{})
	qwen := keypool.NewPool("qwen", nil, nil, keypool.Config{})

	statuses := poolStatuses(map[string]*keypool.Pool{
		"deepseek":           shared,
		"deepseek-anthropic": shared,
		"qwen":               qwen,
	})

	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2 (按 pool 去重,同 vendor 只出现一次)", len(statuses))
	}
	seen := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		if seen[s.ProviderName] {
			t.Errorf("duplicate provider_name %q in poolStatuses", s.ProviderName)
		}
		seen[s.ProviderName] = true
	}
	for _, s := range statuses {
		if s.ProviderName == "deepseek" && s.TotalKeys != 2 {
			t.Errorf("deepseek total_keys = %d, want 2", s.TotalKeys)
		}
	}
}

// TestListInflight P-inflight:
// GET /api/v1/inflight — InflightSnapshot 为 nil 时返回空 requests;非 nil 时
// 返回正确字段(elapsed_ms 由 now-StartedAt 现算,不精确断言绝对值)。
func TestListInflight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	do := func(a *Admin) []byte {
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/inflight", nil)
		a.listInflight(ginCtx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.Bytes()
	}

	// 1) InflightSnapshot nil → {"requests":[]}
	var empty struct {
		Requests []json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(do(&Admin{}), &empty); err != nil {
		t.Fatalf("decode nil case: %v", err)
	}
	if empty.Requests == nil || len(empty.Requests) != 0 {
		t.Fatalf("nil snapshot: requests = %v, want empty", empty.Requests)
	}

	// 2) 非 nil → 返回 snapshot 字段,elapsed_ms >= 0
	now := time.Now()
	start := now.Add(-1500 * time.Millisecond)
	snapFn := func() []*inflight.Snapshot {
		return []*inflight.Snapshot{{
			TraceID:        "trace-1",
			StartedAt:      start,
			RequestedModel: "opus",
			FinalModel:     "deepseek-v4-pro",
			ProviderName:   "deepseek",
			GatewayKeyName: "key-1",
			IsStream:       true,
		}}
	}
	var body struct {
		Requests []struct {
			TraceID        string `json:"trace_id"`
			StartedAt      string `json:"started_at"`
			RequestedModel string `json:"requested_model"`
			FinalModel     string `json:"final_model"`
			ProviderName   string `json:"provider_name"`
			GatewayKeyName string `json:"gateway_key_name"`
			IsStream       bool   `json:"is_stream"`
			ElapsedMs      int64  `json:"elapsed_ms"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(do(&Admin{InflightSnapshot: snapFn}), &body); err != nil {
		t.Fatalf("decode non-nil case: %v", err)
	}
	if len(body.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(body.Requests))
	}
	r := body.Requests[0]
	if r.TraceID != "trace-1" || r.RequestedModel != "opus" || r.FinalModel != "deepseek-v4-pro" || r.ProviderName != "deepseek" ||
		r.GatewayKeyName != "key-1" || !r.IsStream {
		t.Errorf("request fields = %+v, want populated snapshot", r)
	}
	if r.StartedAt != start.UTC().Format(time.RFC3339) {
		t.Errorf("started_at = %q, want %q", r.StartedAt, start.UTC().Format(time.RFC3339))
	}
	if r.ElapsedMs < 0 {
		t.Errorf("elapsed_ms = %d, want >= 0", r.ElapsedMs)
	}
}

// fakeProviderModelStore 最小 database.ProviderModelStore 实现(模型管理端点测试替身)。
type fakeProviderModelStore struct {
	rows        []dbpkg.ProviderModel
	savedVendor string
	savedModel  string
}

func (s *fakeProviderModelStore) All(ctx context.Context) ([]dbpkg.ProviderModel, error) {
	return s.rows, nil
}
func (s *fakeProviderModelStore) ListByVendor(ctx context.Context, vendor string) ([]dbpkg.ProviderModel, error) {
	var out []dbpkg.ProviderModel
	for _, r := range s.rows {
		if r.Vendor == vendor {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *fakeProviderModelStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	return nil
}
func (s *fakeProviderModelStore) SavePricing(ctx context.Context, vendor, modelID string, input, cacheRead, output float64) error {
	s.savedVendor = vendor
	s.savedModel = modelID
	return nil
}

// TestListProviderModels_ModelStoreNil P-model-sync:ModelStore=nil → 503。
func TestListProviderModels_ModelStoreNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/providers/models", nil)
	(&Admin{}).listProviderModels(ginCtx)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestListProviderModels_Grouped P-model-sync:list 按 vendor 分组返回。
func TestListProviderModels_Grouped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeProviderModelStore{rows: []dbpkg.ProviderModel{
		{Vendor: "deepseek", ModelID: "deepseek-v4-flash"},
		{Vendor: "deepseek", ModelID: "deepseek-v4-pro"},
		{Vendor: "qwen", ModelID: "qwen-plus"},
	}}
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/providers/models", nil)
	(&Admin{ModelStore: store}).listProviderModels(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Vendors map[string][]dbpkg.ProviderModel `json:"vendors"`
		Count   int                              `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 2 {
		t.Fatalf("count = %d, want 2", body.Count)
	}
	if len(body.Vendors["deepseek"]) != 2 || len(body.Vendors["qwen"]) != 1 {
		t.Fatalf("vendors grouping = %+v, want deepseek(2) + qwen(1)", body.Vendors)
	}
}

// TestSyncProviderModels P-model-sync:sync 走闭包返回 model 数,且 reload 被调用。
func TestSyncProviderModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reloaded := false
	syncFn := func(ctx context.Context, vendor string) ([]string, error) {
		if vendor != "deepseek" {
			t.Fatalf("vendor = %q, want deepseek", vendor)
		}
		return []string{"m1", "m2", "m3"}, nil
	}
	reloadFn := func() error { reloaded = true; return nil }

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/providers/sync-models",
		strings.NewReader(`{"vendor":"deepseek"}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	(&Admin{ModelSync: syncFn, ModelReload: reloadFn}).syncProviderModels(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Vendor       string `json:"vendor"`
		SyncedModels int    `json:"synced_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SyncedModels != 3 {
		t.Fatalf("synced_models = %d, want 3", body.SyncedModels)
	}
	if !reloaded {
		t.Fatal("ModelReload not called after sync")
	}
}

// TestSaveProviderModelPricing P-model-sync:save 调 store.SavePricing 且 reload 被调用。
func TestSaveProviderModelPricing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeProviderModelStore{}
	reloaded := false
	reloadFn := func() error { reloaded = true; return nil }

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodPut, "/api/v1/providers/models",
		strings.NewReader(`{"vendor":"deepseek","model_id":"deepseek-v4-flash","cost_per_million_input":1.5}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	(&Admin{ModelStore: store, ModelReload: reloadFn}).saveProviderModelPricing(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if store.savedVendor != "deepseek" || store.savedModel != "deepseek-v4-flash" {
		t.Fatalf("SavePricing got (%q,%q), want (deepseek, deepseek-v4-flash)", store.savedVendor, store.savedModel)
	}
	if !reloaded {
		t.Fatal("ModelReload not called after save pricing")
	}
}
