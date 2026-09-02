package provider

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// TestErrorTypeAlignment 守卫测试:provider.ErrorType 的值必须与 keypool.ErrorType
// 完全一致。两者是跨包的两套枚举(值耦合),Provider 产生的错误经 string(errType)
// 传给 keypool.ReportError 用 switch 匹配 —— 一旦某边改名/增删,这里立即失败,
// 防止熔断/冷却/配额判定静默漂移(ReportError 收到 provider 值,却用不相等的
// keypool 字面量匹配 → key 不熔断/不冷却,静默故障)。
//
// 放 provider 侧而非 keypool 侧:provider 已依赖 keypool,若放 keypool 测试会
// 形成 keypool_test → provider → keypool 的 import cycle。
func TestErrorTypeAlignment(t *testing.T) {
	alignments := []struct {
		k    keypool.ErrorType
		prov ErrorType
	}{
		{keypool.ErrorTypeRateLimit, ErrorTypeRateLimit},
		{keypool.ErrorTypeAuth, ErrorTypeAuth},
		{keypool.ErrorTypeInvalidRequest, ErrorTypeInvalidRequest},
		{keypool.ErrorTypeServerError, ErrorTypeServerError},
		{keypool.ErrorTypeTimeout, ErrorTypeTimeout},
		{keypool.ErrorTypeConnection, ErrorTypeConnection},
		{keypool.ErrorTypeClientDisconnected, ErrorTypeClientDisconnected},
		{keypool.ErrorTypeQuotaExceeded, ErrorTypeQuotaExceeded},
	}
	for _, a := range alignments {
		if string(a.k) != string(a.prov) {
			t.Errorf("keypool %q != provider %q — 两套错误枚举漂移了!请同步 internal/keypool/errtype.go 与 internal/provider/provider.go",
				a.k, a.prov)
		}
	}
}
