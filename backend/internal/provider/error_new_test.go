package provider

import (
	"net/http"
	"testing"
)

func TestWithUpstreamHeadersClonesInput(t *testing.T) {
	source := http.Header{
		"X-Upstream-Error": {"one", "two"},
	}
	pe := WithUpstreamHeaders(NewError("relay-test", http.StatusBadGateway, ErrorTypeServerError, "synthetic"), source)

	source["X-Upstream-Error"][0] = "mutated"
	source.Add("X-Later", "not-owned")
	if got := pe.UpstreamHeaders.Values("X-Upstream-Error"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("cloned headers changed with source: %#v", pe.UpstreamHeaders)
	}
	if pe.UpstreamHeaders.Get("X-Later") != "" {
		t.Fatalf("later source header leaked into clone: %#v", pe.UpstreamHeaders)
	}
}

func TestWithUpstreamHeadersMarksEmptyResponseHeaders(t *testing.T) {
	pe := WithUpstreamHeaders(NewError("relay-test", http.StatusForbidden, ErrorTypeAuth, "synthetic"), nil)
	if pe.UpstreamHeaders == nil {
		t.Fatal("nil upstream headers lost the fact that an HTTP response was received")
	}
	if len(pe.UpstreamHeaders) != 0 {
		t.Fatalf("empty upstream headers = %#v, want empty map", pe.UpstreamHeaders)
	}
}
