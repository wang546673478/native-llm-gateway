// Package deepseek — 注册回归测试
// P-provider-vendor: 双协议注册(openai + anthropic)是本次重构的核心不变量,
// 用永久测试钉住,防止未来误删其中一个注册名
package deepseek

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_TwoProtocols(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// "deepseek" — openai 协议,vendor = deepseek
	ds, ok := infos[name]
	if !ok {
		t.Fatalf("registered name %q missing", name)
	}
	if ds.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", name, ds.Protocol)
	}
	if ds.Vendor != "deepseek" {
		t.Errorf("%s vendor = %q, want deepseek", name, ds.Vendor)
	}

	// "deepseek-anthropic" — anthropic 协议,同一 vendor
	dsc, ok := infos[anthropicName]
	if !ok {
		t.Fatalf("registered name %q missing", anthropicName)
	}
	if dsc.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", anthropicName, dsc.Protocol)
	}
	if dsc.Vendor != "deepseek" {
		t.Errorf("%s vendor = %q, want deepseek", anthropicName, dsc.Vendor)
	}
}
