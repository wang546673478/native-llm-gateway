// Package rightapi — 注册回归测试。
// P-provider-vendor: 三协议面注册（codex/grok 走 openai、claude 走 anthropic，
// vendor 都是 rightapi）是核心不变量，用永久测试钉住，防止未来误删。
package rightapi

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_ThreeFaces(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// rightapi-codex — openai，vendor = rightapi
	codex, ok := infos[codexName]
	if !ok {
		t.Fatalf("registered name %q missing", codexName)
	}
	if codex.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", codexName, codex.Protocol)
	}
	if codex.Vendor != vendor {
		t.Errorf("%s vendor = %q, want %q", codexName, codex.Vendor, vendor)
	}

	// rightapi-grok — openai，vendor = rightapi
	grok, ok := infos[grokName]
	if !ok {
		t.Fatalf("registered name %q missing", grokName)
	}
	if grok.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", grokName, grok.Protocol)
	}
	if grok.Vendor != vendor {
		t.Errorf("%s vendor = %q, want %q", grokName, grok.Vendor, vendor)
	}

	// rightapi-claude — anthropic，vendor = rightapi
	claude, ok := infos[claudeName]
	if !ok {
		t.Fatalf("registered name %q missing", claudeName)
	}
	if claude.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", claudeName, claude.Protocol)
	}
	if claude.Vendor != vendor {
		t.Errorf("%s vendor = %q, want %q", claudeName, claude.Vendor, vendor)
	}
}
