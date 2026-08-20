# 上游模型同步 + 手工计费定价设计

> 日期:2026-08-20
> 状态:待用户 review
> 目标:删掉硬编码 `DefaultModels` 常量,模型清单改为「从上游厂商 `/v1/models` 实时拉取」,模型定价改为「同步后手工填入、按每百万 token 计、存 DB」,两者在独立的「模型管理」页面完成;现有 Providers 页完全不动。

---

## 1. 背景与动机

Gateway 当前的「厂商有哪些模型」由两层硬编码来源提供,都会过时、且双真相漂移:

1. 各厂商包顶部 `var DefaultModels = []string{...}`(编译进二进制,改需重编译重启);
2. `config.yaml` 的 `providers.<name>.models[]`(运行时权威,带 `id`/`aliases`/`cost_per_1k_*`/长上下文)。

`Models()` 方法 = 优先 `cfg.Models`,否则 fallback `DefaultModels`;`manager.LoadFromConfig` 再把这些填进内存 `pricing` / `defaultModels`。而 DB 里已有一张 `provider_models` 表(建好、AutoMigrate,**目前是死表,无任何读写**)。

用户诉求:厂商在售模型本该从上游查,不该手写;模型定价也想进 DB 与模型一一对应管理。本设计把 `provider_models` 从死表激活为模型+定价的**唯一权威**,并砍掉所有复杂定价形态。

---

## 2. 目标 / 非目标

**目标**

- 模型清单从上游厂商接口拉取(手动按钮触发),替换硬编码 `DefaultModels` 与 config `models[]`。
- 模型定价在**同一个新页面**手工填写,与模型 id 一一对应,存 DB。
- 删掉 `DefaultModels` 常量;config 不再作为模型清单/定价的权威来源。
- 同步回来的模型**立即可用**(价格为空也不影响路由)。

**非目标(明确不做)**

- ❌ 不做峰谷(分时)定价、缓存写入价、长上下文悬崖等复杂计费形态 —— 只保留「输入 / 缓存命中输入 / 输出」三档、每百万 token 计。
- ❌ 不改 alias(`routing.aliases` / `model_aliases` 表)—— 本次(方案 B)完全不动,后续单独决策。
- ❌ 不动现有 Providers 页;模型管理放独立新页面。
- ❌ 不做定时自动同步;同步为**手动触发**。
- ❌ 不保存上游价格 —— 上游 `/v1/models` 不返回价格(New API 亦然),价格一律本地手工填。

---

## 3. 语义定案(与用户确认)

- **模型清单 = 上游权威**:厂商在售哪几个模型,由拉取上游 `GET /v1/models` 的结果决定,覆盖本地旧清单。
- **计费维度**:输入(未缓存)、缓存命中输入、输出,三档;**每百万 token** 一个价。厂商没有「缓存命中」概念就不填,存 0。
- **未定价模型可用**:同步回来的模型,价格字段为空(或 0)时,**照样进 `Models()` 参与路由**,不阻塞。页面负责标「未定价」。
- **砍掉复杂定价**:不采纳 DeepSeek 峰谷(闲/峰 ×2)、不采纳 MiniMax 长上下文悬崖(>512k ×2)。价格由用户按「每百万 token 多少」手工填一个定值。
- **单位统一**:计费字段统一为「每百万 token」,替换现有 `cost_per_1k_*`(字段名随之重命名,避免「名字叫 1k 存的是 1M」的坑)。
- **DB 是唯一权威**:模型 + 定价都存 `provider_models`(激活);config.yaml 的 `models` 段与服务端 `DefaultModels` 常量全部退役。
- **运行时**:进程已连 PostgreSQL(driver `postgres`,已有 `provider_models` 表)。

---

## 4. 架构

### 4.1 数据模型:`provider_models` 表(改造现有,非新建)

**粒度按厂商(vendor),不按注册面**(用户定案 §4.3 方案 a):一个厂商一行模型,不因 openai/anthropic 多注册面而冗余多份。

现有 `database.ProviderModel` 字段全部**重命名/重定义**为「每百万 token」语义,并把 `provider_name` 语义改为 `vendor`(厂商名):

| 字段 | 类型 | 说明 |
|---|---|---|
| `vendor` | string | 厂商名(如 `minimax`),**非注册面名**。原 `provider_name` 改名并改为厂商语义 |
| `model_id` | string | 上游返回的模型 id(如 `MiniMax-M3`)。复合唯一 `(vendor, model_id)` |
| `cost_per_million_input` | float64 | 输入(未缓存)每百万 token 价;0 = 未填 |
| `cost_per_million_cache_read` | float64 | 缓存命中输入每百万 token 价;无此概念 = 0 |
| `cost_per_million_output` | float64 | 输出每百万 token 价;0 = 未填 |
| `synced_at` | time | 最近一次同步时间 |
| `source` | string | `"upstream"`(同步)/ `"manual"`(手工录入);可选 |

> **厂商 → 注册面的读取映射(方案 A 显式约束)**:同一 vendor 下所有协议面(openai/anthropic/token-plan 等)**共享同一份模型清单**——DB 只按 vendor 存一行,不按注册面冗余。`manager.ModelsFor(注册面)` / `CostFor(注册面, modelId)` 运行时先经 `VendorFor(name) → vendor` 归位,再查该 vendor 的清单。这天然等价于「每个面自己的清单」,前提是**同一 vendor 各面模型清单必须相同**——当前 minimax/mimo/deepseek/glm 各面本就声明同一批,故成立。若未来某厂商需「openai 面与 anthropic 面支持不同模型」,须回到 per-注册面存储(那是另一个设计,不在本 spec 范围内)。

> **新接口方法 `ModelsFor(name) []string`** 是本次新增、会加进 `provider.ProviderLookup` 窄接口(否则 `router.manager`(接口类型)与 `proxy.Manager()` 无法调用)。它返回经 vendor 归位后的模型 id 列表,替代被删除的 `Provider.Models()`。

删除的旧字段:`cost_per_1k_input` / `cost_per_1k_output` / `cost_per_1k_cache_read` / `cost_per_1k_cache_creation` / `long_context_input_threshold` / `long_context_multiplier`。

> **单位换算备忘**:`1 百万 = 1000 千`。写 DB 的值就是「每百万 token 价格」本身;内存 `ModelCost` 结构体字段同步改名,`ComputeCost` 公式里的 `/1000.0` 底数改为 `/1_000_000.0`。

### 4.2 计费结构体 `ModelCost`(provider 包)同步收敛

`provider.ModelCost` 从 6 字段收敛为 3 字段(去掉 cache creation、长上下文):

```go
type ModelCost struct {
    CostPerMillionInput     float64 // 输入(未缓存)
    CostPerMillionCacheRead float64 // 缓存命中输入;无此概念 = 0
    CostPerMillionOutput    float64 // 输出
}
```

`ComputeCost` 据此改写,公式:
```
cost = (promptTokens/1e6)*Input + (cacheReadTokens/1e6)*CacheRead + (completionTokens/1e6)*Output
```
(去掉 `CacheCreation` 与 `LongContext` 分支。)

### 4.3 同步能力:新增「列模型」接口与按钮

**协议面能力**(已查证):

| 协议面 | 能否拉列表 | 机制 |
|---|---|---|
| OpenAI 系 | ✅ | `GET {endpoint}/v1/models`,Bearer 鉴权(deepseek/qwen/mimo/minimax-openai) |
| Anthropic 系 | ❌ | 无 `/models` 端点,复用**同 vendor 的 OpenAI 面**去查 |
| Google/Gemini | ⚠️ | `GET {endpoint}/models?key=`,格式专有,需单独适配 |

**同步粒度 = 厂商(vendor)**:一个 vendor 拉一次(优先走它的 OpenAI 面),结果存入 DB 的 `vendor` 行;`manager` 读时按 `VendorFor` 归位到该厂商所有注册面(它们本质同一厂、同一套在售模型,共享 key 池)。前端「同步」按钮以 vendor 为操作单位。DB **只按 vendor 存一行**,不按注册面冗余。

**鉴权**:调 `/v1/models` 需一把该 vendor 的 key;从该 vendor 的 key pool 里取一把 `ACTIVE` key(现有 `openai_compatible.HealthCheck` 已有 `/v1/models` 请求 + Bearer 注入的现成代码,复用同一请求形状)。

**新增 Provider 接口方法**(窄接口,`ListModels(ctx) ([]string, error)`):
- openai base 实现:`GET /v1/models` 解析 `data[].id`;
- anthropic base 实现:返回 `nil, ErrNotSupported`(同 vendor 用 openai 面兜底);
- google base 实现:`GET /models?key=` 解析专有格式。
- 各厂商包的 openai/anthropic 面**透传给 base**,无需每个厂商单独写。

### 4.4 路由读取:从 DB 而非 config

`manager.LoadFromConfig` 目前从 `cfg.Providers[].Models` / `ModelCosts` 填 `defaultModels` / `pricing`。改为:

- **provider 模型清单**:从 DB `provider_models` 按 `vendor` 读 `model_id`,经 `VendorFor` 归位到各注册面的 `Models()`。
- **provider 定价**:从 DB 读三档每百万价格,填 `pricing["<provider>:<model_id>"]`(provider 取注册面名,与 `CostFor` 现有 key 对齐)。
- **默认模型**:`defaultModels[name]` 取该厂商 DB 里「首个 model_id」(排序确定);如后续仍需要显式 `default_model`,再单列字段。
- `Models()` / `DefaultModelFor()` / `CostFor()` 语义不变,只换数据源 config → DB。

**config 彻底退役 `models` 段**:`providers.<name>.models`(含 `id` / `aliases` / `cost_per_1k_*` / 长上下文)全部删除;`ProviderConfig.Models`、`ManagerProviderConfig.ModelCosts`、`config.ProviderModel` 等对应结构与读取链一并清理。模型名、定价、默认模型全部以 DB `provider_models` 为唯一权威。

### 4.4.1 初始化与迁移顺序(关键:DB 为唯一权威,config 彻底废弃)

**模型清单**:不迁移 config 的任何模型名,第一份数据即从上游 `/v1/models` 读取灌入 DB。config 的 `models[].id` 自此废弃,不作为 DB 初始数据来源。

**价格**:**不做自动迁移**。上游不返回价格,config 旧价格(`cost_per_1k_*`)**全部作废**,由用户在新页面手工重填(每百万 token)。列表里 synced 到但未定价的模型 = 价格空,照常参与路由。

实现顺序:

1. **上游读进 DB**:对每个 vendor 调 `ListModels`(优先 openai 面),把真实模型 id upsert 到 `provider_models.vendor + model_id`(价格列全空,`source="upstream"`)。
2. **manager 改从 DB 读** models / pricing / defaultModels;`Models()` / `CostFor()` / `DefaultModelFor()` 内存数据源换为 DB。
3. **删 config `models` 段** 及相关结构(`ProviderConfig.Models`、`ManagerProviderConfig.ModelCosts`、`config.ProviderModel`),config 不再含任何模型/价格。
4. **新页面手工填价** / 后续「同步」按需覆盖,均热生效。

> 迁移前提:DB `provider_models` 必须在这个顺序的**第 1 步就灌入真实清单**,使换数据源那一刻 DB 非空,避免 `Models()` 空 → 路由 503 / 计费归零。该顺序不允许乱序。

### 4.5 API:新增模型管理端点

- `GET /api/v1/providers/models` —— 列出所有 vendor 及其模型(含价格、synced_at、source),供新页面渲染。
- `POST /api/v1/providers/sync-models` —— 触发某 vendor 的同步,入参 `{ vendor }`,内部调 `ListModels` 并 upsert 到 `provider_models`。
- `PUT /api/v1/providers/models` —— 手工保存某模型的定价,入参 `{ vendor, model_id, cost_per_million_input, cost_per_million_cache_read, cost_per_million_output }`。
- 同步/定价变更后**热生效**到 `manager`(与现有 `ReloadPricing` 同类),避免重启。

### 4.6 前端:独立「模型管理」页面

- 新路由 `/models` + 侧边栏入口;Providers 页**零改动**。
- 每个 vendor 卡片:模型表格(id / 输入价 / 缓存命中价 / 输出价 / 同步时间 / 未定价标记)+「同步」按钮。
- 价格单元格可编辑(未定价置灰提示,但模型仍在路由可用列表里)。
- 复用现有 `client.ts` + naive-ui 的表格/表单组件。

---

## 5. 计费正确性边界

- 三档计价、每百万 token、无缓存概念填 0、未定价可用 —— 不动 `ComputeCost` 的调用方,只收敛内部字段与底数。
- **价格不迁移**:config 旧 `cost_per_1k_*` 全部作废,由用户在新页面手工重录;不做任何「旧价自动带入」逻辑。
- usage/accesslog 的 `Cost` 字段原样保留,不在本设计改动。
- **历史 `usage_records.cost` 口径跳变(已确认接受)**:本设计把定价单位从「每千 token」改成「每百万 token」(定价数值也全部手工重填),新写入的 `cost` 与历史存量 `cost` 口径不同,`SUM(cost)` 聚合 / dashboard `TotalCost` 会有一段时间新旧混显。**不追平历史**——价格本就作废重填,历史只是"过去的价格记录"。这是已知、接受的行为变化。
- **长上下文悬崖(MiniMax M3 >512k ×2)与缓存写入价计费行为消失**:`ComputeCost` 删除 `LongContext` 分支与 `CacheCreation` 计费后,这两类计费行为失效。这是 spec §2/§3「砍掉 MiniMax 特殊定价 + 舍缓存写入」的落实,已确认。

## 6. 边界与后续

- **定时自动同步** → 二期(本版手动触发)。
- **SSE/WebSocket 推送同步进度** → 二期(同步是秒级操作,先同步返回)。
- **alias 是否删除** → 本设计不动(方案 B),后续单独 brainstorm。
- **峰谷/缓存创建/长上下文定价** → 明确砍掉;如某厂商未来需要,再单开设计。
