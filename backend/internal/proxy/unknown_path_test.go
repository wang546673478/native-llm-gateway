package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/relay"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// TestUnknownPathDoesNotReachRelay verifies the full Engine -> Router guard.
// The real relay HTTP implementation is wired to a counting httptest server;
// an unsupported Gateway path must be rejected before any upstream round trip.
func TestUnknownPathDoesNotReachRelay(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	const providerName = "unknown-path-relay"
	rp, err := relay.NewGenericRelayProvider(relay.Config{
		Name: providerName, BaseURL: upstream.URL, ProtocolMode: "single",
		PrimaryProtocol: provider.ProtocolOpenAI,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	defer rp.Close()

	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(providerName, func(provider.ProviderConfig) (provider.Provider, error) {
		return rp, nil
	}, provider.ProtocolOpenAI, providerName, true)
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		providerName: {Enabled: true, Endpoint: upstream.URL, Protocol: provider.ProtocolOpenAI},
	}}); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	// Manager.LoadFromConfig performs a best-effort health check.  Exclude
	// that startup probe from the request-path assertion below.
	upstreamCalls.Store(0)
	now := timeNowForUnknownPathTest()
	pool := keypool.NewPool(providerName, []*keypool.Key{{
		ID: "1", ProviderName: providerName, Key: "synthetic-key",
		Protocols: string(provider.ProtocolOpenAI), Status: keypool.KeyStatusActive,
		BillingSource: string(keypool.BillingSourceAPI), CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
	rp.SetPool(pool)
	rtr := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{providerName: pool}, router.Config{
		Aliases: map[string]router.AliasConfig{
			"synthetic-model": {Providers: []router.ProviderRoute{{Name: providerName}}},
		},
	})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop()})

	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	// Mirror server.registerRoutes' /v1/* NoRoute fallback.
	g.NoRoute(engine.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"synthetic-model"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("unsupported path reached relay %d time(s), want 0", got)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("unsupported path unexpectedly returned success: %d", w.Code)
	}
}

func timeNowForUnknownPathTest() time.Time { return time.Unix(1, 0) }
