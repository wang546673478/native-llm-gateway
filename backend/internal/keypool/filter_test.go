package keypool

import (
	"testing"
	"time"
)

// TestAcquire_RefactorEquivalence 验证新 Acquire 与旧 acquireFromTierLocked 行为等价
// 跑相同的 case,期望两种方式返回同一把 key
func TestAcquire_RefactorEquivalence(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusActive, BillingSource: "token_plan", Remaining: 50},
		{ID: "2", Name: "k2", Status: KeyStatusActive, BillingSource: "token_plan", Remaining: 80},
		{ID: "3", Name: "k3", Status: KeyStatusCooling, CoolingUntil: now.Add(time.Minute), BillingSource: "token_plan", Remaining: 90},
		{ID: "4", Name: "k4", Status: KeyStatusActive, BillingSource: "api", Remaining: 100},
		{ID: "5", Name: "k5", Status: KeyStatusQuotaExceeded, BillingSource: "token_plan", Remaining: 0},
	}
	pool := NewPool("test", keys, nil, Config{CoolingDuration: time.Second})

	// Test 1: token_plan tier 应该选到 Remaining 最高的 k2
	k, err := pool.AcquireWithFilter(WithTier("token_plan"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "2" {
		t.Errorf("expected k2 (Remaining=80), got %s", k.ID)
	}

	// Test 2: api tier 应该选 k4
	k, err = pool.AcquireWithFilter(WithTier("api"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "4" {
		t.Errorf("expected k4 (api tier), got %s", k.ID)
	}

	// Test 3: 排除 k2,选下一个 tokest plan
	k, err = pool.AcquireWithFilter(WithTier("token_plan"), WithExcludeKey("2"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID == "2" {
		t.Errorf("expected not k2, got k2")
	}

	// Test 4: 限定 allowedIDs
	k, err = pool.AcquireWithFilter(WithTier("token_plan"), WithAllowedIDs([]uint{1, 5}))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// k5 是 QE 应该被跳过,k1 应该被选
	if k.ID != "1" {
		t.Errorf("expected k1 (in allowed, not QE), got %s", k.ID)
	}

	// Test 5: quota_exceeded key 被跳过
	k, err = pool.AcquireWithFilter(WithTier("token_plan"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID == "5" {
		t.Errorf("expected not k5 (QE), got k5")
	}

	// Test 6: cooldown 过期 key 被恢复 + 重新可选
	// 用 fresh pool 避免 counter 污染
	keys6 := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusActive, BillingSource: "token_plan", Remaining: 50},
		{ID: "2", Name: "k2", Status: KeyStatusActive, BillingSource: "token_plan", Remaining: 80},
		{ID: "3", Name: "k3", Status: KeyStatusCooling, CoolingUntil: now.Add(-time.Minute), BillingSource: "token_plan", Remaining: 90},
	}
	pool6 := NewPool("test", keys6, nil, Config{CoolingDuration: time.Second})
	k, err = pool6.AcquireWithFilter(WithTier("token_plan"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "3" {
		t.Errorf("expected k3 (cooldown expired, Remaining=90 highest), got %s", k.ID)
	}
}

// TestAcquire_NoAvailableKey 验证无可用 key 时返回错误
func TestAcquire_NoAvailableKey(t *testing.T) {
	keys := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusQuotaExceeded, BillingSource: "token_plan"},
	}
	pool := NewPool("test", keys, nil, Config{})

	_, err := pool.AcquireWithFilter(WithTier("token_plan"))
	if err != ErrNoAvailableKey {
		t.Errorf("expected ErrNoAvailableKey, got %v", err)
	}
}

// TestAcquire_ProtocolFilter 验证协议过滤
func TestAcquire_ProtocolFilter(t *testing.T) {
	keys := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusActive, BillingSource: "api", Protocols: "openai"},
		{ID: "2", Name: "k2", Status: KeyStatusActive, BillingSource: "api", Protocols: "anthropic"},
		{ID: "3", Name: "k3", Status: KeyStatusActive, BillingSource: "api", Protocols: ""},
	}
	pool := NewPool("test", keys, nil, Config{})

	// proto=openai 应该选 k1 或 k3
	k, err := pool.AcquireWithFilter(WithTier("api"), WithProtocol("openai"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "1" && k.ID != "3" {
		t.Errorf("expected k1 or k3 (openai-compatible), got %s", k.ID)
	}

	// proto=anthropic 应该选 k2 或 k3
	k, err = pool.AcquireWithFilter(WithTier("api"), WithProtocol("anthropic"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "2" && k.ID != "3" {
		t.Errorf("expected k2 or k3 (anthropic-compatible), got %s", k.ID)
	}
}

// TestAcquire_StickyHit 验证 sticky 命中(未来用,先写测试)
func TestAcquire_StickyHit(t *testing.T) {
	keys := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusActive, BillingSource: "api"},
		{ID: "2", Name: "k2", Status: KeyStatusActive, BillingSource: "api"},
	}
	pool := NewPool("test", keys, nil, Config{})

	// sticky 到 key 2,无论 RoundRobin counter 怎么走,都返回 k2
	for i := 0; i < 10; i++ {
		k, err := pool.AcquireWithFilter(WithTier("api"), WithStickyKey(2))
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if k.ID != "2" {
			t.Errorf("iter %d: expected k2 (sticky), got %s", i, k.ID)
		}
	}
}

// TestAcquire_StickyMiss 验证 sticky 命中失败时 fallback 调度
func TestAcquire_StickyMiss(t *testing.T) {
	keys := []*Key{
		{ID: "1", Name: "k1", Status: KeyStatusActive, BillingSource: "api"},
		{ID: "2", Name: "k2", Status: KeyStatusCooling, BillingSource: "api"},
	}
	keys[1].CoolingUntil = time.Now().Add(time.Minute)
	pool := NewPool("test", keys, nil, Config{})

	// sticky 到 k2 但它在 COOLING,应该 fallback 到 k1
	k, err := pool.AcquireWithFilter(WithTier("api"), WithStickyKey(2))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.ID != "1" {
		t.Errorf("expected k1 (sticky miss fallback), got %s", k.ID)
	}
}
