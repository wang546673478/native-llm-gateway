package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// TestGenericRelayProvider_NormalizesRequestURL verifies the public relay
// request path instead of a relay-local normalization helper. URL joining is
// owned by the compatible protocol bases, so this test guards the complete
// constructor -> protocol selection -> upstream request chain.
func TestGenericRelayProvider_NormalizesRequestURL(t *testing.T) {
	tests := []struct {
		name        string
		baseSuffix  string
		protocol    provider.Protocol
		requestPath string
		wantPath    string
	}{
		{
			name:        "OpenAI without version suffix",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/chat/completions",
			wantPath:    "/v1/chat/completions",
		},
		{
			name:        "OpenAI without version suffix with trailing slash",
			baseSuffix:  "/",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/chat/completions",
			wantPath:    "/v1/chat/completions",
		},
		{
			name:        "OpenAI with version suffix",
			baseSuffix:  "/v1",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/chat/completions",
			wantPath:    "/v1/chat/completions",
		},
		{
			name:        "OpenAI with version suffix and trailing slash",
			baseSuffix:  "/v1/",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/chat/completions",
			wantPath:    "/v1/chat/completions",
		},
		{
			name:        "OpenAI with path prefix and version suffix",
			baseSuffix:  "/prefix/v1/",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/chat/completions",
			wantPath:    "/prefix/v1/chat/completions",
		},
		{
			name:        "OpenAI Responses without version suffix",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/responses",
			wantPath:    "/v1/responses",
		},
		{
			name:        "OpenAI Responses with version suffix",
			baseSuffix:  "/v1",
			protocol:    provider.ProtocolOpenAI,
			requestPath: "/v1/responses",
			wantPath:    "/v1/responses",
		},
		{
			name:        "Anthropic without version suffix",
			protocol:    provider.ProtocolAnthropic,
			requestPath: "/v1/messages",
			wantPath:    "/v1/messages",
		},
		{
			name:        "Anthropic without version suffix with trailing slash",
			baseSuffix:  "/",
			protocol:    provider.ProtocolAnthropic,
			requestPath: "/v1/messages",
			wantPath:    "/v1/messages",
		},
		{
			name:        "Anthropic with version suffix",
			baseSuffix:  "/v1",
			protocol:    provider.ProtocolAnthropic,
			requestPath: "/v1/messages",
			wantPath:    "/v1/messages",
		},
		{
			name:        "Anthropic with version suffix and trailing slash",
			baseSuffix:  "/v1/",
			protocol:    provider.ProtocolAnthropic,
			requestPath: "/v1/messages",
			wantPath:    "/v1/messages",
		},
		{
			name:        "Anthropic with path prefix",
			baseSuffix:  "/prefix",
			protocol:    provider.ProtocolAnthropic,
			requestPath: "/v1/messages",
			wantPath:    "/prefix/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pathCh := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pathCh <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"upstream-ok"}`))
			}))
			defer upstream.Close()

			relayProvider, err := NewGenericRelayProvider(Config{
				Name:            "relay-url-test",
				BaseURL:         upstream.URL + tt.baseSuffix,
				ProtocolMode:    "single",
				PrimaryProtocol: tt.protocol,
				Timeout:         5,
			})
			if err != nil {
				t.Fatalf("NewGenericRelayProvider: %v", err)
			}
			defer relayProvider.Close()

			now := time.Now()
			key := &keypool.Key{
				ID: "1", ProviderName: "relay-url-test", Name: "test-key", Key: "sk-test",
				Status: keypool.KeyStatusActive, CreatedAt: now, UpdatedAt: now,
			}
			pool := keypool.NewPool("relay-url-test", []*keypool.Key{key}, nil, keypool.Config{})
			relayProvider.SetPool(pool)

			resp, err := relayProvider.SendRequest(context.Background(), &provider.Request{
				Method:  http.MethodPost,
				Path:    tt.requestPath,
				Headers: make(http.Header),
				Body:    []byte(`{"model":"test","messages":[]}`),
				Key:     key,
			})
			if err != nil {
				t.Fatalf("SendRequest: %v", err)
			}
			if resp == nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("response = %+v, want status 200", resp)
			}

			select {
			case gotPath := <-pathCh:
				if gotPath != tt.wantPath {
					t.Errorf("upstream path = %q, want %q", gotPath, tt.wantPath)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream did not receive request")
			}
		})
	}
}

func TestNewGenericRelayProvider_RejectsEmptyBaseURL(t *testing.T) {
	_, err := NewGenericRelayProvider(Config{
		Name:            "relay-url-test",
		ProtocolMode:    "single",
		PrimaryProtocol: provider.ProtocolOpenAI,
	})
	if err == nil {
		t.Fatal("NewGenericRelayProvider accepted an empty base URL")
	}
}

// TestGenericRelayProviderRejectsUnsupportedRecognizedProtocol ensures a
// multi relay never silently falls back to its primary implementation when a
// recognized path has no corresponding protocol face.
func TestGenericRelayProviderRejectsUnsupportedRecognizedProtocol(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, err := NewGenericRelayProvider(Config{
		Name:               "openai-only-multi",
		BaseURL:            upstream.URL,
		ProtocolMode:       "multi",
		PrimaryProtocol:    provider.ProtocolOpenAI,
		SupportedProtocols: []provider.Protocol{provider.ProtocolOpenAI},
	})
	if err != nil {
		t.Fatalf("NewGenericRelayProvider: %v", err)
	}
	defer p.Close()

	key := &keypool.Key{ID: "1", ProviderName: p.Name(), Key: "synthetic", Status: keypool.KeyStatusActive}
	p.SetPool(keypool.NewPool(p.Name(), []*keypool.Key{key}, nil, keypool.Config{}))
	_, err = p.SendRequest(context.Background(), &provider.Request{
		Method: http.MethodPost, Path: "/v1/messages", Headers: make(http.Header),
		Body: []byte(`{"model":"claude-opus-5"}`), Key: key,
	})
	pe, ok := provider.AsProviderError(err)
	if !ok || pe.ErrorType != provider.ErrorTypeInvalidRequest || pe.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %#v, want invalid_request/400", err)
	}
	if calls != 0 {
		t.Fatalf("unsupported protocol reached upstream %d time(s), want 0", calls)
	}
}
