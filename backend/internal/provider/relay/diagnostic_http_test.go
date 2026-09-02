package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func newRelayDiagnosticPool(t *testing.T) *keypool.Pool {
	t.Helper()
	now := time.Now()
	return keypool.NewPool("diag-relay", []*keypool.Key{
		{ID: "key-1", ProviderName: "diag-relay", Name: "first", Key: "relay-key-1", Status: keypool.KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "key-2", ProviderName: "diag-relay", Name: "second", Key: "relay-key-2", Status: keypool.KeyStatusActive, BillingSource: "api", CreatedAt: now.Add(time.Millisecond), UpdatedAt: now.Add(time.Millisecond)},
	}, nil, keypool.Config{})
}

func TestGenericRelayDiagnoseKey_MultiProtocolUsesMatchingFaceAndLeavesPoolUnchanged(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	type seenRequest struct {
		path string
		auth string
		body []byte
	}
	var seen []seenRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: append([]byte(nil), body...)})
		mu.Unlock()
		if r.URL.Path == "/v1/messages" {
			if got := r.Header.Get("x-api-key"); got != "relay-key-1" {
				t.Errorf("Anthropic x-api-key = %q, want relay-key-1", got)
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected relay diagnostic path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer relay-key-2" {
			t.Errorf("OpenAI Authorization = %q, want Bearer relay-key-2", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer upstream.Close()

	relayProvider, err := NewGenericRelayProvider(Config{
		Name: "diag-relay", BaseURL: upstream.URL, ProtocolMode: "multi",
		PrimaryProtocol:    provider.ProtocolOpenAI,
		SupportedProtocols: []provider.Protocol{provider.ProtocolOpenAI, provider.ProtocolAnthropic},
		Timeout:            5,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	pool := newRelayDiagnosticPool(t)
	relayProvider.SetPool(pool)
	before := pool.Keys()
	keys := pool.KeyPtrs()

	authResult, err := relayProvider.DiagnoseKey(context.Background(), keys[0], provider.KeyDiagnosticRequest{
		Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("Anthropic DiagnoseKey: %v", err)
	}
	if authResult == nil || authResult.StatusCode != http.StatusForbidden || authResult.ErrorType != provider.ErrorTypeAuth {
		t.Fatalf("Anthropic result = %+v, want 403/auth", authResult)
	}

	openAIResult, err := relayProvider.DiagnoseKey(context.Background(), keys[1], provider.KeyDiagnosticRequest{
		Protocol: provider.ProtocolOpenAI, Path: "/v1/responses", Model: "gpt-diagnostic",
	})
	if err != nil {
		t.Fatalf("OpenAI DiagnoseKey: %v", err)
	}
	if openAIResult == nil || openAIResult.StatusCode != http.StatusOK || !openAIResult.Reachable {
		t.Fatalf("OpenAI result = %+v, want 200/reachable", openAIResult)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want one per explicit key", got)
	}
	mu.Lock()
	gotSeen := append([]seenRequest(nil), seen...)
	mu.Unlock()
	if len(gotSeen) != 2 || gotSeen[0].path != "/v1/messages" || gotSeen[1].path != "/v1/responses" {
		t.Fatalf("seen relay requests = %+v", gotSeen)
	}
	var openAIBody map[string]any
	if err := json.Unmarshal(gotSeen[1].body, &openAIBody); err != nil {
		t.Fatalf("decode Responses diagnostic body: %v", err)
	}
	if openAIBody["input"] != "diagnostic" || openAIBody["messages"] != nil {
		t.Errorf("Responses diagnostic body = %s, want input only", gotSeen[1].body)
	}
	if !reflect.DeepEqual(pool.Keys(), before) {
		t.Fatalf("relay diagnostic changed pool: before=%+v after=%+v", before, pool.Keys())
	}
}

func TestGenericRelayDiagnoseKey_UnsupportedProtocolOrPathDoesNotSendRequest(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()

	relayProvider, err := NewGenericRelayProvider(Config{
		Name: "diag-relay-openai", BaseURL: upstream.URL, ProtocolMode: "single",
		PrimaryProtocol: provider.ProtocolOpenAI, SupportedProtocols: []provider.Protocol{provider.ProtocolOpenAI}, Timeout: 5,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	pool := newRelayDiagnosticPool(t)
	relayProvider.SetPool(pool)
	key := pool.KeyPtrs()[0]
	for _, tc := range []struct {
		name string
		req  provider.KeyDiagnosticRequest
	}{
		{name: "unsupported protocol", req: provider.KeyDiagnosticRequest{Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "m"}},
		{name: "cross protocol path", req: provider.KeyDiagnosticRequest{Protocol: provider.ProtocolOpenAI, Path: "/v1/messages", Model: "m"}},
		{name: "unknown path", req: provider.KeyDiagnosticRequest{Protocol: provider.ProtocolOpenAI, Path: "/v1/unknown", Model: "m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := relayProvider.DiagnoseKey(context.Background(), key, tc.req)
			if result != nil || !provider.IsDiagnosticUnavailable(err) {
				t.Fatalf("result=%+v err=%v, want typed diagnostic_unavailable", result, err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0 for unsupported diagnostics", got)
	}
}
