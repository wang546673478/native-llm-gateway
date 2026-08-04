// Package handler — Admin API unit tests
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

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
func (f *fakeProvider) Models() []string            { return f.models }
func (f *fakeProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, nil
}
func (f *fakeProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, nil
}
func (f *fakeProvider) HealthCheck(context.Context) error { return nil }
func (f *fakeProvider) SetPool(*keypool.Pool)             {}
func (f *fakeProvider) Close() error                      { return nil }

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

	// Manager:SetForTesting 塞 3 个 fake provider
	mgr := provider.NewManager(provider.NewRegistry(), zap.NewNop())
	mgr.SetForTesting("deepseek", &fakeProvider{name: "deepseek", protocol: provider.ProtocolOpenAI, models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}})
	mgr.SetForTesting("deepseek-anthropic", &fakeProvider{name: "deepseek-anthropic", protocol: provider.ProtocolAnthropic, models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}})
	mgr.SetForTesting("qwen", &fakeProvider{name: "qwen", protocol: provider.ProtocolOpenAI, models: []string{"qwen-plus"}})

	// Registry:vendor 元数据
	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendor("deepseek", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolOpenAI, "deepseek")
	reg.RegisterWithProtocolVendor("deepseek-anthropic", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolAnthropic, "deepseek")
	reg.RegisterWithProtocolVendor("qwen", func(provider.ProviderConfig) (provider.Provider, error) { return nil, nil }, provider.ProtocolOpenAI, "qwen")

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
