package relay

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// DiagnoseKey delegates an explicit one-key probe to the protocol
// implementation selected by the request. The wrapped compatible bases own
// authentication construction and the no-state diagnostic transport.
func (p *GenericRelayProvider) DiagnoseKey(ctx context.Context, key *keypool.Key, d provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
	proto := d.Protocol
	if proto == "" {
		// An explicit endpoint path is stronger than the station's primary
		// protocol in multi-protocol mode.  Otherwise an omitted protocol on
		// /v1/messages would accidentally select the OpenAI primary face and
		// report an opaque upstream failure instead of capability-unavailable.
		if inferred, ok := provider.DiagnosticProtocolForPath(d.Path); ok {
			proto = inferred
		} else {
			proto = p.primaryProtocol
		}
	}
	impl, ok := p.implementations[proto]
	if !ok {
		return nil, provider.NewDiagnosticUnavailable(proto, d.Path, fmt.Sprintf("relay %s does not support requested protocol", p.name))
	}
	diagnoser, ok := impl.(provider.KeyDiagnoser)
	if !ok {
		return nil, provider.NewDiagnosticUnavailable(proto, d.Path, fmt.Sprintf("relay %s protocol %q has no diagnostic implementation", p.name, proto))
	}
	// An omitted protocol is resolved to the selected face before delegation;
	// this keeps the downstream path/protocol validator from treating a valid
	// relay request as an unspecified protocol and prevents cross-protocol
	// fallback.
	d.Protocol = proto
	return diagnoser.DiagnoseKey(ctx, key, d)
}
