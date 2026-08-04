# Provider 额度列显示 — 按数据类型的单位渲染

**日期**: 2026-08-04
**作者**: Claude
**目标版本**: Native LLM Gateway 下一版
**代号**: P-quota-display

---

## 0. Context & 动机

P-quota-balance 上线后,Provider Keys 页面的"余额"列对所有 provider 一律显示 `¥{{remaining.toFixed(2)}}`。但 `Key.Remaining` 的**单位随 provider 而不同**,并非都是金额:

| Provider | Balancer 返回的 Raw 含义 | 当前前端显示 | 正确显示 |
|---|---|---|---|
| minimax / minimax-openai | 0-100 百分比(MIN 跨模型的 `current_interval_remaining_percent`) | `¥43.00` | `43%` |
| deepseek / deepseek-anthropic | CNY 金额(`balance_infos[].total_balance` 求和) | `¥12.34` | `¥12.34` |
| glm / glm-anthropic | `limit - used`(套餐次数,**本期不管**,用户明确延后) | `¥1500.00` | 待定(以后再说) |
| 其余(kimi/qwen/gemini) | 无 Balancer → 未轮询 | `未轮询` | 不变 |

用户要求:
1. token_plan 类 provider(minimax)显示**剩余百分比**;api 付费类显示**剩余余额金额**
2. 列名"余额"改为"额度"

**根因**:单位知识只存在于 balancer 内部(它决定返回什么),数据链路上没有任何字段把"这个数是什么单位"带给前端。前端无从判断,只能假设全是 CNY。

---

## 1. 设计原则

**显示类型由上游实际返回的数据类型决定(balancer 上报),不依赖用户手选的 `billing_source` tier。**

原因:
- minimax 的 token_plan API 无论 key 怎么标 tier,返回的都是百分比
- deepseek 的 balance API 返回的是金额
- `billing_source` 是用户手选的计费分类,可能跟上游真实计费类型不一致(误标);显示必须跟随上游真实数据,否则会出现 `¥43.00%` 之类的语义错位

---

## 2. 数据模型

### 2.1 `quotacheck.Balance` 加 `Kind` 字段

```go
// Balance 余额查询结果
type Balance struct {
    Raw      float64 // 解析出的数值(单位由 Kind 决定)
    HasQuota bool
    Source   string
    Kind     string  // 新增:"percent" | "currency";空 = 兼容旧行为(按 currency)
}
```

每个 Balancer 在返回值中显式声明自己返回的数值类型:

- `minimax` balancer: 返回 `Kind: "percent"`(Raw 本来就是 0-100 百分比,取 MIN 跨模型)
- `deepseek` balancer: 返回 `Kind: "currency"`(金额)
- 未来新 Balancer 按同样方式声明

### 2.2 `keypool.Key` 加运行时字段

```go
type Key struct {
    // ... 既有字段 ...
    Remaining    float64   // 上次 poll 的余额(0 表示确定耗尽)
    LastPolledAt time.Time
    QuotaKind    string    // 新增:P-quota-display — 上次 poll 的数值类型("percent"/"currency"/"")
}
```

同 `Remaining`:**不落 DB**。重启后丢失,首次 poll 后回填。

### 2.3 `auth.ProviderKeyView` 加前端字段

```go
type ProviderKeyView struct {
    // ... 既有字段 ...
    Remaining    float64    `json:"remaining"`
    LastPolledAt *time.Time `json:"last_polled_at"`
    QuotaKind    string     `json:"quota_kind"` // 新增:"percent"/"currency"/""
}
```

由 `toProviderKeyViewFromPool` 从 live `*keypool.Key` 透传。

---

## 3. 组件改动

### 3.1 `internal/quotacheck/prober.go` — `Balance` + `Kind string`(2 行)

### 3.2 `internal/provider/minimax/balancer.go` — 返回 `Kind: "percent"`

`FetchBalance` 的返回值(3 处:错误分支、空 model_remains 分支、正常分支)加 `Kind: "percent"`。

### 3.3 `internal/provider/deepseek/balancer.go` — 返回 `Kind: "currency"`

`FetchBalance` 的返回值(3 处错误分支 + 正常分支)加 `Kind: "currency"`。

### 3.4 `internal/quotacheck/manager.go` — `pollAllBalancers` 写入

`k.Remaining = bal.Raw` 旁边加一行:

```go
k.Remaining = bal.Raw
k.QuotaKind = bal.Kind
k.LastPolledAt = time.Now()
```

### 3.5 `internal/auth/provider_keys_handler.go` — View 透传

`toProviderKeyViewFromPool` 中:

```go
if live != nil {
    v.Remaining = live.Remaining
    v.QuotaKind = live.QuotaKind
    // ...
}
```

---

## 4. 前端改动

### 4.1 `frontend/src/views/ProviderKeys.vue` — 列名 + 按 kind 渲染

列定义(原第 208-231 行附近):

```ts
{
  title: '额度',   // 原 '余额 (CNY)'
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
    return h('span', { style: { color: map[colour] ?? '#999', fontWeight: 500 } }, text)
  },
}
```

`ProviderKeyView` 接口(前端 TS)加 `quota_kind: string`。

**颜色逻辑不动**:tier-relative 阈值(`remaining / tier_max >= warn_threshold_pct/100`)对 0-100 的百分比同样适用,行为不变。

### 4.2 `frontend/src/views/Overview.vue` — QuotaKnownSum 卡片(同款 bug)

现状:对 minimax 池显示 `¥43.00`(百分比求和,无意义,且加 ¥ 前缀误导)。

**显示规则**:
- 池的 `quota_kind == "percent"` → 显示 `—`(百分比不可跨 key 汇总)
- 其余(currency/空)→ 现状 `¥{{sum.toFixed(2)}}`

**后端配合**:`keypool.PoolStatus` 加 `QuotaKind string`(池内 polled keys 的 dominant kind;全部为 percent → "percent",否则 "currency"/""),在 `Status()` 里统计时顺手填。`quota_known_sum` 本身**不改**(仍是 Remaining 之和;percent 池的 sum 前端直接不展示)。

> 注:这条是超出用户原始需求的同域修正(同一 bug:金额前缀显示非金额数据)。如不要,前端按池内 kind 判空即可,**也可划掉此条**。

---

## 5. 不在本次设计范围(YAGNI)

- **GLM 显示形式** — 用户明确"以后再说";本期 glm balancer 的 Raw 照旧写 `Remaining`,`QuotaKind` 暂不设置(空 → 前端按 currency 渲染,维持现状 `¥1500.00`)。等 GLM 的 key 实际接入再定显示形式
- **请求次数显示**(`current_interval_total_count` / `current_interval_usage_count`)— 用户选了百分比;且实测计数为 0,不可靠
- **MiniMax 周窗口百分比**(`current_weekly_remaining_percent`)— 沿用现有 MIN(interval) 逻辑
- **多币种 unit 细化**(deepseek 若同时有 CNY+USD,现有求和逻辑不变,仍显示 ¥)— 维持现状
- `Remaining` / `QuotaKind` 持久化 — 不落 DB,维持运行时状态

---

## 6. 测试

- 现有 balancer 测试(`minimax/balancer_test.go` / `deepseek/balancer_test.go`)只断言 `Raw` / `HasQuota` / error,**加 `Kind` 字段不破坏**
- 新断言(每个 balancer 测试补 1 行):正常分支返回的 `Kind` 与预期一致(minimax → "percent",deepseek → "currency")
- `manager_test.go`:pollAllBalancers 写 `k.QuotaKind` 的断言(现有 fake balancer 加 Kind 即可)
- 前端:无单测基建,手动验证

---

## 7. 成功标准

- Provider Keys 页:minimax key 显示 `43%`(无 ¥ 前缀);deepseek key 显示 `¥12.34`;列名"额度"
- 未轮询 key 仍显示灰色"未轮询"
- Overview 卡片:minimax 池显示 `—`,deepseek 池显示 `¥sum`
- minimax key 被误标 `billing_source: api` 时,额度列仍显示百分比(跟随上游数据,不跟随 tier)
- `go build ./...` + `go test ./...` 通过

---

## 8. 预估 diff

- backend: ~40 行改动,7 个文件(`prober.go` / `minimax/balancer.go` / `deepseek/balancer.go` / `manager.go` / `keypool/key.go` / `keypool/pool.go` / `auth/provider_keys_handler.go`),2 个测试文件补断言
- frontend: ~30 行改动,2 个文件(`ProviderKeys.vue` / `Overview.vue`)
- 无 DB 变更,无配置变更
