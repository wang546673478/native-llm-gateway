# Key 调度树状模型:天然候选 + 分层 + 顺序黏性切换(设计)

日期:2026-08-10
状态:已确认(用户逐点拍板)
关联:这篇是 [tier 降级设计](2026-08-06-tier-failover-design.md) 的**演进**,沿用分层/降档骨架,重定义多 provider 多 key 的候选与选取策略。

## 0. TL;DR

把调度定义为一棵树(Level 0-3),全部**天然、零配置**,仅两层可改写:

```
Level 0 候选名单  = 所有有可用 key 的 provider,天然自动进链,**UI 不展示**
Level 1 分层      = key 的 billing_source → token_plan / api 付费层(不可手改,**UI 展示为两大根节点**)
Level 2 层内 provider 排序 = 默认按最早 key 加入时间；可改写覆盖
Level 3 provider 内 key 排序 = 默认按加入时间，先加先用、用完才切；可改写覆盖
```

- **天然**:加 provider = 加一把 key 即自动进链;加 key = 自动排到队尾;不写 config 候选列表。
- **分层**:每把 key 贴 `billing_source` 标签决定归属,一层内聚合出 provider。
- **顺序黏性(Level 3 引擎)**:当前 key 用完/故障才触发推进,推进从头顺序取第一个可用;恢复(探测/COOLING/熔断 CLOSED)也触发推进,让高位 key 自动回位。
- **可改写**:Level 2/Level 3 优先级默认按时间,网页(拖拽/改数字)可覆盖。

## 1. 背景:从「轮换 + 手写候选列表」到「树状天然排序」

### 1.1 现状(2026-08-06 决策)的取舍

当前 `acquireFromTierLocked`(keypool/pool.go)在同一 tier 桶内:

```
1. 过滤可用(IsUsable / IsPolledAndExhausted / filterBreakers)
2. token_plan 桶按 Remaining 降序稳定排序
3. scheduler.Select —— key_rotation: round_robin 轮流
```

这套在**把同一套餐的额度快速摊平、避免单 key 触限/被限流**上有优势:多把 key 并行消耗,单把 key 不会瞬间撞顶。

### 1.2 现实痛点(本方案要解决的)

实测(minimax weige / key-1):round_robin 会把请求在 weige、key-1 之间来回播,**两把 key 的余额一起往下掉**,没有「先集中烧完某一把」的效果。用户希望:

- **集中烧完一把再用下一把**(把某个套餐额度尽快用光、或让某把 key 优先被消耗);
- **新加 key 不需要配任何字段**,天然排在队尾参与;
- **用完的 key 恢复后自动回最优先位**,不靠人工/配置干预。

### 1.3 这是对 2026-08-06「不新增 key 规则」决策的显式偏离

2026-08-06 设计文档决策 7 明确「**key 生命周期不新增规则、维持 round-robin**」。本文是**用户新的明确诉求**,决定**改变 pool 内选取策略**。因此:

- **保留**:分层降档骨架(token_plan→api→free)、额度探测/COOLING/熔断的恢复能力。
- **改动**:① 池内「挑哪把 key」从轮换 → 顺序黏性(§3);② **候选名单天然化**——不再依赖显式 `catch_all.providers[]` 列表,所有有可用 key 的 provider 自动进链;③ **删掉 `catch_all.providers[].model` 冗余**(由 gateway key 白名单 / provider default_model 承担,消除 routing.model 双语义踩坑根源)。

> ⚠️ 这是行为变更,不是死代码清理。落地要同步改 `docs/config-reference.md`(key_rotation 说明 + catch_all 结构)、2026-08-06 设计文档对应段落,避免文档与行为漂移。删 model 字段涉及 3 份 config 模板同步。

### 1.4 树状排序模型(Level 0-3,本方案的全貌)

用户把整个调度明确为**一棵树**,全部**天然、零配置**,只有两层优先级可改写:

```
Level 0  候选名单    = 所有「有可用 key 的 provider」,天然自动加入,**UI 不展示**
Level 1  分层        = key 的 billing_source 标签 → token_plan / api 付费层(不可手改,**UI 展示为两大根节点**)
Level 2  层内 provider 排序 = 默认按「该 provider 最早加入 key 的时间」；【可改写】
Level 3  provider 内 key 排序 = 默认按「加入时间」，先加先用、用完才切下一把；【可改写】
```

- **Level 0**(候选名单):有可用 key 的 provider 自动在链,不加 config、不手动维护候选列表。加 provider = 加一把 key 即自动进链;**不依赖显式 `catch_all.providers[]`**。
- **Level 1**(分层):按 `provider_api_keys.billing_source` 把 key 分到 token_plan / api 两层;provider 归属由它 key 的标签决定(一个 provider 可跨层,如 mimo 在两层都有 key)。层序固定 token_plan → api → free(分层不变式,不可手改)。
- **Level 2**(层内 provider 排序):默认 = 该 provider 最早加入那把 key 的**加入时间**(先来的 provider 永远优先,即使它后来又加了几把新 key);**可改写覆盖**。
- **Level 3**(provider 内 key 排序):默认 = 加入时间,先加先用;**用完才切下一把**(这正是 §3 的顺序黏性状态机的顺序来源);**可改写覆盖**。

- **优先度改写(Level 2 / Level 3):**
- **默认**:全按加入时间,零配置,天然排序。
- **改写**:网页编辑器(Routing 页)用**拖拽或改数字**调整 Level 2(层内 provider 顺序)与 Level 3(provider 内 key 顺序);不手动改写时,系统用加入时间兜底。改写的 UI 形态(拖拽 vs 数字输入)可另行讨论,不影响底层排序模型。
- **展示边界**:仅 Level 0 不展示(它是隐式聚合);**Level 1(token_plan / api 付费层)作为 UI 的两大根节点展示**,下面展开到 Level 2 provider → Level 3 key。Level 1 的**层归属**由 billing_source 决定、不可手改,但它的**存在**(两层)是要展示的。

**与现有实现的关系:**
- 「加入时间」来源 = `provider_api_keys.created_at`(现有列,零迁移)。
- Level 2 排序生效点:候选链构造(router `buildKeyCandidates` / `routeCatchAllAuto`);Level 3 排序生效点在 keypool `acquireFromTierLocked`。
- 这个树状模型**替代**此前的「显式 catch_all.providers 列表 + 手动编辑」思路(见 §5 改动范围),候选名单天然化 + 去 model 冗余一并纳入。

## 2. 目标状态机

> 本章(§2-§3)是 §1.4 树状模型的 **Level 3(provider 内 key 排序)** 的运行时引擎。Level 0/1/2 见 §1.4 与 §5。

### 2.1 顺序定义

- 池内 key 按 **Level 3 顺序**:默认「加入时间」(provider_api_keys.created_at)或用改写后的优先级;先加先用,下标最小的叫 `A`,其次 `B`、`C` …。
- 顺序**固定**(一次热重载/启动内不变;重载重建 pool 时按新读到的顺序重排)。
- 默认不加字段(用 `created_at`)、可改写时加一个顺序字段(见 §5)。想让某把先被烧 → 网页改写或调 created_at。

### 2.2 状态与约束

- 每个 key 沿用现有 `KeyStatus`:`ACTIVE / LIMITED / COOLING / QUOTA_EXCEEDED` + 熔断状态 + polled/remaining。
- **不变式**:一次请求最多命中 ONE 把 key(选中即锁定);客户端看到的「当前 key」是调度决定,不暴露内部状态。
- 私有状态:`Pool` 持 `currentIdx`(当前在用 key 的下标)。此状态**不外泄**——router/proxy/管理 API/前端都看不到,仍只经 `AcquireFromTier` 拿 `*Key`。

### 2.3 迁移表(用户拍板的精确版本)

| 触发前状态 | 推进后选谁 |
|---|---|
| A可用-B可用-C可用 | **用 A**(第一把可用) |
| A额度耗尽-B可用-C可用 | **用 B** |
| A额度耗尽-B额度耗尽-C可用 | **用 C** |
| A可用-B额度耗尽-C可用(触发推进后) | **用 A**(A 恢复了,回到最优先) |
| A/B/C 全不可用 | **降档到 api 付费层**(见 §4) |

### 2.4 触发源(「状态翻转」即触发一次推进)

推进 `advance()` = 从下标 0 开始顺序取第一个「当前可用」的 key 作为 currentIdx。

触发推进的事件(任一):

| 事件 | 触发点 | 恢复后回位靠的机制 |
|---|---|---|
| **额度耗尽** | 请求 2056/额度码 → `ReportQuotaExceeded`;或 poll 连续读到余额耗光 → QE | 额度探测 probe 探到回归 → QE→ACTIVE → **触发推进,回 A** |
| **429 重试** 10 次(限流) | 当前 key 纯 429,同 key 重试 10 次仍 429 | COOLING 到期 → 反转 → **触发推进,回 A** |
| **熔断 OPEN**(连续 5xx/超时) | `breaker.RecordFailure` 计数超阈值 → OPEN | 熔断 OpenTimeout 后自动 CLOSED(HALF_OPEN 试探)→ **触发推进,回 A** |

> 纯 429 的判定与现有 `ClassifyErrorWithBody` 一致:`429 + 无额度 body` = 限流(走重试/COOLING),`429 + 额度 body` 或 `2056/1008` = 额度耗尽(走 QE)。

## 3. 关键设计决策

| # | 决策 | 理由 |
|---|---|---|
| 1 | **默认不加字段**,顺序 = `provider_api_keys.created_at`(加入时间) | 响应「新 key 零配置」;默认天然排序,热重载重排;改写才写 `route_order`(实体表零改动) |
| 2 | **currentIdx 私有在 Pool 内**,不外泄 | 守住低耦合:router/proxy/前端对 pool 的调用接口不变;状态单一真相,无跨包碎片 |
| 3 | **COOLING 保留,作 429 的恢复时钟** | 429×10 后需冷却期反转状态,让状态机回 A;不是轮换时代的「全局冷却」,而是「单 key 恢复触发」 |
| 4 | **熔断保留,作非额度故障的恢复时钟** | 5xx/超时这类「无额度差异的错误」只有熔断的 OpenTimeout→CLOSED 能提供「自动回 A」的路径;不砍(砍了因故障切走的 key 没有回家时钟) |
| 5 | **429 重试次数提到 10** | 减少瞬时限流的误切换;10 次还限流才走 COOLING → 推进 |
| 6 | **推进 = 从 A 顺序取第一个可用** | 保证 A 恢复必回位;B 劣后;天然实现「用完的 key 自动回最优先」 |
| 7 | token_plan 全不可用 → **降档 api**(现状已如此) | 分层降档不变式;不重复实现 |
| 8 | **候选名单天然化(Level 0)**,删显式 `catch_all.providers[]` 的 model | 所有有可用 key 的 provider 自动进链,加 provider 零 config;model 冗余交给白名单 / default_model |

### 3.1 改写持久化(方案 B:`route_order` 排序表)

Level 2/Level 3 的**用户改写**不落实体表,单独一张 `route_order` 排序表记录;**默认顺序(无改写)始终由 `created_at` 派生**。这样排序逻辑只有一个真相:改写 → `route_order` 叠加,未改写 → created_at。

**表结构(GORM 模型,AutoMigrate 自建):**

```go
type RouteOrder struct {
    ID         uint      `gorm:"primaryKey"`
    Scope      string    // "provider" | "key"
    Provider   string    // provider 名(Level 2)/ key 所属 provider(Level 3)
    Name       string    // provider 名(Scope=provider)或 key 名(Scope=key);空 = 整 provider 兜底
    // 层内 / provider 内的展示顺序(小在前);仅存「改写」的相对位次
    Seq        int
    BillingSource string  // 可选:按层隔离顺序(token_plan/api 各自一段)
    UpdatedAt  time.Time `gorm:"autoUpdateTime"`
}
func (RouteOrder) TableName() string { return "route_order" }
```

**写入:**`PUT /api/v1/routing/order` 接收某层/某 provider 内排好序的目标列表 → upsert 到 `route_order`。

**读取/生效:**路由构造候选时,先取 `route_order` 里该作用域的改写,再与 `created_at` 合成最终顺序:

```
有效顺序 = 有改写(route_order 有该目标的 Seq)? 按 Seq:按 created_at
```

**撤销/重置:**
- 删掉 `route_order` 里对应作用域的行 → 回到默认 `created_at` 顺序,天然支持「恢复默认」。
- 不改写 = 该作用域在 route_order 无行,零开销。

**为什么选方案 B(排序表)而不是实体列:**
- 排序「真相」单一:默认值(created_at)与改写(route_order)分离,不混在一个可空列里(避开 routing.model 那种「一个字段两种语义」的踩坑)。
- 撤销自然:删行即重置,不污染实体表结构。
- Level 2 与 Level 3 共用一张表(Scope 区分),不用给两个实体各加一列。
- 默认零迁移;只有用户真的改写才动 route_order。

**热重载:**改 order 后,沿用现有 provider/key CRUD 的 reload 通道(ReloadProviderPool 同模式)让路由重建时读到新顺序。

## 4. 与 tier 降档的关系(不冲突)

- 本文 §3 只改「**同一个 pool(同一 provider)内选哪把 key**」(Level 3 的运行时引擎)。
- `token_plan 层 → api 付费层` 的降档仍是 `router.buildKeyCandidates` + `RouteIterator.Next` 干的;分层由 key 的 billing_source(Level 1)。token_plan 层**所有 key 都不可用**时才落 api,与现状一致。
- 候选名单天然化(Level 0)后,catch_all 自动模式/显式列表的选择:所有有可用 key 的 provider 自动进链,不再手动维护候选列表;层内 provider 顺序由 Level 2(加入时间 / 改写)决定,替代「显式 catch_all.providers 列表 + model」。
- api 付费层、free 层的同 provider 多 key,复用同一套顺序黏性(同构)。

## 5. 落地改动范围(按树状模型,分三层)

| 文件 | 改动 |
|---|---|
| `backend/internal/keypool/pool.go` | **Level 3**:+私有 `currentIdx`;`AcquireFromTier` 内「挑 key」从轮换→顺序黏性(按加入时间/改写顺序);`advance()` 从下标 0 取第一可用;QE/COOLING/熔断回调接进 `advance()`;熔断 OPEN→CLOSED 触发 `advance()` |
| 请求路径(proxy 或 keypool)| **Level 3**:纯 429 同 key 重试 10 次;10 次仍 429 → 标 COOLING → 推进 |
| `backend/internal/router` | **Level 0+2**:候选名单天然化(有可用 key 的 provider 自动进链);层内 provider 排序按「最早 key 的加入时间 / 改写优先级」;从候选构造去掉对显式 `providers[].model` 的依赖(模型交白名单 / default_model) |
| `backend/internal/config` + 3 config 模板 | **Level 0+2**:删 `catch_all.providers[].model` 冗余;`key_rotation` 说明改「顺序黏性 / round_robin / least_used / random 可选」;候选名单天然化兜底 |
| `backend/internal/database` + models | **Level 2/3 改写持久化(方案 B:排序表)**:新增 `RouteOrder` 模型(表 `route_order`),GORM AutoMigrate 建表;实体表(`providers`/`provider_api_keys`)**不加列** |
| `backend/internal/api/http/handler` | **Level 2/3 改写端点**:`PUT /api/v1/routing/order`(写 Level 2 provider 顺序 / Level 3 key 顺序)→ 写入 `route_order` + 热重载 |
| `frontend` Routing 页 | 树状图展示:**Level 1 两层根节点(token_plan / api 付费层) → Level 2 provider → Level 3 key**;Level 0 不展示;Level 2/3 拖拽或改数字编辑 |
| 2026-08-06 tier-failover + config-reference docs | §决策7 加注顺序黏性;catch_all 结构 / key_rotation 说明同步 |
| 测试 | §1.4 树模型:Level 2 加入时间排序 / 改写覆盖;Level 3 迁移表 5 行 + 触发源;候选天然化;删 model 后白名单模型仍正确 |

**不做的事:**
- 不改 Level 1 分层(token_plan→api→free 硬性、由 billing_source 决定),不改额度探测/COOLING/熔断机制本身(只复用「状态翻转」作推进触发)。
- 不改 `AcquireFromTier` 等接口的使用方契约(router/proxy 对 pool 的调用签名不变;p-key 顺序字段可选,默认零迁移)——不跨契约,低耦合红旗不触发。

## 6. 单点风险与 mitigation(诚实披露)

| 风险 | 说明 | 缓解 |
|---|---|---|
| 单 key 承载全部流量 | 「黏住 A」= 活跃流量全压 A;A 可能更快触 5h 上限/限流 | 429×10 + COOLING + 熔断会在 A「不行」时自动推到 B;A 恢复回位。这是刻意取舍,低并发套餐场景可接受 |
| 候选天然化失去部分 provider | Level 0 天然进链后,无法用「不列入路由」排除某个 provider(只能靠白名单模型间接排除) | 若需显式排除的 provider,用 gateway key 白名单模型控制,或将「排除」设为 Level 0 的可选改写(占位,暂不实现) |
| Level 2/3 改写持久化边界 | 改写的优先级要 persist 且热生效 → 落在 `route_order` 表 + 热重载 | 与既有 ProviderKeys CRUD → ReloadProviderPool 同模式;默认(不改写)走 created_at,零迁移;撤销 = 删 route_order 行 |

## 7. 验证

1. `cd backend && go build ./... && go vet ./...` 全绿。
2. keypool 单测覆盖迁移表 5 行:
   - A 可用 → 用 A
   - A 耗尽 → 用 B
   - A、B 耗尽 → 用 C
   - 推进时 A 已恢复 → 用 A
   - 全耗尽 → ErrNoAvailableKey / 走降档
3. 触发源单测:QE、429×10→COOLING、熔断 OPEN→CLOSED 各自触发 `advance()` 且回到正确 key。
4. **持久化单测(route_order)**:
   - 无改写 → 排序 = created_at(backward compat)
   - 改写某作用域 Seq → 排序按 Seq 覆盖 created_at
   - 删除 route_order 行 → 回到默认 created_at
   - Level 2 与 Level 3 同表 Scope 隔离互不干扰
   - 热重载后读到新顺序
5. **前端/路由**:Level 1 两层根节点展示;Level 2/3 编辑 → PUT 落库 → 刷新不丢。
6. `make test` 28 包 0 fail。
