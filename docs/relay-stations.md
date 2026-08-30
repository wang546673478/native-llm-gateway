# 动态中转站接入指南

动态中转站用于接入标准 OpenAI 或 Anthropic 兼容上游，不需要新增 Go 厂商包。配置
保存在数据库 `relay_stations`，启动和管理操作会动态注册 Provider。

本文以当前代码为准。旧文档以 TokenMarket 为例，但它只是一个普通站点实例；网关没有
TokenMarket、RightAPI 或其他商业站点的硬编码配置。

## 适用范围

当前可靠范围是：

- `protocol_mode: single`；
- `primary_protocol: openai` 或 `anthropic`；
- 标准 Bearer（OpenAI）或 `x-api-key`（Anthropic）鉴权；
- 标准 Chat Completions、Responses、Messages、Models 和 SSE/usage 形状。

以下场景应写[内置厂商包](provider厂商定制包指南.md)，而不是强塞进 relay：专属鉴权、
非标准 URL、请求/响应改写、厂商专属错误分类、非标准 usage 或余额 API。

## 先看当前限制

### 只推荐 single 模式

数据库和 UI 有 `multi` 模式，但当前实现没有形成可靠的多协议运行契约：

- 每个 `name-protocol` face 复用同一个 `GenericRelayProvider`；
- Provider 的 `Protocol()` 永远返回主协议，且没有实现 `MultiProtocolProvider`；
- Router 因此会过滤非主协议，并可能把主协议请求重复投向多个 face；
- 冷启动只按 vendor 建 pool，注入时却按 face 查，multi face 可能拿不到 pool；管理 API
  热重载后才会补上映射；
- 各 face 的 `ListModels` 都合并全部协议实现，模型归属不是协议隔离的。

一个上游同时提供 OpenAI 和 Anthropic 时，当前应创建两个名称不同的 single 站点，
分别配置各自 base URL 和 key。不要依赖 multi 模式。

### Google 尚不可用

前端仍显示 Google 选项，但 relay 构造器明确返回
`google protocol not yet supported for relay stations`。single 选择 Google 或 multi 列表
包含 Google，整个站都不会加载。

### `billing_source` 暂不生效

`relay_stations.billing_source` 当前只被数据库和 UI 保存：

- 动态 `Manager.AddProvider` 没有登记该值，运行时查询回退为 `api`；
- `keys` 同步到 `provider_api_keys` 时也固定写 `BillingSource: "api"`。

因此页面选择 `token_plan` 或 `free` 不会改变调度层，当前所有自动管理的 relay 都按
`api` 运行。请保持选择 `api`，不要依赖该字段做套餐优先级。

### 安全边界

- `relay_stations.keys` 和 `provider_api_keys.key_hash` 都保存**明文**上游 key。
- `GET /api/v1/relay-stations` 会把 `keys` 原样返回给有管理权限的调用方。
- relay 候选当前跳过 Gateway Key 的 `allowed_models` 检查；Provider 绑定仍会过滤。
  需要限制 relay 模型时，必须同时依靠 Gateway Key 的 Provider 绑定和上游账号/分组
  权限，不能只依赖 `allowed_models`。
- 站名没有唯一索引，relay 注册还允许覆盖同名 Registry factory。站名绝不能与内置
  face、另一个站名或另一个 multi face 重名。

管理 API 和数据库应只暴露给可信管理员。备份、日志、命令历史和数据库导出都按包含
密钥的敏感数据处理。

## 创建一个站点

推荐使用管理前端的“中转站”页面。填写：

| 字段 | 建议值 | 实际语义 |
|---|---|---|
| 名称 | 唯一小写标识，如 `relay-codex` | single face 名、vendor 名和 key 归属名；创建后视为不可变 |
| 显示名称 | 可读名称 | 仅展示 |
| Base URL | 上游 API 根路径 | 兼容基座会在其后拼标准路径 |
| 协议模式 | `single` | 当前唯一推荐模式 |
| 主协议 | `openai` 或 `anthropic` | Router 的协议过滤依据 |
| 超时 | `400` 秒 | 非流式单次上游请求超时 |
| 计费来源 | `api` | 其他选项当前不生效 |
| API Keys | 每行一把 | 保存到 station JSON，并同步到 Provider Key 表 |
| 启用 | 按需 | 只有启用的站会加载 |

前端会显式提交 400 秒。直接调用创建 API 时如果省略 `timeout_seconds`，handler 当前会
写入 60 秒，和数据库/relay 的 400 秒默认不一致，所以 REST 调用必须显式填写。

管理 API 支持 `X-Admin-Token` 或已登录的 session Cookie，不接受
`Authorization: Bearer` 作为管理认证。创建示例：

```bash
curl -X POST http://localhost:8080/api/v1/relay-stations \
  -H 'X-Admin-Token: <admin-token>' \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "relay-codex",
    "display_name": "Codex relay",
    "base_url": "https://relay.example.com/v1",
    "protocol_mode": "single",
    "primary_protocol": "openai",
    "supported_protocols": "",
    "keys": "[\"sk-replace-me-12345678\"]",
    "enabled": true,
    "timeout_seconds": 400,
    "billing_source": "api"
  }'
```

`supported_protocols` 和 `keys` 在 JSON API 中是**包含 JSON 的字符串**，不是直接数组。
UI 已负责正确编码。直接 API 只校验 `name`、`base_url`、`primary_protocol` 非空，不校验
URL、枚举、JSON 内容或重名，因此 200 不等于站点一定加载成功。

## Base URL 规则

relay 不是任意 HTTP 反向代理。它根据协议重建 URL 和鉴权 header，再透传 JSON body。

### OpenAI

| Base URL | Chat 上游 URL | Responses 上游 URL | Models 上游 URL |
|---|---|---|---|
| `https://host` | `https://host/v1/chat/completions` | `https://host/v1/responses` | `https://host/v1/models` |
| `https://host/v1` | `https://host/v1/chat/completions` | `https://host/v1/responses` | `https://host/v1/models` |
| `https://host/prefix/v1/` | `https://host/prefix/v1/chat/completions` | `https://host/prefix/v1/responses` | `https://host/prefix/v1/models` |

去重只识别清理尾斜杠后**恰好以 `/v1` 结尾**的 endpoint。Base URL 不要写到
`/chat/completions`、`/responses` 或 `/models`。OpenAI 鉴权使用
`Authorization: Bearer <key>`；流式请求会注入 `stream_options.include_usage=true`。

请求发送和 `ListModels` 会处理 `/v1` 去重；OpenAI `HealthCheck` 当前直接把默认
`/v1/models` 拼到 endpoint，因而 endpoint 已含 `/v1` 时会探测
`/v1/v1/models`。健康检查把 4xx 视为“端点可达”，即使失败也不阻止站点加载；它不能
用于验证最终业务 URL，实际请求和模型同步仍使用上表路径。

### Anthropic

| Base URL | Messages 上游 URL |
|---|---|
| `https://host` | `https://host/v1/messages` |
| `https://host/v1` | `https://host/v1/messages` |
| `https://host/prefix` | `https://host/prefix/v1/messages` |

Anthropic 鉴权使用 `x-api-key`，并设置
`anthropic-version: 2023-06-01`。模型同步先尝试
`{base_url}/api/models` 的 New API 返回形状，再尝试标准 `/v1/models`；两个端点都不
可用时同步失败，但手工已有的 vendor 模型不会被自动清空。

## Key 同步规则

启用站点每次加载时，以 `relay_stations.keys` 为权威同步
`provider_api_keys`：

- key 名取明文最后 8 个字符；少于 8 个字符会被静默忽略；
- 后 8 位相同的 key 会碰撞，只保留一个目标；
- 已存在与目标同名的行不会更新 `key_hash`。只改 key 前缀、保留相同后 8 位时仍会用
  旧 key；
- 不在 station JSON 中的站名下 key 会被删除；在 Provider Keys 页面手工添加的 relay
  key 也可能在下次 reload 被删；
- 删除和新增不是一个事务，单条失败只写日志；
- 自动新增的 key 固定为 `api`，`protocols` 为空（所有协议可用）。

因此 relay key 应只在中转站页面维护。替换 key 时确保新旧后 8 位不同，并在保存后从
Provider/Access Log 状态确认实际 key。禁用站点只卸载运行实例，不清除 station 或
Provider Key 表中的明文；彻底移除请删除站点。

## 模型同步与路由

创建 relay 不会自动同步模型。在模型管理页同步指定 vendor，或调用：

```bash
curl -X POST http://localhost:8080/api/v1/providers/sync-models \
  -H 'X-Admin-Token: <admin-token>' \
  -H 'Content-Type: application/json' \
  -d '{"vendor":"relay-codex"}'
```

当前行为：

- `provider_models` 保存 vendor 级模型和价格；`provider_model_faces` 保存 face 归属；
- 成功同步会**整体替换**该 face 的模型归属，再把模型 upsert 到 vendor 表；
- 同一个站的多把 key 如果能看到不同模型分组，一次同步只反映当次 acquire 到的 key，
  不会自动累积所有分组；
- vendor 旧模型和手工价格不会因同步自动删除；“清理无归属”才会删除无 face 引用的行；
- face 有模型清单时，relay 只参与客户端模型名（大小写不敏感）命中的请求；
- face 没有模型清单时视为通配，仍可在首次同步前透传请求；
- 命中后客户端模型名原样发给上游，不替换为默认模型；
- relay 的 400/404 model/invalid request 被视为候选不适配，会继续尝试下一个 relay。

OpenAI relay 的 Responses API 采用乐观透传，不读取已废弃的
`supports_responses_api` 列。支持能力由实际上游决定；不支持的站会返回 400/404，再由
候选 failover 处理。

同步新增模型的三档价格默认为 0。模型管理页维护人民币每百万 token 的 input、cache
read 和 output 价格；不填价格时 Usage 仍记录 token，但 Cost 为 0。

## 调度、冷却与超时

- relay 实际处于 `api` 层；全局层级仍是 `token_plan -> api -> free`。
- 同层 Provider 顺序由 `route_order` 改写；未改写站点按最早 key 创建时间排序，并排在
  已有显式改写之后。
- 站内 key 使用全局 KeyPool 调度和冷却配置。relay 没有 balancer；自动同步的 key 固定
  为 `api`，明确 `quota_exceeded` 时只计数，后续请求会重新试探。
- 旧数据或手工数据若把 relay key 标为 `token_plan`，连续第 3 次 `rate_limit` 会绕过上述
  probe 保护而直接进入 `QUOTA_EXCEEDED`，且不触发后台 probe 回调。这种异常数据可能要
  通过重启、重建 Pool 或外部状态恢复，不能依赖自动探测。
- 动态 relay 不在 `config.providers` 中，目前不会取得 per-key circuit breaker 配置；
  它仍会进行错误上报、冷却和候选 failover。
- `timeout_seconds` 控制非流式单次请求。relay 运行时兜底是 400 秒；OpenAI/Anthropic
  流式上游 HTTP client 的超时下限是 600 秒，服务端 `write_timeout` 仍是外围限制。

## 热重载和删除

启动时只加载 `enabled=true` 的站点。创建、更新和删除后 handler 会自动执行 reload，
也可手工调用：

```bash
curl -X POST http://localhost:8080/api/v1/relay-stations/reload \
  -H 'X-Admin-Token: <admin-token>'
```

注意：

- reload 先从 Manager 移除全部已加载 relay，再查询数据库重建，不是原子替换；数据库
  查询失败会让 relay 暂时全部卸载；
- 单个站的 key 同步、协议 JSON 或构造失败只写日志并继续，总 reload 仍可能返回成功；
- Registry 没有注销 relay face。删除、禁用或改名后的旧 face 可能继续出现在
  `/api/v1/providers/registered`，但 `loaded=false`；
- UI 禁止改名。REST PUT 实际允许改名、模式和协议，却不会级联清理旧身份数据；把这些
  字段视为不可变，需要变化时删除后重建；
- PUT 除 `enabled` 省略和 `timeout_seconds <= 0` 外是整对象覆盖，不是 PATCH；
- 删除会清理 face 模型归属、路由顺序和站名/face 名下 Provider Key，但保留 vendor
  模型价格；
- 删除与三级清理不在同一事务。中途失败时站已删除、后续清理或 reload 可能未执行，
  需要修复原因后手工 reload 并清理残留。

## 排错

### 页面有记录，但 Provider 列表没有站点

1. 确认 `enabled=true`。
2. 确认使用 single + OpenAI/Anthropic，不含 Google。
3. 检查 `supported_protocols`/`keys` 是否是合法 JSON 字符串。
4. 检查网关日志中的 `[relay] Failed to register` 或 key sync 错误。
5. 检查名称是否与内置 face、其他站点或旧 multi face 冲突。
6. 修正记录后调用 reload；仅看到 CRUD 200 不能证明实例已经加载。

### `503 no available keys`

- key 必须至少 8 个字符；
- 检查末 8 位是否碰撞；
- 换 key 时不要复用旧后缀；
- 保存后检查 `provider_api_keys`/Provider 页面是否出现启用 key；
- multi 冷启动存在 pool 注入缺口，迁移成 single 站点。

### 上游 404 或模型同步路径错误

- Base URL 填 API 根路径，不填具体操作路径；
- 用 Access Log 和上游日志确认最终 `/v1/chat/completions`、`/v1/responses` 或
  `/v1/messages`；
- OpenAI endpoint 只有恰好以 `/v1` 结尾才会去重；
- Anthropic 模型端点可能不受支持，Messages 可用不代表模型同步一定可用；
- 清理过期 face 归属并重新同步，避免旧模型清单把站点错误纳入候选。

## 代码入口

- 表模型：`backend/internal/database/models.go` (`RelayStation`)
- CRUD Store：`backend/internal/database/relay_station_store.go`
- 管理 API：`backend/internal/api/http/handler/admin.go`
- 加载与 key 同步：`backend/internal/provider/relay/loader.go`
- 协议实现：`backend/internal/provider/relay/relay.go`
- 请求分发：`backend/internal/provider/relay/methods.go`
- URL 构造：`backend/internal/provider/openai_compatible/openai_compatible.go`、
  `backend/internal/provider/anthropic_compatible/anthropic_compatible.go`
- 模型路由：`backend/internal/router/router.go`
