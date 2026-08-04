package keypool

import (
	"errors"
	"testing"
	"time"
)

func newTestKeys(n int) []*Key {
	keys := make([]*Key, n)
	now := time.Now()
	for i := 0; i < n; i++ {
		keys[i] = &Key{
			ID:           string(rune('a' + i)),
			ProviderName: "test",
			Name:         "k" + string(rune('a'+i)),
			Key:          "sk-test",
			Status:       KeyStatusActive,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
	}
	return keys
}

func TestPool_AcquireReturnsUsableKey(t *testing.T) {
	pool := NewPool("test", newTestKeys(3), NewScheduler("round_robin"), Config{})
	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k == nil {
		t.Fatal("got nil key")
	}
	if k.Status != KeyStatusActive {
		t.Errorf("status = %q, want ACTIVE", k.Status)
	}
}

func TestPool_AcquireWhenAllDisabled(t *testing.T) {
	keys := newTestKeys(2)
	keys[0].Status = KeyStatusDisabled
	keys[1].Status = KeyStatusDisabled
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	_, err := pool.Acquire()
	if !errors.Is(err, ErrNoAvailableKey) {
		t.Errorf("err = %v, want ErrNoAvailableKey", err)
	}
}

func TestPool_AcquireRecoversFromCooling(t *testing.T) {
	keys := newTestKeys(1)
	past := time.Now().Add(-1 * time.Minute)
	keys[0].Status = KeyStatusCooling
	keys[0].CoolingUntil = past
	keys[0].CoolingCount = 1

	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire should recover expired cooling, got err: %v", err)
	}
	if k.Status != KeyStatusActive {
		t.Errorf("status after recover = %q, want ACTIVE", k.Status)
	}
}

func TestPool_AcquireSkipsStillCooling(t *testing.T) {
	keys := newTestKeys(2)
	// k1 还在冷却,k2 可用
	keys[0].Status = KeyStatusCooling
	keys[0].CoolingUntil = time.Now().Add(1 * time.Minute)
	keys[1].Status = KeyStatusActive

	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 多次获取都不应该返回 k1
	for i := 0; i < 5; i++ {
		k, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire iter %d: %v", i, err)
		}
		if k.ID != keys[1].ID {
			t.Errorf("iter %d: got ID %s, want %s (cooling key must be skipped)", i, k.ID, keys[1].ID)
		}
	}
}

func TestPool_ReportRateLimitTriggersCooling(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{
		CoolingDuration: 30 * time.Second,
		MaxCoolingCount: 3,
	})
	k := keys[0]

	pool.ReportRateLimit(k, 0)
	if k.Status != KeyStatusCooling {
		t.Errorf("after 429: status = %q, want COOLING", k.Status)
	}
	if k.CoolingCount != 1 {
		t.Errorf("cooling_count = %d, want 1", k.CoolingCount)
	}
	if !k.CoolingUntil.After(time.Now()) {
		t.Error("CoolingUntil should be in the future")
	}

	// 再次 429,累计
	pool.ReportRateLimit(k, 0)
	if k.CoolingCount != 2 {
		t.Errorf("cooling_count = %d, want 2", k.CoolingCount)
	}

	// 第 4 次应该超过 max=3 → DISABLED
	pool.ReportRateLimit(k, 0)
	pool.ReportRateLimit(k, 0) // 第 4 次
	if k.Status != KeyStatusDisabled {
		t.Errorf("after 4x 429 (max=3): status = %q, want DISABLED", k.Status)
	}
}

func TestPool_ReportErrorDisablesOnAuth(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	pool.ReportError(keys[0], "auth")
	if keys[0].Status != KeyStatusDisabled {
		t.Errorf("after auth error: status = %q, want DISABLED", keys[0].Status)
	}
}

func TestPool_RoundRobinDistributes(t *testing.T) {
	keys := newTestKeys(3)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	seen := make(map[string]int)
	for i := 0; i < 9; i++ {
		k, _ := pool.Acquire()
		seen[k.ID]++
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct keys, got %d", len(seen))
	}
	for id, n := range seen {
		if n != 3 {
			t.Errorf("key %s got %d requests, want 3 (perfect round-robin)", id, n)
		}
	}
}

func TestPool_LeastUsedPicksColdest(t *testing.T) {
	keys := newTestKeys(3)
	keys[0].TotalRequests = 10
	keys[1].TotalRequests = 100
	keys[2].TotalRequests = 1
	pool := NewPool("test", keys, NewScheduler("least_used"), Config{})

	k, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != keys[2].ID {
		t.Errorf("least_used picked ID %s, want %s (lowest TotalRequests)", k.ID, keys[2].ID)
	}
}

func TestPool_Status(t *testing.T) {
	keys := newTestKeys(4)
	keys[0].Status = KeyStatusActive
	keys[1].Status = KeyStatusActive
	keys[2].Status = KeyStatusCooling
	keys[2].CoolingUntil = time.Now().Add(time.Minute)
	keys[3].Status = KeyStatusDisabled

	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	s := pool.Status()
	if s.TotalKeys != 4 {
		t.Errorf("TotalKeys = %d, want 4", s.TotalKeys)
	}
	if s.ActiveKeys != 2 {
		t.Errorf("ActiveKeys = %d, want 2", s.ActiveKeys)
	}
	if s.CoolingKeys != 1 {
		t.Errorf("CoolingKeys = %d, want 1", s.CoolingKeys)
	}
	if s.DisabledKeys != 1 {
		t.Errorf("DisabledKeys = %d, want 1", s.DisabledKeys)
	}
}

// === P64 AcquireFromTier 测试 ===

func newTestKeysWithTiers(tiers []string) []*Key {
	keys := make([]*Key, len(tiers))
	now := time.Now()
	for i, t := range tiers {
		keys[i] = &Key{
			ID:            string(rune('a' + i)),
			ProviderName:  "test",
			Name:          "k" + string(rune('a'+i)),
			Key:           "sk-test",
			Status:        KeyStatusActive,
			BillingSource: t,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	return keys
}

func TestAcquireFromTier_OnlyRequestedTier(t *testing.T) {
	// 池里有 token_plan 和 api key,指定 api → 只返回 api
	keys := newTestKeysWithTiers([]string{"token_plan", "api"})
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	k, err := pool.AcquireFromTier("api", nil)
	if err != nil {
		t.Fatalf("AcquireFromTier(api): %v", err)
	}
	if k.BillingSource != "api" {
		t.Errorf("got tier %s, want api", k.BillingSource)
	}

	k, err = pool.AcquireFromTier("token_plan", nil)
	if err != nil {
		t.Fatalf("AcquireFromTier(token_plan): %v", err)
	}
	if k.BillingSource != "token_plan" {
		t.Errorf("got tier %s, want token_plan", k.BillingSource)
	}
}

func TestAcquireFromTier_EmptyBucketErr(t *testing.T) {
	// 池里只有 token_plan,指定 api → ErrNoAvailableKey(不降级)
	keys := newTestKeysWithTiers([]string{"token_plan"})
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	_, err := pool.AcquireFromTier("api", nil)
	if !errors.Is(err, ErrNoAvailableKey) {
		t.Errorf("expected ErrNoAvailableKey, got %v", err)
	}
}

func TestAcquire_BackwardCompatibleTierFallback(t *testing.T) {
	// P64 保留 Acquire() 的 tier 降级兼容入口
	// token_plan 死了 → Acquire() 降级到 api
	now := time.Now()
	tpKey := &Key{
		ID: "z", ProviderName: "test", Name: "kz", Key: "sk",
		Status: KeyStatusDisabled, BillingSource: "token_plan",
		CreatedAt: now, UpdatedAt: now,
	}
	apiKey := &Key{
		ID: "a", ProviderName: "test", Name: "ka", Key: "sk",
		Status: KeyStatusActive, BillingSource: "api",
		CreatedAt: now, UpdatedAt: now,
	}
	pool := NewPool("test", []*Key{tpKey, apiKey}, NewScheduler("round_robin"), Config{})

	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if k.BillingSource != "api" {
		t.Errorf("expected api fallback, got %s", k.BillingSource)
	}
}

func TestAcquireFromTier_AllowedIDFilter(t *testing.T) {
	// P34 + P64: 指定 tier + ID 白名单
	now := time.Now()
	mkKey := func(id, tier string) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: "k", Key: "sk",
			Status: KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	keys := []*Key{
		mkKey("100", "token_plan"),
		mkKey("200", "token_plan"),
		mkKey("300", "api"),
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 只允许 ID=100 的 token_plan key
	set := map[uint]struct{}{100: {}}
	k, err := pool.AcquireFromTier("token_plan", set)
	if err != nil {
		t.Fatalf("AcquireFromTier: %v", err)
	}
	if k.ID != "100" {
		t.Errorf("got ID %s, want 100", k.ID)
	}

	// ID=300 是 api,白名单只有 100 → api 桶空
	_, err = pool.AcquireFromTier("api", set)
	if !errors.Is(err, ErrNoAvailableKey) {
		t.Errorf("expected ErrNoAvailableKey, got %v", err)
	}
}

// P68: Test 1 — quota_exceeded 不再被设 DISABLED,标成 QUOTA_EXCEEDED
func TestPool_QuotaExceededIsNotDisabled(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 装回调,验证会 fire
	called := false
	pool.OnQuotaExceeded = func(k *Key, _ KeyStatus) { called = true }

	pool.ReportError(keys[0], "quota_exceeded")
	k := keys[0]
	if k.Status != KeyStatusQuotaExceeded {
		t.Errorf("after quota_exceeded: status = %q, want QUOTA_EXCEEDED", k.Status)
	}
	if k.QuotaProbeAttempts != 0 {
		t.Errorf("QuotaProbeAttempts = %d, want 0", k.QuotaProbeAttempts)
	}
	if k.QuotaExceededSince.IsZero() {
		t.Errorf("QuotaExceededSince not set")
	}
	if k.IsUsable(time.Now()) {
		t.Errorf("IsUsable should be false during QUOTA_EXCEEDED")
	}
	if !called {
		t.Errorf("OnQuotaExceeded callback not fired")
	}
}

// P68: Test 2 — RestoreQuota 让 key 回到 ACTIVE
func TestPool_RestoreQuotaMakesUsable(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	pool.ReportError(keys[0], "quota_exceeded")
	pool.RestoreQuota(keys[0])

	k := keys[0]
	if k.Status != KeyStatusActive {
		t.Errorf("after RestoreQuota: status = %q, want ACTIVE", k.Status)
	}
	if !k.IsUsable(time.Now()) {
		t.Errorf("IsUsable should be true after RestoreQuota")
	}
	if !k.QuotaExceededSince.IsZero() {
		t.Errorf("QuotaExceededSince should be reset, got %v", k.QuotaExceededSince)
	}

	// Acquire 应该能拿到
	got, err := pool.AcquireFromTier("api", nil)
	if err != nil {
		t.Fatalf("AcquireFromTier: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("Acquire returned %s, want %s", got.ID, k.ID)
	}
}

// P68: Test 3 — Router 视角:QuotaExceeded 时 Acquire 跳过,Restore 后能拿到
func TestPool_AcquireSkipsQuotaExceededButCanReturnAfterRestore(t *testing.T) {
	keys := newTestKeys(3)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 标 k1 为 quota_exceeded
	pool.ReportError(keys[0], "quota_exceeded")

	// 跑 5 次 Acquire,k1 永远不出现
	seen := map[string]int{}
	for i := 0; i < 5; i++ {
		got, err := pool.AcquireFromTier("api", nil)
		if err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
		seen[got.ID]++
	}
	if seen[keys[0].ID] > 0 {
		t.Errorf("quota_exceeded key %s was returned by Acquire", keys[0].ID)
	}

	// Restore 后再跑 9 次,k1 应出现至少一次
	pool.RestoreQuota(keys[0])
	for i := 0; i < 9; i++ {
		got, err := pool.AcquireFromTier("api", nil)
		if err != nil {
			t.Fatalf("Acquire after restore #%d: %v", i, err)
		}
		seen[got.ID]++
	}
	if seen[keys[0].ID] == 0 {
		t.Errorf("after RestoreQuota, key %s never returned by Acquire", keys[0].ID)
	}
}

// P68: Status.QuotaExceededKeys 计数正确
func TestPool_StatusCountsQuotaExceeded(t *testing.T) {
	keys := newTestKeys(3)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	pool.ReportError(keys[0], "quota_exceeded")
	pool.ReportError(keys[1], "quota_exceeded")
	pool.ReportError(keys[2], "auth") // DISABLED

	s := pool.Status()
	if s.QuotaExceededKeys != 2 {
		t.Errorf("QuotaExceededKeys = %d, want 2", s.QuotaExceededKeys)
	}
	if s.DisabledKeys != 1 {
		t.Errorf("DisabledKeys = %d, want 1", s.DisabledKeys)
	}
	if s.ActiveKeys != 0 {
		t.Errorf("ActiveKeys = %d, want 0", s.ActiveKeys)
	}
}

func TestKey_RemainingAndLastPolledAt_DefaultZero(t *testing.T) {
	k := &Key{ID: "k", ProviderName: "test"}
	if k.Remaining != 0 {
		t.Errorf("Remaining default = %v, want 0", k.Remaining)
	}
	if !k.LastPolledAt.IsZero() {
		t.Errorf("LastPolledAt default = %v, want zero", k.LastPolledAt)
	}
}

// P-quota-balance: token_plan tier 在 tier 过滤前按 Remaining DESC 稳定排序
func TestAcquireFromTier_TokenPlanSortsByRemainingDesc(t *testing.T) {
	// 3 个 token_plan key,Remaining 不同 — 应按 Remaining 降序返回第一个
	now := time.Now()
	mk := func(id string, remaining float64) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: KeyStatusActive, BillingSource: "token_plan",
			Remaining: remaining,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	keys := []*Key{mk("k-low", 0.3), mk("k-high", 12.5), mk("k-mid", 8.0)}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	k, err := pool.AcquireFromTier("token_plan", nil)
	if err != nil {
		t.Fatalf("AcquireFromTier: %v", err)
	}
	if k.ID != "k-high" {
		t.Errorf("got %q, want k-high (Remaining=12.5)", k.ID)
	}
}

func TestAcquireFromTier_TokenPlanStableWhenEqualRemaining(t *testing.T) {
	// 相同 Remaining 时,稳定排序保留输入顺序 — round-robin 计数器会随后接管
	now := time.Now()
	mk := func(id string) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: KeyStatusActive, BillingSource: "token_plan",
			Remaining: 1.0,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	keys := []*Key{mk("first"), mk("second"), mk("third")}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 三次取 key 应该是 first, second, third(稳定排序保留相对顺序)
	want := []string{"first", "second", "third"}
	for i, w := range want {
		k, err := pool.AcquireFromTier("token_plan", nil)
		if err != nil {
			t.Fatalf("AcquireFromTier #%d: %v", i, err)
		}
		if k.ID != w {
			t.Errorf("call %d: got %q, want %q", i, k.ID, w)
		}
	}
}

func TestAcquireFromTier_ApiTierNotSorted(t *testing.T) {
	// api tier 走原调度顺序,不应被 Remaining 排序影响
	// 验证:即使 Remaining 倒着排,AcquireFromTier("api") 仍按 RoundRobin 原序
	now := time.Now()
	mk := func(id string, remaining float64) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: KeyStatusActive, BillingSource: "api",
			Remaining: remaining,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	// 按 Remaining 倒序构造,期待 RoundRobin 仍按输入顺序轮询
	keys := []*Key{mk("a", 100), mk("b", 50), mk("c", 1)}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	for _, want := range []string{"a", "b", "c", "a"} {
		k, err := pool.AcquireFromTier("api", nil)
		if err != nil {
			t.Fatalf("AcquireFromTier(api): %v", err)
		}
		if k.ID != want {
			t.Errorf("got %q, want %q", k.ID, want)
		}
	}
}

func TestPool_StatusIncludesQuotaSummary(t *testing.T) {
	now := time.Now()
	past := now.Add(-2 * time.Minute)
	mk := func(id string, status KeyStatus, remaining float64, polled bool) *Key {
		k := &Key{
			ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: status, Remaining: remaining,
			CreatedAt: now, UpdatedAt: now,
		}
		if polled {
			k.LastPolledAt = past
		}
		return k
	}
	keys := []*Key{
		mk("a", KeyStatusActive, 10.0, true),
		mk("b", KeyStatusActive, 5.5, true),
		mk("c", KeyStatusActive, 0, false),   // 还没 poll 过
		mk("d", KeyStatusDisabled, 99, true), // 已 DISABLED,仍要算
	}
	pool := NewPool("test", keys, nil, Config{})

	st := pool.Status()
	if st.QuotaPolledKeys != 3 {
		t.Errorf("QuotaPolledKeys = %d, want 3", st.QuotaPolledKeys)
	}
	if st.QuotaKnownSum != 10.0+5.5+99 {
		t.Errorf("QuotaKnownSum = %v, want 114.5", st.QuotaKnownSum)
	}
}

func TestPool_Status_QuotaKindDominant(t *testing.T) {
	now := time.Now()
	past := now.Add(-2 * time.Minute)
	mk := func(id string, kind string) *Key {
		k := &Key{ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: KeyStatusActive, CreatedAt: now, UpdatedAt: now}
		k.LastPolledAt = past
		k.QuotaKind = kind
		return k
	}
	// 全部 percent → "percent"
	all := NewPool("t", []*Key{mk("a", "percent"), mk("b", "percent")}, nil, Config{})
	if got := all.Status().QuotaKind; got != "percent" {
		t.Errorf("all-percent QuotaKind = %q, want %q", got, "percent")
	}
	// 混合(percent + currency)→ "currency"
	mixed := NewPool("t", []*Key{mk("a", "percent"), mk("b", "currency")}, nil, Config{})
	if got := mixed.Status().QuotaKind; got != "currency" {
		t.Errorf("mixed QuotaKind = %q, want %q", got, "currency")
	}
	// 空 Kind polled key(GLM 场景)→ "currency"
	mixedEmpty := NewPool("t", []*Key{mk("a", "percent"), mk("b", "")}, nil, Config{})
	if got := mixedEmpty.Status().QuotaKind; got != "currency" {
		t.Errorf("empty-kind QuotaKind = %q, want %q", got, "currency")
	}
	// 未 poll → ""
	none := NewPool("t", []*Key{{ID: "a", ProviderName: "t", Name: "a", Key: "sk",
		Status: KeyStatusActive, CreatedAt: now, UpdatedAt: now}}, nil, Config{})
	if got := none.Status().QuotaKind; got != "" {
		t.Errorf("no-poll QuotaKind = %q, want empty", got)
	}
}
