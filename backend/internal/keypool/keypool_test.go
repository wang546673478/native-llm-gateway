package keypool

import (
	"errors"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/circuit"
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

func TestPool_AcquireWhenAllUnusable(t *testing.T) {
	keys := newTestKeys(2)
	keys[0].Status = KeyStatusQuotaExceeded
	keys[1].Status = KeyStatusQuotaExceeded
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

	// P-no-disabled: 反复 429 只会反复冷却,不会永久禁用
	pool.ReportRateLimit(k, 0)
	pool.ReportRateLimit(k, 0)
	if k.Status != KeyStatusCooling {
		t.Errorf("after repeated 429: status = %q, want COOLING (no DISABLED state)", k.Status)
	}
	if k.CoolingCount != 4 {
		t.Errorf("cooling_count = %d, want 4", k.CoolingCount)
	}
}

func TestPool_ReportErrorAuthCooling(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	pool.ReportError(keys[0], "auth")
	// P-no-disabled: auth 失败冷却 5 分钟(换 key 后自动恢复),不永久禁用
	if keys[0].Status != KeyStatusCooling {
		t.Errorf("after auth error: status = %q, want COOLING", keys[0].Status)
	}
	if !keys[0].CoolingUntil.After(time.Now().Add(4 * time.Minute)) {
		t.Error("auth cooling should be ~5 minutes")
	}
}

// TestPool_ReportErrorInvalidRequestKeepsKeyActive P-invalid-req:
// 上游 400(invalid_request)通常是请求内容问题(agent 回带其他厂商的
// thinking 块等),不是 key 有问题 — 只计数,不禁用。
// 禁用会把整条链打死且无恢复路径,一次坏请求不应让 provider 永久下线
func TestPool_ReportErrorInvalidRequestKeepsKeyActive(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	pool.ReportError(keys[0], "invalid_request")
	if keys[0].Status != KeyStatusActive {
		t.Errorf("after invalid_request: status = %q, want ACTIVE(不禁用)", keys[0].Status)
	}
	if keys[0].ErrorCount != 1 {
		t.Errorf("error count = %d, want 1(仍计数)", keys[0].ErrorCount)
	}
	// key 仍可取
	if _, err := pool.Acquire(); err != nil {
		t.Errorf("Acquire after invalid_request: %v, want 可用", err)
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
	keys[3].Status = KeyStatusQuotaExceeded

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
	if s.QuotaExceededKeys != 1 {
		t.Errorf("QuotaExceededKeys = %d, want 1", s.QuotaExceededKeys)
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

	k, err := pool.AcquireFromTier("api", nil, "")
	if err != nil {
		t.Fatalf("AcquireFromTier(api): %v", err)
	}
	if k.BillingSource != "api" {
		t.Errorf("got tier %s, want api", k.BillingSource)
	}

	k, err = pool.AcquireFromTier("token_plan", nil, "")
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

	_, err := pool.AcquireFromTier("api", nil, "")
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
		Status: KeyStatusQuotaExceeded, BillingSource: "token_plan",
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

func TestAcquireForProtocol(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "1", Name: "openai-only", Key: "k1", Status: KeyStatusActive, BillingSource: "api", Protocols: "openai", CreatedAt: now, UpdatedAt: now},
		{ID: "2", Name: "all", Key: "k2", Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "3", Name: "anthropic-only", Key: "k3", Status: KeyStatusActive, BillingSource: "api", Protocols: "anthropic", CreatedAt: now, UpdatedAt: now},
	}
	pool := NewPool("p", keys, nil, Config{})

	// anthropic 请求:只能拿到 "all"(Protocols="" 匹配任何)
	k, err := pool.AcquireForProtocol("anthropic")
	if err != nil {
		t.Fatalf("acquire anthropic: %v", err)
	}
	if k.Name != "all" {
		t.Fatalf("anthropic request got key %q, want all", k.Name)
	}

	// openai 请求:可从 openai-only 或 all 中取
	k2, err := pool.AcquireForProtocol("openai")
	if err != nil {
		t.Fatalf("acquire openai: %v", err)
	}
	if k2.Name != "openai-only" && k2.Name != "all" {
		t.Fatalf("openai request got key %q, want openai-only or all", k2.Name)
	}

	// 空 proto = 不过滤,三个都能取
	k3, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_ = k3
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
	k, err := pool.AcquireFromTier("token_plan", set, "")
	if err != nil {
		t.Fatalf("AcquireFromTier: %v", err)
	}
	if k.ID != "100" {
		t.Errorf("got ID %s, want 100", k.ID)
	}

	// ID=300 是 api,白名单只有 100 → api 桶空
	_, err = pool.AcquireFromTier("api", set, "")
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
	got, err := pool.AcquireFromTier("api", nil, "")
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
		got, err := pool.AcquireFromTier("api", nil, "")
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
		got, err := pool.AcquireFromTier("api", nil, "")
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
	pool.ReportError(keys[2], "auth") // P-no-disabled: auth → 冷却,不是禁用

	s := pool.Status()
	if s.QuotaExceededKeys != 2 {
		t.Errorf("QuotaExceededKeys = %d, want 2", s.QuotaExceededKeys)
	}
	if s.CoolingKeys != 1 {
		t.Errorf("CoolingKeys = %d, want 1 (auth → cooling, no DISABLED)", s.CoolingKeys)
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

	k, err := pool.AcquireFromTier("token_plan", nil, "")
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
		k, err := pool.AcquireFromTier("token_plan", nil, "")
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
		k, err := pool.AcquireFromTier("api", nil, "")
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
		mk("c", KeyStatusActive, 0, false),        // 还没 poll 过
		mk("d", KeyStatusQuotaExceeded, 99, true), // QE 但仍 poll 过,要算
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

// TestPool_ReportErrorQuotaProbe_KeepsKeyActive — B-probe-quota: 无 balancer 的
// api 厂商(glm/qwen/gemini,probe 模式)配额耗尽只计数不标记 — 没有轮询恢复通道,
// 标 QUOTA_EXCEEDED 就是永久死 key;每次请求重新探测,充值后自动恢复
func TestPool_ReportErrorQuotaProbe_KeepsKeyActive(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{
		QuotaRecovery: QuotaRecoveryProbe,
	})

	pool.ReportError(keys[0], "quota_exceeded")

	if keys[0].Status != KeyStatusActive {
		t.Errorf("status = %q, want ACTIVE (probe mode must not persist quota mark)", keys[0].Status)
	}
	if keys[0].ErrorCount != 1 {
		t.Errorf("error_count = %d, want 1 (stats still recorded)", keys[0].ErrorCount)
	}
	// key 仍可用 → 下一次请求会重新探测上游
	if _, err := pool.Acquire(); err != nil {
		t.Errorf("Acquire after probe quota error: %v, want usable", err)
	}
}

// TestPool_ReportErrorQuotaPoll_MarksQuotaExceeded — 默认 poll 模式(有 balancer,
// deepseek/minimax)保持现有行为:标 QUOTA_EXCEEDED,等 quotacheck 轮询恢复
func TestPool_ReportErrorQuotaPoll_MarksQuotaExceeded(t *testing.T) {
	keys := newTestKeys(1)
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{}) // 空 = 默认 poll

	pool.ReportError(keys[0], "quota_exceeded")

	if keys[0].Status != KeyStatusQuotaExceeded {
		t.Errorf("status = %q, want QUOTA_EXCEEDED", keys[0].Status)
	}
	if _, err := pool.Acquire(); err == nil {
		t.Error("Acquire after quota mark: want error (key unusable until poll restores)")
	}
}

// Task3: AcquireFromTierExcluding — 换 key 重试时排除刚失败的那把 key
// (Task 5 换 key 重试的底层原语;排除逻辑不依赖轮询位置)
func TestPool_AcquireFromTierExcluding(t *testing.T) {
	now := time.Now()
	mkKey := func(id, tier string) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: "k" + id, Key: "sk",
			Status: KeyStatusActive, BillingSource: tier,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	// pool: 2 个 key — "1"(billing_source=token_plan)、"2"(billing_source=token_plan)
	keys := []*Key{mkKey("1", "token_plan"), mkKey("2", "token_plan")}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 排除 "1" → 必须返回 key "2",不能返回 "1"
	k, err := pool.AcquireFromTierExcluding("token_plan", "1", "")
	if err != nil {
		t.Fatalf("AcquireFromTierExcluding: %v", err)
	}
	if k.ID != "2" {
		t.Fatalf("got key %q, want %q (excluded key 1 must not be returned)", k.ID, "2")
	}

	// 连续第二次调用仍返回 "2"(排除逻辑不依赖轮询位置)
	k, err = pool.AcquireFromTierExcluding("token_plan", "1", "")
	if err != nil {
		t.Fatalf("AcquireFromTierExcluding #2: %v", err)
	}
	if k.ID != "2" {
		t.Fatalf("second call got key %q, want %q (exclusion must not depend on round-robin position)", k.ID, "2")
	}
}

// P-quota-prefer: round-robin 不再把请求分给「已轮询且余额耗尽」的 key —
// healthy key 应始终被选(MiniMax weige 1% 场景,2026-08-06 实测)
func TestAcquireFromTier_SkipsPolledExhaustedKey(t *testing.T) {
	now := time.Now()
	mk := func(id string, remaining float64, kind string) *Key {
		return &Key{
			ID: id, ProviderName: "test", Name: id, Key: "sk",
			Status: KeyStatusActive, BillingSource: "token_plan",
			Remaining: remaining, QuotaKind: kind,
			LastPolledAt: now.Add(-30 * time.Second),
			CreatedAt:    now, UpdatedAt: now,
		}
	}
	keys := []*Key{mk("healthy", 99, "percent"), mk("dead", 1, "percent")}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 连续 4 次获取,都应命中 healthy — 不再轮流分给 dead(1% → MiniMax 直接拒)
	for i := 0; i < 4; i++ {
		k, err := pool.AcquireFromTier("token_plan", nil, "")
		if err != nil {
			t.Fatalf("AcquireFromTier #%d: %v", i, err)
		}
		if k.ID != "healthy" {
			t.Errorf("call %d: got %q, want healthy (dead key must be skipped)", i, k.ID)
		}
	}
}

// P-quota-prefer: 未轮询过的 key(启动窗口,Remaining=0 是默认值)不跳过
func TestAcquireFromTier_NeverPolledNotSkipped(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "a", ProviderName: "test", Name: "a", Key: "sk",
			Status: KeyStatusActive, BillingSource: "token_plan",
			Remaining: 0, CreatedAt: now, UpdatedAt: now}, // 从未 poll
		{ID: "b", ProviderName: "test", Name: "b", Key: "sk",
			Status: KeyStatusActive, BillingSource: "token_plan",
			Remaining: 0, CreatedAt: now, UpdatedAt: now},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	for i := 0; i < 3; i++ {
		if _, err := pool.AcquireFromTier("token_plan", nil, ""); err != nil {
			t.Fatalf("AcquireFromTier #%d: %v (never-polled keys must remain usable)", i, err)
		}
	}
}

// P-quota-prefer: currency 单位余额 > 0 不跳过(deepseek ¥ 余量仍可用)
func TestAcquireFromTier_CurrencyLowBalanceNotSkipped(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "a", ProviderName: "test", Name: "a", Key: "sk",
			Status: KeyStatusActive, BillingSource: "api",
			Remaining: 0.5, QuotaKind: "currency", LastPolledAt: now.Add(-30 * time.Second),
			CreatedAt: now, UpdatedAt: now},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})
	k, err := pool.AcquireFromTier("api", nil, "")
	if err != nil {
		t.Fatalf("AcquireFromTier: %v (currency 0.5 must remain usable)", err)
	}
	if k.ID != "a" {
		t.Errorf("got %q, want a", k.ID)
	}
}

// P-per-key-circuit: 一把 key 5xx 熔断,同 provider 的 healthy key 照常可用
// (2026-08-06 之前是 provider 级熔断 — 一把 key 出问题连坐整 provider)
func TestAcquireFromTier_PerKeyCircuitTripsOnlyBadKey(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "k1", ProviderName: "test", Name: "k1", Key: "sk",
			Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "k2", ProviderName: "test", Name: "k2", Key: "sk",
			Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
	}
	cfg := Config{
		CircuitBreaker: circuit.Config{
			FailureThreshold: 2,
			FailureWindow:    60 * time.Second,
			OpenTimeout:      30 * time.Second,
			HalfOpenRequests: 1,
		},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), cfg)

	// k1 连续 2 个 5xx → k1 熔断
	pool.ReportError(keys[0], "server_error")
	pool.ReportError(keys[0], "server_error")

	// Acquire 必须绕开 k1,给 k2
	k, err := pool.AcquireFromTier("api", nil, "")
	if err != nil {
		t.Fatalf("AcquireFromTier: %v", err)
	}
	if k.ID != "k2" {
		t.Errorf("got %q, want k2 (k1 tripped must not affect healthy k2)", k.ID)
	}

	// k2 也 2 个 5xx → k2 熔断 → 桶空
	pool.ReportError(keys[1], "server_error")
	pool.ReportError(keys[1], "server_error")
	if _, err := pool.AcquireFromTier("api", nil, ""); err != ErrNoAvailableKey {
		t.Errorf("got %v, want ErrNoAvailableKey (both keys tripped)", err)
	}
}

// P-per-key-circuit: OPEN 超时后转 HALF_OPEN 放行试探请求,成功 → CLOSED
func TestAcquireFromTier_PerKeyCircuitHalfOpenRecovers(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "k1", ProviderName: "test", Name: "k1", Key: "sk",
			Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
	}
	cfg := Config{
		CircuitBreaker: circuit.Config{
			FailureThreshold: 2,
			FailureWindow:    60 * time.Second,
			OpenTimeout:      50 * time.Millisecond,
			HalfOpenRequests: 1,
		},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), cfg)

	pool.ReportError(keys[0], "server_error")
	pool.ReportError(keys[0], "server_error")
	if _, err := pool.AcquireFromTier("api", nil, ""); err != ErrNoAvailableKey {
		t.Fatalf("expected tripped, got %v", err)
	}

	// OPEN 超时前仍不可用
	if _, err := pool.AcquireFromTier("api", nil, ""); err != ErrNoAvailableKey {
		t.Fatalf("expected still open, got %v", err)
	}

	// 超时后 → HALF_OPEN 放行试探请求
	time.Sleep(80 * time.Millisecond)
	k, err := pool.AcquireFromTier("api", nil, "")
	if err != nil {
		t.Fatalf("AcquireFromTier after timeout: %v (want half-open probe)", err)
	}
	if k.ID != "k1" {
		t.Fatalf("got %q, want k1", k.ID)
	}

	// 试探成功 → CLOSED,后续正常调度
	pool.ReportSuccess(keys[0])
	k, err = pool.AcquireFromTier("api", nil, "")
	if err != nil {
		t.Fatalf("AcquireFromTier after success: %v", err)
	}
	if k.ID != "k1" {
		t.Errorf("got %q, want k1", k.ID)
	}
}

// P-per-key-circuit: 429(rate_limit)不计入熔断 — 5 次限流不熔断 key
func TestAcquireFromTier_PerKeyCircuitRateLimitNotCounted(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "k1", ProviderName: "test", Name: "k1", Key: "sk",
			Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
	}
	cfg := Config{
		CircuitBreaker: circuit.Config{
			FailureThreshold: 3,
			FailureWindow:    60 * time.Second,
			OpenTimeout:      30 * time.Second,
			HalfOpenRequests: 1,
		},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), cfg)

	for i := 0; i < 5; i++ {
		pool.ReportError(keys[0], "rate_limit")
	}
	if _, err := pool.AcquireFromTier("api", nil, ""); err != nil {
		t.Fatalf("AcquireFromTier after 5 rate_limits: %v (429 must not trip circuit)", err)
	}
}

// P-state-persist: 快照导出/恢复 — QE/未过期 COOLING/余额恢复,过期 COOLING 不恢复
func TestPool_SnapshotApplyRestoresState(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "k1", ProviderName: "test", Name: "k1", Key: "sk",
			Status: KeyStatusQuotaExceeded, BillingSource: "api",
			QuotaExceededSince: now.Add(-10 * time.Minute),
			Remaining:          0, QuotaKind: "percent", LastPolledAt: now.Add(-30 * time.Second),
			CreatedAt: now, UpdatedAt: now},
		{ID: "k2", ProviderName: "test", Name: "k2", Key: "sk",
			Status: KeyStatusCooling, BillingSource: "api",
			CoolingUntil: now.Add(5 * time.Minute), // 未过期 → 恢复
			CreatedAt:    now, UpdatedAt: now},
		{ID: "k3", ProviderName: "test", Name: "k3", Key: "sk",
			Status: KeyStatusCooling, BillingSource: "api",
			CoolingUntil: now.Add(-5 * time.Minute), // 已过期 → 不恢复(ACTIVE)
			CreatedAt:    now, UpdatedAt: now},
	}
	pool := NewPool("test", keys, NewScheduler("round_robin"), Config{})

	// 导出 → 新 pool 恢复(模拟 reload)
	states := pool.Snapshot()
	pool2 := NewPool("test", []*Key{
		{ID: "k1", ProviderName: "test", Name: "k1", Key: "sk", Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "k2", ProviderName: "test", Name: "k2", Key: "sk", Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "k3", ProviderName: "test", Name: "k3", Key: "sk", Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
	}, NewScheduler("round_robin"), Config{})
	pool2.ApplySnapshot(states)

	ks := pool2.Keys()
	byID := map[string]Key{}
	for _, k := range ks {
		byID[k.ID] = k
	}
	if byID["k1"].Status != KeyStatusQuotaExceeded {
		t.Errorf("k1 status = %q, want QUOTA_EXCEEDED (restored)", byID["k1"].Status)
	}
	if byID["k1"].Remaining != 0 || byID["k1"].QuotaKind != "percent" || byID["k1"].LastPolledAt.IsZero() {
		t.Errorf("k1 balance snapshot not restored: %+v", byID["k1"])
	}
	if byID["k2"].Status != KeyStatusCooling {
		t.Errorf("k2 status = %q, want COOLING (unexpired cooling restored)", byID["k2"].Status)
	}
	if byID["k3"].Status != KeyStatusActive {
		t.Errorf("k3 status = %q, want ACTIVE (expired cooling must not restore)", byID["k3"].Status)
	}
}
