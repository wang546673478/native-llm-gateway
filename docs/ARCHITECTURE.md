# 架构总览(ARCHITECTURE)

> 一份「包+职责+边界」的图谱,新人 30 分钟看完能上手改代码。
>
> **本文件是代码现状的镜像**:与 `Native LLM Gateway — 完整实现规格书 v2.md` 不一致时,**代码优先**(规格书顶部已说明)。

---

## 1. 进程与生命周期

### 1.1 单进程

```
cmd/gateway/main.go
   ↓
cobra 命令行(--config / --log-json)
   ↓
config.Load(cfgPath) → *config.Config
   ↓
logger(zap;development 模式有颜色,production JSON)
   ↓
database.Open + database.Migrate(GORM AutoMigrate — 唯一 schema 权威;已删 SQL 迁移)
   ↓
provider.Default() (Registry) + provider.NewManager + manager.LoadFromConfig
   ↓
relay.LoadFromDatabase(db, manager)   ← 中转站动态注册(读 relay_stations 表)
   ↓
server.New(cfg, logger, db, manager)
   ↓   ↑ 这里构造:Provider Key Pool 从 DB(buildKeyPools+buildOnePool 读 provider_api_keys,
   |      SetPool 注入)—— main.go 不再 buildPools(config key 已废,P30 DB 唯一权威);
   |      router / proxy / usage / metrics / accesslog / quotacheck
config.Watch(ctx, cfgPath, fn(srv.Reload))         ← 热重载监听
   ↓
srv.Run(ctx)                                       ← 注册路由 + 启动 3 个后台协程
```

后台 3 个协程:

| 协程 | 谁启动 | 干什么 |
|---|---|---|
| `usageC.Start(ctx)` | `server.Run` | 异步收集 usage_records,批量落库 |
| `accessR.Start(ctx)` | `server.Run` | access log buffer 周期 flush + 30 天保留清理 |
| `quotaM.Start(ctx)` | `server.Run` | 余额轮询 + probe 探测调度 |

### 1.2 优雅关停(SIGTERM/SIGINT)

```
signal.NotifyContext 收到信号
   ↓
s.usageC.Stop()       ← flush 剩余 records
s.accessR.Close()     ← flush buffer + 停止 retention
s.shutdown()          ← http.Server.Shutdown 排空在飞请求
s.saveKeyStateSnapshot() ← 把 pool 的 QUOTA_EXCEEDED / COOLING / 余额写到 key-state.json
```

---

## 2. 包依赖图(简化)

```
                 ┌─ router ───────────────┐
                 │                         │
  cmd/gateway ───┼─ server ─── proxy ──────┼── keypool ─── circuit
                 │           │              │     │
                 │           │              │     └─ quotacheck (callback)
                 │           │              │
                 │           └─ provider ───┼── openai_compatible / anthropic_compatible / google
                 │              │           │
                 │              │           ├─ 厂商包:deepseek / minimax / mimo(2026-08-20 下线 glm/qwen/gemini)
                 │              │           │
                 │              │           └─ relay(中转站动态注册,2026-08-22)
                 │              │
                 ├─ accesslog ──┤
                 ├─ metrics ─────┤
                 ├─ auth ────────┤
                 ├─ usage ───────┤
                 ├─ config ──────┤
                 ├─ database ────┤
                 └─ api/http/ ───┘
```

依赖方向:**自上而下**,无环。

**关键不变量**:
- `keypool` 不 import `provider`(避免 cycle,通过 `providerLookup` 窄接口拿 endpoint)
- `provider` 包不 import `keypool` 的内部,只通过 `keypool.NewPool` 注入

---

## 3. 核心数据结构

### 3.1 `keypool.Key` + `keypool.Pool`

```go
// Key 一把上游 API key
type Key struct {
    ID            string         // 数字字符串(对应 DB provider_api_keys.id)
    ProviderName  string
    Name          string         // 用户起的名字,UI 展示用
    Key           string         // 明文(运行时),落库时加密
    Status        KeyStatus      // ACTIVE / COOLING / QUOTA_EXCEEDED
    CoolingUntil  time.Time
    BillingSource string         // token_plan / api / free
    Protocols     string         // 逗号分隔,空 = 全部
    Remaining     float64        // 余额(percent 或 currency)
    LastPolledAt  time.Time
}

// Pool 一个 Provider 的 key 池
type Pool struct {
    ProviderName string
    keys         []*Key
    scheduler    Scheduler       // RoundRobin / LeastUsed / Random
    breakers     map[string]*circuit.Breaker  // per-key
    sticky       ...             // (未来)
}
```

### 3.2 `auth.GatewayKey` + `provider.Provider`

```go
// GatewayKey 客户端用的凭据
type GatewayKey struct {
    ID            uint
    Name          string
    KeyHash       string           // 哈希存
    Providers     []string         // 绑定的厂商(空 = 不限)
    ProviderKeyIDs []uint          // 绑定的 ProviderAPIKey.id(空 = 不限,P34)
    AllowedModels []string         // 白名单(catch_all 自动模式下也决定候选)
    RPM, TPM      int
}

// Provider 上游厂商实例
type Provider interface {
    Protocol() Protocol
    SendRequest(ctx, req, result) (Response, error)
    SendStream(ctx, req, result, callback) error
    Models() []string
    DefaultModel() string         // 默认模型(catch_all 自动模式候选首选)
    SetPool(*keypool.Pool)        // 注入 key 池
    ...
}
```

---

## 4. 一次请求的完整路径

```
客户端 POST /v1/messages { "model": "claude-opus-5", ... }
   ↓
gin 路由 → Auth Middleware(若有 auth.enabled) → RateLimit → proxy.HandleRequest
   ↓
proxy.handle(c, isStream)
   ↓
1. parse body → provider.Request{Model, Body, IsStream, ...}
2. resolve model alias(若 model 是 alias 名,resolve 成真实 model)
3. e.router.Route(ctx, req, opts...) → *RouteIterator
   ↓
proxy.runWithFirstResult → tryOneCandidate 循环
   ↓
4. iter.Next() → 选下一个候选(*RouteResult{ProviderName, Key, Tier, ...})
   ↓
5. e.attemptOne(req, result)
   ↓
   ├─ get provider instance via manager.Get(result.ProviderName)
   ├─ pv.SendRequest(ctx, req, result)  ← 用路由层已 acquire 的 req.Key
   ├─ 成功 → recordUsageWithTokens + recordMetrics
   └─ 失败 → 错误分类 → tryCandidate 决策树
        ├─ model_not_allowed / quota_exceeded → outcomeContinue(下一候选)
        ├─ network / auth / rate_limit → swapToOtherKey 换 key 重试
        └─ fatal → handleAllFailed 返回错误
   ↓
6. 写 response → access log 异步落库
```

**关键不变量**:
- 路由层 acquire 的 key(`req.Key`)必须透传给 Provider 层,Provider 层不能二次 acquire(踩坑 #15)
- 失败分类在 `tryCandidate` 集中决策,不走分散的 if

---

## 5. 跨包横切关注点

| 横切点 | 谁负责 | 文件 |
|---|---|---|
| 配置加载与热重载 | `config` 包 | `internal/config/config.go`、`watcher.go` |
| 数据库连接 + 迁移 | `database` 包 | `internal/database/database.go` |
| 接入日志(请求/响应 body) | `accesslog` 包 | `internal/accesslog/` |
| 用量记录(token/cost) | `usage` 包 | `internal/usage/collector.go` |
| Prometheus 指标 | `metrics` 包 | `internal/metrics/collector.go` |
| 余额轮询与探测 | `quotacheck` 包 | `internal/quotacheck/manager.go` |
| 熔断器(per-key) | `circuit` 包 + `keypool` 集成 | `internal/circuit/breaker.go`、`keypool/pool.go` |
| 优雅关停与快照 | `server` 包 | `internal/server/server.go`(`saveKeyStateSnapshot`) |
| Gateway Key 鉴权 | `auth` 包 | `internal/auth/authenticator.go` |
| Provider Key 加密存储 | `auth` 包 | `internal/auth/repository.go` |

---

## 6. 启动时的关键不变量

`server.New` 顺序不能乱(改这里要先看注释):

1. `relay.LoadFromDB(db)` — 从 DB 读 relay_stations 并动态注册(2026-08-22)
2. `buildKeyPools(cfg, db, logger)` — 从 DB 读 provider_api_keys 构造 Pool
3. `restoreKeyStateSnapshots(pools, cfg, logger)` — 恢复 `key-state.json`(QE/COOLING/余额)
4. `router.NewRouter(...)` — Router 拿 Pool
5. `authn := cfg.Auth.Enabled ? auth.New(...) : nil` — 客户端鉴权
6. `usage.NewCollector + usage.NewRepository + metrics.NewCollector`
7. `accesslog.NewRecorder(accessCfg, db, logger)`
8. `proxy.NewEngine(...)` — 注入 Router / Usage / Metrics / AccessLog / Auth
9. `injectPools(manager, pools, logger)` — 把 Pool 注入每个 Provider
10. `quotacheck.NewManager(...)` — 注入 pool 引用 + endpoint lookup
11. `s.quotacheck.Manager.Start(ctx)` 在 Run 阶段触发

**为什么这个顺序**:
- 步骤 1 必须在步骤 2 之前(中转站注册到 Registry,后续 buildKeyPools 才能找到对应 Provider)
- 步骤 3 必须在 quotacheck.Start 之前(QE key 恢复后立即被 callback 重入堆,不等 poll 重新确认)
- 步骤 8 在步骤 9 之前(Engine 构造时不依赖 Pool,Pool 注入在 Run 后通过 Provider.SetPool 完成)

---

## 7. 与规格书的差异(2026-08 现状)

完整差异表见规格书顶部横幅。简版:

| 项 | 规格书写法 | 现状 |
|---|---|---|
| 路由 | alias 表 + fallback model | catch_all 自动模式,无路由表 |
| Provider 结构 | 按协议拆目录 | 按厂商一个目录,内含多协议面 |
| 错误处理 | 400 invalid_request 禁用 key | 无终端禁用状态,只 COOLING / QUOTA_EXCEEDED |
| 熔断器 | provider 级 | per-key 级(踩坑 #16) |
| 配额 | 无轮询 | poll(有余额接口)vs probe(无接口)两档 |
| Key 路由 | 两层各自取 key | req.Key 透传,禁止二次 acquire(踩坑 #15) |
| 计费 | 无 tier | `billing_source: token_plan / api / free` 分层 |

---

## 8. 常见疑问

**Q:为什么没有路由表?**
A:catch_all 自动模式(`routing.catch_all: {}`)让所有 enabled provider 自动进链,客户端发什么模型名都行,网关按 tier(token_plan → api → free)降级。

**Q:为什么 Provider 协议面和厂商目录分开?**
A:同厂商的多个协议面(`deepseek` openai + `deepseek-anthropic` anthropic)共享同一个 key 池(vendor 级一份),避免给同一把 key 重复分配到两个池子。

**Q:为什么不用 LiteLLM 那种协议转换?**
A:转换丢失语义;Provider 特性(thinking、reasoning_content、Responses API)无法表达;调试困难。详见规格书 1.2 节。

**Q:为什么 config.yaml + DB 都有配置?**
A:config.yaml 存**结构性配置**(provider / endpoint / 模型清单 / 路由),DB 存**状态性数据**(provider_api_keys / gateway_keys / 模型别名 / relay_stations)。前者加载即生效,后者通过管理 API 改。

**Q:中转站(Relay Station)和内置厂商有什么区别?**
A:
- **内置厂商**(deepseek/minimax/mimo):代码静态注册,`init()` 触发,不可变
- **中转站**(tokenmarket/rightapi):从 DB `relay_stations` 表动态注册,支持前端 CRUD + 热重载
- 两者都复用协议 base(`openai_compatible` / `anthropic_compatible`),但注册机制不同
- 中转站允许重复注册覆盖(支持编辑场景),内置厂商禁止重复注册(防代码错误)

**Q:为什么中转站编辑后要允许重复注册?**
A:用户随时可以在前端编辑中转站配置(修改 endpoint / 切换协议),需要重新注册覆盖旧配置。内置厂商是代码静态的,不会动态变化,重复注册就是错误。见踩坑 #26。

---

## 9. 改动前的检查清单

任何跨包改动前,先回答:

1. 这条改动影响哪些层(包)?列全
2. 有没有横切关注点(accesslog / metrics / usage / auth)受影响?
3. 有没有持久化影响(DB schema / 快照)?
4. 有没有 reload 路径需要同步改?
5. 测试矩阵里有没有覆盖?

详见 `docs/refactoring-considered.md`(若存在)。
