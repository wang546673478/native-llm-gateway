package auth

import (
	"encoding/json"
	"testing"
	"time"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

func TestProviderKeyView_IncludesRemainingAndLastPolledAt(t *testing.T) {
	past := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	view := ProviderKeyView{
		ID: 1, ProviderName: "test", Name: "k",
		KeyMasked: "sk-te...est", Enabled: true,
		Status: "ACTIVE", BillingSource: "token_plan",
		CreatedAt: past, UpdatedAt: past,
		Remaining: 7.0, LastPolledAt: &past, QuotaKind: "percent",
	}
	out, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := parsed["remaining"]; !ok {
		t.Error("missing 'remaining' field in JSON output")
	}
	if _, ok := parsed["last_polled_at"]; !ok {
		t.Error("missing 'last_polled_at' field in JSON output")
	}
	if got, _ := parsed["quota_kind"].(string); got != "percent" {
		t.Errorf("quota_kind = %v, want percent", parsed["quota_kind"])
	}
	// nil-pointer case should serialise as null
	view.LastPolledAt = nil
	out, _ = json.Marshal(view)
	if !contains(string(out), `"last_polled_at":null`) {
		t.Errorf("expected last_polled_at:null when LastPolledAt is nil; got %s", string(out))
	}
}

func TestProviderKeyViewFromPool_IncludesQuotaKind(t *testing.T) {
	now := time.Now()
	live := &keypool.Key{Remaining: 43, QuotaKind: "percent", LastPolledAt: now}
	v := toProviderKeyViewFromPool(dbpkg.ProviderAPIKey{
		ProviderName: "test", Name: "k", KeyHash: "sk-1234567890", Enabled: true,
		BillingSource: "token_plan", CreatedAt: now, UpdatedAt: now,
	}, "ACTIVE", live)

	if v.Remaining != 43 {
		t.Errorf("Remaining = %v, want 43", v.Remaining)
	}
	if v.QuotaKind != "percent" {
		t.Errorf("QuotaKind = %q, want %q (live key kind should pass through)", v.QuotaKind, "percent")
	}
	if v.LastPolledAt == nil || !v.LastPolledAt.Equal(now) {
		t.Errorf("LastPolledAt = %v, want %v", v.LastPolledAt, now)
	}
}

// tiny contains helper — keep file lean
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestProviderKeyView_IncludesProtocols(t *testing.T) {
	past := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	view := ProviderKeyView{
		ID: 1, ProviderName: "deepseek", Name: "k",
		KeyMasked: "sk-te...est", Enabled: true,
		Status: "ACTIVE", BillingSource: "api",
		CreatedAt: past, UpdatedAt: past,
		Remaining: 7.0, LastPolledAt: &past, QuotaKind: "currency",
		Protocols: "openai,anthropic",
	}
	out, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !contains(string(out), `"protocols":"openai,anthropic"`) {
		t.Errorf("missing protocols field in JSON output: %s", string(out))
	}
}

func TestProviderKeyViewFromPool_IncludesProtocols(t *testing.T) {
	now := time.Now()
	live := &keypool.Key{Remaining: 43, QuotaKind: "percent", LastPolledAt: now, Protocols: "anthropic"}
	v := toProviderKeyViewFromPool(dbpkg.ProviderAPIKey{
		ProviderName: "deepseek", Name: "k", KeyHash: "sk-1234567890", Enabled: true,
		BillingSource: "api", Protocols: "openai,anthropic", CreatedAt: now, UpdatedAt: now,
	}, "ACTIVE", live)

	if v.Protocols != "anthropic" {
		t.Errorf("Protocols = %q, want anthropic (live key pass-through)", v.Protocols)
	}
}
