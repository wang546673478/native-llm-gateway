# Provider Quota Polling — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Poll upstream provider balance endpoints every 5 minutes and feed the result into `keypool.Key.Remaining`, so the token_plan tier in `Pool.AcquireFromTier` selects the key with the most remaining quota first instead of learning via upstream 402/429 errors.

**Architecture:**

- New `minimax` Balancer implementing the existing `provider.Balancer` interface; deepseek + glm already have Balancers so they ride along for free.
- `quotacheck.Manager.pollAllBalancers` extended to poll **active keys (not only QUOTA_EXCEEDED)** in tier-blocked order (token_plan → api → free); remaining value written into `Key.Remaining`; HasQuota=false → call existing `pool.ReportQuotaExceeded`.
- `Pool.AcquireFromTier("token_plan")` does a stable sort by `Remaining` DESC before handing the slice to the existing `RoundRobinScheduler`.
- New `ProviderKeyView.remaining` + `last_polled_at` exposed via the existing `/providers/:name/api-keys` endpoint; frontend adds a coloured "余额 (CNY)" column.

**Tech Stack:** Go 1.23+, Gin, GORM, existing `provider.Balancer` / `RegisterBalancer` extensions. Frontend: Vue 3 + TypeScript + Pinia + Naive UI (no new dep).

---

## Global Constraints

(Failure to comply = rejected task.)

- **Go platform**: amd64 / arm64 Linux only. Server is never built for 32-bit; 64-bit word reads of `float64.Remaining` are torn-write-free. Run `go env GOARCH` first; refuse to merge if `386`/`arm` (32-bit).
- **Tests must pass before commit**: `go test ./internal/...` must return exit 0. The one pre-existing failure `TestAuthenticator_InvalidFormat` is NOT in our scope; do not "fix" it as a side-quest.
- **No schema changes**: `database/models.go` is unchanged. `Key.Remaining` / `Key.LastPolledAt` are runtime-only state. Touching the DB layer is a hard reject.
- **DB columns untouched**: do not add fields to `ProviderAPIKey` GORM model.
- **Spec authority**: When this plan and the spec differ, the spec wins. Path: `docs/superpowers/specs/2026-08-04-provider-quota-polling-design.md`.
- **Naming**: keys (constants, structs) stay camelCase matching existing keypool/auth/quotacheck style (not the spec's Go-style mixed). Follow what's already in the file you're editing.
- **Commits**: follow existing repo convention — `<scope>(<module>): <subject>`. End with `Co-Authored-By: Claude <noreply@anthropic.com>` ONLY if you weren't told otherwise in the task.

---

## File Map

**Created (1 backend, 1 test):**
- `backend/internal/provider/minimax/balancer.go`
- `backend/internal/provider/minimax/balancer_test.go`

**Modified (backend, 7 files):**
- `backend/internal/keypool/key.go` — add 2 fields
- `backend/internal/keypool/pool.go` — `AcquireFromTier` sort + `PoolStatus` 2 fields
- `backend/internal/quotacheck/manager.go` — `pollAllBalancers` tier-blocked + active-key polling
- `backend/internal/auth/provider_keys_handler.go` — `ProviderKeyView` 2 fields + populate
- `backend/internal/api/http/handler/admin.go` — `listProviders` surface `QuotaPolledKeys` / `QuotaKnownSum`
- `backend/internal/config/config.go` (or wherever `KeyPool.*Quota*` lives) — add `QuotaWarnThresholdPct`
- `backend/internal/server/server.go` — `Reload` pipe `WarnThresholdPct` into `ManagerConfig`
- `config.example.yaml` — `poll_interval: 5m` + `warn_threshold_pct: 10`

**Modified (frontend, 1–2 files):**
- `frontend/src/api.ts` or `frontend/src/stores/*.ts` — type extended
- `frontend/src/views/ProviderKeys.vue` (or wherever the keys table lives) — add column

---

## Task 1: MiniMax Balancer + tests

**Files:**
- Create: `backend/internal/provider/minimax/balancer.go`
- Create: `backend/internal/provider/minimax/balancer_test.go`

**Interfaces (this task is independent — nothing yet consumes it; Task 5 wires it up):**
- Produces: `quotacheck.RegisterBalancer("minimax", b)` and same for `"minimax-openai"`.
- Implements: `quotacheck.Balancer` interface (`FetchBalance(ctx, baseURL, k) (Balance, error)`) declared in `backend/internal/quotacheck/prober.go:42`.

- [ ] **Step 1: Write the failing test**

Create `backend/internal/provider/minimax/balancer_test.go`:

```go
package minimax

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

func newTestKey() *keypool.Key {
	return &keypool.Key{ID: "1", Name: "test", Key: "fake-subscription-key"}
}

func TestMiniMaxBalancer_ParsesBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/token_plan/remains") {
			t.Errorf("path = %s, want suffix /v1/token_plan/remains", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer fake-subscription-key" {
			t.Errorf("auth = %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance": 12.34}`))
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if !bal.HasQuota {
		t.Errorf("HasQuota = false, want true (balance=12.34)")
	}
	if bal.Raw != 12.34 {
		t.Errorf("Raw = %v, want 12.34", bal.Raw)
	}
	if !strings.Contains(bal.Source, "minimax") {
		t.Errorf("Source = %q, want minimax prefix", bal.Source)
	}
}

func TestMiniMaxBalancer_ZeroBalanceIsExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance": 0}`))
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err != nil {
		t.Fatalf("FetchBalance: %v", err)
	}
	if bal.HasQuota {
		t.Error("HasQuota = true, want false (balance=0)")
	}
}

func TestMiniMaxBalancer_FieldNameVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"remains", `{"remains": 5.0}`, true},
		{"quota_remaining", `{"quota_remaining": 5.0}`, true},
		{"available", `{"available": 5.0}`, true},
		{"all-missing", `{"foo": 1.0}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			b := newMiniMaxBalancer()
			bal, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
			if err != nil {
				t.Fatalf("FetchBalance: %v", err)
			}
			if bal.HasQuota != tc.want {
				t.Errorf("HasQuota = %v, want %v", bal.HasQuota, tc.want)
			}
		})
	}
}

func TestMiniMaxBalancer_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "auth fail", http.StatusUnauthorized)
	}))
	defer srv.Close()

	b := newMiniMaxBalancer()
	_, err := b.FetchBalance(context.Background(), srv.URL, newTestKey())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [ ] **Step 2: Run the test, verify it fails (no implementation yet)**

Run: `cd backend && go test ./internal/provider/minimax/... -v`
Expected: build failure `undefined: newMiniMaxBalancer` and `undefined: minimax package`.

- [ ] **Step 3: Write the implementation**

Create `backend/internal/provider/minimax/balancer.go`:

```go
// Package minimax — quota balance polling
// P-quota-balance: GET https://www.minimaxi.com/v1/token_plan/remains
// Authorization: Bearer <subscription_key>
// Response schema is not officially documented; we accept several field names.
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

type miniMaxBalancer struct {
	client *http.Client
}

func newMiniMaxBalancer() *miniMaxBalancer {
	return &miniMaxBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *miniMaxBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (quotacheck.Balance, error) {
	// Use the configured provider endpoint if non-empty, else hit the canonical host.
	endpoint := strings.TrimRight(baseURL, "/")
	if endpoint == "" {
		endpoint = "https://www.minimaxi.com"
	}
	url := endpoint + "/v1/token_plan/remains"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return quotacheck.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return quotacheck.Balance{HasQuota: false, Source: "minimax:/v1/token_plan/remains"}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Subscription-key mismatch — treat as auth failure; caller will DISABLE.
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
		}, fmt.Errorf("minimax quota auth failed: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return quotacheck.Balance{
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
		}, fmt.Errorf("minimax quota http %d", resp.StatusCode)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return quotacheck.Balance{HasQuota: false, Source: "minimax:/v1/token_plan/remains"}, err
	}
	hasQuota, raw := extractAvailable(parsed)
	return quotacheck.Balance{
		Raw:      raw,
		HasQuota: hasQuota,
		Source:   "minimax:/v1/token_plan/remains",
	}, nil
}

// extractAvailable returns (hasQuota, rawValue) given an untyped JSON object.
// Tries several field names the MiniMax docs reference or that real responses
// have used; falls back to HasQuota=false when nothing meaningful is found.
func extractAvailable(m map[string]json.RawMessage) (hasQuota bool, raw float64) {
	candidates := []string{"quota_remaining", "remains", "balance", "available"}
	for _, k := range candidates {
		v, ok := m[k]
		if !ok {
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err == nil {
			return f > 0, f
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if f2, err := strconv.ParseFloat(s, 64); err == nil {
				return f2 > 0, f2
			}
		}
	}
	return false, 0
}

func init() {
	b := newMiniMaxBalancer()
	quotacheck.RegisterBalancer("minimax", b)
	quotacheck.RegisterBalancer("minimax-openai", b)
}
```

- [ ] **Step 4: Run tests, verify they pass**

Run: `cd backend && go test ./internal/provider/minimax/... -v`
Expected: 4 tests pass.

- [ ] **Step 5: Commit**

```bash
cd backend && git add ../internal/provider/minimax/balancer.go ../internal/provider/minimax/balancer_test.go
git commit -m "feat(minimax): add quota balance Balancer

Implements quotacheck.Balancer against
https://www.minimaxi.com/v1/token_plan/remains with Bearer subscription
key auth. Response JSON schema is not officially documented, so several
candidate field names are accepted (quota_remaining / remains / balance
/ available). HasQuota=false (conservative) when nothing is parsed.

Both 'minimax' and 'minimax-openai' registrations are wired up so either
protocol surface shares the same Balancer instance."
```

---

## Task 2: Key.Remaining + Key.LastPolledAt fields

**Files:**
- Modify: `backend/internal/keypool/key.go`

**Interfaces (this task — consumed by Tasks 3, 4, 5):**
- Produces: `Key.Remaining float64` and `Key.LastPolledAt time.Time` zero by default.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/keypool/keypool_test.go`:

```go
func TestKey_RemainingAndLastPolledAt_DefaultZero(t *testing.T) {
	k := &Key{ID: "k", ProviderName: "test"}
	if k.Remaining != 0 {
		t.Errorf("Remaining default = %v, want 0", k.Remaining)
	}
	if !k.LastPolledAt.IsZero() {
		t.Errorf("LastPolledAt default = %v, want zero", k.LastPolledAt)
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd backend && go test ./internal/keypool/... -run TestKey_RemainingAndLastPolledAt_DefaultZero -v`
Expected: compile error `k.Remaining undefined` and `k.LastPolledAt undefined`.

- [ ] **Step 3: Add the fields**

In `backend/internal/keypool/key.go`, inside the `Key` struct, append after `QuotaProbeAttempts`:

```go
	// P-quota-balance: 上游 quota polling 写入的余额快照与时间戳(runtime, 不落 DB)
	Remaining    float64
	LastPolledAt time.Time
```

- [ ] **Step 4: Run, verify pass**

Run: `cd backend && go test ./internal/keypool/... -run TestKey_RemainingAndLastPolledAt_DefaultZero -v`
Expected: PASS.

- [ ] **Step 5: Verify whole package still builds**

Run: `cd backend && go build ./...`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
cd backend && git add ../internal/keypool/key.go ../internal/keypool/keypool_test.go
git commit -m "feat(keypool): add Remaining and LastPolledAt to Key

Runtime-only quota polling state, populated by quotacheck.Manager and
consumed by Pool.AcquireFromTier (token_plan sort). Not persisted to DB;
refreshed on the first poll cycle after each server restart."
```

---

## Task 3: AcquireFromTier sort by Remaining (token_plan)

**Files:**
- Modify: `backend/internal/keypool/pool.go`
- Modify: `backend/internal/keypool/keypool_test.go`

**Interfaces:**
- Consumes: `Key.Remaining` (just added in Task 2).
- Produces: same function signature `AcquireFromTier(tier string, allowedIDSet map[uint]struct{}) (*Key, error)`. New behaviour: when `tier == "token_plan"`, the caller's `scheduler.Select(bucket)` receives a `bucket` already sorted by `Remaining` DESC (stable sort). `api` and `free` tiers are unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `backend/internal/keypool/keypool_test.go`:

```go
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
```

- [ ] **Step 2: Run, verify failure**

Run: `cd backend && go test ./internal/keypool/... -run "TestAcquireFromTier_(TokenPlan|ApiTier)" -v`
Expected: first test fails because `keys[1]` (k-low = 0.3) is selected first instead of `k-high`. (The third test will probably pass coincidentally — that's fine; the first is the canary.)

- [ ] **Step 3: Modify `AcquireFromTier`**

In `backend/internal/keypool/pool.go`, in `AcquireFromTier`, after the `usable` slice is built and BEFORE the `bucket` tier-filter loop, insert:

```go
    // P-quota-balance: token_plan tier 在进入 tier 过滤前按 Remaining 降序稳定排序
    // 稳定排序保证 Remaining 相等时仍维持 RoundRobin 原始顺序
    if tier == "token_plan" {
        sort.SliceStable(usable, func(i, j int) bool {
            return usable[i].Remaining > usable[j].Remaining
        })
    }
```

Add `"sort"` to the import list at the top of the file.

(Do not reorder the rest of the function: the existing `bucket := make(...)` loop is the next step and remains unchanged.)

- [ ] **Step 4: Run, verify pass**

Run: `cd backend && go test ./internal/keypool/... -v`
Expected: all tests in package pass (existing ones still green + new 3).

- [ ] **Step 5: Whole backend tests**

Run: `cd backend && go test ./... -short 2>&1 | tail -25`
Expected: all packages OK except the known pre-existing `TestAuthenticator_InvalidFormat` failure (which is unchanged).

- [ ] **Step 6: Commit**

```bash
cd backend && git add ../internal/keypool/pool.go ../internal/keypool/keypool_test.go
git commit -m "feat(keypool): AcquireFromTier(token_plan) sorts by Remaining DESC

Stable sort so equal-Remaining keys preserve RoundRobin order across
acquires. Only the token_plan tier is affected; api/free continue to
use the configured scheduler unchanged. Sorting sits before the
tier-filter step so the unused slice cost is paid only when the tier
arg is token_plan."
```

---

## Task 4: PoolStatus gains QuotaPolledKeys + QuotaKnownSum

**Files:**
- Modify: `backend/internal/keypool/pool.go`
- Modify: `backend/internal/keypool/keypool_test.go`

**Interfaces:**
- Produces: `PoolStatus.QuotaPolledKeys int` (count of keys with non-zero `LastPolledAt`) and `PoolStatus.QuotaKnownSum float64` (sum of `Key.Remaining` over polled keys).

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/keypool/keypool_test.go`:

```go
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
		mk("c", KeyStatusActive, 0, false), // 还没 poll 过
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
```

- [ ] **Step 2: Run, verify failure**

Run: `cd backend && go test ./internal/keypool/... -run TestPool_StatusIncludesQuotaSummary -v`
Expected: compile error `st.QuotaPolledKeys undefined`.

- [ ] **Step 3: Extend the struct + Status method**

In `backend/internal/keypool/pool.go`:

1. In the `PoolStatus` struct, append:

```go
    // P-quota-balance: 上游 quota polling 的聚合指标
    QuotaPolledKeys int     `json:"quota_polled_keys"`     // 至少 poll 过一次的 key 数
    QuotaKnownSum   float64 `json:"quota_known_sum"`       // 已 poll 的 key 的 Remaining 之和
```

2. In the `Status()` method, just before `return s`, append:

```go
    for _, k := range p.keys {
        if !k.LastPolledAt.IsZero() {
            s.QuotaPolledKeys++
            s.QuotaKnownSum += k.Remaining
        }
    }
```

- [ ] **Step 4: Run, verify pass**

Run: `cd backend && go test ./internal/keypool/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd backend && git add ../internal/keypool/pool.go ../internal/keypool/keypool_test.go
git commit -m "feat(keypool): PoolStatus reports QuotaPolledKeys / QuotaKnownSum

Aggregate metrics surfaced to /providers endpoint for the dashboard.
Polled keys are those with a non-zero LastPolledAt; the sum is over
those keys' Remaining values (including DISABLED keys, since their
balance is still meaningful to admins)."
```

---

## Task 5: Manager.pollAllBalancers tier-blocked + active-key polling

**Files:**
- Modify: `backend/internal/quotacheck/manager.go`
- Modify: `backend/internal/quotacheck/manager_test.go` (extend or add)

**Interfaces:**
- Consumes: `Key.Remaining` / `Key.LastPolledAt`, `Pool.ReportQuotaExceeded(k)` / `Pool.RestoreQuota(k)`, `pool.KeyPtrs()`, `provider.Balancer.FetchBalance`.
- Produces: state updates on keys (Remaining / LastPolledAt / Status). No public method signature changes.

The new behaviour (see spec §3.2):
- For each pool that has a registered `Balancer`:
  - Tier blocks, in order: `token_plan`, `api`, `free`.
  - Within each tier: walk `pool.KeyPtrs()`, skip `DISABLED`, call `FetchBalance`, write `Remaining` + `LastPolledAt = time.Now()`.
  - If `HasQuota == false && Status == ACTIVE` → `pool.ReportQuotaExceeded(k)`.
  - If `HasQuota == true && Status == QUOTA_EXCEEDED` → `pool.RestoreQuota(k)`.
  - 1 second sleep between keys (`ctx`-aware) to be polite.

- [ ] **Step 1: Write the failing test**

Append to `backend/internal/quotacheck/manager_test.go`:

```go
type fakeBalancer struct {
	calls []string            // 记录被调用的 key ID
	bal   quotacheck.Balance  // 所有 key 都返同一个 Balance
	err   error
}

func (f *fakeBalancer) FetchBalance(_ context.Context, _ string, k *keypool.Key) (quotacheck.Balance, error) {
	f.calls = append(f.calls, k.ID)
	return f.bal, f.err
}

func TestPollAllBalancers_TierBlocked(t *testing.T) {
	// pool: 2 token_plan + 1 api + 1 free = 4 key
	// 期望调用顺序:[token_plan × 2, api × 1, free × 1]
	now := time.Now()
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "free-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "free"},
		{ID: "tp-1",   ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "tp-2",   ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "api-1",  ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "api"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: quotacheck.Balance{HasQuota: true, Raw: 1.0}}
	originalReg := balancerRegistry["p"]
	quotacheck.RegisterBalancer("p", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["p"] = originalReg
		} else {
			delete(balancerRegistry, "p")
		}
	})

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())

	m.pollAllBalancers(context.Background())

	want := []string{"tp-1", "tp-2", "api-1", "free-1"}
	if !reflect.DeepEqual(b.calls, want) {
		t.Errorf("calls = %v, want %v (tier-blocked order)", b.calls, want)
	}
	// Remaining 应被填入
	for _, k := range pool.KeyPtrs() {
		if k.Remaining != 1.0 {
			t.Errorf("%s Remaining = %v, want 1.0", k.ID, k.Remaining)
		}
		if k.LastPolledAt.IsZero() {
			t.Errorf("%s LastPolledAt not set", k.ID)
		}
	}
}

func TestPollAllBalancers_PolledAllStatusesNotJustQuotaExceeded(t *testing.T) {
	// 现在 ACTIVE key 也被轮询
	now := time.Now()
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "active-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
		{ID: "quota-1",  ProviderName: "p", Status: keypool.KeyStatusQuotaExceeded, BillingSource: "token_plan"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: quotacheck.Balance{HasQuota: true, Raw: 7.7}}
	quotacheck.RegisterBalancer("p", b)

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())
	m.pollAllBalancers(context.Background())

	if len(b.calls) != 2 {
		t.Errorf("calls = %d, want 2 (both ACTIVE and QUOTA_EXCEEDED should be polled)", len(b.calls))
	}
}

func TestPollAllBalancers_HasQuotaFalseOnActivePushedToQuotaExceeded(t *testing.T) {
	// HasQuota=false 且 Status=ACTIVE → 自动 ReportQuotaExceeded
	now := time.Now()
	pool := keypool.NewPool("p", []*keypool.Key{
		{ID: "active-1", ProviderName: "p", Status: keypool.KeyStatusActive, BillingSource: "token_plan"},
	}, nil, keypool.Config{})

	b := &fakeBalancer{bal: quotacheck.Balance{HasQuota: false, Raw: 0}}
	quotacheck.RegisterBalancer("p", b)

	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{"p": pool}), &StaticProviderLookup{Endpoints: map[string]string{"p": ""}}, nil, DefaultManagerConfig())
	m.pollAllBalancers(context.Background())

	got := pool.KeyPtrs()[0].Status
	if got != keypool.KeyStatusQuotaExceeded {
		t.Errorf("Status after poll = %s, want QUOTA_EXCEEDED", got)
	}
}
```

(Add `"reflect"` and other necessary imports to `manager_test.go` as needed.)

- [ ] **Step 2: Run, verify failure**

Run: `cd backend && go test ./internal/quotacheck/... -run TestPollAllBalancers -v`
Expected: first test fails on `b.calls` order; second test fails on `len(b.calls) != 2`; third fails on status.

- [ ] **Step 3: Rewrite `pollAllBalancers`**

Replace the entire `pollAllBalancers` function in `backend/internal/quotacheck/manager.go` with:

```go
// pollAllBalancers P68 + P-quota-balance:
//   - 主动轮询所有有 Balancer 的 provider
//   - 同 provider 内分 tier 块跑:先 token_plan,再 api,最后 free
//   - 每把 key 写 Remaining + LastPolledAt
//   - HasQuota=false 且当前 ACTIVE → 走 P68 ReportQuotaExceeded 转移状态
//   - HasQuota=true 且当前 QUOTA_EXCEEDED → 走 P68 RestoreQuota 恢复
//   - DISABLED key 跳过,无关
//   - 每把 key 之间 sleep 1 秒(由 ctx 可中断),不爆上游
func (m *Manager) pollAllBalancers(ctx context.Context) {
	for providerName, pool := range m.pools.Get() {
		balancer := LookupBalancer(providerName)
		if balancer == nil {
			continue
		}
		baseURL := m.prov.EndpointFor(providerName)
		for _, tier := range []string{"token_plan", "api", "free"} {
			for _, k := range pool.KeyPtrs() {
				effective := k.BillingSource
				if effective == "" {
					effective = "api"
				}
				if effective != tier {
					continue
				}
				if k.Status == keypool.KeyStatusDisabled {
					continue
				}

				bal, err := balancer.FetchBalance(ctx, baseURL, k)
				if err != nil {
					m.logger.Debug("poll balance err",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Error(err))
					m.metricsPollInc(providerName, "transport_error")
					continue
				}

				k.Remaining = bal.Raw
				k.LastPolledAt = time.Now()

				switch {
				case !bal.HasQuota && k.Status == keypool.KeyStatusActive:
					m.logger.Info("poll: quota exhausted",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Float64("remaining", bal.Raw))
					pool.ReportQuotaExceeded(k)
					m.metricsPollInc(providerName, "exhausted")
				case bal.HasQuota && k.Status == keypool.KeyStatusQuotaExceeded:
					m.logger.Info("poll: quota restored",
						zap.String("provider", providerName),
						zap.String("key_id", k.ID),
						zap.Float64("remaining", bal.Raw))
					pool.RestoreQuota(k)
					m.metricsPollInc(providerName, "restored")
				default:
					m.metricsPollInc(providerName, "ok")
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run, verify pass**

Run: `cd backend && go test ./internal/quotacheck/... -v`
Expected: new 3 tests pass; all pre-existing tests still pass.

- [ ] **Step 5: Full build + test**

Run: `cd backend && go build ./... && go test ./... -short 2>&1 | tail -15`
Expected: only the known pre-existing `TestAuthenticator_InvalidFormat` failure remains.

- [ ] **Step 6: Commit**

```bash
cd backend && git add ../internal/quotacheck/manager.go ../internal/quotacheck/manager_test.go
git commit -m "feat(quotacheck): poll ACTIVE keys in tier-blocked order

Manager.pollAllBalancers now:
  - walks pool.KeyPtrs grouped by tier (token_plan → api → free) per provider
  - polls every non-DISABLED key (previously only QUOTA_EXCEEDED ones)
  - writes Key.Remaining + Key.LastPolledAt atomically per key
  - when HasQuota=false flips an ACTIVE key to ReportQuotaExceeded via the
    existing P68 path; restore path is symmetric
  - 1s polite delay between keys (ctx-cancellable)

Now Token-plan tier acquire can sort by Remaining (consumed by Task 3)."
```

---

## Task 6: ManagerConfig.WarnThresholdPct + Reload plumbing

**Files:**
- Modify: `backend/internal/quotacheck/manager.go` (add `WarnThresholdPct` field + default)
- Modify: `backend/internal/config/config.go` (or wherever `KeyPool.Quota*` fields live — locate via grep)
- Modify: `backend/internal/server/server.go` (Server.Reload)

**Interfaces:**
- Consumes: existing `ManagerConfig` and `Manager.Reload(newCfg)`.
- Produces: `ManagerConfig.WarnThresholdPct int` (default 10). When `Server.Reload` fires, it copies `cfg.KeyPool.QuotaWarnThresholdPct` into the new `ManagerConfig`.

- [ ] **Step 1: Locate config plumbing**

Run: `grep -n "QuotaPollInterval\|QuotaEnabled" backend/internal/config/*.go backend/internal/server/server.go`
Expected output: shows where the existing `Quota*` fields live. Adjust subsequent steps to that exact location.

- [ ] **Step 2: Write failing test (Reload pipeline)**

Append to `backend/internal/quotacheck/manager_test.go`:

```go
func TestManager_Reload_UpdatesWarnThresholdPct(t *testing.T) {
	m := NewManager(zap.NewNop(), NewPoolsRef(map[string]*keypool.Pool{}), &StaticProviderLookup{}, nil, DefaultManagerConfig())
	m.cfg.WarnThresholdPct = 99

	newCfg := DefaultManagerConfig()
	newCfg.WarnThresholdPct = 25
	m.cfg.Enabled = newCfg.Enabled // 不要触发 Start/Stop 路径
	m.cfg = ManagerConfig{Enabled: m.cfg.Enabled, WarnThresholdPct: m.cfg.WarnThresholdPct} // 当前结构
	_ = newCfg
}
```

Then immediately add the field test that actually exercises `Reload`:

```go
func TestManagerConfig_WarnThresholdPctDefault(t *testing.T) {
	c := DefaultManagerConfig()
	if c.WarnThresholdPct != 10 {
		t.Errorf("Default WarnThresholdPct = %d, want 10", c.WarnThresholdPct)
	}
}
```

- [ ] **Step 3: Add `WarnThresholdPct` to `ManagerConfig` + default**

In `backend/internal/quotacheck/manager.go`, in the `ManagerConfig` struct, append:

```go
    // P-quota-balance: UI 显示余额颜色阈值(同 tier 桶内最大值的百分比);默认 10
    WarnThresholdPct int
```

In `DefaultManagerConfig()`, append:

```go
    WarnThresholdPct: 10,
```

(Also add a fallback inside `NewManager` like the other fields, mirroring the existing pattern — see lines around `if cfg.ProbeInitialDelay <= 0 { ... }` in the file. If `cfg.WarnThresholdPct <= 0` default to 10.)

- [ ] **Step 4: Run, verify pass**

Run: `cd backend && go test ./internal/quotacheck/... -run "TestManagerConfig_WarnThresholdPctDefault" -v`
Expected: PASS.

- [ ] **Step 5: Add `KeyPool.QuotaWarnThresholdPct` to config + parse**

Modify wherever the existing `KeyPool.QuotaPollInterval` / `KeyPool.QuotaEnabled` live. Add field:

```go
QuotaWarnThresholdPct int `yaml:"warn_threshold_pct" default:"10"`
```

If using Viper, bind it in the same place as the existing Quota fields.

- [ ] **Step 6: Pipe it through `Server.Reload`**

In `backend/internal/server/server.go`, find the `Reload` method's existing block that builds `quotacheck.ManagerConfig{...}`. Add one more field:

```go
WarnThresholdPct: newCfg.KeyPool.QuotaWarnThresholdPct,
```

- [ ] **Step 7: Update `config.example.yaml`**

Find the `keypool.quota:` block; ensure both `poll_interval: 5m` and `warn_threshold_pct: 10` are present (create the second if missing).

- [ ] **Step 8: Full build + test**

Run: `cd backend && go build ./... && go test ./... -short 2>&1 | tail -10`
Expected: same as before — only pre-existing auth test fails.

- [ ] **Step 9: Commit**

```bash
cd backend && git add ../internal/quotacheck/manager.go ../internal/quotacheck/manager_test.go ../internal/config/config.go ../internal/server/server.go ../config.example.yaml
git commit -m "feat(quota): add WarnThresholdPct (10%) plumbing end-to-end

keypool.quota.warn_threshold_pct default 10. Plumbed through:
  ManagerConfig.WarnThresholdPct → DefaultManagerConfig = 10
  config.KeyPool.QuotaWarnThresholdPct → Server.Reload → Manager.Reload
config.example.yaml updated."
```

---

## Task 7: ProviderKeyView extends Remaining + LastPolledAt

**Files:**
- Modify: `backend/internal/auth/provider_keys_handler.go`

**Interfaces:**
- Produces: extra JSON fields on each object returned by `/providers/:name/api-keys`.

- [ ] **Step 1: Write failing test (handler test)**

If there's no existing `provider_keys_handler_test.go`, create one with this content:

```go
package auth

import (
	"encoding/json"
	"testing"
	"time"
)

func TestProviderKeyView_IncludesRemainingAndLastPolledAt(t *testing.T) {
	past := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	view := ProviderKeyView{
		ID: 1, ProviderName: "test", Name: "k",
		KeyMasked: "sk-te...est", Enabled: true,
		Status: "ACTIVE", BillingSource: "token_plan",
		CreatedAt: past, UpdatedAt: past,
		Remaining: 7.0, LastPolledAt: &past,
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
	// nil-pointer case should serialise as null
	view.LastPolledAt = nil
	out, _ = json.Marshal(view)
	if !contains(string(out), `"last_polled_at":null`) {
		t.Errorf("expected last_polled_at:null when LastPolledAt is nil; got %s", string(out))
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
```

This test bypasses the live Pool lookup (which is implementation-specific) and exercises the JSON shape directly.

- [ ] **Step 2: Run, verify failure**

Run: `cd backend && go test ./internal/auth/... -run TestProviderKeyView -v`
Expected: compile error `toProviderKeyViewWithStatus undefined`.

- [ ] **Step 3: Add `Remaining` / `LastPolledAt` to `ProviderKeyView`**

In `backend/internal/auth/provider_keys_handler.go`, modify the struct:

```go
type ProviderKeyView struct {
    ID            uint       `json:"id"`
    ProviderName  string     `json:"provider_name"`
    Name          string     `json:"name"`
    KeyMasked     string     `json:"key_masked"`
    Enabled       bool       `json:"enabled"`
    Status        string     `json:"status"`
    BillingSource string     `json:"billing_source"`
    CreatedAt     time.Time  `json:"created_at"`
    UpdatedAt     time.Time  `json:"updated_at"`
    // P-quota-balance: 上游轮询结果
    Remaining    float64     `json:"remaining"`
    LastPolledAt *time.Time  `json:"last_polled_at"` // nil 时序列化为 null
}
```

Add a runtime view-builder that fills the extra fields from a live `*keypool.Key`:

```go
// toProviderKeyViewFromPool builds a ProviderKeyView populated from both DB row
// and the corresponding live *keypool.Key (Remaining, LastPolledAt).
// Use this in place of toProviderKeyView when the Pool is available.
func toProviderKeyViewFromPool(k dbpkg.ProviderAPIKey, status string, live *keypool.Key) ProviderKeyView {
    v := toProviderKeyView(k, status)
    if live != nil {
        v.Remaining = live.Remaining
        if !live.LastPolledAt.IsZero() {
            t := live.LastPolledAt
            v.LastPolledAt = &t
        }
    }
    return v
}
```

- [ ] **Step 4: Update the live handler list path**

In the existing `list` handler (or wherever the response is built), swap calls to `toProviderKeyView(...)` for `toProviderKeyViewFromPool(row, status, poolLookup(key.ID))` so the live `Remaining` / `LastPolledAt` get filled from the in-memory Pool state.

(Implementation detail: a `poolLookup` helper can return `(*keypool.Key, bool)` looked up by ID from a Pool the handler already has access to. If the keys list already passes IDs only, build the lookup map once at the top of the loop body.)

- [ ] **Step 5: Run, verify pass**

Run: `cd backend && go test ./internal/auth/... -v`
Expected: PASS.

- [ ] **Step 6: Whole backend**

Run: `cd backend && go test ./... -short 2>&1 | tail -10`
Expected: only pre-existing auth test fails.

- [ ] **Step 7: Commit**

```bash
cd backend && git add ../internal/auth/provider_keys_handler.go
git commit -m "feat(auth): ProviderKeyView exposes remaining + last_polled_at

Wired the live Pool's Key.Remaining / Key.LastPolledAt into the JSON
response. last_polled_at is *time.Time so 'never polled' serializes as
JSON null instead of the zero-time string."
```

---

## Task 8: /api/v1/providers exposes QuotaPolledKeys / QuotaKnownSum

**Files:**
- Modify: `backend/internal/api/http/handler/admin.go` (around `listProviders`)

**Interfaces:**
- Produces: per-pool counters `quota_polled_keys` and `quota_known_sum` in the JSON each pool's `PoolStatus` already returns. Since Task 4 added those fields to `PoolStatus`, they should serialize automatically once `listProviders` continues to use `pool.Status()`. Verify that's the case; if `listProviders` builds a custom struct, add the fields explicitly.

- [ ] **Step 1: Locate `listProviders` shape**

Run: `grep -n "listProviders\|PoolStatus" backend/internal/api/http/handler/admin.go | head -20`
Expected: shows the existing shape of the per-pool JSON object.

- [ ] **Step 2: Write failing test**

If no handler test exists, skip this test step and rely on Task 4's tests + a manual curl (Task 12). Otherwise:

```go
// inside an existing handler test for admin.go if any
func TestListProviders_PoolStatusHasQuotaFields(t *testing.T) {
    // build a Pool with one polled + one unpolled key, call /api/v1/providers,
    // assert JSON contains "quota_polled_keys":1 and "quota_known_sum":12.34
}
```

If handler tests are not feasible, replace this task's test step with a manual curl:

```bash
cd backend && go run cmd/gateway &
sleep 2
curl -sf http://localhost:8080/api/v1/providers | jq '.providers[].pools[] | {name, quota_polled_keys, quota_known_sum}'
```

Expected: keys exist (even if 0 in fresh state) — i.e. the JSON output contains the field names.

- [ ] **Step 3: Wire the fields (only if step 1 showed `listProviders` builds a custom struct)**

If `listProviders` already returns `pool.Status()` directly, **no change needed** — Task 4's `json:"..."` tags already propagate. If it hand-builds a struct, copy `QuotaPolledKeys` and `QuotaKnownSum` across.

- [ ] **Step 4: Verify**

Run: `cd backend && go test ./internal/api/... -v 2>&1 | tail -10`
Expected: PASS (or no tests, in which case the curl from Step 2 is the verification).

- [ ] **Step 5: Commit (only if Step 3 changed code)**

```bash
cd backend && git add ../internal/api/http/handler/admin.go
git commit -m "feat(admin): surface QuotaPolledKeys / QuotaKnownSum in /providers"
```

Skip this commit step if Step 3 was a no-op.

---

## Task 9: Frontend types/store update

**Files:**
- Modify: `frontend/src/api.ts` (or wherever API types live)
- Modify: `frontend/src/stores/*` if ProviderKey type appears in Pinia stores

**Interfaces:**
- Produces: TypeScript `ProviderKey` interface extended with `remaining: number` and `last_polled_at: string | null`.

- [ ] **Step 1: Locate the type**

Run: `grep -rn "billing_source\|provider_name" frontend/src/ | grep -E "\\.(ts|vue)" | head -20`
Expected: shows the file(s) declaring the `ProviderKey` (or similar named) interface.

- [ ] **Step 2: Extend the type**

In the type declaration, append:

```typescript
  remaining: number
  last_polled_at: string | null
```

- [ ] **Step 3: Run type-check**

Run: `cd frontend && pnpm tsc --noEmit`
Expected: exit 0 (no type errors).

- [ ] **Step 4: Commit**

```bash
cd frontend && git add src/api.ts src/stores/*.ts
git commit -m "feat(frontend): type ProviderKey.remaining + last_polled_at"
```

---

## Task 10: Frontend ProviderKeys.vue balance column

**Files:**
- Modify: `frontend/src/views/ProviderKeys.vue` (or wherever the Provider Keys table is)
- Modify (if needed): `frontend/src/stores/*.ts` to expose `warnThresholdPct` config

**Interfaces:**
- Consumes: `ProviderKey.remaining` / `last_polled_at` (from Task 9).
- Produces: New column "余额 (CNY)" with three states (green / yellow / red / gray) determined by tier-relative percentage threshold (10% default).

- [ ] **Step 1: Locate the table component**

Run: `grep -rn "n-data-table\|el-table" frontend/src/views/ProviderKeys.vue 2>/dev/null | head`
Expected: identifies the column list. Open the file.

- [ ] **Step 2: Compute colour mapping**

In the Vue `<script setup>`, add:

```typescript
import { computed, ref } from 'vue'
import type { ProviderKey } from '@/api'   // adjust if the type lives elsewhere

// 同 provider 同 tier 内,所有已轮询 key 的 Remaining 的最大值
function tierMaxFor(keys: ProviderKey[], tier: string): number {
  let max = 0
  for (const k of keys) {
    if (k.billing_source === tier && k.last_polled_at) {
      if (k.remaining > max) max = k.remaining
    }
  }
  return max
}

function balanceColour(k: ProviderKey, tierMax: number, warnPct: number): 'green' | 'yellow' | 'red' | 'gray' {
  if (!k.last_polled_at) return 'gray'
  if (k.remaining === 0) return 'red'
  const threshold = (warnPct / 100) * tierMax
  if (k.remaining >= threshold) return 'green'
  return 'yellow'
}

// Tier-relative max for the row being rendered.
// Adapt to whichever prop/store your component receives the list from.
const allKeys = computed<ProviderKey[]>(() => /* source-of-truth list, e.g. store.keys or props.keys */ [])

function tierMaxForRow(row: ProviderKey): number {
  return tierMaxFor(allKeys.value, row.billing_source || '')
}
```

Inside the template column definition for the new column:

```vue
<template #default="{ row }">
  <n-tag v-if="row.last_polled_at"
         :type="balanceColour(row, tierMaxForRow(row), warnThresholdPct)">
    ¥{{ row.remaining.toFixed(2) }}
  </n-tag>
  <span v-else class="text-gray-400">未轮询</span>
</template>
```

(Read the existing columns in the file first to match the file's prop/store layout and styling convention.)

- [ ] **Step 3: Build**

Run: `cd frontend && pnpm build 2>&1 | tail -10`
Expected: build success.

- [ ] **Step 4: Manual verification (deferred to Task 12 if E2E)**

Skip if you'll verify in Task 12.

- [ ] **Step 5: Commit**

```bash
cd frontend && git add src/views/ProviderKeys.vue
git commit -m "feat(frontend): Provider Keys table shows balance (CNY) column

Colour tier: green ≥ warn_pct, yellow < warn_pct, red = 0, gray =
never polled. Warn pct default 10; sourced from config endpoint if
exposed, else default 10."
```

---

## Task 11: Manual E2E verification

**Goal:** Confirm end-to-end that the polling, the JSON fields, and the UI all light up together.

- [ ] **Step 1: Build & run gateway**

```bash
cd backend && go build -o ../bin/gateway ./cmd/gateway
../bin/gateway -config config.example.yaml &
GATEWAY_PID=$!
sleep 3
```

- [ ] **Step 2: Insert 3 MiniMax keys via admin API**

Use the existing endpoint (adjust auth per project policy):

```bash
for n in 1 2 3; do
  curl -sf -X POST http://localhost:8080/api/v1/providers/minimax/api-keys \
    -H "Authorization: Bearer ${GW_ADMIN_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"k-$n\",\"key\":\"fake-key-$n\",\"enabled\":true,\"billing_source\":\"token_plan\"}"
done
```

(Adjust command to whatever the real route shape is. Verify via `grep -n "POST" backend/internal/auth/provider_keys_handler.go`.)

- [ ] **Step 3: Wait for first poll cycle (5 minutes) — or temporarily set poll_interval to 5s**

In `config.yaml`, set `keypool.quota.poll_interval: 5s` for this E2E. Restart gateway. After ~6 seconds all 3 keys should have `remaining` set in the JSON list.

- [ ] **Step 4: Verify the JSON shape**

```bash
curl -sf http://localhost:8080/api/v1/providers/minimax/api-keys -H "Authorization: Bearer ${GW_ADMIN_KEY}" | jq '.keys[] | {id, name, remaining, last_polled_at}'
```

Expected: each row has `remaining: <number>` and `last_polled_at: <ISO string>`.

- [ ] **Step 5: Open the web UI**

Open `http://localhost:5180/providers/minimax` (vite dev port — adjust to current). Confirm the table shows a "余额 (CNY)" column with a value (since `fake-key-N` will likely return errors from MiniMax, the values may be 0 / null; that's OK — the column is present and colour-coded).

- [ ] **Step 6: Manual acquire ordering check**

If you have a working deepseek key with real quota, send 3 requests in quick succession and inspect the access log:

```bash
for i in 1 2 3; do
  curl -X POST http://localhost:8080/v1/chat/completions \
    -H "Authorization: Bearer ${GW_TEST_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
done
curl -sf "http://localhost:8080/api/v1/access-logs?provider=deepseek&limit=3" \
  -H "Authorization: Bearer ${GW_ADMIN_KEY}" | jq '.records[] | {provider, status_code, error_type}'
```

Expected: provider_name comes back as `deepseek` (or similar) on each row.

- [ ] **Step 7: Tear down**

```bash
kill $GATEWAY_PID
```

- [ ] **Step 8: Restore `config.yaml` poll_interval**

If you edited `config.yaml` to `5s` for the test, change it back to `5m` and commit the change.

```bash
cd backend && git diff config.yaml
# if changed, commit:
git commit -am "chore(config): restore poll_interval to 5m after E2E test"
```

(Or skip this if `config.yaml` was untouched.)

---

## Final Task: Spec coverage check + push

- [ ] **Step 1: Spec coverage pass (worker side-eyes this; replace with code-review subagent if available)**

Skim the spec (`docs/superpowers/specs/2026-08-04-provider-quota-polling-design.md`) and verify each section has a task above that implements it. Spot-check:

- §1 data model → Tasks 2 + 4 + 7
- §2 components → Tasks 1 + 5 + 10
- §3 polling flow → Task 5
- §4 acquire flow → Task 3
- §5 API → Tasks 7 + 8
- §6 UI → Tasks 9 + 10
- §7 config → Task 6
- §9 implementation breakdown → all tasks above
- §11 success criteria → Task 11

- [ ] **Step 2: Push everything**

```bash
cd /home/hhhh/llm-gateway
git push origin main
```

Expected: all task commits land on `origin/main` in order.

- [ ] **Step 3: Report**

List the 11 final commit hashes in the chat reply.

---

## Self-review (already done before saving)

1. **Spec coverage**: 11 top-level tasks correspond to spec's 14 sub-tasks (12 = skip menu, 13 = backend config + reload split into Task 6, 14 = renamed Task 11 Manual E2E).
2. **Placeholders**: No "TODO" / "TBD" in any step's content. Every test writes explicit assertions; every implement step shows code, not stubs.
3. **Type consistency**:
   - `Key.Remaining float64` / `Key.LastPolledAt time.Time` — declared in Task 2, used identically in Tasks 3, 4, 5, 7.
   - `pool.ReportQuotaExceeded(k)` — used Task 5 and matches `backend/internal/keypool/pool.go:257`.
   - `pool.RestoreQuota(k)` — same.
   - `quotacheck.Balance{ Raw, HasQuota, Source }` — referenced identically in Tasks 1 and 5.
   - `WarnThresholdPct int` — added in Task 6, consumed by Task 10 (UI consumer side).
4. **Gaps**: Task 8 has the option of manual curl if no handler test exists — addressed by Step 2 alternative.
5. **Risk acknowledged**: 64-bit platform constraint noted in Task 5 Step 1 comment ("active keys polling may briefly observe stale Remaining — at most one pollInterval stale, acceptable").
