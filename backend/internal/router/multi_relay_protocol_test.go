package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// newSharedMultiRelayRouter deliberately registers one Provider pointer under
// two protocol faces and exposes only the vendor pool. This is the shape that
// used to leak the primary protocol into the secondary face and lose the pool
// lookup by face name.
func newSharedMultiRelayRouter(t *testing.T, primary provider.Protocol) (*Router, string, string, *keypool.Pool) {
	t.Helper()
	const (
		vendor   = "shared-relay"
		openFace = vendor + "-openai"
		anthFace = vendor + "-anthropic"
		model    = "shared-model"
	)
	shared := &fakeProvider{name: "shared-implementation", proto: primary, models: []string{model}}
	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(openFace, func(provider.ProviderConfig) (provider.Provider, error) {
		return shared, nil
	}, provider.ProtocolOpenAI, vendor, true)
	reg.RegisterWithProtocolVendorRelay(anthFace, func(provider.ProviderConfig) (provider.Provider, error) {
		return shared, nil
	}, provider.ProtocolAnthropic, vendor, true)

	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		openFace: {Enabled: true, Endpoint: "http://relay.invalid", Protocol: provider.ProtocolOpenAI, BillingSource: "api"},
		anthFace: {Enabled: true, Endpoint: "http://relay.invalid", Protocol: provider.ProtocolAnthropic, BillingSource: "api"},
	}}); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	if err := mgr.LoadModelsFromStore(context.Background(), fakeModelStore{
		rows: []provider.DBModelRow{{Vendor: vendor, ModelID: model}},
		faceRows: []provider.DBFaceRow{
			{Vendor: vendor, Face: openFace, ModelID: model, SortOrder: 0},
			{Vendor: vendor, Face: anthFace, ModelID: model, SortOrder: 0},
		},
	}); err != nil {
		t.Fatalf("LoadModelsFromStore: %v", err)
	}

	now := timeNowForMultiRelayTest()
	pool := keypool.NewPool(vendor, []*keypool.Key{
		{ID: "101", ProviderName: vendor, Name: "open-key", Key: "synthetic-open", Protocols: string(provider.ProtocolOpenAI), Status: keypool.KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "102", ProviderName: vendor, Name: "anth-key", Key: "synthetic-anth", Protocols: string(provider.ProtocolAnthropic), Status: keypool.KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
	}, nil, keypool.Config{})
	// Intentionally omit openFace/anthFace aliases. Router must resolve the
	// shared vendor pool through Manager.VendorFor.
	r := NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{vendor: pool}, Config{})
	return r, openFace, anthFace, pool
}

// TestRouterRejectsUnknownPathBeforeCandidates guards the early protocol
// gate.  An unsupported URL must fail locally instead of fanning out across
// every relay face and letting a provider return an invalid-request error.
func TestRouterRejectsUnknownPathBeforeCandidates(t *testing.T) {
	r, _, _, _ := newSharedMultiRelayRouter(t, provider.ProtocolOpenAI)
	r.catchAll = &AliasConfig{Alias: "*"}

	for _, path := range []string{"", "/v1/embeddings", "/v1/messages-extra", "/proxy/unknown"} {
		t.Run(path, func(t *testing.T) {
			it, err := r.Route(context.Background(), &provider.Request{Model: "shared-model", Path: path})
			if !errors.Is(err, ErrNoRoute) {
				t.Fatalf("Route(%q) error = %v, want ErrNoRoute", path, err)
			}
			if it != nil {
				t.Fatalf("Route(%q) returned an iterator on an unsupported path", path)
			}
		})
	}
}

// TestRouterRecognizesOpenAICompletions keeps the legacy OpenAI Completions
// endpoint in the shared path matcher.  It must select the OpenAI face and
// an OpenAI-compatible key just like /v1/responses.
func TestRouterRecognizesOpenAICompletions(t *testing.T) {
	r, openFace, _, _ := newSharedMultiRelayRouter(t, provider.ProtocolAnthropic)
	it, err := r.Route(context.Background(), &provider.Request{Model: "shared-model", Path: "/v1/completions"})
	if err != nil {
		t.Fatalf("Route(/v1/completions): %v", err)
	}
	res, err := it.Next()
	if err != nil {
		t.Fatalf("Next(/v1/completions): %v", err)
	}
	if res.ProviderName != openFace || res.Protocol != provider.ProtocolOpenAI || res.Key == nil || res.Key.ID != "101" {
		t.Fatalf("route = %#v, want OpenAI face/key101", res)
	}
}

// Keep test timestamps deterministic without importing time in every table.
// A tiny helper also makes it obvious that no wall-clock behavior is under test.
func timeNowForMultiRelayTest() time.Time { return time.Unix(1, 0) }

func TestMultiRelayFacesUseFaceProtocolAndSharedVendorPool(t *testing.T) {
	for _, primary := range []provider.Protocol{provider.ProtocolOpenAI, provider.ProtocolAnthropic} {
		t.Run("primary="+string(primary), func(t *testing.T) {
			r, openFace, anthFace, _ := newSharedMultiRelayRouter(t, primary)

			cases := []struct {
				name      string
				path      string
				wantFace  string
				wantProto provider.Protocol
				wantKeyID string
			}{
				{name: "catch-all anthropic messages", path: "/v1/messages", wantFace: anthFace, wantProto: provider.ProtocolAnthropic, wantKeyID: "102"},
				{name: "catch-all openai responses", path: "/v1/responses", wantFace: openFace, wantProto: provider.ProtocolOpenAI, wantKeyID: "101"},
			}
			r.catchAll = &AliasConfig{Alias: "*"}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					it, err := r.Route(context.Background(), &provider.Request{Model: "shared-model", Path: tc.path})
					if err != nil {
						t.Fatalf("Route: %v", err)
					}
					res, err := it.Next()
					if err != nil {
						t.Fatalf("Next: %v", err)
					}
					if res.ProviderName != tc.wantFace || res.Protocol != tc.wantProto || res.Key == nil || res.Key.ID != tc.wantKeyID {
						t.Fatalf("route = %#v, want face=%s protocol=%s key=%s", res, tc.wantFace, tc.wantProto, tc.wantKeyID)
					}
				})
			}
		})
	}
}

func TestMultiRelayFacesFilterExplicitAliasAndDirectModel(t *testing.T) {
	for _, primary := range []provider.Protocol{provider.ProtocolOpenAI, provider.ProtocolAnthropic} {
		t.Run("primary="+string(primary), func(t *testing.T) {
			r, openFace, anthFace, _ := newSharedMultiRelayRouter(t, primary)
			r.aliases = map[string]AliasConfig{
				"alias-model": {
					Strategy: "priority",
					Providers: []ProviderRoute{
						{Name: openFace, Model: "configured-openai-model", Priority: 1},
						{Name: anthFace, Model: "configured-anthropic-model", Priority: 2},
					},
				},
			}

			// The alias is the routing key, while RequestedModel is the model
			// that a relay must preserve and use for face-model filtering.
			it, err := r.Route(context.Background(), &provider.Request{
				Model: "alias-model", RoutingModel: "alias-model", RequestedModel: "shared-model", Path: "/v1/messages",
			})
			if err != nil {
				t.Fatalf("explicit alias Route: %v", err)
			}
			res, err := it.Next()
			if err != nil {
				t.Fatalf("explicit alias Next: %v", err)
			}
			if res.ProviderName != anthFace || res.Protocol != provider.ProtocolAnthropic || res.ModelID != "shared-model" || res.Key == nil || res.Key.ID != "102" {
				t.Fatalf("explicit alias route = %#v, want anthropic face/model/key2", res)
			}

			// Direct model lookup must apply the same face protocol filter.
			r.aliases = map[string]AliasConfig{}
			it, err = r.Route(context.Background(), &provider.Request{Model: "shared-model", Path: "/v1/responses"})
			if err != nil {
				t.Fatalf("direct Route: %v", err)
			}
			res, err = it.Next()
			if err != nil {
				t.Fatalf("direct Next: %v", err)
			}
			if res.ProviderName != openFace || res.Protocol != provider.ProtocolOpenAI || res.Key == nil || res.Key.ID != "101" {
				t.Fatalf("direct route = %#v, want openai face/key1", res)
			}
		})
	}
}

func TestMultiRelayGatewayKeyProtocolAndIDRestrictions(t *testing.T) {
	r, openFace, anthFace, _ := newSharedMultiRelayRouter(t, provider.ProtocolOpenAI)
	// An OpenAI-only allowed ID must not leak into an Anthropic request, even
	// though both faces share one vendor pool.
	tunable := func(ids []uint, path string) (*RouteResult, error) {
		it, err := r.Route(context.Background(), &provider.Request{Model: "shared-model", Path: path}, WithProviderKeyIDs(ids))
		if err != nil {
			return nil, err
		}
		return it.Next()
	}
	if res, err := tunable([]uint{101}, "/v1/messages"); err == nil || res != nil {
		t.Fatalf("openai key crossed into anthropic face: result=%#v err=%v", res, err)
	}
	res, err := tunable([]uint{102}, "/v1/messages")
	if err != nil || res == nil || res.ProviderName != anthFace || res.Key == nil || res.Key.ID != "102" || res.Protocol != provider.ProtocolAnthropic {
		t.Fatalf("anthropic restricted route = %#v err=%v, want %s/key102", res, err, anthFace)
	}
	if res, err := tunable([]uint{102}, "/v1/responses"); err == nil || res != nil {
		t.Fatalf("anthropic key crossed into openai face: result=%#v err=%v", res, err)
	}
	res, err = tunable([]uint{101}, "/v1/responses")
	if err != nil || res == nil || res.ProviderName != openFace || res.Key == nil || res.Key.ID != "101" || res.Protocol != provider.ProtocolOpenAI {
		t.Fatalf("openai restricted route = %#v err=%v, want %s/key101", res, err, openFace)
	}
}
