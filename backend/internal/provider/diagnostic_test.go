package provider

import (
	"context"
	"errors"
	"testing"
)

func TestDiagnosticProtocolForPath(t *testing.T) {
	tests := []struct {
		path  string
		proto Protocol
		ok    bool
	}{
		{path: "/v1/messages", proto: ProtocolAnthropic, ok: true},
		{path: "/messages", proto: ProtocolAnthropic, ok: true},
		{path: "/v1/chat/completions", proto: ProtocolOpenAI, ok: true},
		{path: "/chat/completions", proto: ProtocolOpenAI, ok: true},
		{path: "/v1/responses", proto: ProtocolOpenAI, ok: true},
		{path: "/responses", proto: ProtocolOpenAI, ok: true},
		{path: "/v1beta/models/gemini-2.5-pro:generateContent", proto: ProtocolGoogle, ok: true},
		{path: "/v1beta/models/gemini-2.5-pro:streamGenerateContent", proto: ProtocolGoogle, ok: true},
		{path: "/proxy/v1beta/models/gemini-2.5-pro:generateContent", proto: ProtocolGoogle, ok: true},
		{path: "/v1/messages?x=1", proto: "", ok: false},
		{path: "/v1/unknown", proto: "", ok: false},
		{path: "/v1beta/models/gemini:generateContent-extra", proto: "", ok: false},
		{path: "/v1beta/modelsx:generateContent", proto: "", ok: false},
		{path: "/v1beta/models/:generateContent", proto: "", ok: false},
		{path: "/v1beta/models/gemini:generateContent/extra", proto: "", ok: false},
		{path: "/v1beta/foo:streamGenerateContent", proto: "", ok: false},
		{path: "", proto: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := DiagnosticProtocolForPath(tc.path)
			if got != tc.proto || ok != tc.ok {
				t.Fatalf("DiagnosticProtocolForPath(%q) = %q/%v, want %q/%v", tc.path, got, ok, tc.proto, tc.ok)
			}
		})
	}
}

func TestValidateDiagnosticProtocolPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   Protocol
		path    string
		unavail bool
	}{
		{name: "empty path uses face default", proto: ProtocolAnthropic, path: ""},
		{name: "matching anthropic", proto: ProtocolAnthropic, path: "/v1/messages"},
		{name: "matching responses", proto: ProtocolOpenAI, path: "/v1/responses"},
		{name: "cross protocol", proto: ProtocolOpenAI, path: "/v1/messages", unavail: true},
		{name: "unknown path", proto: ProtocolOpenAI, path: "/v1/other", unavail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDiagnosticProtocolPath(tc.proto, tc.path)
			if tc.unavail {
				if !IsDiagnosticUnavailable(err) {
					t.Fatalf("err = %v, want diagnostic unavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDiagnosticWasAbortedRequiresTerminalParentContext(t *testing.T) {
	if DiagnosticWasAborted(context.Background(), context.DeadlineExceeded) {
		t.Fatal("local/client timeout with live parent context must not be diagnostic_abort")
	}
	if DiagnosticWasAborted(context.Background(), context.Canceled) {
		t.Fatal("wrapped cancellation with live parent context must not be diagnostic_abort")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if !DiagnosticWasAborted(cancelCtx, errors.New("round trip failed")) {
		t.Fatal("terminal canceled parent context must be diagnostic_abort")
	}
	deadlineCtx, deadlineCancel := context.WithCancel(context.Background())
	deadlineCancel()
	if !DiagnosticWasAborted(deadlineCtx, errors.New("body read failed")) {
		t.Fatal("terminal deadline/cancel parent context must be diagnostic_abort")
	}
	if DiagnosticWasAborted(cancelCtx, nil) {
		t.Fatal("nil error means completed diagnostic, even if cancellation races after EOF")
	}
}
