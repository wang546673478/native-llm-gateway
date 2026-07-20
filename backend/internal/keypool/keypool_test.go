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
