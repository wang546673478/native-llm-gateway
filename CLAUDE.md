# Native LLM Gateway — Claude Code 指令

> 协议感知 LLM Gateway · Go 后端 + Vue 前端 · SQLite/PostgreSQL · 多 Provider 自动路由
>
> v0.5.0-p5 · 2026-08

---

## 🚨 第一要素（**高于一切，不可妥协**）

### 低耦合 + 高内聚

**这是本项目的最高原则。** 优先级高于功能完整性、高于 UI 美观、高于性能优化、高于开发速度、高于任何其他考量。

> 改一处不坏其他位置 = 加一处不依赖其他位置 = 每个模块独立可测

### 为什么这是第一要素

- 本项目历史上**至少 4 次**"改一个地方坏其他地方"的真实事故：
  - `routing.model` 字段同时决定"用哪个模型"和"白名单校验" → 改白名单逻辑要改 config + router + server + 前端 4 处
  - `acquireFromTierLocked` 一个函数干 9 件事(恢复 COOLING / 收集 / 过滤 5 层 / 排序 / 调度) → 加 sticky 要改 35 个文件
  - 踩坑 #15:路由层和 Provider 层各自 acquire key → 429 冷却标到没发过请求的 healthy key 上
  - 踩坑 #12:Gateway Key 绑定按注册名精确匹配 → deepseek / deepseek-anthropic 双协议面 403

- 这些事故的共同根因：**没有把"低耦合高内聚"当作不可妥协的第一要素**。

- 现在把它明确为第一要素 → 所有 AI agent / 人都要服从。

### 具体含义

| 维度 | 低耦合（Low Coupling） | 高内聚（High Cohesion） |
|------|---------------------|----------------------|
| 模块之间 | 一个模块改动**不**影响其他模块 | 一个模块**只**负责一件事 |
| 调用关系 | 走稳定的接口边界（不跨包直接操作内部状态） | 内部实现细节不外泄 |
| 数据流 | 通过参数 / context 显式传递 | 每个模块边界清晰 |
| 测试 | 可单独测试一个模块 | 替换模块实现不影响其它 |
| 配置 | 每个字段只做一件事 | 不在配置里混状态 |

### 不允许的妥协

| 场景 | 不允许的"快速方案" | 必须的"正确方案" |
|------|------------------|------------------|
| 加新字段 | 在已有函数里加参数(如 acquireFromTierLocked 已有 4 个参数) | 用 Option 模式或拆分函数 |
| 加新依赖 | pool 直接 import circuit / quotacheck | 接口注入或回调 |
| 跨包修改 | 改了 pool 不改 router/proxy 的调用方 | 一起改,编译器强制发现 |
| 白名单校验 | 校验内部候选模型名(result.ModelID) | 校验客户端原始模型名(req.Model) |
| Key 状态转换 | COOLING 反复刷新没有退出机制 | 加连续冷却超限 → QE 升级 |
| 想"临时绕开" | 先这样,回头改 | 立刻改,否则不提交 |

### 何时此原则让步？

**没有。** 如果你发现"低耦合"和"某个功能"冲突，那是功能设计有问题，不是原则有问题。

---

## 🚥 一句话速记

| 严禁 | 必须 |
|------|------|
| 一个函数干 3 件事以上 | 拆成独立函数,每个 ≤ 2 件事 |
| 删除一个字段要改 2 处以上 | 拆成两个字段,各自独立 |
| pool 直接 import circuit/quotacheck | 接口注入或回调槽 |
| `c.Get("gateway_key")` magic key | 显式参数传递 |
| `swapToOtherKey` 里同时操作 Pool + Auth | 拆成 4 个独立函数 |
| `ReportRateLimit` 只刷新不升级 | 连续冷却超限 → QE |
| config 改了不改三个模板 | 必须同步改 config.yaml / example / docker |
| 新 provider 忘了 blank import | `cmd/gateway/main.go` 加一行 |
| 流式请求在详情页找 token | 去 Usage 页 |
| SQLite 单写者扛高并发 | 生产用 PostgreSQL |

---

## 📁 关键目录

```
backend/
├── cmd/gateway/main.go        ← 入口(加载 config → 构造 pool → 启动 server)
├── internal/
│   ├── provider/              ← Provider 接口 + 厂商包(deepseek/minimax/mimo/glm/qwen/gemini)
│   │   ├── openai_compatible/   ← OpenAI 兼容共享实现
│   │   ├── anthropic_compatible/← Anthropic 兼容共享实现
│   │   ├── google/              ← Google Generative AI 共享实现
│   │   └── registry.go / manager.go
│   ├── router/                ← 路由(catch_all / alias / tier 拉平)
│   ├── proxy/                 ← 代理引擎(failover / 白名单逐候选 / swapToOtherKey)
│   ├── keypool/               ← key 池(tier 桶 / 额度状态机 / per-key 熔断 / 调度器)
│   │   ├── scheduler.go        ← RoundRobin / LeastUsed / Random
│   │   ├── pool.go             ← acquireFromTierLocked(核心,待拆分)
│   │   └── key.go              ← IsUsable / IsPolledAndExhausted
│   ├── circuit/               ← per-key 熔断器(CLOSED → OPEN → HALF_OPEN)
│   ├── quotacheck/            ← 余额轮询 + probe 探测
│   ├── accesslog/             ← 接入日志(body 文件 + 30 天保留)
│   ├── auth/                  ← 客户端鉴权 + Provider Key 管理
│   ├── usage/                 ← 用量异步收集 + 批量落库
│   ├── metrics/               ← Prometheus 指标
│   ├── config/                ← 配置加载 + 热重载
│   ├── database/              ← DB 连接 + GORM 模型
│   ├── server/                ← 服务编排(优雅关停 / key 状态快照)
│   └── api/http/              ← 管理 API
├── migrations/                ← 001-005 数据库迁移

frontend/
├── src/
│   ├── views/                 ← 7 个页面(Overview/Providers/ProviderKeys/Keys/Routing/Usage/AccessLogs)
│   ├── api/client.ts          ← 后端 API 封装
│   └── router/index.ts        ← vue-router 路由

scripts/
├── gateway-reload.sh          ← 无感重载(编译 → systemctl restart)
├── gateway-ctl.sh             ← 进程管理(systemd)
├── gateway-log-rotate.sh      ← 日志按天轮转 + 7 天清理
└── pg-init.sh                 ← PostgreSQL 初始化

docs/
├── ARCHITECTURE.md            ← 架构总览(包+职责+边界)
├── providers.md               ← 厂商目录
├── subsystems.md              ← quotacheck / auth / usage
├── cross-cutting.md           ← accesslog / metrics / circuit
├── config-reference.md        ← config.yaml 完整字段
├── frontend.md                ← 前端管理 UI
├── operations.md              ← 部署 / 脚本 / 监控
├── 踩坑与排错.md               ← 22 个实战坑
└── provider厂商定制包指南.md     ← 新增厂商 6 步实操
```

---

## 🧪 验证命令（**改完必跑**）

```bash
# 1. 全量测试（最高优先级 — 必跑）
make test

# 2. go vet
make vet

# 3. 编译检查
make build

# 4. 前端类型检查
cd frontend && npx tsc --noEmit
```

---

## 🚀 启动顺序

```bash
# 本地开发
make build && make start         # 后端 :8080
cd frontend && npm run dev       # 前端 :5173(代理到 8080)

# 生产
docker compose up -d             # gateway + PostgreSQL
# 或
sudo systemctl start llm-gateway # systemd 托管
```

---

## 🚫 不要做的事

- 不要在一个函数里干 3 件事以上(如 acquireFromTierLocked 干 9 件事)
- 不要删除一个字段只改 1 处而其他 2 处不管(如 routing.model)
- 不要 pool 直接 import circuit / quotacheck(用接口注入)
- 不要用 `c.Get("gateway_key")` magic key 传状态(用显式参数)
- 不要在 `swapToOtherKey` 里同时操作 Pool + Auth + RouteResult
- 不要在 `ReportRateLimit` 里只刷新 CoolingUntil 而不升级到 QE
- 不要改 config struct 不改三个 config 模板
- 不要加新 provider 忘了 `cmd/gateway/main.go` 的 blank import
- 不要在 access log 详情页找流式请求的 token(去 Usage 页)
- 不要用 SQLite 扛生产高并发(用 PostgreSQL)
- **不要在发现耦合问题时说"先这样,回头改"** — 立刻改,否则不提交

---

## 📐 改代码前自检清单

每次提交代码前 30 秒扫一眼：

- [ ] 我加的新函数干了几件事? ≤ 2 件?
- [ ] 我加的新字段删除后只改 1 处?
- [ ] 我有没有跨包直接 import(如 pool → circuit)?
- [ ] config 改了?三个模板都改了吗?
- [ ] 新 provider 加了 blank import 吗?
- [ ] 新字段有 GORM tag + migration 吗?
- [ ] `make test` 是绿的吗?
- [ ] `make vet` 是绿的吗?

**任何一项打 ❌ = 不能提交。**

---

## 📋 重构进度(2026-08-08)

### 已完成(通过全部测试,网关稳定)

**耦合解耦(十一轮 30+ commit):**

| 类别 | 改动 | 效果 |
|---|---|---|
| 死代码 | 删 `AcquireWithFilter` filter chain + `BuildPoolFromStrings` + `routeDirectModel` + 休眠 migrations/*.sql | 消除双实现漂移 + Schema 双真相(AutoMigrate 唯一权威) |
| 裸串魔数 | keypool.ErrorType + BillingSource 常量 + 守卫测试 | 消除 error type / billing source "改一处改多处"漂移 |
| 复制粘贴 | provider.ToPool(六合一)/ClassifyTransportError/NewError/ParseRetryAfter/pickAllowedModel/配额关键词单源(LooksLikeQuotaError) | 协议 base + vendor + 关键词表收敛单源 |
| 前后端契约 | client.ts 收编 raw axios + constants.ts 集中枚举 + ProviderKeyView 单类型 | 前后端路径/类型/枚举单一真相 |
| 行为类(用户决断) | StreamTimeoutFloor 可配置 / 429 核心单源各家分叉 / Manager 改 ProviderLookup 窄接口 | 流式超时可调、429 共享语义、router/proxy 依赖窄接口 |
| 并发 | ReloadProviderPool 整表原子替换(修崩溃)、MutateKey(修竞态)、shutdownCtx+Stop(修泄漏)、SendOrAbort(修流阻塞泄漏) | 消除进程崩溃 + 竞态 + goroutine/流泄漏 |
| DB | ProviderAPIKey(ProviderName+Name) 复合唯一索引 | 修复重复 key 可插入 |
| 配置孤岛 | DefaultUsageXxx / DefaultManagerConfig 单源 / authErrorCooling / provider_default 消费 / probe 用 HTTPTimeout | 配置默认单一来源 |
| 文档漂移 | metric 名 / aliases"已退役" / SQL迁移"编号执行" 三处修正 | 修 misleading doc(PromQL 抄错、删活字段、启用漂移迁移) |
| gin路由/观测 | magic-key 契约字符串 auth 单源 / 探针 metric 泄漏(metricsProbeInc) / 429 classify 按上游成因 | 防白名单静默失效、防 metric 双计、修 429→5xx 错记 |
| 工程层 | 构建 flags 单源(reload 委托 make build) / 健康检查端口读 config.yaml / hot-reload 需重启 Warn | 防部署二进制漂移、端口硬编码、reload 静默半生效 |

**单点修复:**

| 改动 | 效果 |
|---|---|
| `acquireFromTierLocked` 拆分 → **已收敛为单一实现** | filter chain 曾是死代码,已删;`acquireFromTierLocked` 为唯一路径 |
| `swapToOtherKey` 拆分 | `poolForFailover` / `allowedIDSetFromRequest` / `swapToOtherKey` 3 个职责 |
| `routing.model` fallback | `filterCandidates` model 空自动用 `default_model` |
| Pool 解耦 circuit | `BreakerFactory` 接口注入,`keypool` 不再 import `circuit` |
| magic key 抽象 | `GatewayKeyContext`(context.go),消除 5 处 `c.Get("gateway_key")` 散布 |
| Provider 自动注册 | 新增 `provider/builtin/`,main.go 6 个 blank import → 1 个 |
| Bug: weige QE 死循环 | `ReportSuccess` + `CheckQuota` 才恢复 QE |
| Bug: COOLING 卡死 | `ReportRateLimit` token_plan 连续冷却升级 QE |
| handler→mimo 解耦 | 管理 API handler 不再直连厂商包,闭包注入(bda7ad0) |
| magic key→gkCtx | proxy 5 处 `c.Get("gateway_key")` 统一走接口(a75ea23) |

### 剩余耦合(评估为合理保留,保网关稳定)

| 耦合 | 位置 | 判断 |
|---|---|---|
| `provider` import `keypool` | provider/registry.go `Pool interface{}` + `Request.Key` | 合理类型依赖(pool 注入,非构造),保留 |
| `proxy.Engine` 持 3 个具体跨包引用 | `*router.Router`/`*auth.Authenticator`/`*accesslog.Recorder` | 窄方法协作方,接口化收益<复杂度,保留 |
| `provider/{deepseek,glm,mimo,minimax}→quotacheck` | 厂商 balancer 实现 quotacheck.Balancer | 依赖倒置(消费者定接口,实现方注册),保留 |
| circuit 内建默认(5/60s/30s/1) | circuit.New 硬编码 | 合法包内单源;不为集中去 import config,保留 |
| write_timeout 双语义 | http.Server 原始值 vs 引擎 2m 流式兜底 | 有意设计差异(socket 绝对上限 vs chunk 续期),保留 |
| `mimo.quotaCookie` 全局单例 | provider/mimo/balancer.go | 通过 MimoQuotaSet 闭包注入隔离,proxy 不直接碰,保留 |
| config providers[].keys[] dual-path | main.go legacy pool builder 被 DB 路径遮蔽 | 删除需彻底追 main.go,风险>收益,暂缓 |
| 测试 fakeProvider ×5 | 各 test 包局部重复 | 抽共享 testutil 是更大重构,暂缓 |
| 前端每 view 独立 fetch providers/keypool | Providers/ProviderKeys/Keys/AccessLogs/Routing | 抽 Pinia store 是更大重构,暂缓 |
| 前端 usePagination 已建未接 | Usage/AccessLogs 仍内联分页 | 接上需改模板绑定,低优先 |
| hot-reload 需重启字段 | database/server/usage/providers 等 | 已加 Warn 提示;彻底支持是大重构,字段明确需重启 |
| PG role/DB 常量(pg-init vs docker) | 不同层、无 schema 影响 | AutoMigrate 自愈,schema 无风险,保留 |
