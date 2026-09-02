package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

type relayResponseProvider struct {
	name      string
	status    int
	headers   http.Header
	body      []byte
	err       *provider.ProviderError
	stream    bool
	callCount int
}

func (p *relayResponseProvider) Name() string                { return p.name }
func (p *relayResponseProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *relayResponseProvider) SendRequest(context.Context, *provider.Request) (*provider.Response, error) {
	p.callCount++
	if p.err != nil {
		return nil, p.err
	}
	return &provider.Response{StatusCode: p.status, Headers: p.headers.Clone(), Body: append([]byte(nil), p.body...)}, nil
}
func (p *relayResponseProvider) SendStreamRequest(context.Context, *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	p.callCount++
	if p.err != nil {
		return nil, nil, p.err
	}
	ch := make(chan *provider.StreamChunk, 2)
	ch <- &provider.StreamChunk{Data: append([]byte(nil), p.body...)}
	ch <- &provider.StreamChunk{Err: io.EOF}
	close(ch)
	return ch, &provider.Response{StatusCode: p.status, Headers: p.headers.Clone()}, nil
}
func (*relayResponseProvider) HealthCheck(context.Context) error            { return nil }
func (*relayResponseProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (*relayResponseProvider) SetPool(*keypool.Pool)                        {}
func (*relayResponseProvider) Close() error                                 { return nil }

func TestRelayResponse_PreservesSuccessStatusHeadersAndBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			p := &relayResponseProvider{
				name: "relay-response", status: http.StatusCreated, stream: stream,
				headers: http.Header{
					"Content-Type":         {"text/event-stream"},
					"Cache-Control":        {"upstream-cache"},
					"Connection":           {"keep-alive, X-Hop"},
					"Content-Length":       {"999"},
					"Set-Cookie":           {"a=1", "b=2"},
					"X-Hop":                {"remove-me"},
					"X-Request-Id":         {"upstream-trace"},
					"X-Upstream-Extension": {"one", "two"},
				},
				body: []byte("data: {\"ok\":true}\r\n\r\n"),
			}
			engine := newRelayResponseEngine(t, p)
			metrics := &relayTTFTMetrics{}
			engine.metrics = metrics
			w := serveRelayResponseRequest(engine, stream)

			if w.Code != http.StatusCreated || w.Body.String() != string(p.body) {
				t.Fatalf("response = status %d body %q", w.Code, w.Body.String())
			}
			if got := w.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
				t.Fatalf("Set-Cookie = %#v", got)
			}
			if got := w.Header().Values("X-Upstream-Extension"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
				t.Fatalf("extension values = %#v", got)
			}
			if w.Header().Get("X-Request-Id") != "upstream-trace" {
				t.Fatalf("X-Request-Id = %q, want upstream trace", w.Header().Get("X-Request-Id"))
			}
			for _, name := range []string{"Connection", "Content-Length", "X-Hop"} {
				if w.Header().Get(name) != "" {
					t.Errorf("hop-by-hop header %s leaked: %q", name, w.Header().Get(name))
				}
			}
			if got := metrics.eventCount(p.name, "candidate_attempt", "none"); got != 1 {
				t.Fatalf("candidate attempt events = %d, want 1", got)
			}
			if got := metrics.activeCount(p.name); got != 0 {
				t.Fatalf("active upstreams after response = %d, want 0", got)
			}
			stage := map[bool]string{false: "body", true: "data"}[stream]
			if got := metrics.eventCount(p.name, "response_committed", stage); got != 1 {
				t.Fatalf("response committed %s events = %d, want 1", stage, got)
			}
		})
	}
}

func TestRelayResponse_FinalUpstreamErrorIsRaw(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			raw := []byte("{ \"error\": \"synthetic upstream failure\" }\n")
			p := &relayResponseProvider{
				name: "relay-error", stream: stream,
				err: &provider.ProviderError{
					ProviderName: "relay-error", StatusCode: http.StatusServiceUnavailable,
					ErrorType: provider.ErrorTypeServerError, Message: "upstream returned 503",
					RawError: raw,
					UpstreamHeaders: http.Header{
						"Content-Type":        {"application/problem+json"},
						"Connection":          {"X-Hop"},
						"Content-Length":      {"999"},
						"Retry-After":         {"7"},
						"X-Hop":               {"remove-me"},
						"X-Upstream-Error-Id": {"err-one", "err-two"},
					},
				},
			}
			engine := newRelayResponseEngine(t, p)
			accessRecorder, err := accesslog.NewRecorder(accesslog.RecorderConfig{
				Enabled: true, BodyDir: t.TempDir(),
			}, nil, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			engine.accessLog = accessRecorder
			w := serveRelayResponseRequest(engine, stream)

			if w.Code != http.StatusServiceUnavailable || w.Body.String() != string(raw) {
				t.Fatalf("final error = status %d body %q", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("Retry-After") != "7" {
				t.Fatalf("upstream error headers lost: %#v", w.Header())
			}
			if got := w.Header().Values("X-Upstream-Error-Id"); len(got) != 2 {
				t.Fatalf("multi-value error header = %#v", got)
			}
			for _, name := range []string{"Connection", "Content-Length", "X-Hop"} {
				if w.Header().Get(name) != "" {
					t.Errorf("hop-by-hop error header %s leaked", name)
				}
			}
			if p.callCount != 1 {
				t.Fatalf("relay calls = %d, want 1", p.callCount)
			}
			relPath := accesslog.BodyFilePath("gateway-trace", time.Now().UTC().Format("2006-01-02"), "resp")
			loggedBody, err := accessRecorder.ReadBody(relPath)
			if err != nil {
				t.Fatalf("read access-log response body: %v", err)
			}
			if string(loggedBody) != string(raw) {
				t.Fatalf("access-log response body = %q, want raw upstream body %q", loggedBody, raw)
			}
		})
	}
}

func TestNonStreamWriteFailureMarksClientDisconnected(t *testing.T) {
	p := &relayResponseProvider{
		name:    "relay-write-failure",
		status:  http.StatusOK,
		headers: http.Header{"Content-Type": {"application/json"}},
		body:    []byte(`{"ok":true}`),
	}
	engine := newRelayResponseEngine(t, p)

	writer := &failingBodyWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(writer)
	req := &provider.Request{
		Method: http.MethodPost, Path: "/v1/responses", IsStream: false,
		TraceID: "write-failure", Body: []byte(`{"model":"synthetic-model"}`),
		Headers: http.Header{},
	}
	result := &router.RouteResult{
		ProviderName: p.name, ModelID: "synthetic-model", Protocol: provider.ProtocolOpenAI,
	}
	entry := &accesslog.AccessEntry{}

	engine.writeNonStreamResponse(c, req, &provider.Response{
		StatusCode: http.StatusOK, Headers: p.headers, Body: p.body,
	}, result, time.Millisecond, entry)

	if entry.ErrorType != string(provider.ErrorTypeClientDisconnected) {
		t.Fatalf("write failure error type = %q, want %q", entry.ErrorType, provider.ErrorTypeClientDisconnected)
	}
}

func newRelayResponseEngine(t *testing.T, p *relayResponseProvider) *Engine {
	t.Helper()
	reg := provider.NewRegistry()
	reg.RegisterWithProtocolVendorRelay(p.name, func(provider.ProviderConfig) (provider.Provider, error) { return p, nil }, provider.ProtocolOpenAI, p.name, true)
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{
		p.name: {Enabled: true, Protocol: provider.ProtocolOpenAI},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pool := keypool.NewPool(p.name, []*keypool.Key{{
		ID: "1", ProviderName: p.name, Key: "synthetic-key", Status: keypool.KeyStatusActive,
		BillingSource: "api", Protocols: string(provider.ProtocolOpenAI), CreatedAt: now, UpdatedAt: now,
	}}, nil, keypool.Config{})
	rtr := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{p.name: pool}, router.Config{Aliases: map[string]router.AliasConfig{
		"synthetic-model": {Providers: []router.ProviderRoute{{Name: p.name}}},
	}})
	return NewEngine(Config{Router: rtr, Logger: zap.NewNop(), RelayFirstByteTimeout: time.Second})
}

func serveRelayResponseRequest(engine *Engine, stream bool) *httptest.ResponseRecorder {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.POST("/v1/responses", engine.HandleRequest)
	body := `{"model":"synthetic-model","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "gateway-trace")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
