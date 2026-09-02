package provider

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCopyRelayRequestHeaders(t *testing.T) {
	src := http.Header{
		"Accept":              {"application/json"},
		"Accept-Language":     {"zh-CN", "en-US"},
		"Anthropic-Beta":      {"feature-a", "feature-b"},
		"Anthropic-Version":   {"2099-01-01"},
		"Authorization":       {"Bearer gateway-credential"},
		"Connection":          {"keep-alive, X-Connection-Only"},
		"Content-Length":      {"123"},
		"Cookie":              {"admin=session"},
		"Forwarded":           {"for=untrusted"},
		"Host":                {"gateway.invalid"},
		"Proxy-Authorization": {"Basic gateway-proxy"},
		"Te":                  {"trailers"},
		"Transfer-Encoding":   {"chunked"},
		"User-Agent":          {"client/1.0"},
		"X-Admin-Token":       {"admin-token"},
		"X-Api-Key":           {"gateway-key"},
		"X-Connection-Only":   {"remove-me"},
		"X-Custom-Extension":  {"one", "two"},
		"X-Forwarded-For":     {"203.0.113.10"},
	}

	dst := make(http.Header)
	CopyRelayRequestHeaders(dst, src)

	want := http.Header{
		"Accept":             {"application/json"},
		"Accept-Language":    {"zh-CN", "en-US"},
		"Anthropic-Beta":     {"feature-a", "feature-b"},
		"Anthropic-Version":  {"2099-01-01"},
		"User-Agent":         {"client/1.0"},
		"X-Custom-Extension": {"one", "two"},
	}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("forwarded headers mismatch:\n got: %#v\nwant: %#v", dst, want)
	}

	dst["X-Custom-Extension"][0] = "changed"
	if src["X-Custom-Extension"][0] != "one" {
		t.Fatal("destination shares header value storage with source")
	}
}

func TestCopyRelayRequestHeaders_CaseInsensitiveConnectionNomination(t *testing.T) {
	src := http.Header{
		// Middleware is allowed to construct a Header map directly.  These keys
		// deliberately do not use net/http's canonical spelling.
		"connection":         {"x-lower-hop, X-Another-Hop"},
		"x-lower-hop":        {"must-not-forward"},
		"X-Another-Hop":      {"must-not-forward"},
		"AUTHORIZATION":      {"Bearer gateway-secret"},
		"X-Custom-Extension": {"kept"},
	}

	dst := make(http.Header)
	CopyRelayRequestHeaders(dst, src)

	if got := dst.Get("x-lower-hop"); got != "" {
		t.Fatalf("lower-case Connection nomination leaked header: %q", got)
	}
	if got := dst.Get("X-Another-Hop"); got != "" {
		t.Fatalf("mixed-case Connection nomination leaked header: %q", got)
	}
	if got := dst.Get("Authorization"); got != "" {
		t.Fatalf("case-insensitive credential denylist failed: %q", got)
	}
	if got := dst.Get("X-Custom-Extension"); got != "kept" {
		t.Fatalf("ordinary extension header was not preserved: %q", got)
	}
}
