package provider

// NewError 构造 *ProviderError。
//
// 单一职责:anthropic_compatible / google 两个 base 此前各有一份字节相同的
// newError(仅 ProviderName 取 b.cfg.Name)。抽出单源,消除复制粘贴型耦合 —
// 若给 ProviderError 加字段/改构造逻辑,只改这一处,不再漏改。
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
