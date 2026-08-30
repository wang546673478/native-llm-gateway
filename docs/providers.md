# Provider 与中转站目录

本文记录当前二进制实际加载的 Provider。内置厂商来自 Go 包和
`config.yaml`；中转站来自数据库 `relay_stations`，两者不是同一种接入方式。

新增标准兼容中转站见 [动态中转站接入指南](relay-stations.md)。只有需要专属鉴权、
URL、错误分类、用量解析或余额查询时，才按
[Provider 厂商定制包指南](provider厂商定制包指南.md)增加 Go 包。

## 核心术语

- **vendor**：厂商级身份，例如 `mimo`。Gateway Key 的 Provider 绑定、上游 Key
  存储、定价和 Access Log 都按 vendor 归一。
- **face**：一个具体协议/端点注册名，例如 `mimo-token-plan-anthropic`。路由协议
  过滤、模型归属和面级配置按 face 工作。
- 同 vendor 的多个内置 face 共享一个 KeyPool。`provider_api_keys.provider_name`
  存 vendor；`protocols` 可限制 key 只能用于 `openai` 或 `anthropic`，空值表示不限制。
- `billing_source` 是 `token_plan`、`api` 或 `free`。路由先按计费层展开候选，再按
  `route_order` 和默认顺序调度。

## 当前内置厂商

`backend/internal/provider/builtin/builtin.go` 当前只 blank import 三个包：DeepSeek、
MiniMax 和 MiMo。Google 协议基础实现仍在代码中，但没有内置厂商注册，也不能作为
动态中转站协议使用。

下表中的计费层和 Responses 开关来自仓库示例配置，不是厂商包硬编码；部署配置可
禁用 face 或改变计费层。

| Vendor | Face | 协议 | 示例计费层 | 请求路径 | Responses |
|---|---|---|---|---|---|
| `deepseek` | `deepseek` | OpenAI | `api` | `/chat/completions` | `/v1/responses` |
| `deepseek` | `deepseek-anthropic` | Anthropic | `api` | `/v1/messages` | 不适用 |
| `minimax` | `minimax` | Anthropic | `token_plan` | `/v1/messages` | 不适用 |
| `minimax` | `minimax-openai` | OpenAI | `token_plan` | `/chat/completions` | `/responses` |
| `mimo` | `mimo` | OpenAI | `api` | `/chat/completions` | `/responses` |
| `mimo` | `mimo-anthropic` | Anthropic | `api` | `/v1/messages` | 不适用 |
| `mimo` | `mimo-token-plan` | OpenAI | `token_plan` | `/chat/completions` | `/responses` |
| `mimo` | `mimo-token-plan-anthropic` | Anthropic | `token_plan` | `/v1/messages` | 不适用 |

内置 face 只有在 `providers.<face>.enabled: true` 时才由 Manager 实例化。
`responses_api` 也只用于内置 face；路由会排除未声明支持 Responses API 的内置
Provider。

## DeepSeek

代码位于 `backend/internal/provider/deepseek/`。

- OpenAI endpoint 通常是 `https://api.deepseek.com`。包显式使用
  `/chat/completions`，不会套用兼容基座默认的 `/v1/chat/completions`。
- Anthropic endpoint 由配置提供，仓库示例为
  `https://api.deepseek.com/anthropic`；兼容基座再拼 `/v1/messages`。
- OpenAI face 打开 Responses 时使用 `/v1/responses`。
- 两个 face 注册到同一个 `deepseek` vendor，共享 key 池。Anthropic face 可通过
  `force_thinking_disabled` 在上行前强制写入 `thinking.type=disabled`。
- `balancer.go` 使用 `GET {scheme}://{host}/user/balance`，并按
  `is_available` 和 `balance_infos[].total_balance` 判断余额。两个 face 都注册同一个
  balancer。
- OpenAI 用量解析支持标准缓存字段以及 `prompt_cache_hit_tokens`；缓存 token 会从
  普通输入 token 中扣除，避免重复计费。

## MiniMax

代码位于 `backend/internal/provider/minimax/`。

- Anthropic face 的 endpoint 示例为 `https://api.minimaxi.com/anthropic`。
- OpenAI face 的 endpoint 示例已含 `/v1`：`https://api.minimaxi.com/v1`，因此包显式
  使用 `/chat/completions`、`/responses` 和 `/models`。
- 两个 face 注册到 `minimax` vendor，并在示例配置中都属于 `token_plan` 层。
- OpenAI 和 Anthropic 兼容基座都会识别 HTTP 200 body 中的
  `base_resp.status_code`。`1008` 和 `2056` 被分类为额度耗尽；其余非零状态视为上游
  错误。
- `balancer.go` 查询未公开的
  `https://www.minimaxi.com/v1/token_plan/remains`，取各模型当前窗口剩余百分比的最小
  值。该端点不是稳定公开契约，失败时仍需依赖请求错误分类。

## MiMo

代码位于 `backend/internal/provider/mimo/`。

- 按量 OpenAI endpoint：`https://api.xiaomimimo.com/v1`。
- 套餐 OpenAI endpoint：`https://token-plan-cn.xiaomimimo.com/v1`。
- 对应 Anthropic endpoint 分别以 `/anthropic` 结尾，由基座继续拼
  `/v1/messages`。
- `sk-` 按量 key 和 `tp-` 套餐 key 属于同一个 `mimo` 池，但必须通过每把 key 的
  `billing_source` 隔离。OpenAI face 的模型同步和健康检查会按本 face 的计费层取
  key。
- 四个 face 共用一个 balancer。它按 `key.BillingSource` 选择控制台的套餐用量或
  余额端点，并使用账号 Cookie 而不是 API key 鉴权。Cookie 可由配置启动注入，也可
  通过管理 API 更新并持久化。
- 这些控制台端点不是公开稳定 API。Cookie 缺失、过期或查询失败时，余额轮询不会把
  查询失败直接当作确认耗尽；请求错误分类仍是兜底。

当前 Anthropic 兼容基座没有 `BillingSource` 配置项。MiMo 的两个 Anthropic face
共享池时，`ListModels`/健康检查只能按协议取 key，不能进一步按 `api` 与
`token_plan` 隔离；正常代理请求仍使用路由层已经选好的 `req.Key`。

MiMo wrapper 的 `Name()` 当前还固定返回基础注册面：Token Plan OpenAI 返回 `mimo`，
Token Plan Anthropic 返回 `mimo-anthropic`。Manager 本身仍按配置中的 face 名保存实例，
但显式 alias 的模型白名单选择会通过 `Provider.Name()` 查询模型，可能读到基础 face 的
清单；需要严格按 face 隔离时优先使用自动 catch-all，并在修复 wrapper 身份后补回归测试。

## 模型、归属与定价

模型不再写在 `config.yaml`：

- `provider_models`：`(vendor, model_id)` 级模型和三档人民币每百万 token 价格：
  input、cache read、output。
- `provider_model_faces`：`(face, model_id)` 级归属和面内顺序。
- 同步一个 vendor 时，Manager 遍历其所有 face 调 `ListModels`。成功的 face 会整体
  替换自己的归属；失败的 face 保留旧归属。所有成功结果合并后 upsert 到 vendor
  模型表，已有手工价格不被覆盖。
- face 没有任何归属行时，运行时回退到 vendor 全量模型；这使没有模型列表端点的
  Anthropic face 可以共享同厂商 OpenAI face 的清单。
- 上游下架的模型不会自动从 `provider_models` 删除。模型管理页的“清理无归属”才会
  删除没有任何 face 引用的旧行。

同步端点为 `POST /api/v1/providers/sync-models`，body 是
`{"vendor":"deepseek"}`；也可使用模型管理页或
`POST /api/v1/providers/sync-all-models`。同步或修改价格后，管理 handler 会重新把
模型数据加载进 Manager。

## 注册与 KeyPool

内置厂商包在 `init()` 中调用：

```go
provider.RegisterGlobalWithProtocolVendor(face, factory, protocol, vendor)
```

仅有 `init()` 不够；包还必须在 `backend/internal/provider/builtin/builtin.go` 被 blank
import，才会进入二进制。Registry 中的 vendor 映射决定：

- 多 face 是否共享池；
- Provider Key 创建时归一到哪个 `provider_name`；
- Gateway Key 的 Provider 绑定如何匹配；
- Access Log 如何把 face 归一为 vendor。

Provider 发送请求时必须优先使用路由层传入的 `req.Key`。只有健康检查和模型同步这类
无路由上下文调用，才应自行从池中 acquire；否则上游实际使用的 key 和被冷却/熔断的
key 可能不一致。

## 余额恢复和熔断

- vendor 的任一 face 注册 balancer 后，共享池使用 poll 恢复模式。没有 balancer 的
  vendor（包括动态中转站）使用 probe 语义：`quota_exceeded` 只计数、不持久标记
  `QUOTA_EXCEEDED`，下一次请求会重新试探。
- 该 probe 保护不覆盖 `ReportRateLimit` 的升级旁路：`token_plan` key 连续第 3 次
  `rate_limit` 会直接进入 `QUOTA_EXCEEDED`，且当前不会触发 probe 调度回调。无 balancer
  的此类 key 可能需要重启、重建 Pool 或外部状态恢复；动态中转站自动创建的 key 固定为
  `api`，正常路径不会触发这一例外。
- `token_plan`、`api`、`free` 是跨 Provider 的固定层级，不等同于 key 状态。
- 熔断器按 key 隔离，内置厂商从该 vendor 的 Provider 配置读取熔断参数。
- 动态中转站不在 `config.providers` 中，因此目前拿不到厂商级熔断配置；它仍有 key
  冷却和错误驱动的 failover，但没有这套 per-key breaker。

## 动态中转站

中转站不是内置厂商目录的一部分。启动时
`provider/relay.LoadFromDatabase` 查询启用的 `relay_stations`，注册 face、同步 key，
再由 Server 建池。创建、更新和删除管理记录后会自动热重载。

当前中转站只实现 OpenAI 与 Anthropic 兼容协议。Responses 对 OpenAI 中转站采取
乐观透传：路由不读取已废弃的能力列；不支持 `/v1/responses` 的站由上游 400/404 和
候选 failover 处理。完整限制见 [动态中转站接入指南](relay-stations.md)。

## 代码入口

- Registry：`backend/internal/provider/registry.go`
- Manager：`backend/internal/provider/manager.go`
- 内置装载点：`backend/internal/provider/builtin/builtin.go`
- OpenAI 基座：`backend/internal/provider/openai_compatible/`
- Anthropic 基座：`backend/internal/provider/anthropic_compatible/`
- Google 基座（当前无内置厂商）：`backend/internal/provider/google/`
- 动态中转站：`backend/internal/provider/relay/`
- KeyPool：`backend/internal/keypool/`
- 模型存储：`backend/internal/database/provider_model_store.go`
