package provider

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

var relayRequestHeaderDenylist = map[string]struct{}{
	"api-key":             {},
	"authorization":       {},
	"connection":          {},
	"content-length":      {},
	"cookie":              {},
	"forwarded":           {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"x-admin-token":       {},
	"x-api-key":           {},
}

// CopyRelayRequestHeaders copies client end-to-end headers while removing
// Gateway credentials, untrusted forwarding metadata and hop-by-hop headers.
// Values are deep-copied so each candidate owns an independent header map.
func CopyRelayRequestHeaders(dst, src http.Header) {
	connectionHeaders := make(map[string]struct{})
	for _, value := range headerValuesFold(src, "Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(strings.ToLower(token)); token != "" {
				connectionHeaders[token] = struct{}{}
			}
		}
	}

	for name, values := range src {
		lowerName := strings.ToLower(name)
		if _, denied := relayRequestHeaderDenylist[lowerName]; denied {
			continue
		}
		if _, hopByHop := connectionHeaders[lowerName]; hopByHop {
			continue
		}
		if strings.HasPrefix(lowerName, "x-forwarded-") {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

// headerValuesFold returns values for a header name without assuming that the
// caller used net/http's canonical map key.  Header maps assembled by custom
// transports, tests, or middleware can contain lower/upper-case keys even
// though Header.Get/Values canonicalize their lookup argument.
func headerValuesFold(src http.Header, name string) []string {
	var values []string
	for key, entries := range src {
		if strings.EqualFold(key, name) {
			values = append(values, entries...)
		}
	}
	return values
}

// SetHeaderDefault installs a protocol default without overwriting a client
// value preserved by CopyRelayRequestHeaders.
func SetHeaderDefault(header http.Header, name, value string) {
	// Header.Get canonicalizes its lookup key, but Header maps can be built by
	// middleware with non-canonical keys (for example, "content-type"). Scan
	// case-insensitively so a relay never installs a duplicate protocol default
	// over an existing client header.
	for key, values := range header {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, existing := range values {
			if existing != "" {
				return
			}
		}
	}
	if header.Get(name) == "" {
		header.Set(name, value)
	}
}

// URLWithRawQuery attaches the client's raw query without decoding and
// re-encoding its values.
func URLWithRawQuery(target, rawQuery string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if rawQuery != "" {
		u.RawQuery = rawQuery
	}
	return u.String(), nil
}

// NewPassthroughHTTPClient disables Go's implicit gzip negotiation and
// decompression. This keeps Content-Encoding consistent with raw body bytes.
func NewPassthroughHTTPClient(timeout time.Duration) *http.Client {
	// The process-wide default is commonly replaced by tests, tracing
	// middleware, or an application-specific RoundTripper.  Do not assert that
	// it is always *http.Transport: a relay request must remain usable even when
	// a custom transport is installed.  Clone the standard transport when it is
	// available so DisableCompression does not mutate global state; custom
	// transports are reused as-is because they own their compression policy.
	var transport http.RoundTripper
	if standard, ok := http.DefaultTransport.(*http.Transport); ok && standard != nil {
		clone := standard.Clone()
		clone.DisableCompression = true
		transport = clone
	} else {
		transport = http.DefaultTransport
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}
