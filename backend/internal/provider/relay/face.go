package relay

import (
	"context"
	"fmt"
	"sync"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// faceProvider is the provider view registered for one protocol face of a
// multi-protocol relay station.  A GenericRelayProvider owns all protocol
// implementations, but a router candidate must expose exactly one protocol;
// otherwise Protocol() leaks the station's primary protocol into routing and
// key-pool filtering.
type faceProvider struct {
	station  *GenericRelayProvider
	impl     provider.Provider
	name     string
	protocol provider.Protocol
}

// Face returns a protocol-scoped view of the relay.  The view delegates all
// HTTP behavior to the selected implementation while keeping face metadata
// stable for Manager, Router, and admin callers.
func (p *GenericRelayProvider) Face(name string, protocol provider.Protocol) (provider.Provider, error) {
	if p == nil {
		return nil, fmt.Errorf("relay provider is nil")
	}
	if name == "" {
		return nil, fmt.Errorf("relay face name is required")
	}
	impl, ok := p.implementations[protocol]
	if !ok || impl == nil {
		return nil, fmt.Errorf("relay protocol implementation %q is not available", protocol)
	}
	return &faceProvider{
		station:  p,
		impl:     impl,
		name:     name,
		protocol: protocol,
	}, nil
}

func (p *faceProvider) Name() string { return p.name }

// Endpoint exposes the shared station endpoint to Manager/quota workers.
func (p *faceProvider) Endpoint() string {
	if p == nil || p.station == nil {
		return ""
	}
	return p.station.Endpoint()
}

// BillingSource exposes the station default to route metadata while keeping
// per-key billing restrictions in the shared pool authoritative.
func (p *faceProvider) BillingSource() string {
	if p == nil || p.station == nil {
		return "api"
	}
	return p.station.BillingSource()
}

// Protocol is deliberately face-scoped.  Do not replace this with the
// GenericRelayProvider primary protocol: Router uses it to filter candidates
// and acquire protocol-compatible keys.
func (p *faceProvider) Protocol() provider.Protocol { return p.protocol }

// SupportsProtocol keeps a face from being treated as a multi-protocol
// provider by callers that use the optional capability interface.
func (p *faceProvider) SupportsProtocol(protocol provider.Protocol) bool {
	return protocol == p.protocol
}

func (p *faceProvider) SupportedProtocols() []provider.Protocol {
	return []provider.Protocol{p.protocol}
}

func (p *faceProvider) SetPool(pool *keypool.Pool) {
	// Keep the station and every implementation on the same shared vendor
	// pool.  This is idempotent and also makes a face safe when Server injects
	// the pool once per registered face.
	if p.station != nil {
		p.station.SetPool(pool)
		return
	}
	p.impl.SetPool(pool)
}

func (p *faceProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if err := p.validateRequest(req); err != nil {
		return nil, err
	}
	return p.impl.SendRequest(ctx, req)
}

func (p *faceProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if err := p.validateRequest(req); err != nil {
		return nil, nil, err
	}
	return p.impl.SendStreamRequest(ctx, req)
}

func (p *faceProvider) validateRequest(req *provider.Request) error {
	if req == nil {
		return provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest, "relay request is nil")
	}
	// A face is a protocol-scoped route.  An unknown path cannot be safely
	// assigned to one of the station's faces: the compatible bases otherwise
	// fall back to their primary endpoint (for example, an unknown OpenAI path
	// would become Chat Completions).  Reject before calling the implementation
	// so a multi station never sends a request through the wrong protocol.
	inferred := detectProtocolFromPath(req.Path)
	if inferred == "" {
		return provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest,
			"request path does not identify a supported relay protocol")
	}
	if inferred != p.protocol {
		return provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest,
			fmt.Sprintf("request path protocol %q does not match relay face %q", inferred, p.protocol))
	}
	return nil
}

func (p *faceProvider) HealthCheck(ctx context.Context) error { return p.impl.HealthCheck(ctx) }

func (p *faceProvider) ListModels(ctx context.Context) ([]string, error) {
	return p.impl.ListModels(ctx)
}

// DiagnoseKey is intentionally explicit instead of relying on embedding the
// Provider interface: an embedded interface would hide the implementation's
// optional KeyDiagnoser method from type assertions in the admin handler.
func (p *faceProvider) DiagnoseKey(ctx context.Context, key *keypool.Key, d provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
	if d.Protocol != "" && d.Protocol != p.protocol {
		return nil, provider.NewDiagnosticUnavailable(d.Protocol, d.Path, "diagnostic protocol does not match relay face")
	}
	if err := provider.ValidateDiagnosticProtocolPath(p.protocol, d.Path); err != nil {
		return nil, err
	}
	d.Protocol = p.protocol
	diagnoser, ok := p.impl.(provider.KeyDiagnoser)
	if !ok {
		return nil, provider.NewDiagnosticUnavailable(p.protocol, d.Path, "relay face has no key diagnostic implementation")
	}
	return diagnoser.DiagnoseKey(ctx, key, d)
}

func (p *faceProvider) Close() error {
	if p.station != nil {
		return p.station.closeImplementation(p.protocol)
	}
	return p.impl.Close()
}

// Compile-time checks document the optional capabilities exposed by a face.
var _ provider.Provider = (*faceProvider)(nil)
var _ provider.MultiProtocolProvider = (*faceProvider)(nil)
var _ provider.KeyDiagnoser = (*faceProvider)(nil)

// faceCloseState makes closing a shared station idempotent.  Manager removes
// every face independently, while tests and legacy callers may still close
// the GenericRelayProvider itself.
type faceCloseState struct {
	once sync.Once
	err  error
}
