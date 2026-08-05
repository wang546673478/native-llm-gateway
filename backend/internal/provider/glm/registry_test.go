// Package glm — 注册回归测试
// P-provider-vendor: 双协议注册(openai + anthropic)是本次重构的核心不变量,
// 用永久测试钉住,防止未来误删其中一个注册名
package glm

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

func TestRegistration_TwoProtocols(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// "glm" — openai 协议,vendor = glm
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

	// "glm-anthropic" — anthropic 协议,同一 vendor
	ga, ok := infos[anthropicName]
	if !ok {
		t.Fatalf("registered name %q missing", anthropicName)
	}
	if ga.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", anthropicName, ga.Protocol)
	}
	if ga.Vendor != "glm" {
		t.Errorf("%s vendor = %q, want glm", anthropicName, ga.Vendor)
	}

	// B-glm-quota: 两个注册名都必须注册 balancer — 漏一个 → 该协议面额度永不
	// 标记/恢复(server.vendorHasBalancer 据此决定 poll vs probe 模式)
	for _, n := range []string{name, anthropicName} {
		if quotacheck.LookupBalancer(n) == nil {
			t.Errorf("balancer not registered for %q", n)
		}
	}
}
