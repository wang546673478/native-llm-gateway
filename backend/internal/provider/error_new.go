package provider

import "net/http"

// NewError 构造 *ProviderError。
//
// 单一职责:anthropic_compatible / google 两个 base 此前各有一份字节相同的
// newError(仅 ProviderName 取 b.cfg.Name),openai_compatible 更早是一份内联
// 字面量。抽出单源,消除复制粘贴型耦合 —
// 若给 ProviderError 加字段/改构造逻辑,只改这一处,不再漏改。
//
// rawErr(可选)填 RawError。RetryAfter 不在本构造器里(openai 429 需额外设置
// 时用 pe := NewError(...) 后改 pe.RetryAfter —— 单字段按需 set,不为此扩参)。
func NewError(providerName string, status int, errType ErrorType, msg string, rawErr ...[]byte) *ProviderError {
	pe := &ProviderError{
		ProviderName: providerName,
		StatusCode:   status,
		ErrorType:    errType,
		Message:      msg,
	}
	if len(rawErr) > 0 {
		pe.RawError = rawErr[0]
	}
	return pe
}

// WithUpstreamHeaders attaches an owned copy of the HTTP response headers to
// an error. The copy prevents later transport or retry mutations from changing
// the final response contract.
func WithUpstreamHeaders(pe *ProviderError, headers http.Header) *ProviderError {
	if pe != nil {
		if headers == nil {
			// A custom RoundTripper may legally return a response with no header
			// map. Preserve the fact that an HTTP response was received so relay
			// error handling can still return its status/body instead of turning
			// it into a synthetic 502 transport failure.
			pe.UpstreamHeaders = make(http.Header)
		} else {
			pe.UpstreamHeaders = headers.Clone()
		}
	}
	return pe
}
