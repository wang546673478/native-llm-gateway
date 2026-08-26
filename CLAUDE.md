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
| 新 provider 忘了 blank import | `provider/builtin/builtin.go` 加一行 |
| 流式请求在详情页找 token | 去 Usage 页 |
| 删结构体字段就当改完了 | 手工 DROP COLUMN(AutoMigrate 只加不删) |
| SQLite 单写者扛高并发 | 生产用 PostgreSQL |
| Provider 绑定留到 proxy 层否决 | 下沉路由层过滤(否则白 acquire key 推歪 sticky,#30) |
| 改完 DB 接口不补 test fake | fake 缺方法 = 整包 build failed(`make test` 必跑) |
| 排障 `fmt.Printf("DEBUG:...")` 留在代码里 | 提交前清干净或改 logger.Debug |
| 失败判定只看状态码 | 200 也可能是错误(流内错误事件 / base_resp,#31) |
| 加 error_type 新值只改写入方 | 同步三处:写入 + admin 白名单 + 前端过滤项 |
| 热路径预筛比解析器窄 | 预筛必须是解析器命中集的**超集**(否则静默漏判,#31) |

---

## 📁 关键目录

```
backend/
├── cmd/gateway/main.go        ← 入口(加载 config → 构造 pool → 启动 server)
├── internal/
│   ├── provider/              ← Provider 接口 + 厂商包(deepseek/minimax/mimo;google 协议层无消费者)
│   │   ├── openai_compatible/   ← OpenAI 兼容共享实现
│   │   ├── anthropic_compatible/← Anthropic 兼容共享实现
│   │   ├── google/              ← Google Generative AI 共享实现
│   │   ├── relay/               ← 中转站(DB 动态注册,零代码接入:URL+协议)
│   │   └── registry.go / manager.go
│   ├── router/                ← 路由(catch_all / alias / tier 拉平 / Provider 绑定过滤)
│   ├── proxy/                 ← 代理引擎(failover / 白名单逐候选 / swapToOtherKey)
│   │   └── relay.go            ← 中转站直通判定(isRelayPassthrough)
│   ├── keypool/               ← key 池(tier 桶 / 额度状态机 / per-key 熔断 / 调度器)
│   │   ├── scheduler.go        ← Sticky(顺序黏性,默认) / RoundRobin / LeastUsed / Random
│   │   ├── pool.go             ← acquireFromTierLocked(核心,待拆分)
│   │   └── key.go              ← IsUsable / IsPolledAndExhausted
│   ├── circuit/               ← per-key 熔断器(CLOSED → OPEN → HALF_OPEN)
│   ├── quotacheck/            ← 余额轮询 + probe 探测
│   ├── fingerprint/           ← 设备指纹归一化(多机共用上游 key 抹平 device_id/平台多头信号,防封号)
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
│   ├── views/                 ← 11 个页面(Overview/Providers/ProviderKeys/Keys/Routing/Usage/AccessLogs/Inflight/Models/RelayStations)
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
├── 踩坑与排错.md               ← 31 个实战坑(2026-08-26 补 #31:流内上游错误)
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

# 4. 前端类型检查(Vue 项目须用 vue-tsc,裸 tsc 会误报 .vue 模块缺失)
cd frontend && npx vue-tsc --noEmit
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
- 不要加新 provider 忘了 `provider/builtin/builtin.go` 的 blank import
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
- [ ] 新 provider 在 `provider/builtin/builtin.go` 加了 blank import 吗?
- [ ] 新字段有 GORM tag + migration 吗?
- [ ] **删了 GORM 结构体字段/关联吗?→ 必须手工 `ALTER TABLE ... DROP COLUMN`** —— AutoMigrate 只加不删,留下的 NOT NULL 死列会让 INSERT 全炸(踩坑 #23)
- [ ] `make test` 是绿的吗?
- [ ] `make vet` 是绿的吗?

**任何一项打 ❌ = 不能提交。**

---

## 📋 重构进度(2026-08-08)

### 已完成(通过全部测试,网关稳定)

**耦合解耦(十六轮 45+ commit):**

| 类别 | 改动 | 效果 |
|---|---|---|
| 死代码 | 删 `AcquireWithFilter` filter chain + `BuildPoolFromStrings` + `routeDirectModel` + 休眠 migrations/*.sql | 消除双实现漂移 + Schema 双真相(AutoMigrate 唯一权威) |
| 裸串魔数 | keypool.ErrorType + BillingSource 常量 + 守卫测试 | 消除 error type / billing source "改一处改多处"漂移 |
| 复制粘贴 | provider.ToPool(六合一)/ClassifyTransportError/NewError/ParseRetryAfter/pickAllowedModel/配额关键词单源(LooksLikeQuotaError);openai 13 处 NewError 迁移(o069cbc) | 协议 base + vendor + 关键词表收敛单源;三协议 base 构造器统一 NewError |
| 前后端契约 | client.ts 收编 raw axios + constants.ts 集中枚举 + ProviderKeyView 单类型 | 前后端路径/类型/枚举单一真相 |
| 行为类(用户决断) | StreamTimeoutFloor 可配置 / 429 核心单源各家分叉 / Manager 改 ProviderLookup 窄接口 | 流式超时可调、429 共享语义、router/proxy 依赖窄接口 |
| 并发 | ReloadProviderPool 整表原子替换(修崩溃)、MutateKey(修竞态)、shutdownCtx+Stop(修泄漏)、SendOrAbort(修流阻塞泄漏) | 消除进程崩溃 + 竞态 + goroutine/流泄漏 |
| DB | ProviderAPIKey(ProviderName+Name) 复合唯一索引 | 修复重复 key 可插入 |
| 配置孤岛 | DefaultUsageXxx / DefaultManagerConfig 单源 / authErrorCooling / provider_default 消费 / probe 用 HTTPTimeout | 配置默认单一来源 |
| 文档漂移 | metric 名 / aliases"已退役" / SQL迁移"编号执行" 三处修正 | 修 misleading doc(PromQL 抄错、删活字段、启用漂移迁移) |
| gin路由/观测 | magic-key 契约字符串 auth 单源 / 探针 metric 泄漏(metricsProbeInc) / 429 classify 按上游成因 | 防白名单静默失效、防 metric 双计、修 429→5xx 错记 |
| 工程层 | 构建 flags 单源(reload 委托 make build) / 健康检查端口读 config.yaml / hot-reload 需重启 Warn | 防部署二进制漂移、端口硬编码、reload 静默半生效 |
| 重大重构(十三轮) | config keys dual-path 追查 + APIKeys 死写链删除 / Pinia providers store 接入 5 view | APIKeys 纯写死通道(删);5 view 厂商清单共享 fetch(3a1ede1+d1aba91) |
| 重大重构(十四轮) | timeouts 死配置孤岛删除(server_read/write/idle+request_total) / openai 13 处 NewError 单源 + io_error 归类分歧消除 / Pool interface{}→*keypool.Pool + 删 main.go buildPools / 前端 vendorOptions/regToVendor/quotaDisplay 单源 | DB 是 config key 唯一权威;三协议 base 构造收敛 NewError;per-key 熔断对 io 失败一致生效;config keys[] dual-path 彻底消除(009236a+o069cbc+2ff6d3e+afb5e83) |
| 深度审计(十五轮) | 数据竞态 F1/F2/F4 / DB 数据完整性 H1/M1/M2 / 热重载 s.cfg 分歧 / 前端过滤契约补漏 / 文档漂移 | 锁边界 + 熔断热路径竞态(3 subagent:并发/DB/热载审计)全修 → -race 0 race;usage/accesslog/gateway-key 静默丢数据+重复插+reload 误吞全修(ef9308d+e2f163d+c0d12dc+93e929e+545cc72) |
| HTTP/balancer/常量(十六轮) | status 白名单单源破坏(修自引 bug) / ClassifyErrorWithBody 400 quota 盲区 / gemini+qwen probe fallback 误路由 / glm Bearer 统一 / 低危契约规范化 | 前端过滤加项漏后端白名单(全坏)修复+守卫测试;400 quota→failover;QE 永不复原修;死端点/错误 token/doc 漂移清理(7b00819+dac1ddf+8492123) |
| Key 调度树状模型(十七轮) | 同 provider key 顺序黏性(sticky 先用尽一把再切) / 候选名单天然化(catch_all `{}`) / 429 同 key 重试10次 / route_order 排序改写(方案B,表+GET/PUT+热生效) / 前端拖拽树状图 | 加 priority 无需字段;层内 provider 按最早 key 时间;route_order 改写覆盖(Level2 provider/Level3 key);保存→重进保持;低耦合:StickyScheduler 无状态(始终选最高优先级可用 key,恢复自动回位)、顺序覆盖接口注入、枚举单源(8893971+769a927+d9341a2+5674a5a+994a710+682dd62+b23ea78+0a234c8+0682a6d+0ba9b49+13f07c1)<br>/十八轮(2026-08-10):补「熔断/网络错误恢复后 sticky 回位」— StickyScheduler 删 current/Rewind,改为始终选 keys[0](最高优先级可用 key);额度 poll 与熔断 HALF_OPEN→CLOSED 恢复的 key 重建 bucket 后即 keys[0,自动回位;429 仍重试 10 次、网络错误仍走熔断(用户选型 B) | 修复 key-1 一次 connection 错误把 sticky 永久推到 weige 的卡死:高位 key 恢复后不再粘死在低位(放弃 current 指针的"stay-put",改最高优先级优先,满足"先耗尽再切、恢复回位";无状态更纯,删死代码) |
| 模型进 DB + 排障实录(十九轮,2026-08-20) | provider_models 成模型/定价唯一真相源(上游同步 + 手工定价 + 模型管理页) / 下线 gemini/qwen/glm 三厂商(历史用量 glm 53、qwen/gemini 0) / 排障 8 连修:ListModels 硬编码 /v1/models 路径、mimo openai 面双 /v1(ChatPath 默认撞上已含 /v1 的 endpoint,该面 0 条成功记录)、默认模型字典序(MiniMax-M3 会掉到 M2 → sort_order 保上游顺序)、跨命名空间外键(Provider.Models 关联 → AutoMigrate 建 vendor FK → 启动崩溃循环)、按面计费源取 key(tp- key 发 api 端点必 401)、前端 dist 未重建、AutoMigrate 只加不删留下 NOT NULL 死列 | 网关从「全部 503」恢复至三厂商(deepseek/minimax/mimo)正常路由,默认模型与改动前逐家一致;守卫测试 ×5 防回潮(503e614+2792a5a+25c9e0f+6984bf4+96ae49a) |
| 模型归属下沉到协议面(二十轮,2026-08-21) | 新表 `provider_model_faces`(`(face, model_id)` 唯一)把模型归属从 vendor 下沉到注册面 —— 中转站厂商(rightapi 三个后缀端点、模型互不相通)不再让 codex/grok 面拿到 claude 模型发给自己端点(404 model not found,两面 0 条成功记录);`provider_models` 保持不动继续当定价唯一真相源(不加 face 列:deepseek 双面共享模型,加列会重复行、同一模型填两次价);核心不变式 = **该面**无归属行时回退 vendor 级全量(覆盖「未同步过」与「anthropic 面无模型端点」两种正当情形,按 vendor 判定会让 deepseek-anthropic 失去全部候选);同步只有成功的面才整体替换归属(失败面不动,防抖动清空);模型管理页面 tab + ⚠无归属标记 + 「清理无归属」(prune 对无归属数据的 vendor 整体跳过,防 `NOT IN (空集)` 删光) | rightapi 三面首次全部跑通(grok-4.5 / gpt-5.4 / claude-opus-5 各 200);deepseek/minimax/mimo 行为不变;守卫测试 ×8 防回潮(踩坑 #25) |
| 中转站直通模式 + Provider 绑定下沉(二十一轮,2026-08-25) | 新表 `relay_stations`:中转站从 DB 动态注册(name/base_url/protocol_mode/primary_protocol/keys),热重载允许 Registry 覆盖注册;`relay.LoadFromDatabase` 启动时加载启用中转站;多协议模式按后缀拆分注册面(如 rightapi-openai / rightapi-anthropic);前端 RelayStations 页 CRUD + 热重载按钮。**中转站直通模式**(P-relay-passthrough):Gateway Key 绑定的 Providers **全是**中转站 → 路由跳过白名单选择,proxy 跳过白名单校验,直接透传客户端模型名;混合绑定(中转站+普通厂商) → 中转站也参与普通路由,使用 default_model;`isRelayPassthrough` 两维度判定(Providers 字段 / ProviderKeyIDs 反查)。**Provider 绑定下沉路由层**(P19):Router.routeCatchAllAuto 在候选收集时就过滤 `WithAllowedProviders`,不允许的 provider 不 acquire key(防止 sticky 指针被推进 / metrics 污染 / Inflight 闪现);白名单(AllowedModels)仍在 proxy 层逐候选校验(需 req.Model / result.ModelID 双路 fallback)。test fake 补 `AddFaceModels` / `CountVendorModels`;删 loader/anthropic/openai/proxy 的 DEBUG 打印;handler listProviderModels Manager=nil 时不过滤(降级保险);anthropic test 改 TestListModels_NoPool | 全测试通过;中转站按需透传/路由两用;Provider 绑定过滤位置正确(路由层);守卫测试 ×2(fake 完整性);踩坑 #29(直通语义) / #30(过滤位置) |
| 流内上游错误识别 + failover(二十二轮,2026-08-26) | **HTTP 200 之后在流里发错误事件** → 此前记成 `200/ok`:不冷却 key、不喂熔断、**不换 key**(实测 `tokenmarket-codex` 整条流只有一个 `response.failed` + `rate_limit_exceeded`,262 字节、挂住 ~32s;用户侧 `stream disconnected before completion`)。根因:失败判定只看状态码,`doStream` 在读第一个 chunk **之前**就 `reportKeySuccess` + 写 200 头,循环里只看 `chunk.Err` 从不看 `chunk.Data`。修复沿用**已有范式**(两个 Base 早就在 `ReportSuccess` 前 peek 流头 2 行跑 `ParseMiniMaxStreamBaseResp`),在同一位置加 SSE 错误判定 → 客户端零字节收到,failover 仍可行:①openai Base ②anthropic Base(对称)③proxy chunk 循环兜底 peek 窗口外的中途错误(**只标 error_type 不动 key**:流已正常开跑,实测中途错误是内容审核 `output new_sensitive (1027)`,冷却是误杀)。分类**刻意不用** `ErrorTypeRateLimit`(那走 `retrySameKeyRateLimit` 同 key 无延迟 10 次,而每次挂 ~32s ≈ 320s,比不识别更糟)→ 用 `ErrorTypeServerError` 落 `isNetworkClass` 立刻 `swapToOtherKey` + 喂 per-key 熔断。判定**认结构不认关键词**(顶层/`response` 内层 `error` 对象、`status=="failed"`),正文含 "error" 字样的正常回答放过。新 `upstream_stream_error` 同步三处(写入 + `validStatusTokens` + AccessLogs.vue)。覆盖面:12 个 `SendStreamRequest` 全部是这两个 Base 或委托它们(中转站内嵌 `*openai_compatible.Base` 未覆写 → codex 面自动覆盖) | 语料回放 24605 条真实 body 跑真分类器:16 流头命中 + 1 中途命中、**0 误判**、预筛拒绝 93.9%;两个负向验证都红在准确断言上(去 openai 修复 → "却返回成功 — failover 不会启动";去 proxy 兜底 → `error_type = "", want upstream_stream_error`)且配套正常流测试保持绿;守卫测试 11 函数 / 27 用例,含**预筛超集不变式**(首版预筛只查 `"error"` 漏掉只给 `status=failed` 的形状 → 反向从解析器正样本校验,不手抄第二份清单)+ 分类决策锁死(防改回 rate_limit);线上 3 条真实请求 200/9KB 零误判,但上游当时 `limit_reached:false` **未复现并发限制** → failover 未在线上实录(踩坑 #31) |

**单点修复:**

| 改动 | 效果 |
|---|---|
| `acquireFromTierLocked` 拆分 → **已收敛为单一实现** | filter chain 曾是死代码,已删;`acquireFromTierLocked` 为唯一路径 |
| `swapToOtherKey` 拆分 | `poolForFailover` / `allowedIDSetFromRequest` / `swapToOtherKey` 3 个职责 |
| `routing.model` fallback | `filterCandidates` model 空自动用 `default_model`(2026-08-20 起 default_model 改由 DB `provider_models` 的 sort_order 首行提供) |
| Pool 解耦 circuit | `BreakerFactory` 接口注入,`keypool` 不再 import `circuit` |
| magic key 抽象 | `GatewayKeyContext`(context.go),消除 5 处 `c.Get("gateway_key")` 散布 |
| Provider 自动注册 | 新增 `provider/builtin/`,main.go 6 个 blank import → 1 个(2026-08-20 起只剩 deepseek/minimax/mimo 3 个) |
| Bug: weige QE 死循环 | `ReportSuccess` + `CheckQuota` 才恢复 QE |
| Bug: COOLING 卡死 | `ReportRateLimit` token_plan 连续冷却升级 QE |
| handler→mimo 解耦 | 管理 API handler 不再直连厂商包,闭包注入(bda7ad0) |
| magic key→gkCtx | proxy 5 处 `c.Get("gateway_key")` 统一走接口(a75ea23) |
| 设备指纹归一化 | 新包 `internal/fingerprint`,proxy 层闭包注入(与 QuotaChecker 同构),抹平多机共用上游 key 的 `device_id`/platform/shell/os-version 多头信号;`/api/v1/fingerprint` 热开关 + Overview 卡片 + config `fingerprint` 块。只归一无副作用的纯指纹,**不碰 workdir/对话内容**(见 docs/fingerprint-sanitize-plan.md) |

### 剩余耦合(评估为合理保留,保网关稳定)

| 耦合 | 位置 | 判断 |
|---|---|---|
| `provider` import `keypool` | provider/registry.go `Pool interface{}` + `Request.Key` | 合理类型依赖(pool 注入,非构造),保留 |
| `proxy.Engine` 持 3 个具体跨包引用 | `*router.Router`/`*auth.Authenticator`/`*accesslog.Recorder` | 窄方法协作方,接口化收益<复杂度,保留 |
| `provider/{deepseek,minimax}→quotacheck` | 厂商 balancer 实现 quotacheck.Balancer(glm 已随包删除;mimo 无官方余额端点走 probe 不写 balancer) | 依赖倒置(消费者定接口,实现方注册),保留 |
| circuit 内建默认(5/60s/30s/1) | circuit.New 硬编码 | 合法包内单源;不为集中去 import config,保留 |
| write_timeout 双语义 | http.Server 原始值 vs 引擎 2m 流式兜底 | 有意设计差异(socket 绝对上限 vs chunk 续期),保留 |
| `mimo.quotaCookie` 全局单例 | provider/mimo/balancer.go | 通过 MimoQuotaSet 闭包注入隔离,proxy 不直接碰,保留 |
| 测试 fakeProvider ×5 | 各 test 包局部重复 | 抽共享 testutil 会过度耦合(rich behavior mock vs 最简 stub),保留 |
| 前端每 view 依赖 VendorInfo shape | Providers/ProviderKeys/Keys/AccessLogs/Routing 经 store 消费 | store 只共享 fetch,不抽象后端契约;shape 解耦需契约层,暂缓 |
| hot-reload 需重启字段 | database/server/usage/providers 等 | 已加 Warn 提示;彻底支持是大重构,字段明确需重启 |
| PG role/DB 常量(pg-init vs docker) | 不同层、无 schema 影响 | AutoMigrate 自愈,schema 无风险,保留 |
| admin list 锁外读 *Key(F3) | server SetKeyStatusLookup/SetPoolLookup + auth handler 读 k.Status | admin 频次 + amd64 不撕裂;为低危竞态在热路径加锁风险>收益,保留 |
| **/api/v1+/admin 管理面无鉴权(3.1)** | mark-quota-exceeded/create key 等敏感突变端点无 auth | **安全风险,用户决断**:keys CRUD 设计上证 auth-free(trusted network),但整个管理面无鉴权有成真实暴露风险——待用户决定加 admin auth/网络绑定,不擅自锁死 |
| Logging.output/file_path + usage.retention_days + keypool.health_check_interval(#140) | 零消费 config 字段 | 疑似"拟建未接"功能(文件日志/用量保留)非意外孤岛,移除是产品决定,暂缓评估 |
| 前端树聚合粒度 ≠ 路由候选单元部分 | Routing 树按 vendor 折叠(minimax 一面显 weige/key-1);路由把 minimax/anthropic vs minimax-openai 当独立候选 | 展示简化非耦合错误;树拖拽命中 vendor key 序(scope=key provider=minimax)共享两协议面 pool。已修:Level 2 provider 改写按 (billing_source, vendor) 分层归位,协议面先归 vendor 再查改写,同 vendor 子树对协议面正确生效(2026-08-10)。剩余展示差异(minimax 一面显所有 key)为产品拟增强,保留 |
