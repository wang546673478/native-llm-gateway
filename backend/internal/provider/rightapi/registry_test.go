// Package rightapi — 注册回归测试。
// P-provider-vendor: 五个协议面注册（codex/grok/gemini 走 openai，claude/claude-aws
// 走 anthropic，vendor 都是 rightapi）是核心不变量，用永久测试钉住，防止未来误删。
package rightapi

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_AllFaces(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// 每个注册面 → 期望的 (协议, vendor)
	want := map[string]struct {
		proto  provider.Protocol
		vendor string
	}{
		codexName:     {provider.ProtocolOpenAI, vendor},
		grokName:      {provider.ProtocolOpenAI, vendor},
		geminiName:    {provider.ProtocolOpenAI, vendor},
		claudeName:    {provider.ProtocolAnthropic, vendor},
		claudeAWSName: {provider.ProtocolAnthropic, vendor},
	}
	if len(infos) < len(want) {
		t.Fatalf("registered count = %d, want at least %d faces", len(infos), len(want))
	}
	for name, w := range want {
		info, ok := infos[name]
		if !ok {
			t.Errorf("registered name %q missing", name)
			continue
		}
		if info.Protocol != w.proto {
			t.Errorf("%s protocol = %q, want %q", name, info.Protocol, w.proto)
		}
		if info.Vendor != w.vendor {
			t.Errorf("%s vendor = %q, want %q", name, info.Vendor, w.vendor)
		}
	}
}
