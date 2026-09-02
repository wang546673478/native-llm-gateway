package provider

import "testing"

func TestRegisterWithProtocolVendor(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithProtocolVendor("deepseek", func(ProviderConfig) (Provider, error) { return nil, nil }, ProtocolOpenAI, "deepseek")
	r.RegisterWithProtocolVendor("deepseek-anthropic", func(ProviderConfig) (Provider, error) { return nil, nil }, ProtocolAnthropic, "deepseek")

	infos := r.ListRegisteredInfo()
	if infos["deepseek"].Vendor != "deepseek" {
		t.Fatalf("deepseek vendor = %q, want deepseek", infos["deepseek"].Vendor)
	}
	if infos["deepseek-anthropic"].Vendor != "deepseek" {
		t.Fatalf("deepseek-anthropic vendor = %q, want deepseek", infos["deepseek-anthropic"].Vendor)
	}
	if infos["deepseek"].Protocol != ProtocolOpenAI {
		t.Fatalf("deepseek protocol = %q, want openai", infos["deepseek"].Protocol)
	}
	if got := r.VendorFor("deepseek-anthropic"); got != "deepseek" {
		t.Fatalf("VendorFor = %q, want deepseek", got)
	}
}

func TestVendorForDefault(t *testing.T) {
	r := NewRegistry()
	r.Register("solo", func(ProviderConfig) (Provider, error) { return nil, nil })
	if got := r.VendorFor("solo"); got != "solo" {
		t.Fatalf("VendorFor default = %q, want solo", got)
	}
	if got := r.VendorFor("unknown"); got != "unknown" {
		t.Fatalf("VendorFor unknown = %q, want unknown", got)
	}
}

func TestUnregisterRelayRemovesOnlyRelayMetadata(t *testing.T) {
	r := NewRegistry()
	r.Register("builtin", func(ProviderConfig) (Provider, error) { return nil, nil })
	r.RegisterWithProtocolVendorRelay("dynamic-face", func(ProviderConfig) (Provider, error) { return nil, nil }, ProtocolAnthropic, "dynamic", true)

	if !r.IsRelay("dynamic-face") || r.ProtocolFor("dynamic-face") != ProtocolAnthropic || r.VendorFor("dynamic-face") != "dynamic" {
		t.Fatalf("relay registration metadata was not installed")
	}
	r.UnregisterRelay("dynamic-face")
	if r.IsRelay("dynamic-face") {
		t.Fatalf("deleted relay still marked as relay")
	}
	if got := r.ProtocolFor("dynamic-face"); got != "" {
		t.Fatalf("deleted relay protocol = %q, want empty", got)
	}
	if got := r.VendorFor("dynamic-face"); got != "dynamic-face" {
		t.Fatalf("deleted relay vendor = %q, want name fallback", got)
	}
	if _, ok := r.ListRegisteredInfo()["dynamic-face"]; ok {
		t.Fatalf("deleted relay still present in registered info")
	}

	// UnregisterRelay must not remove an ordinary provider registration.
	r.UnregisterRelay("builtin")
	if _, ok := r.ListRegisteredInfo()["builtin"]; !ok {
		t.Fatalf("ordinary provider was removed by UnregisterRelay")
	}
}
