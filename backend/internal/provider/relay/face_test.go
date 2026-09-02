package relay

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// countingFaceImplementation is deliberately transport-free: these tests
// verify the face boundary and shared close state, not protocol HTTP details.
type countingFaceImplementation struct {
	sends  atomic.Int32
	closes atomic.Int32
}

func (p *countingFaceImplementation) Name() string { return "counting-relay" }

func (p *countingFaceImplementation) Protocol() provider.Protocol {
	return provider.ProtocolOpenAI
}

func (p *countingFaceImplementation) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	p.sends.Add(1)
	return &provider.Response{StatusCode: 200}, nil
}

func (p *countingFaceImplementation) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	p.sends.Add(1)
	ch := make(chan *provider.StreamChunk)
	close(ch)
	return ch, &provider.Response{StatusCode: 200}, nil
}

func (p *countingFaceImplementation) HealthCheck(context.Context) error { return nil }

func (p *countingFaceImplementation) ListModels(context.Context) ([]string, error) {
	return []string{"synthetic"}, nil
}

func (p *countingFaceImplementation) SetPool(*keypool.Pool) {}

func (p *countingFaceImplementation) Close() error {
	p.closes.Add(1)
	return nil
}

func TestFaceProvider_RejectsUnknownPathBeforeImplementation(t *testing.T) {
	impl := &countingFaceImplementation{}
	station := &GenericRelayProvider{
		name:            "multi-station",
		protocolMode:    "multi",
		primaryProtocol: provider.ProtocolOpenAI,
		implementations: map[provider.Protocol]provider.Provider{provider.ProtocolOpenAI: impl},
		closeStates:     nil, // exercise the legacy/zero-value construction path
	}
	face, err := station.Face("multi-station-openai", provider.ProtocolOpenAI)
	if err != nil {
		t.Fatalf("Face: %v", err)
	}

	_, err = face.SendRequest(context.Background(), &provider.Request{Path: "/v1/unknown"})
	if err == nil {
		t.Fatal("unknown path was accepted by a protocol face")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) || pe.ErrorType != provider.ErrorTypeInvalidRequest {
		t.Fatalf("error = %v, want invalid_request ProviderError", err)
	}
	if got := impl.sends.Load(); got != 0 {
		t.Fatalf("implementation sends = %d, want 0", got)
	}
}

func TestGenericRelayProvider_CloseIsIdempotentAcrossFacesAndConcurrentCalls(t *testing.T) {
	impl := &countingFaceImplementation{}
	station := &GenericRelayProvider{
		name:            "multi-station",
		protocolMode:    "multi",
		primaryProtocol: provider.ProtocolOpenAI,
		implementations: map[provider.Protocol]provider.Provider{provider.ProtocolOpenAI: impl},
		closeStates:     nil, // lazy initialization must be race-safe and persistent
	}
	face, err := station.Face("multi-station-openai", provider.ProtocolOpenAI)
	if err != nil {
		t.Fatalf("Face: %v", err)
	}

	const calls = 32
	var wg sync.WaitGroup
	wg.Add(calls)
	for i := 0; i < calls; i++ {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := face.Close(); err != nil {
					t.Errorf("face.Close: %v", err)
				}
				return
			}
			if err := station.Close(); err != nil {
				t.Errorf("station.Close: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := impl.closes.Load(); got != 1 {
		t.Fatalf("underlying implementation Close calls = %d, want 1", got)
	}
}
