# Provider 额度列显示(按数据类型渲染)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Provider Keys 页面的额度列按上游返回的数据类型渲染(minimax 等 token_plan → 百分比,deepseek 等 api 付费 → 金额),并把列名"余额"改为"额度"。

**Architecture:** 单位知识由 balancer 在数据源头上报:`quotacheck.Balance` 加 `Kind` 字段(minimax→"percent",deepseek→"currency"),经 `keypool.Key.QuotaKind` → `auth.ProviderKeyView.quota_kind` 透传到前端,前端按 kind 渲染。`PoolStatus` 加 `QuotaKind`(池级 dominant kind)供 Overview 仪表盘卡片判断是否可汇总。

**Tech Stack:** Go(backend)、Vue 3 + TypeScript + naive-ui(frontend)、Gin、GORM。

**Spec:** `docs/superpowers/specs/2026-08-04-provider-quota-display-design.md`(commit `1f3690f`)

## Global Constraints

- 后端构建/测试:`cd /home/hhhh/llm-gateway/backend && go build ./... && go test ./...`
- 前端类型检查/构建:`cd /home/hhhh/llm-gateway/frontend && npm run build`(vue-tsc + vite)
- 不落 DB、不改 config.yaml、不加依赖
- Kind 约定:仅两个字面量 `"percent"` / `"currency"`;空串 = 兼容旧行为(前端按 currency 渲染)
- Commit 直接提交 main(仓库既有惯例,见 git log),conventional commit message
- 现有测试只断言 `Raw` / `HasQuota` / error,加 `Kind` 字段不破坏它们

---

### Task 1: 数据链路 — Balance.Kind + Key.QuotaKind + pollAllBalancers 写入

**Files:**
- Modify: `backend/internal/quotacheck/prober.go:48-53`(Balance struct)
- Modify: `backend/internal/keypool/key.go:45-47`(Key struct)
- Modify: `backend/internal/quotacheck/manager.go:589-590`(pollAllBalancers 写入)
- Test: `backend/internal/quotacheck/manager_test.go:323-361`(TestPollAllBalancers_TierBlocked)

**Interfaces:**
- Produces: `quotacheck.Balance.Kind string`(字段,`"percent"`/`"currency"`/`""`);`keypool.Key.QuotaKind string`(运行时字段,同 `Remaining` 不落 DB)

- [ ] **Step 1: 加结构体字段(编译脚手架)**

`prober.go` 的 `Balance` struct(第 48-53 行)追加字段:

```go
// Balance 余额查询结果
type Balance struct {
	Raw      float64 // 解析出的余额数值(单位由 Kind 决定)
	HasQuota bool    // true 表示余额 > 0,key 可用
	Source   string  // "deepseek:/user/balance" 之类,用于日志/metrics
	// P-quota-display: 数值类型 — "percent" | "currency";空 = 兼容旧行为(按 currency)
	Kind string
}
```

`key.go` 的 `Key` struct(第 45-47 行)追加字段:

```go
	// P-quota-balance: 上游 quota polling 写入的余额快照与时间戳(runtime, 不落 DB)
	Remaining    float64
	LastPolledAt time.Time
	// P-quota-display: 上次 poll 的数值类型("percent"/"currency"/"")— 前端按此渲染单位
	QuotaKind string
```

- [ ] **Step 2: 在现有测试里加 QuotaKind 断言(先红)**

`manager_test.go` 的 `TestPollAllBalancers_TierBlocked`(第 333 行)改 stub 返回:

```go
	b := &fakeBalancer{bal: Balance{HasQuota: true, Raw: 1.0, Kind: "percent"}}
```

在该测试末尾的 Remaining/LastPolledAt 断言循环里(第 353-360 行)追加:

```go
	// P-quota-display: QuotaKind 应随 poll 写入(pipeline: balancer → Key)
	for _, k := range pool.KeyPtrs() {
		if k.QuotaKind != "percent" {
			t.Errorf("%s QuotaKind = %q, want %q", k.ID, k.QuotaKind, "percent")
		}
	}
```

- [ ] **Step 3: 运行测试,确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/quotacheck/ -run TestPollAllBalancers_TierBlocked -v`
Expected: FAIL — `QuotaKind = "" , want "percent"`(manager 还没写入)

- [ ] **Step 4: 实现写入**

`manager.go` `pollAllBalancers` 第 589-590 行改为:

```go
				k.Remaining = bal.Raw
				k.QuotaKind = bal.Kind
				k.LastPolledAt = time.Now()
```

- [ ] **Step 5: 运行测试,确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/quotacheck/ -run TestPollAllBalancers -v`
Expected: PASS(3 个 TestPollAllBalancers_* 全过)

- [ ] **Step 6: 全量后端测试 + 提交**

Run: `cd /home/hhhh/llm-gateway/backend && go build ./... && go test ./...`
Expected: 全过

```bash
git add backend/internal/quotacheck/prober.go backend/internal/keypool/key.go backend/internal/quotacheck/manager.go backend/internal/quotacheck/manager_test.go
git commit -m "feat(quotacheck): carry quota kind (percent/currency) through poll pipeline"
```

---

### Task 2: Balancer 上报 Kind(minimax→percent,deepseek→currency)

**Files:**
- Modify: `backend/internal/provider/minimax/balancer.go`(所有返回点,共 8 处)
- Modify: `backend/internal/provider/deepseek/balancer.go`(所有返回点,共 5 处)
- Test: `backend/internal/provider/minimax/balancer_test.go:32-72`
- Test: `backend/internal/provider/deepseek/balancer_test.go:17-51`

**Interfaces:**
- Consumes: `quotacheck.Balance.Kind`(Task 1)
- Produces: `minimax` balancer 返回 `Kind: "percent"`;`deepseek` balancer 返回 `Kind: "currency"`

- [ ] **Step 1: 写失败断言**

`minimax/balancer_test.go` 的 `TestMiniMaxBalancer_ParsesRealSchema`(第 66-72 行 HasQuota 断言后)追加:

```go
	if got.Kind != "percent" {
		t.Errorf("Kind = %q, want %q", got.Kind, "percent")
	}
```

`deepseek/balancer_test.go` 的 `TestDeepseekBalancer_ParsesBalance`(第 48-50 行 Raw 断言后)追加:

```go
	if got.Kind != "currency" {
		t.Errorf("Kind = %q, want %q", got.Kind, "currency")
	}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/provider/minimax/ ./internal/provider/deepseek/ -run "Parses" -v`
Expected: FAIL — `Kind = "" , want "percent"` / `Kind = "" , want "currency"`

- [ ] **Step 3: minimax balancer 加 Kind**

`minimax/balancer.go` 的 8 个 `quotacheck.Balance{...}` 返回点全部加 `Kind: "percent"`(错误分支 6 个 + 空 model_remains 分支 + 正常分支),例如:

```go
	// 正常分支
	return quotacheck.Balance{
		Raw:      minPct,
		HasQuota: minPct > 0,
		Source:   "minimax:/v1/token_plan/remains",
		Kind:     "percent",
	}, nil
```

错误分支示例(401/403):

```go
		return quotacheck.Balance{
			Raw:      0,
			HasQuota: false,
			Source:   "minimax:/v1/token_plan/remains",
			Kind:     "percent",
		}, fmt.Errorf("minimax quota auth: HTTP %d", resp.StatusCode)
```

- [ ] **Step 4: deepseek balancer 加 Kind**

`deepseek/balancer.go` 的 5 个 `quotacheck.Balance{...}` 返回点全部加 `Kind: "currency"`(注意:NewRequest err 分支返回零值 `Balance{}`,不是 `Balance{HasQuota:false, Source:...}` 结构,保持零值即可,其余 4 处加 Kind),例如正常分支:

```go
	return quotacheck.Balance{
		Raw:      raw,
		HasQuota: parsed.IsAvailable && raw > 0,
		Source:   "deepseek:/user/balance",
		Kind:     "currency",
	}, nil
```

- [ ] **Step 5: 运行测试,确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/provider/minimax/ ./internal/provider/deepseek/ -v`
Expected: PASS(全部 balancer 测试)

- [ ] **Step 6: 提交**

```bash
git add backend/internal/provider/minimax/balancer.go backend/internal/provider/minimax/balancer_test.go backend/internal/provider/deepseek/balancer.go backend/internal/provider/deepseek/balancer_test.go
git commit -m "feat(balancers): minimax reports percent, deepseek reports currency"
```

---

### Task 3: API 透传 — ProviderKeyView.quota_kind

**Files:**
- Modify: `backend/internal/auth/provider_keys_handler.go:82-98`(ProviderKeyView struct)
- Modify: `backend/internal/auth/provider_keys_handler.go:120-130`(toProviderKeyViewFromPool)
- Test: `backend/internal/auth/provider_keys_handler_test.go`(追加新测试)

**Interfaces:**
- Consumes: `keypool.Key.QuotaKind`(Task 1)
- Produces: `auth.ProviderKeyView.QuotaKind string \`json:"quota_kind"\``

- [ ] **Step 1: View struct 加字段**

`provider_keys_handler.go` 的 `ProviderKeyView`(第 95-97 行)追加:

```go
	// P-quota-balance: 上游轮询结果
	Remaining    float64    `json:"remaining"`
	LastPolledAt *time.Time `json:"last_polled_at"` // nil 时序列化为 null
	// P-quota-display: 数值类型("percent"/"currency"/"")— 前端按此渲染单位
	QuotaKind string `json:"quota_kind"`
```

- [ ] **Step 2: 写失败测试**

`provider_keys_handler_test.go` 追加(import 加 `"github.com/wang546673478/native-llm-gateway/internal/keypool"` 和 `dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"`):

```go
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
```

- [ ] **Step 3: 运行测试,确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/auth/ -run TestProviderKeyViewFromPool_IncludesQuotaKind -v`
Expected: FAIL — `QuotaKind = "" , want "percent"`(toProviderKeyViewFromPool 还没拷贝)

- [ ] **Step 4: 实现透传**

`toProviderKeyViewFromPool`(第 120-130 行)改为:

```go
func toProviderKeyViewFromPool(k dbpkg.ProviderAPIKey, status string, live *keypool.Key) ProviderKeyView {
	v := toProviderKeyView(k, status)
	if live != nil {
		v.Remaining = live.Remaining
		v.QuotaKind = live.QuotaKind
		if !live.LastPolledAt.IsZero() {
			t := live.LastPolledAt
			v.LastPolledAt = &t
		}
	}
	return v
}
```

- [ ] **Step 5: 运行测试,确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/auth/ -v`
Expected: PASS(含既有 TestProviderKeyView_IncludesRemainingAndLastPolledAt)

- [ ] **Step 6: 提交**

```bash
git add backend/internal/auth/provider_keys_handler.go backend/internal/auth/provider_keys_handler_test.go
git commit -m "feat(api): expose quota_kind in provider key views"
```

---

### Task 4: PoolStatus.QuotaKind(Overview 卡片用)

**Files:**
- Modify: `backend/internal/keypool/pool.go:317-327`(PoolStatus struct)
- Modify: `backend/internal/keypool/pool.go:347-352`(Status() 聚合循环)
- Test: `backend/internal/keypool/keypool_test.go`(追加新测试)

**Interfaces:**
- Consumes: `keypool.Key.QuotaKind`(Task 1)
- Produces: `keypool.PoolStatus.QuotaKind string \`json:"quota_kind"\`` — 池级 dominant kind

- [ ] **Step 1: 写失败测试**

`keypool_test.go` 追加:

```go
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
	// 未 poll → ""
	none := NewPool("t", []*Key{{ID: "a", ProviderName: "t", Name: "a", Key: "sk",
		Status: KeyStatusActive, CreatedAt: now, UpdatedAt: now}}, nil, Config{})
	if got := none.Status().QuotaKind; got != "" {
		t.Errorf("no-poll QuotaKind = %q, want empty", got)
	}
}
```

- [ ] **Step 2: 运行测试,确认编译失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/keypool/ -run TestPool_Status_QuotaKindDominant -v`
Expected: FAIL(编译错 — `PoolStatus.QuotaKind` 字段不存在)

- [ ] **Step 3: PoolStatus 加字段**

`pool.go` 第 324-326 行后追加:

```go
	// P-quota-display: polled keys 的类型 — 全部 percent → "percent";否则 "currency"
	// (空 Kind 如 GLM 按 currency,前端维持 ¥ 渲染)
	QuotaKind string `json:"quota_kind"`
```

- [ ] **Step 4: Status() 计算 dominant kind**

`Status()`(第 347-352 行)改为:

```go
	for _, k := range p.keys {
		if !k.LastPolledAt.IsZero() {
			s.QuotaPolledKeys++
			s.QuotaKnownSum += k.Remaining
		}
	}
	// P-quota-display: dominant kind — 全部 percent → "percent",否则 "currency"
	if s.QuotaPolledKeys > 0 {
		s.QuotaKind = "currency"
		allPercent := true
		for _, k := range p.keys {
			if k.LastPolledAt.IsZero() {
				continue
			}
			if k.QuotaKind != "percent" {
				allPercent = false
				break
			}
		}
		if allPercent {
			s.QuotaKind = "percent"
		}
	}
	return s
```

- [ ] **Step 5: 运行测试,确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/keypool/ -v`
Expected: PASS(含既有 TestPool_StatusIncludesQuotaSummary)

- [ ] **Step 6: 提交**

```bash
git add backend/internal/keypool/pool.go backend/internal/keypool/keypool_test.go
git commit -m "feat(keypool): PoolStatus.QuotaKind for dashboard rendering"
```

---

### Task 5: 前端 ProviderKeys.vue — 列名"额度" + 按 kind 渲染

**Files:**
- Modify: `frontend/src/views/ProviderKeys.vue:78-92`(TS interface)
- Modify: `frontend/src/views/ProviderKeys.vue:208-231`(额度列定义)

**Interfaces:**
- Consumes: API 新字段 `quota_kind`(Task 3 产出)
- Produces: 额度列按 `quota_kind === 'percent'` → `43%`,否则 → `¥12.34`

- [ ] **Step 1: TS interface 加字段**

`ProviderKeys.vue` 的 `ProviderKeyView` interface(第 89-91 行)追加:

```ts
  remaining: number
  last_polled_at: string | null
  // P-quota-display: 数值类型 — "percent" / "currency" / ""(空按 currency)
  quota_kind: string
```

- [ ] **Step 2: 列名 + 渲染逻辑**

`ProviderKeys.vue` 第 208-231 行(整个 `title: '余额 (CNY)'` 列定义)替换为:

```ts
  {
    // P-quota-display: 列名"额度";渲染按 quota_kind:
    //   percent → "43%"(取整);currency/空 → "¥12.34"
    title: '额度',
    key: 'remaining',
    width: 130,
    render: (row) => {
      if (!row.last_polled_at) {
        return h('span', { style: { color: '#999' } }, '未轮询')
      }
      const colour = balanceColour(row, tierMaxForRow(row), warnThresholdPct.value)
      const map: Record<string, string> = {
        green:  '#18a058',
        yellow: '#f0a020',
        red:    '#d03050',
        gray:   '#999',
      }
      const text =
        row.quota_kind === 'percent'
          ? `${Math.round(row.remaining)}%`
          : `¥${row.remaining.toFixed(2)}`
      return h(
        'span',
        { style: { color: map[colour] ?? '#999', fontWeight: 500 } },
        text,
      )
    },
  },
```

- [ ] **Step 3: 类型检查构建**

Run: `cd /home/hhhh/llm-gateway/frontend && npm run build`
Expected: 构建成功(vue-tsc 无类型错误)

- [ ] **Step 4: 提交**

```bash
git add frontend/src/views/ProviderKeys.vue
git commit -m "feat(frontend): render quota column by kind, rename 余额→额度"
```

---

### Task 6: 前端 Overview.vue + client.ts — percent 池显示 "—"

**Files:**
- Modify: `frontend/src/api/client.ts:168-171`(DashboardResp.keypools 类型)
- Modify: `frontend/src/views/Overview.vue:77-81`(QuotaKnownSum 卡片)

**Interfaces:**
- Consumes: API 新字段 `quota_kind`(Task 4 产出,池级)

- [ ] **Step 1: client.ts 类型加字段**

`client.ts` 的 `keypools` 数组项(第 168-171 行)追加:

```ts
    quota_polled_keys: number
    quota_known_sum: number
    // P-quota-display: polled keys 的类型 — "percent" 池不可汇总,前端显示 —
    quota_kind: string
```

- [ ] **Step 2: Overview.vue 卡片按 kind 渲染**

`Overview.vue` 第 77-81 行的 `.bs-val` 改为:

```html
<span class="bs-val">
  {{ row.quota_kind === 'percent' ? '—' : `¥${row.quota_known_sum.toFixed(2)}` }}
</span>
```

- [ ] **Step 3: 类型检查构建**

Run: `cd /home/hhhh/llm-gateway/frontend && npm run build`
Expected: 构建成功

- [ ] **Step 4: 提交**

```bash
git add frontend/src/api/client.ts frontend/src/views/Overview.vue
git commit -m "fix(frontend): dashboard QuotaKnownSum card — percent pools show —"
```

---

### Task 7: 全量验证

**Files:** 无(只跑命令)

- [ ] **Step 1: 后端全量测试**

Run: `cd /home/hhhh/llm-gateway/backend && go build ./... && go test ./...`
Expected: 全过

- [ ] **Step 2: 前端构建**

Run: `cd /home/hhhh/llm-gateway/frontend && npm run build`
Expected: 构建成功

- [ ] **Step 3: 手动 E2E(可选,有真实 key 时)**

1. 启动 gateway + 前端 dev server(`make run` 或等价方式,后端 8088)
2. Provider Keys 页面:minimax key 额度列显示 `43%` 样式(无 ¥ 前缀);deepseek key 显示 `¥12.34`;列名"额度"
3. 等待 60s(poll_interval=60s)后刷新,minimax 额度出现百分比
4. Overview 页面:minimax 池的"可用额度"显示 `—`,deepseek 池显示 `¥sum`
5. 若手头有 minimax key 被误标 `billing_source: api`,确认额度列仍显示百分比(跟随上游数据)

- [ ] **Step 4: 提交收尾**

无新代码改动则不提交。若有发现的问题,修复后并入对应 Task 的提交或新增 commit。
