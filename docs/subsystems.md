# 核心子系统

本文说明认证、Provider Key Pool、配额恢复和用量计费的当前实现。路由和请求主链见 `ARCHITECTURE.md`。

## 1. Gateway Key 认证

Gateway Key 用于客户端访问代理端点，与上游 Provider API Key 是两类凭证。只有 `auth.enabled=true` 时，代理路由才挂载认证和限流中间件。

### 1.1 加载与校验

启动时，配置中的 `auth.keys` 只作为初始 seed：按名称插入数据库，已存在的行不覆盖。随后 Authenticator 从 `gateway_keys` 加载到内存。

认证头优先级为：

1. `Authorization: Bearer <token>`
2. `x-api-key: <token>`

两者同时存在时使用 `Authorization`。数据库字段 `key_hash` 当前保存的是凭证明文；加载进 Authenticator 后才对原值做 SHA-256，并以内存 hash 索引完成请求校验。

认证成功后，中间件将完整 `GatewayKey` 和 ID 放入 Gin context。Proxy 使用这些字段执行：

- `Providers`：限制可用 vendor；比较按 vendor 归一，可覆盖同厂商多个协议面。
- `ProviderKeyIDs`：限制只能使用指定 `provider_api_keys.id`。
- `AllowedModels`：模型白名单；空列表或 `*` 表示不限制。
- `DefaultModel`：原模型无路由或白名单不允许时，尝试改写到默认模型；默认模型本身仍必须通过白名单。
- `RPM` / `TPM`：客户端限流配置。

### 1.2 限流实际行为

当前入口中间件只调用 RPM 检查，使用进程内一分钟滑动窗口。重启会清空计数，多实例之间也不共享。

TPM 计数器已经存在，但请求入口没有调用 `CheckTPM`，所以 TPM 当前不会拒绝请求。Proxy 只在成功响应拿到 usage 后追加实际 token；无 usage、失败候选和未完成的请求不计入 TPM。

### 1.3 CRUD 和当前限制

`/api/v1/keys` 的写操作完成后会从数据库重载整个 Authenticator，下一次请求立即使用新内存索引。当前实现有以下必须明确的限制：

- `LoadFromDB` 和 CRUD 后的 `reloadAll` 都没有过滤 `enabled`，Authenticator 也没有 Enabled 字段。因此数据库中 disabled 的 Gateway Key 当前仍可认证。
- `GET /api/v1/keys` 和 `PUT /api/v1/keys/:name` 当前仍通过 `toView` 返回 `key` 明文；虽然代码定义了不含 Key 的 `KeyViewSafe`，但这两个路径没有使用它。
- CRUD 重载路径没有把 `DefaultModel` 复制到内存对象；任意 Gateway Key CRUD 触发全量重载后，所有 Key 的默认模型会在内存中丢失，直到配置热重载或进程重启走 `LoadFromDB`。
- Repository 的 Update 字段表没有 `default_model`，所以 PUT 对默认模型的修改当前不会持久化。
- 数据库字段名虽为 `key_hash`，但没有静态加密或不可逆 hash；数据库读取权限等同于凭证读取权限。

这些是代码现状，不应在部署说明中把 `enabled`、TPM 或密钥隐藏描述为已经强制生效。

## 2. 管理员认证

管理员认证由 `admin_auth.enabled` 控制，与 Gateway Key 认证互不影响。

启用时：

- 启动阶段确保至少有一个 root 用户；没有 root 用户时会创建 `admin / Gateway@2026`。
- `/api/v1` 管理组使用 Admin Session 中间件。
- Session token 优先从 `X-Admin-Token` 读取，后备为 `session_token` HttpOnly cookie。
- 登录失败次数、账号锁定、用户 enabled、角色权限和密码 bcrypt 校验由 `adminauth.Manager` 处理。
- 登录路由始终注册，Manager 为空时返回 `feature_disabled`；登出、当前用户、改密和用户管理路由只在 Manager 存在时注册。

当前配置装配没有调用 `adminauth.DefaultConfig`。`server.New` 将 `session_ttl`、`max_login_attempts`、`login_ban_duration` 的配置值原样传入，因此只写 `enabled: true` 而省略这些值会使用 Go 零值：session 立即过期，登录锁定逻辑也退化。部署配置必须显式填写这些字段。

`session_cleanup_age` 当前没有运行时消费者，`CleanExpiredSessions` 也未由 Server 启动。过期 Session 在校验时不会通过，但旧行会继续留在数据库中。

## 3. Provider Key 与 Pool

### 3.1 数据来源和共享

上游凭证来自 `provider_api_keys`，字段 `key_hash` 当前同样保存明文。列表 API 只返回脱敏后的 `key_masked`，创建接口接收明文并落库。

Pool 构建规则：

- 只将 enabled 行加入运行时 Pool。
- 存储和构建按 vendor 归一；同 vendor 的 OpenAI/Anthropic 等注册面共享同一个 Pool。
- 每把 Key 带有 `billing_source`（`token_plan`、`api`、`free`）和可选协议列表。
- Provider Key CRUD 后按 vendor 重建 Pool，并把新实例原子替换给 Server、Router、Provider 和 QuotaCheck。
- 配置中的 `providers.*.keys` 不进入 Pool。

### 3.2 Key 状态机

运行时状态只有：

| 状态 | 是否参与调度 | 恢复方式 |
| --- | --- | --- |
| `ACTIVE` | 是 | 无需恢复 |
| `LIMITED` | 是 | 预留状态，当前没有主动迁移逻辑 |
| `COOLING` | 否，直到 `CoolingUntil` | Acquire 或 quota poll 发现到期后转回 ACTIVE |
| `QUOTA_EXCEEDED` | 否 | balance poll、probe 或真实成功恢复 |

没有永久 `DISABLED` 运行时状态。数据库的 `enabled=false` 通过“不把 Key 放入 Pool”实现，而不是状态机迁移。

错误反馈对状态的影响：

- `rate_limit`：按 `Retry-After` 冷却；没有该头时使用 `keypool.cooling_duration`。
- token-plan Key 连续进入冷却 3 次后升级为 `QUOTA_EXCEEDED`。
- 上述第 3 次 `rate_limit` 升级不会检查 balancer，也不会调用 `OnQuotaExceeded` 或
  `QuotaRecoveryProbe`。因此手工放入 token-plan 层且没有 balancer 的 Key 可能一直停在
  `QUOTA_EXCEEDED`，直到 Pool 重建、进程重启或其他外部恢复路径介入。自动管理的 relay Key
  当前固定属于 `api` 层，正常不会进入这个分支。
- `auth`：固定冷却 5 分钟。
- `quota_exceeded`：有 balance 恢复通道的 Pool 标记为 `QUOTA_EXCEEDED`；没有 balancer 的 Pool 在该分支只计数，避免进入无法恢复的永久死状态。
- `invalid_request`：只累计错误，不改变可用状态。
- `server_error`、`timeout`、`connection`：累计错误并送入该 Key 自己的 Circuit Breaker。
- 成功：累计请求并向 Circuit 报告成功；若 Key 当时是 `QUOTA_EXCEEDED`，恢复到 ACTIVE。

### 3.3 选择策略

Pool 先按状态、绑定 ID、协议、熔断、tier 和已知余额过滤，再交给 Scheduler。默认 Scheduler 是 `sticky`，始终选择过滤结果中的第一把 Key；它不保存上次请求位置。

显式策略行为：

- `round_robin`：在本次可用列表内轮询。
- `least_used`：选择 `TotalRequests` 最小者，平局取首个。
- `random`：随机选择。
- 未知值回退到 `sticky`。

默认 Key 顺序来自数据库 ID/创建顺序，Key 级 `route_order` 可覆盖前部顺序。余额数值不会改变 sticky 排序，只会在已轮询且耗尽时把 Key过滤掉。

### 3.4 状态快照

正常关停会保存：状态、冷却截止、耗尽时间、余额、轮询时间、余额类型、连续零余额次数和冷却次数。快照不包含 Key 明文。

启动恢复时：

- 未过期的 `COOLING` 会恢复。
- `QUOTA_EXCEEDED` 会恢复，并由 QuotaCheck 冷启动扫描重新入 probe 队列。
- 已过期冷却不恢复。
- ACTIVE 没有额外状态需要恢复。

## 4. QuotaCheck

QuotaCheck 同时维护两条恢复路径：

```text
定期 poll: 有 Balancer 的 Pool -> 拉余额 -> 更新 Remaining/状态
事件 probe: QUOTA_EXCEEDED -> 最小堆定时探测 -> 恢复或退避重排
```

当前 deepseek、minimax、mimo 的各协议注册面都注册了 Balancer。共享 Pool 按指针去重，每轮只 poll 一次该 vendor 的 Key。

### 4.1 balance poll

Poll worker 按配置间隔和 jitter 运行，遍历顺序为 `token_plan -> api -> free`，相邻 Key 之间等待 1 秒，避免集中请求余额接口。

每次成功查询会写入 `QuotaKind` 和 `LastPolledAt`：

- 有额度：写 `Remaining`，清空零余额 streak；若状态为 QE，恢复 ACTIVE 并清冷却计数。
- 无额度：只有 ACTIVE Key 连续两轮返回无额度，才覆盖 `Remaining` 并标记 QE；单次零值保留上一次已知正余额。
- 查询失败：保留原余额和状态，只记录 transport error 指标。

### 4.2 probe 调度

Key 进入 QE 时，回调把 `(provider, key-id)` 放入按 `nextAt` 排序的最小堆。Probe worker 每秒取出到期项：

- 优先调用该 Provider 的 Balancer。
- 没有 Balancer 时，使用专用 Prober；仍没有时按注册名后缀选择 Anthropic 或 OpenAI 协议默认 Prober。
- `restored`：恢复 ACTIVE 并从队列移除。
- `still_exhausted`：增加尝试次数，指数退避并加 jitter。
- `auth_failed`：不进入终端禁用，仍按退避重新调度。
- `transport_error`：不消耗 quota attempt，但仍退避重调度。

探测没有“尝试 N 次后永久禁用”的终态。

### 4.3 请求路径主动检查

Proxy 只在 token-plan 候选发生持续网络类错误、且换 Key 后仍无法确认时调用 `CheckQuota`。该检查只使用 Balancer：

- 明确无额度，才提供跨 tier 的 quota evidence。
- 有额度或无 Balancer，按“未证明耗尽”处理，不允许仅凭网络故障降档。
- 查询出错同样不作为耗尽证据。

`keypool.quota_enabled=false` 会停 poll/probe worker，但 Pool 的错误状态迁移仍存在；此时 QE Key 不会由后台自动恢复。

## 5. Usage 和计费

### 5.1 记录时机

Proxy 只为最终成功的非流式响应和已经提交的流式响应写 Usage。失败的中间候选不会写 usage row，但会写 metrics 和 access-log 失败尝试。

流式响应在发出 HTTP 200 后即进入“已提交”语义；即使后续标记 `stream_interrupted`、`upstream_stream_error` 或 idle timeout，仍会生成一条 usage row。是否有 token 取决于上游是否在流结束前提供 usage。

记录字段包括：

- trace、Gateway Key、Provider 注册名、模型和协议。
- billing source；优先使用实际 Key 的值，后备为 Provider 默认值。
- 未缓存输入、缓存读取、缓存创建、输出和总 token。
- 成本、总延迟、流式 TTFT、状态码和错误类型。

上游响应包含真实模型名时，Usage row 的 `model_id` 会使用上游值；价格查找仍使用路由候选的 `result.ModelID`。

### 5.2 Token 统一口径

新记录的统一契约是：

```text
PromptTokens 与 CacheReadTokens 互斥，不重叠
PromptTokens + CacheReadTokens + CacheCreationTokens + CompletionTokens ~= TotalTokens
```

OpenAI compatible 上游通常把缓存 token 包含在 prompt/input 中，解析器会扣除缓存；Anthropic compatible 的 input 本来不含 cache，直接使用。历史数据存在多种旧口径，查询层通过 `usage/tokensplit.go` 的 SQL 表达式逐行归一，明细和聚合共用同一规则。

### 5.3 费用计算

价格来自 `provider_models`，单位为 CNY/百万 token：

```text
cost = uncached_input / 1_000_000 * input_price
     + cache_read    / 1_000_000 * cache_read_price
     + output        / 1_000_000 * output_price
```

`CacheCreationTokens` 当前保留在记录中，但不参与费用公式。模型没有任何价格时费用为 0。

### 5.4 异步写入边界

- Collector channel 固定容量 1024，`Record` 非阻塞；满时打印 warning 并丢记录。
- 达到 batch size 或 flush interval 后批量写库。
- 单批数据库错误最多尝试 3 次；三次仍失败后丢弃该批。
- 正常关停会 flush 当前 batch。
- `usage.retention_days` 当前没有消费者，不会自动删除旧 usage rows。

因此 Usage 是异步 best-effort 账本，并非严格事务式计费流水。

## 6. 子系统协作边界

一次成功请求的主要反馈链如下：

```text
Router 选中 Key
  -> Provider 发请求
  -> Pool.ReportSuccess / ReportError
  -> per-key Circuit 与 quota 状态更新
  -> Proxy 写响应
  -> Metrics 同步计数
  -> Usage 非阻塞入队
  -> TPM 仅追加实际 token
  -> AccessLog metadata 非阻塞入队
```

这些子系统不共享一个数据库事务。上游请求成功不保证 usage/access metadata 一定落库；反过来，观测写入失败也不会改变已经发给客户端的响应。
