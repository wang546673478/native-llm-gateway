# 架构总览

本文描述当前代码的运行时架构。实现行为以 `backend/` 下的 Go 代码和数据库模型为准；配置项是否存在，不等于运行时一定消费该配置项。

## 1. 进程模型与启动顺序

Gateway 是单个 Go 进程。入口是 `backend/cmd/gateway/main.go`，启动顺序如下：

```text
config.Load(config.yaml)
  -> 初始化 zap logger
  -> database.Open + database.Migrate
  -> provider.Registry + provider.Manager.LoadFromConfig
  -> relay.LoadFromDatabase
  -> adminauth.EnsureRootUser（仅 admin_auth.enabled=true）
  -> server.New
  -> config.Watch
  -> server.Run
```

`server.New` 完成运行时装配：

1. 从 `provider_api_keys` 构建 Provider Key Pool；同一 vendor 的多个协议注册面共享同一个 Pool。
2. 从 `key-state.json` 恢复可恢复的 Key 状态、余额和冷却信息。
3. 创建 Router，并加载数据库中的 Provider 层、Key 层路由顺序。
4. 当 `auth.enabled=true` 时，将配置中的初始 Gateway Key 按名称 seed 到数据库，再从数据库构建内存 Authenticator。
5. 创建 Usage、Metrics、AccessLog、Inflight、Fingerprint 和 Proxy Engine。
6. 将共享 Pool 注入 Provider 实例。
7. 从 `provider_models`、`provider_model_faces` 加载模型归属、默认顺序和价格。
8. 创建 QuotaCheck Manager 和可选的 AdminAuth Manager。

`server.Run` 注册 HTTP 路由，并启动三个后台子系统：

- Usage 批量落库 worker。
- AccessLog metadata 批量落库和 retention worker。
- QuotaCheck 的 balance poll 与 quota probe worker。

## 2. 运行时组件

| 组件 | 主要职责 | 主要状态来源 |
| --- | --- | --- |
| `server` | 顶层装配、HTTP 路由、热重载和关停 | 配置、数据库 |
| `provider.Registry` | Provider 工厂及 vendor/relay 元数据 | 内置注册、Relay 动态注册 |
| `provider.Manager` | Provider 实例、endpoint、协议、模型和价格查询 | 配置、模型表、Relay 表 |
| `router` | alias/catch-all/真实模型解析，生成有限候选迭代器 | 配置、Manager、Pool、`route_order` |
| `keypool` | 上游 Key 的分层、过滤、选择和状态机 | `provider_api_keys`、内存状态、快照 |
| `proxy.Engine` | 请求解析、候选尝试、重试、failover、响应转发 | Router、Provider、横切接口 |
| `quotacheck` | 余额轮询和耗尽 Key 恢复探测 | Pool、Provider balancer/prober |
| `auth` / `adminauth` | 客户端凭证与管理端 session | 数据库、内存索引 |
| `usage` / `accesslog` / `metrics` / `inflight` | 用量、接入日志、指标和活跃请求 | 内存队列、数据库、body 文件 |

依赖关系由 `server` 汇合。Router 只依赖 Provider 查询接口；Proxy 通过窄接口调用 quota、usage、metrics 等子系统，避免这些横切模块反向依赖代理实现。

## 3. Provider、vendor 和数据边界

### 3.1 注册面与 vendor

当前内置加载点只注册 `deepseek`、`mimo`、`minimax` 三个厂商。一个厂商可以暴露 OpenAI、Anthropic 等多个“注册面”，每个注册面有自己的协议和 endpoint，但同 vendor 的注册面共享上游 Key Pool。

OpenAI、Anthropic、Google compatible 包是协议实现，不代表对应厂商一定已启用。当前没有内置 Google 厂商，Server 也没有可用的 Google 对外代理链路；动态 Relay 只实现 OpenAI 和 Anthropic。Relay Station 从 `relay_stations` 动态注册，不需要新增硬编码 Provider 包。

### 3.2 配置与数据库各自负责什么

- `config.yaml` 的 `providers` 决定内置注册面的启用、endpoint、协议、超时、计费来源及部分能力开关。
- Provider API Key 的权威来源是 `provider_api_keys`。`providers.*.keys` 即使出现在配置结构中，也不参与 Pool 构建。
- vendor 级模型与价格来自 `provider_models`。
- 注册面可服务的模型及其顺序来自 `provider_model_faces`。
- Relay 配置来自 `relay_stations`；Relay 模型清单尚未同步时按通配处理，同步后才按客户端模型过滤。
- Provider 层和 Key 层人工排序来自 `route_order`。

## 4. HTTP 入口

固定公开端点包括：

- `GET /healthz`
- `GET /readyz`
- `GET /metrics`
- `/api/v1/*` 管理 API
- `POST /v1/chat/completions`
- `POST /v1/messages`
- `POST /v1/completions`（仅注册兼容入口；没有 legacy Completions 专用转换）
- `POST /responses` 和 `POST /v1/responses`
- 其他 `POST /v1/*` 由 `NoRoute` 交给 Proxy；Provider 仍会重建其支持的资源路径，不是
  任意路径透明透传

当配置了 `server.static_dir` 时，未匹配的 `GET`/`HEAD` 请求由 Go 进程提供静态文件，并回退到 `index.html`。未配置时返回 JSON 404。

当客户端认证启用时，所有代理入口依次经过 Gateway Key 认证和 RPM 限流。管理认证是独立开关：启用后 `/api/v1` 管理组使用 Admin Session 中间件。登录端点始终注册；登出、当前用户、改密和用户管理端点只在管理认证启用时注册。

## 5. 一次代理请求的完整路径

```text
Gin route
  -> 可选 Gateway Key auth + RPM limit
  -> 取得或生成 X-Request-Id
  -> 读取 body
  -> /responses 请求剥离跨厂商 reasoning 块
  -> 写 access-log 请求 body
  -> 从 body 提取 model 和 stream
  -> 解析 alias，必要时重写 model
  -> Fingerprint Sanitize
  -> 建立 inflight 快照
  -> Router.Route 生成有限候选迭代器
  -> Gateway Key 的 provider/model/key-ID 约束过滤
  -> 逐候选选择具体 Key 并调用 Provider
  -> 成功响应或错误/failover
  -> metrics、usage、access-log 收尾
  -> 删除 inflight 快照
```

有几个边界需要注意：

- `stream` 以 JSON body 中的值为准，不以调用了哪个 Handler 为准。
- `/responses` 的 reasoning 清理发生在请求 body 落 access 文件之前；alias、候选模型和 fingerprint 改写发生在其后。因此 access 文件不是所有场景下“发往上游的最终字节”。
- alias 短格式解析和候选选择都可能重写 `model`；每次 failover 到不同候选时还会再次按该候选的真实模型重写。
- Router 已经选出的 Key 会绑定到 `provider.Request`，Provider 不应再次从 Pool 获取另一把 Key。

## 6. 路由解析

### 6.1 匹配优先级

Router 对请求模型按以下顺序处理：

1. 命中 `routing.aliases`：使用该 alias。
2. 未命中 alias，但配置了 `routing.catch_all`：始终使用 catch-all；即使客户端给的是 Provider 已声明的真实模型，也不会走真实模型自动发现。
3. 未配置 catch-all：按客户端模型在所有注册面中自动发现。

`catch_all: {}` 是有效配置，表示自动纳入所有协议匹配的已启用 Provider，而不是“未配置”。

### 6.2 alias 与 catch-all 形态

- `target_model` 或 alias 短格式：找到声明目标模型且协议匹配的注册面。
- `providers`：使用显式 Provider/模型列表。
- `chain_ref`：先展开共享链，再按显式列表处理。
- 空 catch-all：内置厂商使用默认模型，或从 Gateway Key 白名单中选择第一个已声明模型；Relay 始终以客户端模型为准。

请求路径决定协议面：`/v1/messages` 对应 Anthropic，`chat/completions` 和 `responses` 对应 OpenAI。协议检测代码也能识别 Google generate-content 路径，但当前没有已注册的内置 Google 厂商，动态 Relay 构造器同样拒绝 Google，因此该路径没有可用上游链路。内置 Provider 处理 `/responses` 时还要求声明原生 Responses 能力；Relay 因能力未知而宽松放行。

Gateway Key 可以进一步限制：

- vendor/Provider 范围；比较时按 vendor 归一，因此绑定 `deepseek` 可覆盖该 vendor 的多个协议面。
- 具体 `provider_api_keys.id` 集合。
- 模型白名单和默认模型。

### 6.3 Provider 排序

显式 alias/catch-all 列表先应用策略：

| 策略 | 当前行为 |
| --- | --- |
| `priority` | `priority` 数字升序，默认策略 |
| `weight` | 按权重随机选一个候选置顶，其余保持原顺序 |
| `cost` | 当前仅按 `weight` 字段升序，并未读取模型价格 |
| `health` | 当前保持输入顺序；实际健康过滤发生在 Key Pool |

空 catch-all 自动模式会在每个计费层内应用数据库 Provider 顺序；没有人工顺序时，按该注册面最早 enabled Key 的创建时间排序，最后按名称稳定兜底。

当前限制：Provider 层 `route_order` 只在空 catch-all 自动模式中消费。真实模型自动发现和短 alias 自动发现从 Manager 的 map 枚举候选，没有额外稳定排序；显式链则以配置策略为准。

## 7. 计费层和 Key 调度

Router 把 Provider 候选展开为 `(provider, tier)` 候选，再统一按以下层级拉平：

```text
token_plan -> api -> free
```

每个候选只从指定 tier 获取 Key，不在 Pool 内自行跨层。Pool 依次过滤：

1. 运行时状态是否可用。
2. Gateway Key 绑定的 Provider Key ID。
3. 排除刚失败的 Key（仅换 Key 重试路径）。
4. Key 的协议限制。
5. per-key circuit 是否允许。
6. billing source 是否属于当前 tier。
7. 已轮询且确认耗尽的余额。

Key 默认按数据库创建/ID 顺序进入 Pool；`route_order` 的 Key 顺序可将指定 Key 提前，未列出的 Key 保持原相对顺序并排在其后。

### 7.1 sticky 的准确语义

`keypool.key_rotation` 为空时默认是 `sticky`。它没有“上次成功 Key 游标”，也没有成功后的冷却时间；每次选择都返回当前过滤结果中的 `keys[0]`。

- 最高位 Key 健康时，后续新请求持续使用它。
- 最高位 Key 处于 `COOLING`、`QUOTA_EXCEEDED`、熔断 `OPEN`、余额耗尽或不满足绑定条件时，才选择下一把。
- 高位 Key 恢复后会重新成为 `keys[0]`，下一次请求自动回到它。

因此，观察到请求继续使用较低位 Key，意味着更高位 Key 在那次 Acquire 时被过滤，而不是调度器记住了“上次成功位置”。显式可选策略还有 `round_robin`、`least_used` 和 `random`。

## 8. 错误、重试和 failover

Provider 将错误归类为 `rate_limit`、`quota_exceeded`、`auth`、`invalid_request`、`model_not_found`、`server_error`、`timeout`、`connection` 等。Proxy 基于分类和 HTTP 状态决定下一步。

当前主要规则：

- 纯 HTTP 429 `rate_limit`：首次失败后，同一把 Key 最多再尝试 10 次；仍为 429 时尝试同 Provider、同 tier 的另一把 Key一次，然后继续下一候选。
- 非 429 的 `rate_limit`（例如某些 403 或 200 内嵌错误）：不在原 Key 上循环，立即尝试另一把 Key一次，再继续下一候选。
- `connection`、`timeout`、`server_error`：同 Provider、同 tier 换另一把 Key重试一次。
- `auth`：该 Key 冷却，并换另一把 Key重试一次；仍失败可继续同层其他候选。
- `quota_exceeded`：token-plan 层产生“额度耗尽证据”；API/free 层的额度或限流错误不会成为跨层证据。
- `invalid_request`、`model_not_found` 通常终止；当它只说明某个候选模型/Relay 不适配时，可以继续下一候选。
- 客户端断开不重试。

跨 tier 不是普通错误的无条件 failover。当前循环在一个 tier 有实际失败但没有额度证据时直接收尾；token-plan 层出现额度证据后，才允许进入下一 tier。纯白名单跳过没有真正发请求，可以直接越过空层。

`retry.max_attempts` 是“每个 tier 实际尝试的候选数”预算：

- `0` 表示不人为截断，尝试该层有限迭代器中的全部候选。
- 正整数表示达到预算后跳过该层剩余候选；同一候选内部的 429 循环和换 Key 不额外消耗候选预算。

当前 `retry.enabled`、`retry.no_failover_on`、`retry.failover_on` 没有运行时消费者，不能依赖它们改变上述决策。

## 9. 流式响应的提交边界

流式 failover 只在客户端尚未收到 HTTP 200 和响应字节时可行：

1. OpenAI/Anthropic compatible Provider 在返回 channel 前同步读取前两行，识别流头的结构化错误。命中时直接返回 `ProviderError`。
2. Proxy 再读取一个 chunk。该 chunk 的 `Err` 发生在写 200 前，仍可进入普通 failover。
3. 第一个 chunk 可用后，Proxy 上报 Key 成功、写 SSE headers 和 HTTP 200。此后不能透明切换 Provider。

提交后的行为：

- `chunk.Err`：向客户端写 SSE error，access log 标记 `stream_interrupted`。
- 流中的结构化上游 error event：原样转发，标记 `upstream_stream_error`；不冷却 Key，也不 failover。
- 客户端写失败：标记 `client_disconnected`，不 failover。
- request context 结束：标记 `context_canceled`。
- 长时间无 chunk：写 `stream_idle_timeout` SSE error 并结束。

空闲超时由请求 body 大小硬编码分段为 10、15、20、30、45 秒。Engine 虽持有 `StreamIdleTimeout` 字段，但当前 `calculateIdleTimeout` 不读取它；动态 timer 也只在第一个 Proxy 缓冲 chunk 已处理并发送 200 后启动。

## 10. 热重载边界

配置文件变化后，`Server.Reload` 当前会更新：

- aliases、chains 展开结果、catch-all 和默认路由策略。
- Manager 内的 billing source 和 Responses 能力。
- 从数据库重新加载模型、模型面和价格。
- 已存在 Authenticator 时，从数据库重载 Gateway Key。
- QuotaCheck 的 enabled、poll、probe、jitter、timeout 等配置。
- `s.cfg` 指针；后续由管理 API 触发 Pool 重建时会使用新 KeyPool 配置。

需要重启或显式重建才能完整生效的内容包括：

- HTTP host/port/timeouts、静态目录、数据库和 AccessLog/Usage 构造参数。
- Provider 实例、endpoint、Provider timeout、`force_thinking_disabled` 和现有 Pool/Circuit 实例。
- `retry.max_attempts` 和 fingerprint 的配置文件值。
- `auth.enabled` / `admin_auth.enabled` 的中间件拓扑。
- Key rotation、cooling、circuit 等 Pool 配置不会因文件变化立即重建；Provider Key CRUD 或重启后才应用到新 Pool。

Relay、Provider Key、路由顺序、模型同步等管理 API 有各自的运行时重载回调，不依赖配置 watcher。

## 11. 优雅关停

收到 `SIGINT` 或 `SIGTERM` 后，正常分支按以下顺序关停：

1. 停止 QuotaCheck 后台生产者。
2. 调用 `http.Server.Shutdown`，在 `server.shutdown_timeout` 内排空在飞 HTTP 请求。
3. 停止并 flush Usage Collector。
4. 关闭 AccessLog Recorder，排空 metadata buffer 并停止 retention。
5. 保存 Key 状态快照；SQLite 使用数据库同目录，PostgreSQL 使用当前工作目录下的 `key-state.json`。
6. 返回入口后关闭 Provider Manager 和数据库连接。

这个顺序保证在飞请求先结束，再关闭其 usage/access-log 消费者。快照只保存运行时状态和余额，不包含上游 Key 明文。
