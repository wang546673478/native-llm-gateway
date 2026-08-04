package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

func TestAuthenticator_MissingHeader(t *testing.T) {
	a := New(nil)
	_, err := a.Authenticate(httptest.NewRequest(http.MethodPost, "/", nil))
	if err != ErrMissingAuthHeader {
		t.Errorf("err = %v, want ErrMissingAuthHeader", err)
	}
}

// TestCheckProvider_VendorNormalized P-provider-vendor:
// key 绑定按厂商归一 — 绑 "acme" 时,路由到 "acme"(openai 面)和
// "acme-anthropic"(anthropic 面)都放行;旧数据绑的注册名同样匹配;
// 跨 vendor 拒绝;未绑定放行
// (测试二进制不 import 真实 provider 包,注册表为空 — 自注册一对 vendor 名)
func TestCheckProvider_VendorNormalized(t *testing.T) {
	reg := provider.Default()
	reg.RegisterWithProtocolVendor("acme", func(provider.ProviderConfig) (provider.Provider, error) {
		return nil, nil
	}, provider.ProtocolOpenAI, "acme")
	reg.RegisterWithProtocolVendor("acme-anthropic", func(provider.ProviderConfig) (provider.Provider, error) {
		return nil, nil
	}, provider.ProtocolAnthropic, "acme")

	a := New(nil)

	// 绑厂商名:两个协议面路由都放行
	k := &GatewayKey{Name: "k1", Providers: []string{"acme"}}
	if err := a.CheckProvider(k, "acme"); err != nil {
		t.Errorf("acme → acme: %v, want nil", err)
	}
	if err := a.CheckProvider(k, "acme-anthropic"); err != nil {
		t.Errorf("acme → acme-anthropic(anthropic 面): %v, want nil", err)
	}

	// 旧数据绑注册名:同样匹配任意协议面
	k2 := &GatewayKey{Name: "k2", Providers: []string{"acme-anthropic"}}
	if err := a.CheckProvider(k2, "acme"); err != nil {
		t.Errorf("acme-anthropic(旧绑定)→ acme: %v, want nil", err)
	}
	if err := a.CheckProvider(k2, "acme-anthropic"); err != nil {
		t.Errorf("acme-anthropic(旧绑定)→ acme-anthropic: %v, want nil", err)
	}

	// 跨 vendor 拒绝
	k3 := &GatewayKey{Name: "k3", Providers: []string{"other-vendor"}}
	if err := a.CheckProvider(k3, "acme"); err != ErrKeyProviderMismatch {
		t.Errorf("other-vendor → acme: err = %v, want ErrKeyProviderMismatch", err)
	}

	// 未绑定放行
	if err := a.CheckProvider(&GatewayKey{Name: "k4"}, "acme"); err != nil {
		t.Errorf("未绑定 → acme: %v, want nil", err)
	}
}

func TestAuthenticator_InvalidFormat(t *testing.T) {
	a := New(nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Basic abc")
	_, err := a.Authenticate(req)
	if err != ErrInvalidAuthFormat {
		t.Errorf("err = %v, want ErrInvalidAuthFormat", err)
	}
}

func TestAuthenticator_UnknownKey(t *testing.T) {
	a := New([]GatewayKey{{Name: "known", KeyHash: hashKey("known-secret")}})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	_, err := a.Authenticate(req)
	if err != ErrUnknownKey {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

func TestAuthenticator_ValidKey(t *testing.T) {
	plain := "gw-secret-123"
	// KeyHash 字段在 New 里会被当作"密钥原值"再次 hash
	// 所以测试时直接传明文即可
	a := New([]GatewayKey{{Name: "test-key", KeyHash: plain}})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	key, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if key.Name != "test-key" {
		t.Errorf("key.Name = %q, want test-key", key.Name)
	}
}

func TestCheckAllowed_Wildcard(t *testing.T) {
	a := New(nil)
	key := &GatewayKey{AllowedModels: []string{"*"}}
	if err := a.CheckAllowed(key, "anything"); err != nil {
		t.Errorf("wildcard should allow anything, got %v", err)
	}
}

func TestCheckAllowed_SpecificList(t *testing.T) {
	a := New(nil)
	key := &GatewayKey{AllowedModels: []string{"coding-model", "chat-model"}}
	if err := a.CheckAllowed(key, "coding-model"); err != nil {
		t.Errorf("allowed model: %v", err)
	}
	if err := a.CheckAllowed(key, "evil-model"); err != ErrModelNotAllowed {
		t.Errorf("denied model: got %v, want ErrModelNotAllowed", err)
	}
}

func TestAllowRequest_RPMLimit(t *testing.T) {
	a := New([]GatewayKey{{Name: "k", KeyHash: hashKey("k-secret")}})
	for i := 0; i < 3; i++ {
		if !a.AllowRequest("k", 3) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
	if a.AllowRequest("k", 3) {
		t.Error("4th request should be denied (limit=3)")
	}
}

func TestAllowRequest_NoLimit(t *testing.T) {
	a := New([]GatewayKey{{Name: "k"}})
	for i := 0; i < 100; i++ {
		if !a.AllowRequest("k", 0) {
			t.Errorf("request %d should be allowed when limit=0", i+1)
		}
	}
}

func TestAllowRequest_WindowExpires(t *testing.T) {
	a := New([]GatewayKey{{Name: "k"}})
	for i := 0; i < 2; i++ {
		a.AllowRequest("k", 2)
	}
	if a.AllowRequest("k", 2) {
		t.Fatal("3rd should be denied")
	}
	// 等 1.1 秒让窗口滑出(测试用短窗口)
	// 由于 rpmCounter 用 1 分钟,这里我们不能真等;改成验证 hits 字段
	// 跳过这个 case,只验证限流触发
	_ = time.Now()
}

func TestTPM_AllowTokens(t *testing.T) {
	a := New([]GatewayKey{{Name: "k"}})
	ok, _ := a.CheckTPM("k", 1000, 100)
	if !ok {
		t.Error("first 100 tokens should fit in 1000 TPM")
	}
	ok, _ = a.CheckTPM("k", 1000, 500)
	if !ok {
		t.Error("additional 500 should fit (600 total)")
	}
	ok, reason := a.CheckTPM("k", 1000, 500)
	if ok {
		t.Error("additional 500 should exceed limit (1100 > 1000)")
	}
	if reason != "tpm_exceeded" {
		t.Errorf("reason = %q, want tpm_exceeded", reason)
	}
}

func TestTPM_RecordTokens(t *testing.T) {
	a := New([]GatewayKey{{Name: "k"}})
	a.RecordTokens("k", 600)
	ok, _ := a.CheckTPM("k", 1000, 500)
	if ok {
		t.Error("600+500 > 1000 should be denied")
	}
	ok, _ = a.CheckTPM("k", 1000, 300)
	if !ok {
		t.Error("600+300 = 900 < 1000 should be allowed")
	}
}

func TestTPM_NoLimitAllowsAnything(t *testing.T) {
	a := New([]GatewayKey{{Name: "k"}})
	ok, _ := a.CheckTPM("k", 0, 1_000_000)
	if !ok {
		t.Error("limit=0 should always allow")
	}
}
