// Package glm — 注册回归测试
// P-provider-vendor: 注册名 + vendor 是重构核心不变量,用永久测试钉住,
// 防止未来误删注册名或 vendor 填错
package glm

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_OpenAIProtocol(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	g, ok := infos[name]
	if !ok {
		t.Fatalf("registered name %q missing", name)
	}
	if g.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", name, g.Protocol)
	}
	if g.Vendor != "glm" {
		t.Errorf("%s vendor = %q, want glm", name, g.Vendor)
	}
}
