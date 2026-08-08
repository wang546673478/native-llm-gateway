package keypool

import "testing"

// TestTierOrderBalance 守卫:跨包 tier 优先级(tier 降档顺序 token_plan → api → free)
// 必须固定。proxy/router/quotacheck 都消费 keypool.TierOrder,任何改动都要先审。
func TestTierOrderBalance(t *testing.T) {
	want := []string{"token_plan", "api", "free"}
	if len(TierOrder) != len(want) {
		t.Fatalf("TierOrder 长度 %d != %d", len(TierOrder), len(want))
	}
	for i, w := range want {
		if TierOrder[i] != w {
			t.Fatalf("TierOrder[%d]=%q, want %q", i, TierOrder[i], w)
		}
	}
}

// TestNormalizeBillingSource 守卫:Normalize 只接受 token_plan/api/free,空→api,非法→false。
// proxy/router/auth/quotacheck/DB 都依赖这三个值,不接受第四种值。
func TestNormalizeBillingSource(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "api", true}, // 默认兜底
		{"api", "api", true},
		{"token_plan", "token_plan", true},
		{"free", "free", true},
		{"ENTERPRISE", "", false}, // 新档位必须先在各消费方同步后再放开,禁止直接注入
		{"enterprise", "", false},
		{"", "api", true},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("Normalize(%q)=%q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestIsTokenPlan 守卫:IsTokenPlan 判定一致
func TestIsTokenPlan(t *testing.T) {
	if !IsTokenPlan("token_plan") {
		t.Error("IsTokenPlan(token_plan) should be true")
	}
	if IsTokenPlan("api") || IsTokenPlan("free") || IsTokenPlan("") {
		t.Error("IsTokenPlan should reject non-token_plan")
	}
}
