# Provider Quota Polling — 设计文档

**日期**: 2026-08-04
**作者**: Claude
**目标版本**: Native LLM Gateway 下一版
**代号**: P-quota-balance

---

## 0. Context & 动机

当前 Gateway 对 provider key 的"剩余余额"**只在 quota 错误发生后被动感知**:

1. 请求先打过去 → upstream 返 402 / 429-with-quota / 403-with-quota
2. `Pool.ReportError(k, "quota_exceeded")` 把 key 标 `QUOTA_EXCEEDED`
3. 后续 `AcquireFromTier` 直接过滤掉,直到 `quotacheck.Manager` worker 探测到余额恢复才标回 `ACTIVE`

两个缺陷:

- **用户体验上有 1 个失败请求** 才能发现额度没了 — 每次新启动、或配额窗口刚切换时,第一批请求都会撞死掉的 key
- **调度无信息可用**:即便我们提前探到 "key A 还剩 12 元、key B 还剩 0.5 元",也没机制把请求先导到 A

本次改动:
- 每 5 分钟主动拉一次上游余额(已知可查的 provider:deepseek / glm / minimax)
- 把余额写到 `Key.Remaining` + `Key.LastPolledAt`
- token_plan tier 的 acquire **按 Remaining 降序排序**后再 round-robin
- 余额到 0 时**自动**走现有 P68 路径标 `QUOTA_EXCEEDED`(无缝衔接,无需新转移路径)
- 前端 Provider Keys 页面展示余额 + 颜色
- 扩展点留给后续 provider — 新加一个 Balancer 文件即可,其余代码不动

---

## 1. 数据模型

### 1.1 `keypool.Key` 加 2 个 runtime 字段

```go
// P-quota-balance
type Key struct {
    // ... 既有字段 ...
    Remaining    float64    // 上次 poll 的余额(0 表示确定耗尽)
    LastPolledAt time.Time  // 上次 poll 成功时间;零值 = 还没 poll 过
}
```

**不落 DB**。重启后会丢,首次 poll 后回填。`Pool.Keys()` snapshot 已经把整个 Key 拷出来,前端直接读 `Remaining`。

### 1.2 `auth.ProviderKeyView` 加 2 个前端字段

```go
type ProviderKeyView struct {
    // ... 既有字段 ...
    Remaining    float64    `json:"remaining"`
    LastPolledAt *time.Time `json:"last_polled_at"` // 指针:零值显示 null = 未 poll
}
```

`Remaining` 单位由 `ProviderKeyView.BillingSource` 推断(CNY for deepseek / glm / minimax per today)。

### 1.3 `keypool.PoolStatus` 加 2 个聚合指标

```go
type PoolStatus struct {
    // ... 既有字段 ...
    QuotaPolledKeys int       `json:"quota_polled_keys"`     // 至少 poll 过一次的 key 数
    QuotaKnownSum   float64   `json:"quota_known_sum"`       // 最近一次 poll 的余额累加(粗略"整池可用额度")
}
```

前端 dashboard 顶部展示(后续若要画趋势图就用这条)。

---

## 2. 组件划分

```
internal/provider/
├── deepseek/balancer.go            # 已有,不动
├── glm/balancer.go                  # 已有,不动
└── minimax/balancer.go              # 新增

internal/keypool/
├── key.go                           # + Remaining / LastPolledAt
└── pool.go                          # AcquireFromTier sort by Remaining (token_plan tier)

internal/quotacheck/
└── manager.go                       # pollAllBalancers:扩到 ACTIVE keys + tier-blocked order

internal/auth/
└── provider_keys_handler.go         # ProviderKeyView + Remaining / LastPolledAt

frontend/src/views/ProviderKeys/    # 新增"余额"列 + 颜色
```

外部接口:
- `provider.Balancer` 接口保持不变
- `quotacheck.RegisterBalancer(name, b)` 接口保持不变
- 这两条**就是扩展点**:新 provider 添加只需 `provider/<x>/balancer.go` 里 `init()` 注册一次,Polling / 调度 / UI 立刻生效

---

## 3. Poll 流程(每 5 min)

### 3.1 触发

`quotacheck.Manager.Start(ctx)` 已经启动 `pollLoop`(每 `PollInterval` 跑一次);默认值由代码内的 fallback `5 * time.Minute` + `config.yaml` `keypool.quota.poll_interval: 5m` 决定。`Server.Reload` 已经能 hot-reload 这个值。

### 3.2 顺序:`pollAllBalancers` 改写为 tier-blocked

```go
func (m *Manager) pollAllBalancers(ctx context.Context) {
    for _, pool := range m.pools.Get() {
        balancer := LookupBalancer(providerName)
        if balancer == nil {
            continue // 没 Balancer 的 provider 跳过
        }
        baseURL := m.prov.EndpointFor(providerName)

        // tier-blocked order: 先 token_plan, 再 api, 最后 free
        for _, tier := range []string{"token_plan", "api", "free"} {
            for _, k := range pool.KeyPtrs() {
                // BillingSource 空字符串约定等同 "api"(与 Pool.AcquireFromTier 一致)
                effective := k.BillingSource
                if effective == "" {
                    effective = "api"
                }
                if effective != tier {
                    continue
                }
                // 不 poll DISABLED
                if k.Status == keypool.KeyStatusDisabled {
                    continue
                }
                bal, err := balancer.FetchBalance(ctx, baseURL, k)
                if err != nil {
                    m.logger.Debug("poll err", zap.String("provider", providerName),
                        zap.String("key_id", k.ID), zap.Error(err))
                    m.metricsPollInc(providerName, "transport_error")
                    continue
                }
                k.Remaining = bal.Raw
                k.LastPolledAt = time.Now()

                switch {
                case bal.HasQuota == false && k.Status == keypool.KeyStatusActive:
                    // 余额到 0 → 走现有 P68 path,worker 接着探恢复
                    m.logger.Info("poll: quota exhausted", zap.String("provider", providerName),
                        zap.String("key_id", k.ID), zap.Float64("remaining", bal.Raw))
                    pool.ReportQuotaExceeded(k) // P68 已有的公开方法
                    m.metricsPollInc(providerName, "exhausted")
                case bal.HasQuota == true && k.Status == keypool.KeyStatusQuotaExceeded:
                    // 余额回来 → 走现有 P68 path 恢复 ACTIVE
                    m.logger.Info("poll: quota restored", zap.String("provider", providerName),
                        zap.String("key_id", k.ID), zap.Float64("remaining", bal.Raw))
                    pool.RestoreQuota(k) // P68 已有的公开方法
                    m.metricsPollInc(providerName, "restored")
                default:
                    m.metricsPollInc(providerName, "ok")
                }

                // provider 速率礼貌
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

**关键点**:
- `pool.MarkQuotaExceeded(k)` 和 `pool.RestoreQuota(k)` 已有(来自 P68),**不改**
- 现有 `ProbeMaxAttempts` 等 P68 worker 触发恢复的逻辑**不动** — 一旦 poll 把 key 标 `QUOTA_EXCEEDED`,worker 会在 5 min 内(probe 间隔)再试;后续 5 min 轮询 + probe 两路并行校验
- `k.Remaining` / `k.LastPolledAt` 写入时已经持 `Pool.mu` 由 AcquireFromTier 同一把锁保证 — **不需要新加锁**

### 3.3 MiniMax Balancer

```go
// internal/provider/minimax/balancer.go
package minimax

import (
    // ...
    "github.com/wang546673478/native-llm-gateway/internal/quotacheck"
)

type minimaxBalancer struct{ client *http.Client }

func newMiniMaxBalancer() *minimaxBalancer {
    return &minimaxBalancer{client: &http.Client{Timeout: 10 * time.Second}}
}

// GET https://www.minimaxi.com/v1/token_plan/remains
// 响应:{ ... } (官方 FAQ 未给出 schema — 暂按通用解析,只取 hasQuota boolean)
func (b *minimaxBalancer) FetchBalance(ctx context.Context, _ string, k *keypool.Key) (quotacheck.Balance, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet,
        "https://www.minimaxi.com/v1/token_plan/remains", nil)
    if err != nil { return quotacheck.Balance{}, err }
    req.Header.Set("Authorization", "Bearer "+k.Key)
    req.Header.Set("Content-Type", "application/json")

    resp, err := b.client.Do(req)
    if err != nil { return quotacheck.Balance{}, err }
    defer resp.Body.Close()
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

    if resp.StatusCode == 401 || resp.StatusCode == 403 {
        // 关键:Subscription Key 才能查 pay-as-you-go 没权 — 这是 key 类型不匹配
        // 走 ResultAuthFailed → 由 caller 决定
        return quotacheck.Balance{
            HasQuota: false,
            Source:   "minimax:/v1/token_plan/remains",
        }, fmt.Errorf("minimax quota auth: %d", resp.StatusCode)
    }
    if resp.StatusCode >= 500 {
        return quotacheck.Balance{HasQuota: false,
            Source: "minimax:/v1/token_plan/remains"}, fmt.Errorf("minimax quota http %d", resp.StatusCode)
    }

    // 暂未文档化 schema — 解析尽量宽松:
    // 看是否存在 "quota_remaining" / "remains" / "balance" / "available" 字段,任一存在且 > 0 即 HasQuota=true
    var parsed map[string]json.RawMessage
    _ = json.Unmarshal(body, &parsed)
    hasQuota, raw := parseAvailableQuota(parsed)
    return quotacheck.Balance{
        Raw:      raw,
        HasQuota: hasQuota,
        Source:   "minimax:/v1/token_plan/remains",
    }, nil
}

// parseAvailableQuota 兼容多字段命名
func parseAvailableQuota(m map[string]json.RawMessage) (hasQuota bool, raw float64) {
    keys := []string{"quota_remaining", "remains", "balance", "available"}
    for _, k := range keys {
        v, ok := m[k]
        if !ok { continue }
        // 尝试解析成 number
        var f float64
        if err := json.Unmarshal(v, &f); err == nil {
            return f > 0, f
        }
        // 字符串数字也接受
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
    quotacheck.RegisterBalancer("minimax-openai", b) // 两条协议线共用 Balancer
}
```

⚠️ **MiniMax 响应 JSON schema 官方 FAQ 未文档化** — `parseAvailableQuota` 用宽松匹配先实现;等真实 schema 出来后锁定成具体字段。一旦解析有问题,fallback 到 `HasQuota=false`(保守,会让 Manager 走 transport_error metric,**不**误标 QUOTA_EXCEEDED)。

测试套件 `minimax/balancer_test.go` 用 `httptest.NewServer` 模拟 4 种响应:
- 正常有数:`{"balance": 12.34}` → HasQuota=true, Raw=12.34
- 余额为 0:`{"balance": 0}` → HasQuota=false
- 401/403 无 schema → 返 err
- 字段命名变种:`{"remains": 5.0}` / `{"quota_remaining": 5.0}` → 都能解析

---

## 4. Acquire 路径(`token_plan` tier 排序)

`internal/keypool/pool.go` 改一处:

```go
func (p *Pool) AcquireFromTier(tier string, allowedIDSet map[uint]struct{}) (*Key, error) {
    if tier == "" { tier = "api" }
    p.mu.Lock()
    defer p.mu.Unlock()
    now := time.Now()

    // 1. 恢复过期的 COOLING(原本就有)
    for _, k := range p.keys {
        if k.Status == KeyStatusCooling && now.After(k.CoolingUntil) {
            k.Status = KeyStatusActive
            k.UpdatedAt = now
        }
    }

    // 2. 收集可用 Key
    usable := make([]*Key, 0, len(p.keys))
    for _, k := range p.keys {
        if !k.IsUsable(now) { continue }
        if allowedIDSet != nil {
            id := parseKeyIDUint(k.ID)
            if _, ok := allowedIDSet[id]; !ok { continue }
        }
        usable = append(usable, k)
    }
    if len(usable) == 0 { return nil, ErrNoAvailableKey }

    // 3. P-quota-balance: token_plan tier 按 Remaining 降序稳定排序(其余 tier 不动)
    if tier == "token_plan" {
        sort.SliceStable(usable, func(i, j int) bool {
            // Remaining 高的排前;相等则保持原顺序(交给 round-robin)
            return usable[i].Remaining > usable[j].Remaining
        })
    }

    // 4. tier 桶过滤(原来就有)
    bucket := make([]*Key, 0, len(usable))
    for _, k := range usable {
        bs := k.BillingSource
        if bs == "" { bs = "api" }
        if bs == tier {
            bucket = append(bucket, k)
        }
    }
    if len(bucket) == 0 { return nil, ErrNoAvailableKey }

    return p.scheduler.Select(bucket)
}
```

**注意 sort 在 tier 过滤之前**:`usable` 里三种 tier 都可能混,排序后 step 4 才按 tier 过滤。这样排序开销只付一次但其实只对 token_plan tier 有意义 — 不影响 api/free tier(它们 `bucket` 直接按原顺序进 scheduler)。

**实测影响**:3 个 token_plan key,Remaining 分别是 12.5 / 0.3 / 8.0 → 排序后顺序 [12.5, 8.0, 0.3],scheduler 在这个有序 slice 上 round-robin → 第一次拿到 12.5(用掉部分),第二次 8.0,第三次 0.3,然后回到 12.5(假设它还领先)。

**关键不变量**:排序是稳定的(`sort.SliceStable`),相同 `Remaining` 的 key 保持原始顺序 — 这意味着 `RoundRobinScheduler` 的 counter 仍然在同级 `Remaining` 内的 key 之间轮询,而不是被 sort "破坏" 了原调度顺序。

---

## 5. API

### 5.1 `GET /api/v1/providers/:name/api-keys`(扩展)

返回的 `keys[]` 中每条加 `remaining` / `last_polled_at` 字段:

```json
{
  "keys": [
    {
      "id": 1,
      "provider_name": "deepseek",
      "name": "key-1",
      "key_masked": "sk-deeps...",
      "enabled": true,
      "status": "ACTIVE",
      "billing_source": "token_plan",
      "remaining": 12.34,
      "last_polled_at": "2026-08-04T10:30:00Z",
      "created_at": "...",
      "updated_at": "..."
    }
  ]
}
```

`last_polled_at` 是 `*time.Time` 序列化形式:有值 = ISO8601,null = 未 poll。

### 5.2 `GET /api/v1/providers`(扩展)

`PoolStatus` 加 `QuotaPolledKeys` / `QuotaKnownSum` 字段(`pools[name]` 下挂),前端 Dashboard 顶部用。

---

## 6. UI

### 6.1 Provider Keys 页面加"余额"列

```
┌────────────────────────────────────────────────────────────┐
│ Provider: deepseek  (3 keys)            [刷新] [轮询中 ●]   │
├────┬────────┬────────┬─────────────┬───────────┬─────────────┤
│ ID │ Name   │ Status │ BillingSrc  │ 余额(CNY) │ 上次轮询   │
├────┼────────┼────────┼─────────────┼───────────┼─────────────┤
│ 1  │ prod-a │ ✓ ACT  │ token_plan  │ ¥12.34    │ 2 分钟前   │
│ 2  │ dev-b  │ ✓ ACT  │ token_plan  │ ¥8.05     │ 2 分钟前   │
│ 3  │ old-c  │ ⚠ QUOTA│ token_plan  │ ¥0.00     │ 2 分钟前   │
│ 4  │ paid-1 │ ✓ ACT  │ api         │ ¥5.50     │ 2 分钟前   │
└────┴────────┴────────┴─────────────┴───────────┴─────────────┘
```

颜色规则(行内单元格底色,百分比参照"同 provider 同 tier 桶内 `Remaining` 的最大值"):
- `remaining / tier_max_remaining >= warn_threshold_pct / 100` → 绿
- `0 < remaining < warn_threshold_pct / 100 × tier_max_remaining` → 黄
- `remaining = 0` → 红
- `last_polled_at = null`(即 `*time.Time == nil`) → 灰 (unknown,"未轮询")

`warn_threshold_pct` 默认 `10`(即阈值 = 桶内最大值的 10%);`config.yaml` 加 `keypool.quota.warn_threshold_pct: 10`,可通过 `ManagerConfig.WarnThresholdPct` hot-reload。

### 6.2 Dashboard 顶卡片

Pool 列表里每行显示 `QuotaKnownSum` — 整池可用额度粗略值。点击展开看明细(转到 Provider Keys 页面)。

---

## 7. 配置

`config.example.yaml`:

```yaml
keypool:
  quota:
    enabled: true
    poll_interval: 5m              # 改:60s → 5m
    warn_threshold_pct: 10        # 新增
    probe_initial_delay: 5m
    probe_max_backoff: 30m
    probe_jitter_pct: 20
    probe_max_attempts: 8
    poll_jitter_pct: 10
    http_timeout: 10s
    user_agent: native-llm-gateway/quota-restore-1.0
```

`ManagerConfig.WarnThresholdPct` 也加同样字段,默认值 10。

`Server.Reload` 已经能 hot-reload `KeyPool.QuotaPollInterval`(看现有 commit `e84302` / `b15c1d` 等),`WarnThresholdPct` 沿用同一路径。

---

## 8. 不在本次设计范围(YAGNI)

- MiniMax 响应 JSON schema lock-in(目前宽松解析;等官方文档到位再硬编码)
- 整池可用额度历史趋势图(本期只显示聚合 Sum,不存时序)
- 按 key 维度手动 mark 余额阈值(只暴露 warn_threshold_pct 一个全局阈值)
- DashScope / Gemini / Kimi Balancer — 用户后续会给 curl,只要接口扩展点留好即可
- `Key.Remaining` 持久化(运行时不落 DB,Server 重启后第一次 poll 周期内前 5 min 会"未知")
- 自动 expire stale `LastPolledAt`(若 manager 停掉超过 5 min,UI 会一直显示旧值)— 由 next boot 第一次 poll 覆盖

---

## 9. 实施拆解(为 writing-plans 准备)

按 task 划分,每个 task 配独立单元测试:

1. **`internal/provider/minimax/balancer.go` 新增** — MiniMax Balancer + init 注册 + `balancer_test.go`(httptest 4 个 case)
2. **`internal/keypool/key.go` 改 1 处** — `Key.Remaining` + `Key.LastPolledAt`
3. **`internal/keypool/pool.go` 改 1 处** — `AcquireFromTier` 在 token_plan 分支排序
4. **`internal/keypool/pool.go` `PoolStatus` 扩展** — `QuotaPolledKeys` / `QuotaKnownSum`(每次 Status() 计算)
5. **`internal/quotacheck/manager.go` 改 1 处** — `pollAllBalancers` 改 tier-blocked,余额写入 + 状态转换
6. **`internal/auth/provider_keys_handler.go` 改 1 处** — `ProviderKeyView` + `last` 时填入 `Remaining` / `LastPolledAt`(指针)
7. **`internal/api/http/handler/admin.go` 改 1 处** — `listProviders` 返 `pools[*].QuotaPolledKeys` / `QuotaKnownSum`
8. **`config.example.yaml` 改** — `poll_interval: 5m` + `warn_threshold_pct: 10`
9. **`internal/config/config.go` 改** — `KeyPool.QuotaWarnThresholdPct` 字段 + 解析
10. **`internal/server/server.go` 改** — `Reload` 把 `WarnThresholdPct` 透传给 `ManagerConfig`
11. **前端 `views/ProviderKeys`** — 加"余额"列 + 颜色(n-data-table 增加 column)
12. **前端 `stores` 或 `api.ts`** — 调整返回类型适配新字段
13. **接入点 `App.vue` 菜单** — 不需要新菜单(已在 Provider Keys 页)
14. **手动 E2E** — 启 gateway,创建 3 把 deepseek key(其中 1 把余额为 0),观察 5 min 后 UI 显示 + acquire 顺序

预估 diff:
- backend: ~250 行新增,~30 行改动,7 个文件
- frontend: ~80 行新增,~20 行改动
- tests: ~150 行新增

---

## 10. 关键风险与缓解

| 风险 | 缓解 |
|---|---|
| 5 min × 多 provider × 多 key 大量并发 HTTP → 触发上游 RATE_LIMIT | 每把 key 间 `time.Sleep(1 * time.Second)`;受 `config.yaml.jitter` 随机化;total 单轮 ≤ 60s 完全跑完 |
| MiniMax JSON schema 不确定 → 解析失败 | 宽松解析 fallback 到 `HasQuota=false`(保守);不误标 EXHAUSTED |
| token_plan 排序后,**always-pick-first** 模式导致前 N 把 key 磨损严重 | Remaining 是**动态**的,head key 用过一轮之后 Remaining 下降,下次排序它可能不再是头;且 round-robin 在已经排序的 slice 上跑,不是 strict take-first |
| Server 重启后 5 min 内首次 acquire,所有 key Remaining=0 | sort descending 后 Remaining=0 全在末尾,等于回退到原 round-robin 行为(没有"首个 key 永远跌底"的退化) |
| `LastPolledAt` 写入与 AcquireFromTier 读取竞态 | 都通过 `Pool.mu` 串行化 — `AcquireFromTier` 已 `p.mu.Lock()`,poll 写 `k.Remaining` 在 `pollAllBalancers` 中,后者也在 `KeyPtrs` 返回的指针上写 — 注意:`KeyPtrs` 返回的是指针的副本 slice,**底层 key 仍由 Pool.mu 保护**;`pollAllBalancers` 当前**未拿 Pool.mu**(设计上为了不阻塞 acquire 路径),所以写入 `k.Remaining` 不持锁。这有 race 风险 ↓ |
| `Key.Remaining` race:poller 写入与 AcquireFromTier 读取竞态 | `Remaining` 是 float64,x86-64 / arm64 平台 64-bit 字读写 **torn-write-free**(Go runtime 文档明确);读取方拿到一个陈旧值或新值,不会撕裂;最坏 case 排序结果短暂不完美,下次 poll 修复。**生产前提**:Server 跑在 64-bit Linux(规格书前提) |

⚠️ **poller 写 `Remaining` 不持锁** — 但因为是 64-bit 平台 + int 单字段写,不会撕裂;实现里要在 `pool.AcqurieFromTier` 排序列加一行注释"Remaining 值可能稍微陈旧(最多一个 pollInterval),无害"。

---

## 11. 成功标准

- 启 gateway + 配 deepseek 3 把 key 后 5 min 内,UI 上 3 把 key 的"余额"列都有数字
- 余额到 0 的 key 自动标 QUOTA_EXCEEDED(orange),后续 30 秒内该 key 不再被 acquire(等 worker 探测恢复)
- `ProviderKeyView.last_polled_at` 5 min 后肯定非 null
- 配置 `keypool.quota.poll_interval: 1s`(测试用)后,UI 在 2 秒内显示新余额(无错乱)
- `gateway_acquire_sorted_by_remaining_total` Prometheus 指标 = 排序触发的次数(可选,用以验证排序路径在跑)
- 文档里"扩展点"描述:新加 provider 时,implementer 只需要写 1 个 `balancer.go` + 注册,polling / 调度 / UI 自动覆盖

---

## 12. 与现有模块的关系

| 现有模块 | 影响 |
|---|---|
| `proxy.Engine` | 不动 |
| `router` | 不动 |
| `auth.CheckAllowed` | 不动 |
| `usage` | 不动 |
| `database.models` | **不动**(`Remaining` / `LastPolledAt` 不落 DB) |
| `server.Reload` | 加 1 个字段 `WarnThresholdPct` |
| `quotacheck.Manager` | `pollAllBalancers` 行为变了(`ACTIVE` 也 poll + tier-blocked) |
| `keypool.Pool` | `AcquireFromTier` 加 sort + `PoolStatus` 加 2 字段 |
| `keypool.Key` | 加 2 字段 |
| `auth.ProviderKeyView` | 加 2 字段 |
| `provider.Balancer` / `RegisterBalancer` | **零改动** — 这就是扩展点 |
| 前端 ProviderKeys.vue | 改列结构 |

数据库 schema 不变(纯 runtime 状态)。其他模块零侵入。
