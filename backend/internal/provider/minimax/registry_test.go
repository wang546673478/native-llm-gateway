// Package minimax — 注册回归测试
// P-provider-vendor: 双协议注册(anthropic + openai)是本次重构的核心不变量,
// 用永久测试钉住,防止未来误删其中一个注册名
package minimax

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_TwoProtocols(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// "minimax" — anthropic 协议,vendor = minimax
	m, ok := infos[name]
	if !ok {
		t.Fatalf("registered name %q missing", name)
	}
	if m.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", name, m.Protocol)
	}
	if m.Vendor != "minimax" {
		t.Errorf("%s vendor = %q, want minimax", name, m.Vendor)
	}

	// "minimax-openai" — openai 协议,同一 vendor
	mo, ok := infos[openaiName]
	if !ok {
		t.Fatalf("registered name %q missing", openaiName)
	}
	if mo.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", openaiName, mo.Protocol)
	}
	if mo.Vendor != "minimax" {
		t.Errorf("%s vendor = %q, want minimax", openaiName, mo.Vendor)
	}
}
