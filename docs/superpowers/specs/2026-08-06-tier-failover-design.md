# tier 降档语义改造:额度驱动分层 failover

日期:2026-08-06
状态:已确认(用户逐点拍板)

## 1. 背景:事故复盘

2026-08-06 17:44:15 一次请求(minimax key-1)connection 挂死 90 秒(timeout 90s),failover 直接落到 api 付费层 deepseek(deepseek-v4-flash)承接,105 秒完成。两个问题暴露:

1. **healthy key 空转**:minimax 有两把 key(id=7 key-1 / id=8 weige),key-1 失败后请求内不尝试 weige——候选链是 (provider, tier) 粒度,一个 provider 只有一个位置,一次 acquire,失败即推进(router.go:338 RouteIterator)。不是 key 状态绑定 provider(熔断已 per-key,pool.go:107),是「请求内无同 provider 换 key 重试」的路径。
2. **tier 语义错误驱动而非额度驱动**:token_plan 层额度充足(17:45:43 的下一条请求 minimax 成功),但 connection 失败被当作降档理由直接跳 api 层。失败一律简单推进,没有「层内优先」概念——即使未来 token_plan 层有多个 provider,一个 provider 网络故障会废掉整个层。

## 2. 用户决策(2026-08-06 确认)

| # | 决策 |
|---|---|
| 1 | **最高原则(不变式)**:token_plan 层额度未耗尽 → 请求绝不落到 api 付费层。此优先级最大,压过「请求成功率优先」。 |
| 2 | **额度判定三源**:轮询(quotacheck 1m 标记「已轮询且耗尽」)+ 错误码(402 / 429+quota body / base_resp 1008·2056)+ 主动查询(网络类错误时调余额接口确认)。 |
| 3 | **主动查询必须是统一基础方法**:复用 `quotacheck.RegisterBalancer` 注册表(各厂商包已实现 `FetchBalance`),请求路径经统一入口查;厂商包不各自写调用。provider-vendor skill 只需补一句「balancer 会被请求路径主动查询复用」。 |
| 4 | **主动查询失败 → 按未耗尽处理**:继续换同层「还有额度」的 key(轮询数据未标记耗尽的优先)。拿不到耗尽证据就不降档。 |
| 5 | **网络类错误(connection/timeout/5xx)**:provider 内换 key 重试 → 同层换 provider → 全层穷尽 = **请求失败返回,不降档**。 |
| 6 | **额度类错误**:标记 key → 同层换有额度的 key/provider → 全层额度穷尽 = 降档下一 tier。 |
| 7 | **key 生命周期不新增规则**:现有熔断(计数阈值 5 → OPEN 30s → HALF_OPEN 试探)已覆盖「失败 key 的退出与恢复」;瞬时故障 key 下个请求自动再试(自愈),持续故障自然淘汰。connection 不设 COOLING,只有 429 走 COOLING 60s。 |
| 8 | **配套**:minimax `timeout: 90s → 30s`(否则换 key 重试会分钟级累积)。 |
| 9 | **api 层同规则**:网络类层内穷尽 → 失败;api 层无套餐额度概念,probe 模式不动。free 层(现无 provider)规则同构,不特殊处理。 |
| 10 | **为未来预留**:现在 token_plan 层只有 minimax,「换同层 provider」路径暂时走不到(kimi 等进来后自动生效),但语义与测试按完整层写。 |

## 3. 错误分类

| 错误类型 | 分类 | 处理 |
|---|---|---|
| connection / timeout / server_error(5xx) | 网络类 | 层内解决:换 key → 换同层 provider;穷尽 → 失败返回 |
| quota_exceeded / 429(套餐耗尽)/ base_resp 1008·2056 | 额度类 | 标记 key → 同层换有额度的候选;全层额度穷尽 → 降档 |
| auth(403) | key 类 | 换 key(现 IsRetryable 已允许);穷尽 → 失败返回(不降档——所有 key 都坏,去 api 层一样坏) |
| invalid_request / model_not_found / client_disconnected | 不可重试 | 直接失败,不重试不降档(现有语义,不动) |

## 4. 行为矩阵(层内失败处理,以 token_plan 层为例)

| 场景 | 行为 |
|---|---|
| key 网络类失败,provider 还有 healthy key | 换 key 重试(每个 key 一次机会) |
| provider 全部 key 网络类失败 | **主动查询**(统一方法)→ 有额度 → 留在层内 |
| 同层还有 provider | 换同层「有额度」的 provider 重试 |
| key 额度类失败 | 标记该 key → 同层换有额度的 key/provider |
| 同层全部穷尽,期间有额度证据 | 降档到下一 tier(api) |
| 同层全部穷尽,全是网络类/查询失败 | 请求失败返回,不降档(不变式) |

## 5. 架构设计

### 5.1 统一主动查询方法(基础方法)

新增统一入口,放 `quotacheck` 包(注册表所在地):

```
CheckQuota(ctx, providerName, key) (hasQuota bool, err error)
```

- 内部查该 provider 注册的 balancer(`FetchBalance`);未注册 → 返回 `(true, nil)`(未知 = 未耗尽,与决策 4 一致)
- token_plan 厂商必有 balancer(provider-vendor skill 概念速记已有此要求),所以主动查询对 token_plan 层总是可用
- 触发时机:仅网络类错误后(provider 全 key 网络失败时),不在请求路径上无条件查,不加每请求延迟

### 5.2 候选链改造(tier-aware failover)

- `RouteIterator.Next()` 返回候选时携带 tier 信息(KeyCandidate 已有 Tier 字段,只需暴露)
- proxy 候选循环按错误分类路由:
  - 网络类 → 同 provider 换 key(见 5.3)→ 仍失败 → 同 tier 下一个候选(iterator 提供「下一个同 tier 候选」能力)→ 同 tier 穷尽 → 失败返回
  - 额度类 → 标记 → 同 tier 下一个有额度候选 → 同 tier 穷尽 → 降档(tier 推进)
- iterator 需要新增能力:跳过 tier 推进(层内迭代),以及「同 tier 是否还有候选」的查询——实现为 Next 返回候选时带 `Tier`,proxy 记录当前 tier,失败后要求 iterator 只在同 tier 内找下一个;层内穷尽后由 proxy 根据失败分类决定降档或终止
- 降档入口保留现有 tier 顺序(token_plan → api → free)

### 5.3 同 provider 换 key 重试

- proxy 循环内:候选失败(网络类)且 provider 有 pool → 重新 acquire 一把**排除刚失败 key 的** healthy key(熔断未 OPEN、COOLING 未过期的都算 healthy)→ 换 `req.Key` 后重发
- 排除方式:keypool 新增 `AcquireFromTierExcluding(tier, excludeID, proto)`(或等价)——避免轮询又选回同一把
- 每 key 一次机会,provider 内重试上限 = 该 provider healthy key 数(自然终止)
- 429 冷却语义保持:换 key 重发是路由层显式重新 acquire,不是 provider 内部二次 acquire(踩坑 #15 的教训:冷却要标到真正发请求的 key 上——新 key 失败时 reportKeyError 标的就是新 key)

### 5.4 降档判定(层额度穷尽)

token_plan 层「额度用完」的判定 = 三源中的任一给出证据:
- 轮询已标记该层所有 key「已轮询且耗尽」(IsPolledAndExhausted 全部)
- 请求中所有候选都收到额度类错误(错误码)
- 主动查询确认无余额

层内尝试过程中只要出现过额度类证据,层穷尽 → 降档;否则 → 失败返回。

## 6. 改动文件清单

| 文件 | 改动 |
|---|---|
| `backend/internal/quotacheck/` | 新增 `CheckQuota` 统一方法(查 RegisterBalancer 注册表) |
| `backend/internal/keypool/pool.go` | 新增排除指定 key 的 acquire(5.3) |
| `backend/internal/router/router.go` | RouteIterator 暴露 tier、支持同 tier 内迭代 |
| `backend/internal/proxy/proxy.go` | 候选循环错误分类路由:网络类层内解决 / 额度类降档 / 不可重试直接失败 |
| `backend/config.yaml` | minimax timeout 90s → 30s |
| `.claude/skills/provider-vendor/SKILL.md` + `docs/provider厂商定制包指南.md` | 补一句:balancer 会被请求路径主动查询复用(不做其他改动) |
| `docs/踩坑与排错.md` | 新坑:网络类错误不降档,层内穷尽失败返回(不变式) |
| 测试 | proxy 集成测试(错误序列:网络类穷尽 → 失败不降档;额度类 → 降档;换 key 重试成功);keypool/router 单测 |

不做的事:
- 不改熔断器机制(决策 7)
- 不动 quotacheck 轮询
- 不新增 key 状态(无终端禁用状态保持)
- free 层不特殊处理(决策 9)

## 7. 验证方式

1. `cd backend && go build ./... && go test ./...` 全绿
2. proxy 集成测试覆盖行为矩阵 6 行:网络类穷尽失败 / 额度类降档 / 换 key 重试 / 查询失败按未耗尽 / 同层换 provider / 全层额度穷尽降档
3. 手工验证(可选):模拟 minimax 连接故障,确认请求层内重试 weige 成功、不落 api 层
