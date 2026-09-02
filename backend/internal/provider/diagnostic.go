package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// KeyDiagnosticRequest describes one explicit, single-shot key probe.
// It intentionally contains only protocol routing information; the key
// material is supplied separately and is never part of a response value.
type KeyDiagnosticRequest struct {
	Protocol Protocol
	Path     string
	Model    string
}

// KeyDiagnosticResult is the safe, metadata-only result returned by a key
// diagnostic. Response bodies and headers are deliberately omitted because
// upstream error payloads can contain credentials or user data.
type KeyDiagnosticResult struct {
	ProviderName    string    `json:"provider_name"`
	KeyID           string    `json:"key_id"`
	Protocol        Protocol  `json:"protocol"`
	StatusCode      int       `json:"status_code"`
	ErrorType       ErrorType `json:"error_type,omitempty"`
	Message         string    `json:"message,omitempty"`
	Reachable       bool      `json:"reachable"`
	LatencyMs       int64     `json:"latency_ms"`
	FirstByteMs     int64     `json:"first_byte_ms,omitempty"`
	DiagnosticAbort bool      `json:"diagnostic_abort"`
}

// KeyDiagnoser is implemented by protocol providers that can execute an
// explicit one-key diagnostic without changing scheduler/key-pool state.
// Implementations must perform at most one upstream round trip and must not
// call Pool.ReportSuccess, ReportError, or ReportRateLimit.
type KeyDiagnoser interface {
	DiagnoseKey(context.Context, *keypool.Key, KeyDiagnosticRequest) (*KeyDiagnosticResult, error)
}

// ErrDiagnosticUnavailable identifies a requested diagnostic capability that
// is not implemented by the selected provider face.  It is deliberately
// separate from transport/upstream failures: callers can return a stable
// "diagnostic_unavailable" response without pretending that an upstream
// request was attempted.
var ErrDiagnosticUnavailable = errors.New("provider: key diagnostic unavailable")

// DiagnosticUnavailableError carries only local capability details.  The
// HTTP handler must not expose Error() to clients because it may include an
// internal provider/path name; use IsDiagnosticUnavailable instead.
type DiagnosticUnavailableError struct {
	Protocol Protocol
	Path     string
	Reason   string
}

func (e *DiagnosticUnavailableError) Error() string {
	if e == nil {
		return ErrDiagnosticUnavailable.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", ErrDiagnosticUnavailable, e.Reason)
	}
	return ErrDiagnosticUnavailable.Error()
}

func (e *DiagnosticUnavailableError) Unwrap() error { return ErrDiagnosticUnavailable }

// NewDiagnosticUnavailable creates a typed capability error for provider
// implementations and protocol adapters.
func NewDiagnosticUnavailable(protocol Protocol, path, reason string) error {
	return &DiagnosticUnavailableError{Protocol: protocol, Path: path, Reason: reason}
}

// IsDiagnosticUnavailable reports whether err denotes an unsupported
// provider/protocol/path combination (including wrapped errors).
func IsDiagnosticUnavailable(err error) bool {
	return errors.Is(err, ErrDiagnosticUnavailable)
}

// ProtocolForPath maps a Gateway request path to its wire protocol.
//
// Paths may contain a deployment-specific prefix, so matching is done on
// complete endpoint suffixes rather than arbitrary substrings.  This keeps a
// path such as /v1/messages-extra from being mistaken for Anthropic while
// still allowing /proxy/v1/messages.  The returned bool is false for paths
// that the request router cannot safely dispatch.
func ProtocolForPath(path string) (Protocol, bool) {
	p := strings.ToLower(strings.TrimSpace(path))
	if p == "" || strings.ContainsAny(p, "?\r\n") {
		return "", false
	}
	suffix := func(value string) bool {
		return p == value || strings.HasSuffix(p, value)
	}
	switch {
	case suffix("/v1/messages"), suffix("/messages"):
		return ProtocolAnthropic, true
	case suffix("/v1/chat/completions"), suffix("/chat/completions"),
		suffix("/v1/responses"), suffix("/responses"),
		suffix("/v1/completions"), suffix("/completions"):
		return ProtocolOpenAI, true
	case isGoogleGenerateContentPath(p):
		return ProtocolGoogle, true
	default:
		return "", false
	}
}

// isGoogleGenerateContentPath accepts only a complete Gemini generation
// endpoint.  A substring check (for example strings.Contains(path,
// ":generatecontent")) would classify malformed paths such as
// /v1beta/modelsx:generateContent-extra as Google and could send them to an
// unintended upstream operation.
func isGoogleGenerateContentPath(path string) bool {
	const modelsSegment = "/models/"
	for _, action := range []string{":generatecontent", ":streamgeneratecontent"} {
		if !strings.HasSuffix(path, action) {
			continue
		}
		prefix := strings.TrimSuffix(path, action)
		idx := strings.LastIndex(prefix, modelsSegment)
		if idx < 0 {
			return false
		}
		model := prefix[idx+len(modelsSegment):]
		// A model is one non-empty path segment. Escaped slashes remain part of
		// the segment and are safe to pass to the provider's URL builder.
		if model == "" || strings.Contains(model, "/") {
			return false
		}
		return true
	}
	return false
}

// DiagnosticProtocolForPath maps the supported diagnostic endpoint suffixes
// to their wire protocol.  It intentionally shares the request router's
// matcher so a diagnostic cannot claim a path that the normal router rejects.
func DiagnosticProtocolForPath(path string) (Protocol, bool) {
	return ProtocolForPath(path)
}

// ValidateDiagnosticProtocolPath enforces the explicit path -> protocol
// matrix shared by compatible bases.  An empty path means "use this face's
// default endpoint" and is valid; a non-empty unknown path or a path belonging
// to another protocol is a capability error, never a reason to silently
// downgrade to the provider's primary protocol.
func ValidateDiagnosticProtocolPath(protocol Protocol, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	inferred, ok := DiagnosticProtocolForPath(path)
	if !ok {
		return NewDiagnosticUnavailable(protocol, path, "unsupported diagnostic path")
	}
	if protocol != "" && protocol != inferred {
		return NewDiagnosticUnavailable(protocol, path, "diagnostic path does not match protocol")
	}
	return nil
}

// DiagnosticWasAborted reports whether an unsuccessful diagnostic was ended
// by its caller's context. A transport or body-read error can wrap the
// context sentinel, and some RoundTrippers return their own error after the
// context has become terminal; both cases are an aborted probe rather than a
// key/upstream health result. A nil error means the response was drained (or
// otherwise completed), so a context cancellation that races after EOF does
// not incorrectly mark a successful diagnostic as aborted.
func DiagnosticWasAborted(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// Only the caller's context proves that the probe was deliberately
	// cancelled.  A provider/http.Client-local timeout can also wrap
	// context.DeadlineExceeded while ctx remains live; that is an upstream
	// timeout result, not diagnostic_abort.
	return ctx != nil && ctx.Err() != nil
}

const diagnosticBodyLimit = 1 << 20

// ReadDiagnosticBody drains r completely while retaining only a bounded copy
// for error classification. Draining matters for relay diagnostics: aborting
// after the first byte would make the upstream report client_gone. The second
// return value is the elapsed time until the first body byte, or zero if the
// response had no body.
func ReadDiagnosticBody(r io.Reader) ([]byte, time.Duration, error) {
	if r == nil {
		return nil, 0, nil
	}
	started := time.Now()
	var firstByte time.Duration
	retained := make([]byte, 0, 1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if firstByte == 0 {
				firstByte = time.Since(started)
				if firstByte == 0 {
					firstByte = time.Nanosecond
				}
			}
			if len(retained) < diagnosticBodyLimit {
				remaining := diagnosticBodyLimit - len(retained)
				if n < remaining {
					remaining = n
				}
				retained = append(retained, buf[:remaining]...)
			}
		}
		if err != nil {
			if err == io.EOF {
				return retained, firstByte, nil
			}
			return retained, firstByte, err
		}
		// A conforming Reader may return (0, nil), so keep reading rather than
		// treating a temporary empty read as an upstream failure.
	}
}

// Milliseconds converts a duration for the diagnostic JSON contract. A
// positive duration that rounds below one millisecond is represented as 1 so
// callers can distinguish an observed byte from an empty response.
func Milliseconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}
