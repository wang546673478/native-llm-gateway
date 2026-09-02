package anthropic_compatible

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func newDiagnosticAnthropicPool(t *testing.T) *keypool.Pool {
	t.Helper()
	now := time.Now()
	return keypool.NewPool("diag-anthropic", []*keypool.Key{{
		ID: "key-1", ProviderName: "diag-anthropic", Name: "primary", Key: "secret-anthropic",
		Status: keypool.KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
}

func assertDiagnosticAnthropicPoolUnchanged(t *testing.T, pool *keypool.Pool, before []keypool.Key) {
	t.Helper()
	after := pool.Keys()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("diagnostic mutated key pool:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestDiagnoseKey_HTTPStatusMatrix_DrainsBodyAndDoesNotTouchPool(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		bodyParts []string
		wantType  provider.ErrorType
	}{
		{name: "200", status: http.StatusOK, bodyParts: []string{`{"id":"ok"}`}},
		{name: "403 auth", status: http.StatusForbidden, bodyParts: []string{`{"error":{"type":"authentication_error","message":"bad key"}}`}, wantType: provider.ErrorTypeAuth},
		{name: "403 quota at final byte", status: http.StatusForbidden, bodyParts: []string{`{"error":{"type":"permission_error","message":"not `, `quota"}}`}, wantType: provider.ErrorTypeQuotaExceeded},
		{name: "429", status: http.StatusTooManyRequests, bodyParts: []string{`{"error":{"type":"rate_limit_error","message":"slow"}}`}, wantType: provider.ErrorTypeRateLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			var gotPath, gotAuth, gotVersion string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("x-api-key")
				gotVersion = r.Header.Get("anthropic-version")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				for _, part := range tc.bodyParts {
					_, _ = io.WriteString(w, part)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
				// Returning only after all parts have been written makes a
				// classification keyword in the final part prove full drain.
			}))
			defer upstream.Close()

			pool := newDiagnosticAnthropicPool(t)
			before := pool.Keys()
			base := NewBase(Config{Name: "diag-anthropic", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})
			result, err := base.DiagnoseKey(context.Background(), pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
				Protocol: provider.ProtocolAnthropic,
				Path:     "/v1/messages",
				Model:    "claude-opus-5",
			})
			if err != nil {
				t.Fatalf("DiagnoseKey: %v", err)
			}
			if result == nil {
				t.Fatal("DiagnoseKey returned nil result")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("upstream requests = %d, want exactly 1", got)
			}
			if gotPath != "/v1/messages" {
				t.Errorf("path = %q, want /v1/messages", gotPath)
			}
			if gotAuth != "secret-anthropic" {
				t.Errorf("x-api-key = %q, want configured key", gotAuth)
			}
			if gotVersion != "2023-06-01" {
				t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
			}
			if !strings.Contains(string(gotBody), `"model":"claude-opus-5"`) {
				t.Errorf("diagnostic body does not contain model: %s", gotBody)
			}
			if result.StatusCode != tc.status || !result.Reachable {
				t.Errorf("result status/reachable = %d/%v, want %d/true", result.StatusCode, result.Reachable, tc.status)
			}
			if result.ErrorType != tc.wantType {
				t.Errorf("error type = %q, want %q", result.ErrorType, tc.wantType)
			}
			if result.DiagnosticAbort {
				t.Error("ordinary completed response marked diagnostic_abort")
			}
			assertDiagnosticAnthropicPoolUnchanged(t, pool, before)
		})
	}
}

func TestDiagnoseKey_TransportErrorDoesNotTouchPool(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	endpoint := upstream.URL
	upstream.Close()

	pool := newDiagnosticAnthropicPool(t)
	before := pool.Keys()
	base := NewBase(Config{Name: "diag-anthropic", Endpoint: endpoint, Timeout: 500 * time.Millisecond, Pool: pool})
	result, err := base.DiagnoseKey(context.Background(), pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
		Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("DiagnoseKey returned setup error: %v", err)
	}
	if result == nil || result.Reachable || result.StatusCode != 0 {
		t.Fatalf("transport result = %+v, want unreachable status=0", result)
	}
	if result.ErrorType != provider.ErrorTypeConnection {
		t.Errorf("transport error type = %q, want connection", result.ErrorType)
	}
	if result.DiagnosticAbort {
		t.Error("transport failure with live parent context marked diagnostic_abort")
	}
	assertDiagnosticAnthropicPoolUnchanged(t, pool, before)
}

func TestDiagnoseKey_UnsupportedPathDoesNotSendRequest(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()

	pool := newDiagnosticAnthropicPool(t)
	base := NewBase(Config{Name: "diag-anthropic", Endpoint: upstream.URL, Pool: pool})
	result, err := base.DiagnoseKey(context.Background(), pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
		Protocol: provider.ProtocolAnthropic, Path: "/v1/chat/completions", Model: "claude-opus-5",
	})
	if result != nil || !provider.IsDiagnosticUnavailable(err) {
		t.Fatalf("mismatched diagnostic = result:%+v err:%v, want typed unavailable", result, err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0 for unsupported path", got)
	}
}

func TestDiagnoseKey_CancelBeforeHeaders(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(started)
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	pool := newDiagnosticAnthropicPool(t)
	before := pool.Keys()
	base := NewBase(Config{Name: "diag-anthropic", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result *provider.KeyDiagnosticResult
		err    error
	}
	out := make(chan outcome, 1)
	go func() {
		result, err := base.DiagnoseKey(ctx, pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
			Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "claude-opus-5",
		})
		out <- outcome{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive diagnostic request")
	}
	cancel()
	var got outcome
	select {
	case got = <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("DiagnoseKey did not stop after cancellation")
	}
	if got.err != nil || got.result == nil {
		t.Fatalf("cancel result = %+v, err=%v", got.result, got.err)
	}
	if got.result.ErrorType != provider.ErrorTypeClientDisconnected || !got.result.DiagnosticAbort {
		t.Errorf("cancel result = %+v, want client_disconnected + diagnostic_abort", got.result)
	}
	if got.result.Reachable {
		t.Error("headers-before cancellation reported reachable")
	}
	if requests.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1", requests.Load())
	}
	assertDiagnosticAnthropicPoolUnchanged(t, pool, before)
}

func TestDiagnoseKey_DeadlineBeforeHeaders(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		close(started)
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	pool := newDiagnosticAnthropicPool(t)
	before := pool.Keys()
	base := NewBase(Config{Name: "diag-anthropic", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resultCh := make(chan *provider.KeyDiagnosticResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := base.DiagnoseKey(ctx, pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
			Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "claude-opus-5",
		})
		resultCh <- result
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive diagnostic request")
	}
	var result *provider.KeyDiagnosticResult
	var err error
	select {
	case result = <-resultCh:
		err = <-errCh
	case <-time.After(2 * time.Second):
		t.Fatal("DiagnoseKey did not stop after deadline")
	}
	if err != nil || result == nil {
		t.Fatalf("deadline result = %+v, err=%v", result, err)
	}
	if result.ErrorType != provider.ErrorTypeTimeout || !result.DiagnosticAbort {
		t.Errorf("deadline result = %+v, want timeout + diagnostic_abort", result)
	}
	if requests.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1", requests.Load())
	}
	assertDiagnosticAnthropicPoolUnchanged(t, pool, before)
}

func TestDiagnoseKey_CancelDuringBodyDrain(t *testing.T) {
	var requests atomic.Int32
	bodyStarted := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partial":`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(bodyStarted)
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	pool := newDiagnosticAnthropicPool(t)
	before := pool.Keys()
	base := NewBase(Config{Name: "diag-anthropic", Endpoint: upstream.URL, Timeout: 5 * time.Second, Pool: pool})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan *provider.KeyDiagnosticResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := base.DiagnoseKey(ctx, pool.KeyPtrs()[0], provider.KeyDiagnosticRequest{
			Protocol: provider.ProtocolAnthropic, Path: "/v1/messages", Model: "claude-opus-5",
		})
		resultCh <- result
		errCh <- err
	}()
	select {
	case <-bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream did not send diagnostic body")
	}
	cancel()
	var result *provider.KeyDiagnosticResult
	var err error
	select {
	case result = <-resultCh:
		err = <-errCh
	case <-time.After(2 * time.Second):
		t.Fatal("DiagnoseKey did not stop while draining body")
	}
	if err != nil || result == nil {
		t.Fatalf("body cancel result = %+v, err=%v", result, err)
	}
	if !result.DiagnosticAbort || result.ErrorType != provider.ErrorTypeClientDisconnected {
		t.Errorf("body cancel result = %+v, want client_disconnected + diagnostic_abort", result)
	}
	if requests.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1", requests.Load())
	}
	assertDiagnosticAnthropicPoolUnchanged(t, pool, before)
}
