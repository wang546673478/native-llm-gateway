package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	providerrelay "github.com/wang546673478/native-llm-gateway/internal/provider/relay"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

func TestHandle_RelayKeepsOriginalBodyAndAccessLogSnapshot(t *testing.T) {
	raw := []byte("{\n  \"model\": \"client-alias\",\n  \"input\": [{\"type\":\"reasoning\",\"content\":\"synthetic\"}],\n" +
		"  \"metadata\": {\"user_id\": \"{\\\"device_id\\\":\\\"client-device\\\"}\"}\n}\n")
	relayProvider := &fakeProvider{
		name: "relay-snapshot", proto: provider.ProtocolOpenAI,
		respStatus: http.StatusOK, respBody: `{"ok":true}`,
	}

	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(relayProvider.name, func(provider.ProviderConfig) (provider.Provider, error) {
		return relayProvider, nil
	}, relayProvider.proto, relayProvider.name, true)
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		relayProvider.name: {Enabled: true, Protocol: relayProvider.proto},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pool := keypool.NewPool(relayProvider.name, []*keypool.Key{{
		ID: "relay-key", ProviderName: relayProvider.name, Key: "upstream-key",
		Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
	rtr := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{relayProvider.name: pool}, router.Config{
		Aliases: map[string]router.AliasConfig{"client-alias": {
			Providers: []router.ProviderRoute{{Name: relayProvider.name, Model: "must-not-be-used"}},
		}},
	})
	bodyDir := t.TempDir()
	accessRecorder, err := accesslog.NewRecorder(accesslog.RecorderConfig{Enabled: true, BodyDir: bodyDir}, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	sanitizeCalls := 0
	engine := NewEngine(Config{
		Router: rtr, Logger: zap.NewNop(), AccessLog: accessRecorder,
		FingerprintSanitizer: func(body []byte) []byte {
			sanitizeCalls++
			return []byte(`{"fingerprint":"changed"}`)
		},
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.POST("/v1/responses", engine.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?beta=a%2Fb", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "trace-snapshot-test")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(relayProvider.gotBody, raw) {
		t.Fatalf("relay got changed body:\n got: %q\nwant: %q", relayProvider.gotBody, raw)
	}
	if sanitizeCalls != 0 {
		t.Fatalf("fingerprint sanitizer called %d times for relay", sanitizeCalls)
	}

	var logged []byte
	err = filepath.WalkDir(bodyDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.Contains(d.Name(), "-req") {
			logged, walkErr = os.ReadFile(path)
			return walkErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(logged, raw) {
		t.Fatalf("access log request snapshot changed:\n got: %q\nwant: %q", logged, raw)
	}
}

func TestHandle_LargeSyntheticBodyReachesRelayByteExact(t *testing.T) {
	body := buildLargeSyntheticRelayBody(t)
	wantHash := sha256.Sum256(body)
	type capture struct {
		length   int
		hash     [sha256.Size]byte
		rawQuery string
		method   string
		path     string
	}
	captured := make(chan capture, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		got, _ := io.ReadAll(r.Body)
		captured <- capture{
			length: len(got), hash: sha256.Sum256(got), rawQuery: r.URL.RawQuery,
			method: r.Method, path: r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"synthetic-success"}`)
	}))
	defer upstream.Close()

	relayProvider, err := providerrelay.NewGenericRelayProvider(providerrelay.Config{
		Name: "relay-large-snapshot", BaseURL: upstream.URL,
		ProtocolMode: "single", PrimaryProtocol: provider.ProtocolAnthropic, Timeout: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relayProvider.Close()
	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(relayProvider.Name(), func(provider.ProviderConfig) (provider.Provider, error) {
		return relayProvider, nil
	}, provider.ProtocolAnthropic, relayProvider.Name(), true)
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		relayProvider.Name(): {Enabled: true, Protocol: provider.ProtocolAnthropic},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	key := &keypool.Key{
		ID: "1", ProviderName: relayProvider.Name(), Key: "synthetic-upstream-key",
		Status: keypool.KeyStatusActive, BillingSource: "api", Protocols: string(provider.ProtocolAnthropic),
		CreatedAt: now, UpdatedAt: now,
	}
	pool := keypool.NewPool(relayProvider.Name(), []*keypool.Key{key}, nil, keypool.Config{})
	relayProvider.SetPool(pool)
	rtr := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{relayProvider.Name(): pool}, router.Config{
		Aliases: map[string]router.AliasConfig{"claude-synthetic": {
			Providers: []router.ProviderRoute{{Name: relayProvider.Name(), Model: "ignored-for-relay"}},
		}},
	})
	engine := NewEngine(Config{Router: rtr, Logger: zap.NewNop()})
	metrics := &relayTTFTMetrics{}
	engine.metrics = metrics

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.POST("/v1/messages", engine.HandleRequest)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=a%2Fb&x=1&x=2", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := <-captured
	if extra := len(captured); extra != 0 {
		t.Fatalf("unexpected extra upstream requests = %d; first=%s %s", extra, got.method, got.path)
	}
	if got.length != len(body) || got.hash != wantHash {
		t.Fatalf("relay upstream body mismatch: length=%d/%d hash_equal=%v", got.length, len(body), got.hash == wantHash)
	}
	if got.rawQuery != "beta=a%2Fb&x=1&x=2" {
		t.Fatalf("raw query = %q", got.rawQuery)
	}
	if got := metrics.eventCount(relayProvider.Name(), "body_mismatch", "request"); got != 0 {
		t.Fatalf("large synthetic body mismatch events = %d, want 0", got)
	}
	if got := metrics.eventCount(relayProvider.Name(), "candidate_attempt", "none"); got != 1 {
		t.Fatalf("large synthetic candidate attempts = %d, want 1", got)
	}
	if got := metrics.activeCount(relayProvider.Name()); got != 0 {
		t.Fatalf("large synthetic active upstreams = %d, want 0", got)
	}
}
