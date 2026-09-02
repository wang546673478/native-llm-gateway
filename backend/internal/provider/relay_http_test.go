package provider

import (
	"net/http"
	"testing"
	"time"
)

type relayHTTPTestRoundTripper struct{}

func (*relayHTTPTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestNewPassthroughHTTPClient_AllowsCustomDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })

	custom := &relayHTTPTestRoundTripper{}
	http.DefaultTransport = custom

	client := NewPassthroughHTTPClient(time.Second)
	if client == nil {
		t.Fatal("NewPassthroughHTTPClient returned nil")
	}
	if client.Transport != custom {
		t.Fatalf("custom default transport was replaced: got %T, want %T", client.Transport, custom)
	}
}

func TestSetHeaderDefault_RecognizesNonCanonicalExistingHeader(t *testing.T) {
	header := http.Header{
		"content-type": {"application/vnd.client+json"},
		"ACCEPT":       {"application/vnd.client"},
	}

	SetHeaderDefault(header, "Content-Type", "application/json")
	SetHeaderDefault(header, "Accept", "text/event-stream")

	if got := header["Content-Type"]; len(got) != 0 {
		t.Fatalf("installed duplicate canonical content type: %#v", got)
	}
	if got := header["Accept"]; len(got) != 0 {
		t.Fatalf("installed duplicate canonical accept: %#v", got)
	}
	if got := header["content-type"]; len(got) != 1 || got[0] != "application/vnd.client+json" {
		t.Fatalf("existing content type changed: %#v", got)
	}
}
