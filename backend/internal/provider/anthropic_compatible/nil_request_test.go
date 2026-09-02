package anthropic_compatible

import (
	"context"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestBaseRejectsNilRequest(t *testing.T) {
	b := NewBase(Config{Name: "nil-request"})

	if _, err := b.SendRequest(context.Background(), nil); err == nil {
		t.Fatal("SendRequest(nil) returned nil error")
	} else if pe, ok := err.(*provider.ProviderError); !ok || pe.ErrorType != provider.ErrorTypeInvalidRequest {
		t.Fatalf("SendRequest(nil) error = %#v, want invalid_request ProviderError", err)
	}
	if _, _, err := b.SendStreamRequest(context.Background(), nil); err == nil {
		t.Fatal("SendStreamRequest(nil) returned nil error")
	} else if pe, ok := err.(*provider.ProviderError); !ok || pe.ErrorType != provider.ErrorTypeInvalidRequest {
		t.Fatalf("SendStreamRequest(nil) error = %#v, want invalid_request ProviderError", err)
	}
}
