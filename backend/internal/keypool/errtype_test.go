package keypool

import (
	"testing"
)

// TestTripsBreaker 验证哪些错误类型触发熔断(与 circuit 默认可计数集、provider 枚举一致)
// 跨包一致性守卫:keypool.ErrorType 与 provider.ErrorType 的值对齐由
// internal/provider/errtype_alignment_test.go 兜底(放 provider 侧避免 import cycle)。
func TestTripsBreaker(t *testing.T) {
	trip := map[string]bool{
		"server_error":        true,
		"timeout":             true,
		"connection":          true,
		"client_disconnected": false,
		"rate_limit":          false,
		"auth":                false,
		"invalid_request":     false,
		"quota_exceeded":      false,
		"model_not_found":     false,
		"":                    false,
	}
	for errType, want := range trip {
		if got := TripsBreaker(errType); got != want {
			t.Errorf("TripsBreaker(%q)=%v, want %v", errType, got, want)
		}
	}
}
