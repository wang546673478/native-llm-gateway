package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRelayStream_ForwardsPingImmediatelyAndPreservesBytes(t *testing.T) {
	tests := []struct {
		name     string
		protocol provider.Protocol
		path     string
		rest     []byte
	}{
		{
			name:     "anthropic",
			protocol: provider.ProtocolAnthropic,
			path:     "/v1/messages",
			rest: []byte("event: vendor_unknown\r\n" +
				"data: first-line\r\n" +
				"data: second-line\r\n\r\n" +
				"event: message_start\r\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":3}}}\r\n\r\n" +
				"event: message_delta\r\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\r\n\r\n"),
		},
		{
			name:     "openai",
			protocol: provider.ProtocolOpenAI,
			path:     "/v1/chat/completions",
			rest: []byte("event: vendor_unknown\r\n" +
				"data: not-json\r\n" +
				"data: still-not-json\r\n\r\n" +
				"data: {\"model\":\"gpt-test\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\r\n\r\n" +
				"data: [DONE]\r\n\r\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ping := []byte(": PING\r\n\r\n")
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(ping)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					for offset := 0; offset < len(tt.rest); offset += 7 {
						end := min(offset+7, len(tt.rest))
						_, _ = w.Write(tt.rest[offset:end])
						w.(http.Flusher).Flush()
					}
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()

			relayProvider, err := NewGenericRelayProvider(Config{
				Name:            "relay-stream-" + tt.name,
				BaseURL:         upstream.URL,
				ProtocolMode:    "single",
				PrimaryProtocol: tt.protocol,
				Timeout:         5,
			})
			if err != nil {
				t.Fatalf("NewGenericRelayProvider: %v", err)
			}
			defer relayProvider.Close()

			key := &keypool.Key{
				ID: "1", ProviderName: relayProvider.Name(), Key: "sk-test",
				Status: keypool.KeyStatusActive, Protocols: string(tt.protocol),
			}
			pool := keypool.NewPool(relayProvider.Name(), []*keypool.Key{key}, nil, keypool.Config{})
			relayProvider.SetPool(pool)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chunks, resp, err := relayProvider.SendStreamRequest(ctx, &provider.Request{
				Method: http.MethodPost, Path: tt.path, IsStream: true,
				Headers: http.Header{}, Body: []byte(`{"model":"test","stream":true}`), Key: key,
			})
			if err != nil {
				t.Fatalf("SendStreamRequest: %v", err)
			}

			var got []byte
			select {
			case chunk := <-chunks:
				if chunk == nil || chunk.Err != nil {
					t.Fatalf("first chunk = %#v, want raw ping", chunk)
				}
				got = append(got, chunk.Data...)
				if !bytes.Equal(chunk.Data, ping) {
					t.Fatalf("first chunk = %q, want ping %q", chunk.Data, ping)
				}
			case <-time.After(250 * time.Millisecond):
				close(release)
				t.Fatal("relay did not forward upstream ping immediately")
			}

			close(release)
			for chunk := range chunks {
				if chunk == nil {
					continue
				}
				if chunk.Err != nil && chunk.Err != io.EOF {
					t.Fatalf("stream error: %v", chunk.Err)
				}
				got = append(got, chunk.Data...)
			}
			want := append(append([]byte(nil), ping...), tt.rest...)
			if !bytes.Equal(got, want) {
				t.Fatalf("stream bytes changed:\n got %q\nwant %q", got, want)
			}
			if resp.GetUsage() == nil {
				t.Fatal("passive usage observer did not retain stream usage")
			}
		})
	}
}

func TestRelayStream_CancelClosesRawProducer(t *testing.T) {
	requestGone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": PING\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(requestGone)
	}))
	defer upstream.Close()

	relayProvider, key := newPassthroughTestRelay(t, upstream.URL, provider.ProtocolAnthropic)
	defer relayProvider.Close()
	ctx, cancel := context.WithCancel(context.Background())
	chunks, _, err := relayProvider.SendStreamRequest(ctx, &provider.Request{
		Method: http.MethodPost, Path: "/v1/messages", IsStream: true,
		Headers: http.Header{}, Body: []byte(`{"model":"synthetic-model","stream":true}`), Key: key,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case chunk := <-chunks:
		if chunk == nil || chunk.Err != nil || !bytes.Equal(chunk.Data, []byte(": PING\n\n")) {
			cancel()
			t.Fatalf("first chunk = %#v, want raw ping", chunk)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("relay did not receive initial ping")
	}

	cancel()
	producerDone := make(chan struct{})
	go func() {
		for range chunks {
		}
		close(producerDone)
	}()
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("raw relay producer did not exit after cancellation")
	}
	select {
	case <-requestGone:
	case <-time.After(time.Second):
		t.Fatal("upstream HTTP request remained active after cancellation")
	}
}
