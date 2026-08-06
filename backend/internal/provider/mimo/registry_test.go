// Package mimo — 注册回归测试
// P-provider-vendor: 四协议注册(openai/anthropic × 按量付费/套餐)是本次重构的核心
// 不变量,用永久测试钉住,防止未来误删其中一个注册名
package mimo

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestRegistration_FourFaces(t *testing.T) {
	infos := provider.Default().ListRegisteredInfo()

	// "mimo" — openai 协议,按量付费端点,vendor = mimo
	mo, ok := infos[name]
	if !ok {
		t.Fatalf("registered name %q missing", name)
	}
	if mo.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", name, mo.Protocol)
	}
	if mo.Vendor != "mimo" {
		t.Errorf("%s vendor = %q, want mimo", name, mo.Vendor)
	}

	// "mimo-token-plan" — openai 协议,套餐端点,同一 vendor
	mt, ok := infos[tokenPlanName]
	if !ok {
		t.Fatalf("registered name %q missing", tokenPlanName)
	}
	if mt.Protocol != provider.ProtocolOpenAI {
		t.Errorf("%s protocol = %q, want openai", tokenPlanName, mt.Protocol)
	}
	if mt.Vendor != "mimo" {
		t.Errorf("%s vendor = %q, want mimo", tokenPlanName, mt.Vendor)
	}

	// "mimo-anthropic" — anthropic 协议,同一 vendor
	ma, ok := infos[anthropicName]
	if !ok {
		t.Fatalf("registered name %q missing", anthropicName)
	}
	if ma.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", anthropicName, ma.Protocol)
	}
	if ma.Vendor != "mimo" {
		t.Errorf("%s vendor = %q, want mimo", anthropicName, ma.Vendor)
	}

	// "mimo-token-plan-anthropic" — anthropic 协议,同一 vendor
	mta, ok := infos[tokenPlanAnthropicName]
	if !ok {
		t.Fatalf("registered name %q missing", tokenPlanAnthropicName)
	}
	if mta.Protocol != provider.ProtocolAnthropic {
		t.Errorf("%s protocol = %q, want anthropic", tokenPlanAnthropicName, mta.Protocol)
	}
	if mta.Vendor != "mimo" {
		t.Errorf("%s vendor = %q, want mimo", tokenPlanAnthropicName, mta.Vendor)
	}
}
