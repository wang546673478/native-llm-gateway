package router

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// TestRouterReloadConfigOwnsCallerData verifies that hot-reload setters do
// not retain caller-owned maps, pointers, or nested provider slices. A
// caller may safely reuse its decode buffers after the setter returns.
func TestRouterReloadConfigOwnsCallerData(t *testing.T) {
	mgr := newFakeManager(t, &fakeProvider{
		name: "reload-provider", proto: provider.ProtocolOpenAI,
		models: []string{"reload-model"},
	})
	r := NewRouter(zap.NewNop(), mgr, nil, Config{})

	providers := []ProviderRoute{{Name: "reload-provider", Model: "reload-model", Priority: 1}}
	aliases := map[string]AliasConfig{
		"reload-alias": {Strategy: "priority", Providers: providers},
	}
	r.ReloadAliases(aliases)
	providers[0].Name = "mutated-provider"
	delete(aliases, "reload-alias")
	gotAliases := r.Aliases()
	got, ok := gotAliases["reload-alias"]
	if !ok || len(got.Providers) != 1 || got.Providers[0].Name != "reload-provider" {
		t.Fatalf("ReloadAliases retained caller data: %#v", gotAliases)
	}

	catch := &AliasConfig{Alias: "*", Providers: []ProviderRoute{{Name: "reload-provider", Model: "reload-model"}}}
	r.ReloadCatchAll(catch)
	catch.Providers[0].Name = "mutated-provider"
	catch.Providers = nil
	gotCatch := r.CatchAllConfig()
	if gotCatch == nil || len(gotCatch.Providers) != 1 || gotCatch.Providers[0].Name != "reload-provider" {
		t.Fatalf("ReloadCatchAll retained caller data: %#v", gotCatch)
	}

	order := map[string]map[string]int{"api": {"reload-provider": 3}}
	r.SetProviderOrder(order)
	order["api"]["reload-provider"] = 99
	delete(order, "api")
	r.mu.RLock()
	gotOrder := r.cfg.ProviderOrder["api"]["reload-provider"]
	r.mu.RUnlock()
	if gotOrder != 3 {
		t.Fatalf("SetProviderOrder retained caller data: got %d, want 3", gotOrder)
	}
}

// TestRouterRouteAndReloadConfigAreRaceFree exercises the same read/write
// pattern used by config hot reload and request routing. Run with -race to
// guard the ownership contract above against future direct field reads.
func TestRouterRouteAndReloadConfigAreRaceFree(t *testing.T) {
	nowPool := keypool.NewPool("reload-provider", []*keypool.Key{{
		ID: "1", ProviderName: "reload-provider", Name: "k1", Key: "synthetic",
		Status: keypool.KeyStatusActive, BillingSource: "api",
	}}, nil, keypool.Config{})
	mgr := newFakeManager(t, &fakeProvider{
		name: "reload-provider", proto: provider.ProtocolOpenAI,
		models: []string{"reload-model"},
	})
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{"reload-provider": nowPool}, Config{
		Aliases:  map[string]AliasConfig{},
		CatchAll: &AliasConfig{Alias: "*"},
	})
	req := &provider.Request{Model: "client-model", Path: "/v1/chat/completions"}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				it, err := r.Route(context.Background(), req)
				if err == nil && it != nil {
					_, _ = it.Next()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			name := "reload-provider"
			r.ReloadAliases(map[string]AliasConfig{
				"reload-alias": {Strategy: "priority", Providers: []ProviderRoute{{Name: name, Model: "reload-model"}}},
			})
			r.ReloadCatchAll(&AliasConfig{Alias: "*"})
			r.SetProviderOrder(map[string]map[string]int{"api": {name: n}})
			r.ReloadAliases(map[string]AliasConfig{})
			r.ReloadCatchAll(&AliasConfig{Alias: "*"})
		}
	}()
	wg.Wait()
}
