package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRelayRequest_PreservesBodyHeadersAndRawQuery(t *testing.T) {
	for _, tt := range []struct {
		name     string
		protocol provider.Protocol
		path     string
		stream   bool
	}{
		{name: "anthropic non-stream", protocol: provider.ProtocolAnthropic, path: "/v1/messages"},
		{name: "anthropic stream", protocol: provider.ProtocolAnthropic, path: "/v1/messages", stream: true},
		{name: "openai responses non-stream", protocol: provider.ProtocolOpenAI, path: "/v1/responses"},
		{name: "openai responses stream", protocol: provider.ProtocolOpenAI, path: "/v1/responses", stream: true},
		{name: "openai chat non-stream", protocol: provider.ProtocolOpenAI, path: "/v1/chat/completions"},
		{name: "openai chat stream", protocol: provider.ProtocolOpenAI, path: "/v1/chat/completions", stream: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var rawBody []byte
			if tt.protocol == provider.ProtocolAnthropic {
				rawBody = []byte(fmt.Sprintf(`{
  "model": "synthetic-model",
  "max_tokens": 256,
  "stream": %t,
  "thinking": {"type":"adaptive"},
  "context_management": {"edits":[{"type":"clear_tool_uses_20250919"}]},
  "tools": [{"name":"synthetic_tool","input_schema":{"type":"object"}}],
  "messages": [{"role":"user","content":"synthetic"}]
}
`, tt.stream))
			} else {
				rawBody = []byte(fmt.Sprintf(`{
  "model": "synthetic-model",
  "stream": %t,
  "reasoning": {"effort":"high"},
  "tools": [{"type":"function","name":"synthetic_tool"}],
  "input": [{"role":"user","content":"synthetic"}]
}
`, tt.stream))
			}
			type captured struct {
				body     []byte
				headers  http.Header
				rawQuery string
			}
			gotCh := make(chan captured, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotCh <- captured{body: body, headers: r.Header.Clone(), rawQuery: r.URL.RawQuery}
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()

			relayProvider, key := newPassthroughTestRelay(t, upstream.URL, tt.protocol)
			defer relayProvider.Close()
			req := &provider.Request{
				Method: http.MethodPost, Path: tt.path, RawQuery: "beta=a%2Fb&x=1&x=2",
				Body: rawBody, IsStream: tt.stream, Key: key, TraceID: "generated-trace",
				Headers: http.Header{
					"Accept":              {"application/vnd.client"},
					"Accept-Language":     {"zh-CN", "en-US"},
					"Anthropic-Beta":      {"feature-a", "feature-b"},
					"Anthropic-Version":   {"2099-01-01"},
					"Authorization":       {"Bearer gateway-credential"},
					"Connection":          {"X-Connection-Only"},
					"Content-Type":        {"application/vnd.client+json"},
					"Cookie":              {"admin=session"},
					"Proxy-Authorization": {"Basic gateway-proxy"},
					"User-Agent":          {"synthetic-client/1.0"},
					"X-Admin-Token":       {"admin-token"},
					"X-Api-Key":           {"gateway-key"},
					"X-Connection-Only":   {"remove-me"},
					"X-Custom-Extension":  {"one", "two"},
					"X-Forwarded-For":     {"203.0.113.10"},
					"X-Request-Id":        {"client-trace"},
				},
			}

			if tt.stream {
				chunks, _, err := relayProvider.SendStreamRequest(context.Background(), req)
				if err != nil {
					t.Fatalf("SendStreamRequest: %v", err)
				}
				for range chunks {
				}
			} else if _, err := relayProvider.SendRequest(context.Background(), req); err != nil {
				t.Fatalf("SendRequest: %v", err)
			}

			got := <-gotCh
			if !bytes.Equal(got.body, rawBody) {
				t.Fatalf("body changed:\n got: %q\nwant: %q", got.body, rawBody)
			}
			if got.rawQuery != req.RawQuery {
				t.Fatalf("raw query = %q, want %q", got.rawQuery, req.RawQuery)
			}
			for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Admin-Token", "X-Connection-Only", "X-Forwarded-For"} {
				if got.headers.Get(name) != "" {
					t.Errorf("denied header %s reached upstream", name)
				}
			}
			if got.headers.Get("Content-Type") != "application/vnd.client+json" ||
				got.headers.Get("Accept") != "application/vnd.client" ||
				got.headers.Get("User-Agent") != "synthetic-client/1.0" ||
				got.headers.Get("Anthropic-Version") != "2099-01-01" ||
				got.headers.Get("X-Request-Id") != "client-trace" {
				t.Errorf("client end-to-end headers changed: %#v", got.headers)
			}
			if !reflect.DeepEqual(got.headers.Values("Anthropic-Beta"), []string{"feature-a", "feature-b"}) ||
				!reflect.DeepEqual(got.headers.Values("X-Custom-Extension"), []string{"one", "two"}) {
				t.Errorf("multi-value extension headers changed: %#v", got.headers)
			}
			if tt.protocol == provider.ProtocolAnthropic {
				if got.headers.Get("x-api-key") != "upstream-key" || got.headers.Get("Authorization") != "" {
					t.Errorf("anthropic upstream credentials incorrect: %#v", got.headers)
				}
			} else if got.headers.Get("Authorization") != "Bearer upstream-key" || got.headers.Get("x-api-key") != "" {
				t.Errorf("openai upstream credentials incorrect: %#v", got.headers)
			}
		})
	}
}

func TestRelayRequest_AddsOnlyMissingProtocolHeaderDefaults(t *testing.T) {
	for _, tt := range []struct {
		name       string
		protocol   provider.Protocol
		path       string
		wantAuth   string
		wantAPIKey string
	}{
		{
			name: "anthropic", protocol: provider.ProtocolAnthropic, path: "/v1/messages",
			wantAPIKey: "upstream-key",
		},
		{
			name: "openai", protocol: provider.ProtocolOpenAI, path: "/v1/responses",
			wantAuth: "Bearer upstream-key",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gotCh := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotCh <- r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {}\n\n")
			}))
			defer upstream.Close()

			relayProvider, key := newPassthroughTestRelay(t, upstream.URL, tt.protocol)
			defer relayProvider.Close()
			chunks, _, err := relayProvider.SendStreamRequest(context.Background(), &provider.Request{
				Method: http.MethodPost, Path: tt.path, Headers: http.Header{},
				Body: []byte(`{"model":"synthetic-model","stream":true}`), Key: key,
			})
			if err != nil {
				t.Fatalf("SendStreamRequest: %v", err)
			}
			for range chunks {
			}

			got := <-gotCh
			if got.Get("Content-Type") != "application/json" || got.Get("Accept") != "text/event-stream" {
				t.Fatalf("protocol defaults missing: %#v", got)
			}
			if got.Get("Authorization") != tt.wantAuth || got.Get("x-api-key") != tt.wantAPIKey {
				t.Fatalf("upstream credentials incorrect: %#v", got)
			}
			if tt.protocol == provider.ProtocolAnthropic && got.Get("anthropic-version") != "2023-06-01" {
				t.Fatalf("anthropic-version default = %q", got.Get("anthropic-version"))
			}
		})
	}
}

func TestRelayRequest_DoesNotAutoDecompressGzipResponse(t *testing.T) {
	plain := []byte(`{"id":"synthetic-response"}`)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(plain)
	_ = zw.Close()
	wantBody := bytes.Clone(compressed.Bytes())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(wantBody)
	}))
	defer upstream.Close()

	relayProvider, key := newPassthroughTestRelay(t, upstream.URL, provider.ProtocolOpenAI)
	defer relayProvider.Close()
	resp, err := relayProvider.SendRequest(context.Background(), &provider.Request{
		Method: http.MethodPost, Path: "/v1/responses", Headers: http.Header{},
		Body: []byte(`{"model":"synthetic-model"}`), Key: key,
	})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if resp.Headers.Get("Content-Encoding") != "gzip" || !bytes.Equal(resp.Body, wantBody) {
		t.Fatalf("gzip response changed: encoding=%q body_equal=%v", resp.Headers.Get("Content-Encoding"), bytes.Equal(resp.Body, wantBody))
	}
}

func TestRelayRequest_ErrorCarriesUpstreamHeaders(t *testing.T) {
	for _, protocol := range []provider.Protocol{provider.ProtocolAnthropic, provider.ProtocolOpenAI} {
		t.Run(string(protocol), func(t *testing.T) {
			raw := []byte(`{"error":"synthetic failure"}`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Upstream-Error", "one")
				w.Header().Add("X-Upstream-Error", "two")
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(raw)
			}))
			defer upstream.Close()

			relayProvider, key := newPassthroughTestRelay(t, upstream.URL, protocol)
			defer relayProvider.Close()
			req := &provider.Request{Method: http.MethodPost, Headers: http.Header{}, Body: []byte(`{"model":"synthetic-model"}`), Key: key}
			if protocol == provider.ProtocolAnthropic {
				req.Path = "/v1/messages"
			} else {
				req.Path = "/v1/responses"
			}
			_, err := relayProvider.SendRequest(context.Background(), req)
			var pe *provider.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v, want ProviderError", err)
			}
			if pe.StatusCode != http.StatusServiceUnavailable || !bytes.Equal(pe.RawError, raw) {
				t.Fatalf("provider error = status %d body %q", pe.StatusCode, pe.RawError)
			}
			if got := pe.UpstreamHeaders.Values("X-Upstream-Error"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
				t.Fatalf("upstream error headers = %#v", pe.UpstreamHeaders)
			}
		})
	}
}

func TestRelayRequest_HTTP200EmbeddedErrorRemainsRaw(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1008,"status_msg":"synthetic quota marker"}}`)
	for _, protocol := range []provider.Protocol{provider.ProtocolAnthropic, provider.ProtocolOpenAI} {
		t.Run(string(protocol), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(raw)
			}))
			defer upstream.Close()

			relayProvider, key := newPassthroughTestRelay(t, upstream.URL, protocol)
			defer relayProvider.Close()
			req := &provider.Request{
				Method: http.MethodPost, Headers: http.Header{},
				Body: []byte(`{"model":"synthetic-model"}`), Key: key,
			}
			if protocol == provider.ProtocolAnthropic {
				req.Path = "/v1/messages"
			} else {
				req.Path = "/v1/responses"
			}
			resp, err := relayProvider.SendRequest(context.Background(), req)
			if err != nil {
				t.Fatalf("SendRequest returned embedded relay error: %v", err)
			}
			if resp.StatusCode != http.StatusOK || !bytes.Equal(resp.Body, raw) {
				t.Fatalf("response = status %d body %q, want raw HTTP 200", resp.StatusCode, resp.Body)
			}
		})
	}
}

func newPassthroughTestRelay(t *testing.T, baseURL string, protocol provider.Protocol) (*GenericRelayProvider, *keypool.Key) {
	t.Helper()
	relayProvider, err := NewGenericRelayProvider(Config{
		Name: "relay-request-test", BaseURL: baseURL,
		ProtocolMode: "single", PrimaryProtocol: protocol, Timeout: 5,
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	key := &keypool.Key{
		ID: "synthetic-key-id", ProviderName: relayProvider.Name(), Key: "upstream-key",
		Status: keypool.KeyStatusActive, Protocols: string(protocol),
	}
	relayProvider.SetPool(keypool.NewPool(relayProvider.Name(), []*keypool.Key{key}, nil, keypool.Config{}))
	return relayProvider, key
}
