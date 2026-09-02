package anthropic_compatible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// DiagnoseKey executes one minimal Anthropic Messages request with exactly the
// supplied key. It is intentionally separate from SendRequest: diagnostics are
// observability operations and must not alter key cooldown, quota, breaker, or
// request counters.
func (b *Base) DiagnoseKey(ctx context.Context, key *keypool.Key, d provider.KeyDiagnosticRequest) (*provider.KeyDiagnosticResult, error) {
	if key == nil {
		return nil, errors.New("diagnostic key is required")
	}
	if d.Protocol != "" && d.Protocol != provider.ProtocolAnthropic {
		return nil, provider.NewDiagnosticUnavailable(d.Protocol, d.Path, "anthropic face does not support requested protocol")
	}
	if err := provider.ValidateDiagnosticProtocolPath(provider.ProtocolAnthropic, d.Path); err != nil {
		return nil, err
	}
	if d.Model == "" {
		return nil, errors.New("diagnostic model is required")
	}
	path := d.Path
	if path == "" {
		path = "/v1/messages"
	}

	body, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model:     d.Model,
		MaxTokens: 1,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: "diagnostic"}},
	})
	if err != nil {
		return nil, fmt.Errorf("build diagnostic request: %w", err)
	}

	req := &provider.Request{
		Method:         http.MethodPost,
		Path:           path,
		Headers:        make(http.Header),
		Body:           body,
		Model:          d.Model,
		RequestedModel: d.Model,
		Key:            key,
	}
	target, err := b.requestURL(req)
	if err != nil {
		return nil, fmt.Errorf("build diagnostic URL: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build diagnostic HTTP request: %w", err)
	}
	b.applyRequestHeaders(httpReq, req, key, false)

	result := &provider.KeyDiagnosticResult{
		ProviderName:    b.cfg.Name,
		KeyID:           key.ID,
		Protocol:        provider.ProtocolAnthropic,
		DiagnosticAbort: false,
	}
	started := time.Now()
	resp, err := b.client.Do(httpReq)
	if err != nil {
		result.LatencyMs = provider.Milliseconds(time.Since(started))
		result.ErrorType = provider.ClassifyTransportError(ctx, err)
		result.DiagnosticAbort = provider.DiagnosticWasAborted(ctx, err)
		result.Message = "upstream request failed"
		return result, nil
	}
	defer resp.Body.Close()

	bodyStarted := time.Now()
	respBody, firstByte, readErr := provider.ReadDiagnosticBody(resp.Body)
	result.Reachable = true
	result.StatusCode = resp.StatusCode
	result.LatencyMs = provider.Milliseconds(time.Since(started))
	if firstByte > 0 {
		result.FirstByteMs = provider.Milliseconds(bodyStarted.Sub(started) + firstByte)
	}
	if readErr != nil {
		result.ErrorType = provider.ClassifyTransportError(ctx, readErr)
		result.DiagnosticAbort = provider.DiagnosticWasAborted(ctx, readErr)
		result.Message = "upstream response read failed"
		return result, nil
	}

	errType, message := classifyDiagnosticAnthropic(resp.StatusCode, respBody)
	if errType != "" {
		result.ErrorType = errType
		result.Message = message
	}
	return result, nil
}

// classifyDiagnosticAnthropic intentionally does not use balanceGuardHealthy:
// a diagnostic reports what this key/upstream returned, rather than applying
// request-path heuristics that may downgrade a quota response to rate_limit.
func classifyDiagnosticAnthropic(status int, body []byte) (provider.ErrorType, string) {
	if code, _ := provider.ParseMiniMaxBaseResp(body); code != 0 {
		if provider.IsMiniMaxQuotaCode(code) {
			return provider.ErrorTypeQuotaExceeded, fmt.Sprintf("upstream quota response (%d)", code)
		}
		return provider.ErrorTypeServerError, fmt.Sprintf("upstream error response (%d)", code)
	}
	if status >= 400 || status < 200 || status >= 300 {
		return provider.ClassifyErrorWithBody(status, body), fmt.Sprintf("upstream returned %d", status)
	}
	return "", ""
}
