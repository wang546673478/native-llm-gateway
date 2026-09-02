# 动态中转站接入指南

动态中转站用于接入标准 OpenAI 或 Anthropic 兼容上游，不需要新增 Go 厂商包。配置
保存在数据库 `relay_stations`，启动和管理操作会动态注册 Provider。

本文以当前代码为准。旧文档以 TokenMarket 为例，但它只是一个普通站点实例；网关没有
TokenMarket、RightAPI 或其他商业站点的硬编码配置。

## 适用范围

当前可靠范围是：

- `protocol_mode: single`；
- `primary_protocol: openai` 或 `anthropic`；
- 标准 Bearer（OpenAI）或当前适配器默认的 `x-api-key`（Anthropic）鉴权；上游文档若要求
  Anthropic Bearer，必须先在受控环境验证/补充 auth-mode 配置，不能在透传层盲目替换；
- Chat Completions、Responses、Messages、Models，以及保持原始响应字节的 SSE 流。

以下场景应写[内置厂商包](provider厂商定制包指南.md)，而不是强塞进 relay：专属鉴权、
非标准 URL、请求/响应改写、厂商专属错误分类、非标准 usage 或余额 API。

## 先看当前限制

### 只推荐 single 模式

数据库和 UI 有 `multi` 模式，工作树已增加 `ProtocolFor` 和协议作用域 face wrapper，并已
补齐基础的 face protocol 过滤、face→vendor pool/endpoint 回退、未知 path 门禁、严格协议
选择和 Base pool 原子替换。`multi_relay_protocol_test.go`、`pool_fallback_test.go` 和
`unknown_path_test.go` 已覆盖这些本地行为（包括未知 path 不产生上游请求），但这些只代表
本地验证，尚未形成可生产承诺的双协议契约：

- 每个 `name-protocol` face 仍可能复用同一个 `GenericRelayProvider` 和 vendor pool；共享
  pool 的状态反馈、quota worker 热重载和删除/重新加入需要真实 DB 集成验证；
- face protocol 和未知 path 的本地门禁已存在，但完整 DB 增删、热重载、并发请求和真实上游
  生命周期仍需验证，不能把单元/集成测试结果当成线上支持承诺；
- 各 face 的模型清单、key protocol/tier/ID 约束、quota endpoint 生命周期和并发行为仍需
  通过真实 DB/并发压力与受控灰度验证；当前工作树的全量 race 已通过，但不代表生产验收。

一个上游同时提供 OpenAI 和 Anthropic 时，在 P7 完成前应创建两个名称不同的 single 站点，
分别配置各自 base URL 和 key。不要依赖尚未验收的 multi 模式。

### Google 尚不可用

前端仍显示 Google 选项，但 relay 构造器明确返回
`google protocol not yet supported for relay stations`。single 选择 Google 或 multi 列表
包含 Google，整个站都不会加载。

### `billing_source` 运行语义

`relay_stations.billing_source` 的受支持运行时值是 `token_plan`、`api`、`free`，会在动态
加载时登记到对应 face/vendor，并作为**新建** `provider_api_keys` 记录的默认值；未填写时
默认 `api`。注意 relay CRUD 当前对非空 station 值没有与 Provider Key API 同等严格的枚举
校验，直接写入其他字符串可能导致该 station 没有可匹配的候选，应在保存前自行校验。
KeyPool 调度以每一把 key 自身的 `BillingSource` 为准，multi face 共享 vendor pool 但仍保留
计费面、协议和允许 key 集合约束。已有同名（由末 8 位生成）的 key 行不会因站点 reload 或
重复同步自动更新 `key_hash` 或 `BillingSource`；变更旧 key 的 tier 必须显式编辑/删除后重建，
再核对运行时池和候选顺序。当前 Provider Keys 页面没有 relay key 编辑入口，生产数据库变更
必须走备份、审批和维护窗口。

该字段只决定 Gateway 的候选层，不改变上游协议、鉴权方式或请求 body；站点若实际把不同
tier key 绑定到不同 endpoint，仍需在受控环境验证每个 key 的可用性。

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
| 计费来源 | `api` | 有效值为 `token_plan`、`api`、`free`；作为 station 默认层，实际调度看每把 key |
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

relay 不是任意 HTTP 反向代理。它根据协议重建 URL、替换上游鉴权并过滤连接级 header，
除此之外按本文“透明透传契约”转发客户端请求和上游响应。

### OpenAI

| Base URL | Chat 上游 URL | Responses 上游 URL | Models 上游 URL |
|---|---|---|---|
| `https://host` | `https://host/v1/chat/completions` | `https://host/v1/responses` | `https://host/v1/models` |
| `https://host/v1` | `https://host/v1/chat/completions` | `https://host/v1/responses` | `https://host/v1/models` |
| `https://host/prefix/v1/` | `https://host/prefix/v1/chat/completions` | `https://host/prefix/v1/responses` | `https://host/prefix/v1/models` |

去重只识别清理尾斜杠后**恰好以 `/v1` 结尾**的 endpoint。Base URL 不要写到
`/chat/completions`、`/responses` 或 `/models`。OpenAI 鉴权使用
`Authorization: Bearer <key>`；relay 不会为流式请求注入
`stream_options.include_usage=true`。

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

Anthropic 鉴权使用 `x-api-key`。客户端已有 `anthropic-version` 时保持原值，缺失时才补
`2023-06-01`。模型同步先尝试
`{base_url}/api/models` 的 New API 返回形状，再尝试标准 `/v1/models`；两个端点都不
可用时同步失败，但手工已有的 vendor 模型不会被自动清空。

## 透明透传契约

透明性按每一个实际 relay 候选判断，与 Gateway Key 是纯 relay 绑定还是混合绑定无关。
客户端请求进入 Proxy 后会先保存不可变快照；每个候选都从该快照独立派生。

relay 请求保证：

- entity body 与 Gateway 入站 body 逐字节一致，不清理 `/responses` reasoning，不改
  `model`、thinking、metadata、Environment 或其他业务字段；全局 fingerprint 默认开启也
  不作用于 relay；
- 保留 raw query，以及 `Content-Type`、`Accept`、`User-Agent`、`anthropic-beta`、
  `anthropic-version` 和未知端到端多值 header；协议默认值只在客户端缺失时补充；
- 删除 hop-by-hop header、`Host`、`Content-Length`、客户端 Gateway 鉴权、Cookie、管理
  token 和不可信转发链 header，再安装当前 relay key；Gateway Key 绝不能发给第三方；
- HTTP Transport 禁止隐式 gzip 解压，使 `Content-Encoding` 与 body 字节保持一致。

relay 响应保证：

- 非流式和流式响应保留上游 HTTP status、过滤后的端到端多值 headers 和 body；
- SSE 以原始字节为主通道，`: PING`、注释、CRLF、未知 event 和 data 原样转发。usage 和
  错误观察器只读取字节副本，不得修改输出；
- 上游已有 request ID 时保持原值，缺失时才补 Gateway trace ID；
- 候选耗尽且最后一个 relay 已返回 HTTP response 时，原样返回它的 status、headers 和
  error body。只有 DNS、TLS、连接或 deadline 等没有 HTTP response 的错误才由 Gateway
  生成 502/504；
- relay 的 HTTP 200 内嵌结构化错误保持 HTTP 200 和原始 body，不套用内置厂商的专属错误
  分类。需要这种适配时应实现内置厂商包。

传输分块、HTTP/2 DATA frame 和 TCP packet 边界可以变化；验收的是 HTTP entity body
拼接后的字节序列，不是底层包边界。

## Key 同步规则

启用站点每次加载时，以 `relay_stations.keys` 为权威同步
`provider_api_keys`：

- key 名取明文最后 8 个字符；少于 8 个字符会被静默忽略；
- 后 8 位相同的 key 会碰撞，只保留一个目标；
- 已存在与目标同名的行不会更新 `key_hash`。只改 key 前缀、保留相同后 8 位时仍会用
  旧 key；
- 已存在与目标同名的行也不会更新 `BillingSource`。修改 station 默认值后，旧 key 仍留在
  原计费层，需显式编辑/删除重建才能改变其 tier；
- 不在 station JSON 中的站名下 key 会被删除；在 Provider Keys 页面手工添加的 relay
  key 也可能在下次 reload 被删；
- 删除和新增不是一个事务，单条失败只写日志；
- 自动新增的 key 使用 station 的 `billing_source`（为空时为 `api`），`protocols` 为空
  （所有协议可用）。

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

- relay 的候选层最终由每把 key 的 `billing_source` 决定；station 默认值只用于 face/vendor
  元数据和新同步 key。全局层级仍是 `token_plan -> api -> free`，同一站的两把 key 可能因
  tier 不同而落在不同层。
- 同层 Provider 顺序由 `route_order` 改写；未改写站点按最早 key 创建时间排序，并排在
  已有显式改写之后。
- 站内 key 使用全局 KeyPool 调度和冷却配置。relay 没有 balancer；自动同步的新 key 使用
  station 的 `billing_source`（为空时为 `api`），明确 `quota_exceeded` 时只计数，后续
  请求会重新试探。同站第二 key 只有在同 tier、同 protocol、Gateway Key 允许且状态可用时
  才是当前候选；不满足条件会按原规则进入下一 Provider，而不是“有两把 key 就必然切换”。
- 旧数据或手工数据若把 relay key 标为 `token_plan`，连续第 3 次 `rate_limit` 会绕过上述
  probe 保护而直接进入 `QUOTA_EXCEEDED`，且不触发后台 probe 回调。这种异常数据可能要
  通过重启、重建 Pool 或外部状态恢复，不能依赖自动探测。
- 动态 relay 不在 `config.providers` 中，目前不会取得 per-key circuit breaker 配置；
  它仍会进行错误上报、冷却和候选 failover。
- `timeout_seconds` 控制非流式单次请求。relay 运行时兜底是 400 秒；OpenAI/Anthropic
  流式上游 HTTP client 的超时下限是 600 秒，服务端 `write_timeout` 仍是外围限制。
- `retry.relay_first_byte_timeout` 控制单个 relay 流候选从开始请求到首个非空原始正文
  chunk 的预算，默认 180 秒。超时发生在 headers 前或 headers 后正文静默时，当前尝试会
  关闭并按原路由规则切换；该配置需要重启生效。
- 一旦收到任意原始正文，包括 `: PING`，Proxy 就提交上游 status/headers/字节。此后不能
  再切换候选；上游中途断流只记录 `stream_interrupted`，不会插入 Gateway SSE error。
- 客户端取消立即终止整条候选链，不冷却 key、不计入 circuit，也不尝试下一把 key 或下一
  Provider。上游仍可能把这次真实断开记录为 `client_gone`，但 Gateway 会标为
  `client_disconnected`，而不是伪装成多次 `connection` 和最终 502。

因此透明透传不会耽误正常路由切换：响应尚未提交时仍可按首包预算和现有错误规则切换；
收到 ping/data/body 后不再切换是 HTTP 响应已经对客户端生效的必要边界。

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
- multi 虽已有本地 face/pool 修复，但尚未完成生产矩阵验收；生产排障仍建议迁移成 single
  站点，或先禁用该 station。

### 第二把 key 没有被尝试

先按同一 trace 检查候选顺序和每次的 Provider Key ID，再核对两把 key 是否同时满足：

1. 属于同一个 provider/vendor 池，且 `enabled`、runtime status 和 circuit 都可用；
2. `BillingSource`（计费层）与首 key 相同；
3. `Protocols` 与请求路径协议匹配，且 Gateway Key 没有限制掉第二把 key；
4. 没有发生父请求取消、deadline、首字节已提交或共享候选预算耗尽。

如果首 key 返回明确 `quota_exceeded`，修复后的 Proxy 会先在上述集合中排除首 key 并补试一把，
成功后不再调用下一 Provider；两把 key 不同 tier、协议不匹配或不在允许 ID 集合时，第二把
不会被强行调用。2026-08-30 线上旧构建在 direct-quota 分支会直接跳到下一 Provider，若日志中
只出现首 key，应先确认运行二进制版本，而不要据此断定第二把 key 无效。

注意：站点默认 `billing_source` 变更不会自动改写已存在的同后缀 key。需要改变旧 key 的
计费层时，显式更新/删除并用新后缀重建，然后 reload 并从 Access Log 核对实际候选。

### 上游 404 或模型同步路径错误

- Base URL 填 API 根路径，不填具体操作路径；
- 用 Access Log 和上游日志确认最终 `/v1/chat/completions`、`/v1/responses` 或
  `/v1/messages`；
- OpenAI endpoint 只有恰好以 `/v1` 结尾才会去重；
- Anthropic 模型端点可能不受支持，Messages 可用不代表模型同步一定可用；
- 清理过期 face 归属并重新同步，避免旧模型清单把站点错误纳入候选。

### 上游显示 `client_gone`

- 先按 trace ID 对齐 Gateway 候选开始/结束、TTFT、`upstream_started` 和 Access Log，不要只
  按秒级时间戳归因；中转站延迟落日志可能与后一条 Gateway 请求相邻。
- `client_gone` 表示 relay 的直接 HTTP 客户端已断开，该客户端可能是 Gateway 或前置
  Cloudflare，不一定是最终调用方；
- 检查 `error_type=client_disconnected`、`first_byte_timeout`、403/524 和候选扇出。客户端
  取消之后不应再出现新的 `candidate_attempt`；status 0 也不能单独证明该 key 无效，需看
  `upstream_started` 以及 URL/DNS/TLS/dial 阶段。
- 测首字时主动终止 curl 会制造预期的 `client_gone`。成功判定应完整读取响应，或明确把
  该次记录标成诊断主动中止。

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
