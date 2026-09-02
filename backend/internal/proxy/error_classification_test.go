package proxy

import (
	"net/http"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestClassifyError_PreservesStructuredTransportCauseOverSyntheticStatus(t *testing.T) {
	cases := []struct {
		name string
		kind provider.ErrorType
		want string
	}{
		{name: "timeout", kind: provider.ErrorTypeTimeout, want: "timeout"},
		{name: "connection", kind: provider.ErrorTypeConnection, want: "connection_error"},
		{name: "rate limit", kind: provider.ErrorTypeRateLimit, want: "upstream_429"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := &provider.ProviderError{ErrorType: tc.kind, StatusCode: 0}
			if got := classifyError(http.StatusBadGateway, false, pe, false); got != tc.want {
				t.Fatalf("classifyError(502,%s) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}
