package circuit

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// TestDefaultCountableAlignsWithKeypool 守卫测试:circuit 默认熔断计数集合
// (defaultCountableErrors)必须与 keypool 触发熔断的错误集合(keypool.TripsBreaker)
// 完全一致。两处独立决定「哪个 errType 计入熔断」,靠 keypool.ReportError 先判断是否
// 调 RecordFailure、circuit.shouldCount 再判断是否真的计数 —— 任何一边改集合另一边
// 漏改,熔断行为都会静默漂移(该熔断的不熔断 / 不该熔断的误熔断)。
//
// 放 circuit 侧:keypool 生产不 import circuit(兄弟包),circuit_test → keypool
// 无 import cycle。
func TestDefaultCountableAlignsWithKeypool(t *testing.T) {
	for errType := range defaultCountableErrors {
		if !keypool.TripsBreaker(errType) {
			t.Errorf("circuit 默认计入熔断 %q,但 keypool.TripsBreaker 为 false — 熔断计数集合漂移了!请同步", errType)
		}
	}
	// 反向:keypool 视为触发熔断的,circuit 默认也应计入
	for _, e := range []string{"server_error", "timeout", "connection"} {
		if !defaultCountableErrors[e] {
			t.Errorf("keypool 触发熔断 %q,但 circuit 默认不计入 — 熔断计数集合漂移了!请同步", e)
		}
	}
}
