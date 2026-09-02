package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

func TestBuildAttemptRequest_RelayUsesImmutableClientSnapshot(t *testing.T) {
	raw := []byte("{\n  \"model\": \"client-alias\",\n  \"stream\": false,\n  \"input\": [" +
		"{\"type\":\"reasoning\",\"content\":\"synthetic\"}],\n" +
		"  \"metadata\": {\"user_id\": \"{\\\"device_id\\\":\\\"client-device\\\"}\"}\n}\n")
	base := &provider.Request{
		Method: http.MethodPost, Path: "/v1/responses", RawQuery: "beta=a%2Fb&x=1&x=2",
		Headers: http.Header{"X-Custom": {"one", "two"}}, Body: raw,
		RequestedModel: "client-alias", RoutingModel: "builtin-target", Model: "builtin-target",
		TraceID: "trace-synthetic", GatewayKeyID: "gateway-key-id",
	}

	e := engineWithRelayFlags(t, []string{"relay"}, []string{"builtin"})
	sanitizeCalls := 0
	e.fingerprintSanitizer = func(body []byte) []byte {
		sanitizeCalls++
		return append(bytes.Clone(body), ' ')
	}
	relayKey := &keypool.Key{ID: "relay-key", Key: "upstream-relay-key"}
	relayAttempt := e.buildAttemptRequest(base, &router.RouteResult{
		ProviderName: "relay", ModelID: "client-alias", Key: relayKey,
	})

	if !bytes.Equal(relayAttempt.Body, raw) {
		t.Fatalf("relay body changed:\n got: %q\nwant: %q", relayAttempt.Body, raw)
	}
	if relayAttempt.Model != "client-alias" || relayAttempt.RequestedModel != "client-alias" {
		t.Fatalf("relay models = Model:%q Requested:%q", relayAttempt.Model, relayAttempt.RequestedModel)
	}
	if relayAttempt.RawQuery != base.RawQuery {
		t.Fatalf("raw query = %q, want %q", relayAttempt.RawQuery, base.RawQuery)
	}
	if relayAttempt.Key != relayKey {
		t.Fatal("relay attempt did not bind selected key")
	}
	if sanitizeCalls != 0 {
		t.Fatalf("fingerprint sanitizer called %d times for relay", sanitizeCalls)
	}

	builtinAttempt := e.buildAttemptRequest(base, &router.RouteResult{
		ProviderName: "builtin", ModelID: "builtin-final",
	})
	if bytes.Equal(builtinAttempt.Body, raw) {
		t.Fatal("builtin candidate did not apply its existing request adaptations")
	}
	if !bytes.Contains(builtinAttempt.Body, []byte(`"model":"builtin-final"`)) {
		t.Fatalf("builtin body model not rewritten: %s", builtinAttempt.Body)
	}
	if bytes.Contains(builtinAttempt.Body, []byte(`"type":"reasoning"`)) {
		t.Fatalf("builtin Responses reasoning was not removed: %s", builtinAttempt.Body)
	}
	if sanitizeCalls != 1 {
		t.Fatalf("fingerprint sanitizer calls = %d, want 1 for builtin only", sanitizeCalls)
	}

	// A later relay candidate must derive from the original snapshot, not from
	// the body/header modified for the preceding builtin candidate.
	relayAgain := e.buildAttemptRequest(base, &router.RouteResult{ProviderName: "relay", ModelID: "client-alias"})
	if !bytes.Equal(relayAgain.Body, raw) {
		t.Fatal("builtin candidate polluted later relay body")
	}
	if !bytes.Equal(base.Body, raw) || base.Headers.Get("Authorization") != "" {
		t.Fatal("base request snapshot was mutated")
	}
	relayAttempt.Headers["X-Custom"][0] = "mutated"
	if base.Headers["X-Custom"][0] != "one" {
		t.Fatal("attempt headers share storage with immutable snapshot")
	}
}

func TestBuildAttemptRequest_LargeSyntheticRelayBodyIsByteExact(t *testing.T) {
	body := buildLargeSyntheticRelayBody(t)
	if len(body) != 1_373_051 {
		t.Fatalf("fixture size = %d, want 1373051", len(body))
	}

	var shape struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(shape.Messages) != 1510 || len(shape.Tools) != 25 {
		t.Fatalf("fixture shape = %d messages/%d tools", len(shape.Messages), len(shape.Tools))
	}

	e := engineWithRelayFlags(t, []string{"relay"}, nil)
	base := &provider.Request{
		Method: http.MethodPost, Path: "/v1/messages", Headers: http.Header{}, Body: body,
		Model: "claude-synthetic", RequestedModel: "claude-synthetic", RoutingModel: "other-model",
	}
	attempt := e.buildAttemptRequest(base, &router.RouteResult{ProviderName: "relay", ModelID: "claude-synthetic"})
	if len(attempt.Body) != len(body) || sha256.Sum256(attempt.Body) != sha256.Sum256(body) {
		t.Fatal("large relay body length/SHA-256 changed")
	}
}

func TestRelaySwapKeyChangesOnlyAttemptCredential(t *testing.T) {
	e := engineWithRelayFlags(t, []string{"relay"}, nil)
	e.gkCtx = defaultGatewayKeyContext
	now := time.Now()
	key1 := &keypool.Key{ID: "1", ProviderName: "relay", Key: "upstream-one", Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now}
	key2 := &keypool.Key{ID: "2", ProviderName: "relay", Key: "upstream-two", Status: keypool.KeyStatusActive, CreatedAt: now.Add(time.Second), UpdatedAt: now}
	e.router.SetPool("relay", keypool.NewPool("relay", []*keypool.Key{key1, key2}, nil, keypool.Config{}))

	raw := []byte("{ \"model\": \"client-model\", \"messages\": [] }\n")
	base := &provider.Request{
		Method: http.MethodPost, Path: "/v1/messages", Headers: http.Header{"X-Custom": {"one", "two"}},
		Body: raw, Model: "client-model", RequestedModel: "client-model", RoutingModel: "client-model",
	}
	result := &router.RouteResult{ProviderName: "relay", ModelID: "client-model", Key: key1, Protocol: provider.ProtocolAnthropic, Tier: "api"}
	attempt := e.buildAttemptRequest(base, result)
	wantHash := sha256.Sum256(attempt.Body)

	gin.SetMode(gin.ReleaseMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(context.Background())
	if !e.swapToOtherKey(c, attempt, result) {
		t.Fatal("swapToOtherKey returned false")
	}
	if result.Key != key2 || attempt.Key != key2 || attempt.Headers.Get("Authorization") != "Bearer upstream-two" {
		t.Fatalf("swapped credential not installed: result=%v attempt=%v auth=%q", result.Key, attempt.Key, attempt.Headers.Get("Authorization"))
	}
	if sha256.Sum256(attempt.Body) != wantHash || !bytes.Equal(attempt.Body, raw) {
		t.Fatal("same-provider key swap changed relay body")
	}
	if base.Headers.Get("Authorization") != "" || !bytes.Equal(base.Body, raw) {
		t.Fatal("key swap mutated immutable client snapshot")
	}
}

func buildLargeSyntheticRelayBody(t *testing.T) []byte {
	t.Helper()
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type tool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	type fixture struct {
		Model     string         `json:"model"`
		MaxTokens int            `json:"max_tokens"`
		Thinking  map[string]any `json:"thinking"`
		Messages  []message      `json:"messages"`
		Tools     []tool         `json:"tools"`
		Padding   string         `json:"synthetic_padding"`
	}
	f := fixture{
		Model: "claude-synthetic", MaxTokens: 64000,
		Thinking: map[string]any{"type": "adaptive"},
		Messages: make([]message, 1510), Tools: make([]tool, 25),
	}
	for i := range f.Messages {
		f.Messages[i] = message{Role: []string{"user", "assistant"}[i%2], Content: fmt.Sprintf("synthetic-message-%04d", i)}
	}
	for i := range f.Tools {
		f.Tools[i] = tool{
			Name: fmt.Sprintf("synthetic_tool_%02d", i), Description: "synthetic fixture tool",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		}
	}
	base, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	const target = 1_373_051
	if len(base) >= target {
		t.Fatalf("fixture base unexpectedly large: %d", len(base))
	}
	f.Padding = strings.Repeat("x", target-len(base))
	body, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
