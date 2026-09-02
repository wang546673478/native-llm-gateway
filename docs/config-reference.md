# 配置与数据

本文以 `backend/internal/config/config.go`、`backend/internal/server/server.go` 和
`backend/internal/database/models.go` 为准。可运行模板见根目录
`config.example.yaml`（本机/SQLite）与 `config.docker.example.yaml`（容器/PostgreSQL）。

## 配置来源

进程只读取启动参数指定的 YAML：

```bash
./bin/gateway --config ./config.yaml
```

`config.yaml` 被 `.gitignore` 排除，可包含 DSN 和控制台 cookie。上游 API key 和通过
管理页面创建的 Gateway Key 以数据库为权威；YAML 中 `auth.keys` 只在启动时补种不存在的
Gateway Key，不覆盖数据库已有同名记录。

## 最小配置

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 600s
  idle_timeout: 120s
  shutdown_timeout: 30s
  static_dir: frontend/dist

database:
  driver: sqlite
  dsn: data/gateway.db

auth:
  enabled: true
  keys:
    - name: bootstrap
      key: gw-change-me
      allowed_models: ["*"]
      rate_limit: { rpm: 100, tpm: 500000 }

providers:
  deepseek:
    enabled: true
    endpoint: https://api.deepseek.com
    protocol: openai
    billing_source: api
    timeout: 60s
    responses_api: true

routing:
  catch_all: {}
  default_strategy: priority

keypool:
  key_rotation: sticky
  cooling_duration: 60s
  quota_enabled: true

timeouts:
  provider_default: 60s

retry:
  max_attempts: 0
  relay_first_byte_timeout: 180s

usage:
  flush_interval: 10s
  batch_size: 100

admin_auth:
  enabled: true
  session_ttl: 168h
  max_login_attempts: 5
  login_ban_duration: 15m
```

## 当前生效字段

### `server`

| 字段 | 语义 |
|---|---|
| `host`, `port` | HTTP 监听地址；修改后需重启 |
| `read_timeout` | `http.Server.ReadTimeout` |
| `write_timeout` | 非流式响应的绝对写上限；流式写入会按 chunk 续期 |
| `idle_timeout` | HTTP keep-alive 空闲超时 |
| `shutdown_timeout` | SIGINT/SIGTERM 后等待请求排空的时间 |
| `static_dir` | 前端 `dist` 目录；为空时不托管 UI，未命中文件走 SPA fallback |
| `access_log.*` | 接入日志异步队列、body 目录和保留期；修改后需重启 |

`access_log` 的运行时零值兜底为：`body_dir=./data/access`、`buffer_size=10000`、
`batch_size=100`、`flush_interval=1s`、`retention=24h`。每个 body 文件最多 16 MiB；
超过时写截断文件。JSONL 是导出格式，不是磁盘 body 的存储格式。

### `database`

| 字段 | 语义 |
|---|---|
| `driver` | 只接受 `sqlite` 或 `postgres` |
| `dsn` | SQLite 文件路径或 PostgreSQL DSN，必填 |
| `max_open_conns`, `max_idle_conns` | `database/sql` 连接池大小 |
| `conn_max_lifetime` | 连接最大复用时间 |

示例：

```yaml
database:
  driver: postgres
  dsn: "host=postgres user=gateway password=CHANGE_ME dbname=gateway port=5432 sslmode=disable"
  max_open_conns: 25
  max_idle_conns: 10
  conn_max_lifetime: 300s
```

### `auth`

这是代理端点的 Gateway Key 鉴权，不是管理后台登录。

- `enabled=true` 时代理接受 `Authorization: Bearer <key>` 或 `x-api-key: <key>`。
- `keys` 是启动种子；页面/API 创建的记录保存在 `gateway_keys`。
- `allowed_models` 支持 `"*"`，也参与自动路由的真实模型选择。
- Gateway Key 还能在数据库中绑定 vendor、具体 `provider_api_keys.id` 和
  `default_model`；这些字段通过管理页面维护，不在 YAML 种子结构中声明。
- `rpm` 当前强制执行；`tpm` 会记录实际消耗，但请求入口暂未调用 TPM 拒绝检查。

### `providers.<name>`

| 字段 | 语义 |
|---|---|
| `enabled` | 是否在启动时实例化该注册面 |
| `endpoint` | 上游 base URL；协议实现负责去重末尾 `/v1` 和资源路径 |
| `protocol` | `openai`、`anthropic` 或 `google` |
| `timeout` | 该注册面的单次上游请求超时；0 时用 `timeouts.provider_default` |
| `billing_source` | 注册面默认计费层：`token_plan`、`api`、`free`；单 key 值可覆盖 |
| `responses_api` | 内置注册面是否允许 OpenAI Responses 请求 |
| `force_thinking_disabled` | 当前供 DeepSeek Anthropic 面处理 thinking 兼容性 |
| `quota_cookie` | MiMo 控制台额度查询 cookie；敏感，只放本地配置 |
| `circuit_breaker.*` | per-key 熔断阈值、窗口、开放时间和半开探针数 |

Provider key 不应写在 `providers.<name>.keys`。该兼容字段仍能解析，但运行时池只从
`provider_api_keys` 读取。模型和每百万 token 定价只从 `provider_models` 读取。

同一 vendor 可以有多个协议面，例如 `deepseek` 与 `deepseek-anthropic`。注册表的
vendor 映射决定它们是否共享 key 池；不要仅凭名称后缀猜测。

### `routing`

推荐自动模式：

```yaml
routing:
  catch_all: {}
  default_strategy: priority
```

`catch_all: {}` 与“不写 `catch_all`”不同：前者启用自动发现，后者遇到未知模型时不
兜底。自动模式按请求路径过滤协议面，并从数据库模型清单和 Gateway Key 白名单选择
真实模型。也支持 alias 短格式、显式候选和共享 chain：

```yaml
routing:
  aliases:
    coding: deepseek-v4-flash
    expensive:
      chain_ref: primary
  chains:
    primary:
      - { name: minimax, model: MiniMax-M3, priority: 1 }
      - { name: deepseek-anthropic, model: deepseek-v4-flash, priority: 2 }
  default_strategy: priority
  catch_all: {}
```

策略实现包括 `priority`、`weight`、`cost`、`health`；未知值退回 `priority`。无论使用
哪种策略，候选最终仍按 `token_plan -> api -> free` 分层。管理页保存的 Level 2
provider 顺序和 Level 3 key 顺序写入 `route_order`，会覆盖各层默认创建顺序。

### `keypool`

| 字段 | 语义 |
|---|---|
| `key_rotation` | 默认/空值为 `sticky`；也支持 `round_robin`、`least_used`、`random` |
| `cooling_duration` | 429 未提供有效 `Retry-After` 时的冷却时间，默认 60s |
| `quota_enabled` | 启停额度 poll/probe worker |
| `quota_probe_initial_delay` | 无余额接口时的首次恢复探测延迟，零值兜底 5m |
| `quota_probe_max_backoff` | 恢复探测指数退避上限，零值兜底 30m |
| `quota_probe_jitter_pct` | probe 抖动百分比 |
| `quota_poll_interval` | 有 balancer 时的轮询间隔，零值兜底 60s |
| `quota_poll_jitter_pct` | poll 抖动百分比 |
| `quota_http_timeout` | 额度查询 HTTP 超时，零值兜底 10s |
| `quota_user_agent` | 额度查询 User-Agent |
| `quota_warn_threshold_pct` | 管理页额度预警阈值，零值兜底 10 |

`sticky` 始终选择当前最高优先级的可用 key。高位 key 冷却、额度耗尽或熔断时才使用
下一把；高位 key 恢复后自动回位。上游 401/普通 403 使用固定 5 分钟冷却，不受
`cooling_duration` 控制。

### `timeouts`、`retry`、`logging`、`usage`

- `timeouts.provider_default`：provider 未单独设置 timeout 时的兜底。
- `retry.max_attempts`：每个计费层最多尝试的候选数；`0` 表示尝试该层全部候选。
  同 key 的 429 重试和换 key 不按独立候选计数。
- `retry.relay_first_byte_timeout`：只限制 relay 流式候选从开始上游请求到收到首个非空
  response body chunk 的等待时间，默认 180s。预算覆盖等待 response headers 和 headers
  后无正文两个阶段；到期时仅在响应尚未提交的情况下取消当前候选并继续正常 failover。
  收到 `: PING`、其他 SSE 注释或 data 字节后立即停止计时并承诺该候选，因此不限制后续
  完整生成时长，也不会在已经提交响应后切路由。该值应基于
  `gateway_stream_ttft_seconds` 的分层 P99 调整，不要设得低于正常冷请求的首字延迟。
- `logging.level`、`logging.format`：分别控制 zap 等级和 `console|json` 输出格式；
  `--log-json` 强制 JSON。
- `usage.flush_interval`、`usage.batch_size`：异步用量批写；零值兜底 10s/100。

### `fingerprint`

```yaml
fingerprint:
  enabled: true
  canonical_device_id: "0123...64-hex-chars"
```

`enabled` 未写时默认开启。它只归一化请求体中的 `metadata.user_id.device_id` 和
`# Environment` 内的 platform/shell/os version，不修改消息、工具或工作目录。
`enabled` 可由管理 API 热切换；`canonical_device_id` 仅启动时读取，为空时每次进程
启动随机生成一个内存值。

### `admin_auth`

```yaml
admin_auth:
  enabled: true
  session_ttl: 168h
  max_login_attempts: 5
  login_ban_duration: 15m
```

启用后，除登录外的管理 API 需要 session。当前 Server 会直接把三个数值传给认证
管理器，没有统一的零值兜底，因此启用时必须显式填写。首次没有 root 用户时会创建
`admin / Gateway@2026`，应立即修改密码。

## 仅解析、当前未消费的字段

下列字段保留在结构或模板中，但当前运行路径没有读取，修改它们不会改变行为：

- 整个 `redis` 块。
- `keypool.health_check_interval`。
- `retry.enabled`、`retry.no_failover_on`、`retry.failover_on`；错误矩阵由代码实现。
- `logging.output`、`logging.file_path`；日志写 stdout/stderr，由进程管理器重定向。
- `metrics.enabled`、`metrics.path`、`metrics.port`；当前 `/metrics` 始终注册在主端口。
- `usage.retention_days`；只有 access log 实现了自动 retention。
- `admin_auth.session_cleanup_age`；过期 session 清理函数目前未由 Server 启动。
- `circuit_breaker.countable_errors`、`excluded_errors` 当前虽传入熔断器，但实际判定仍
  使用固定集合 `server_error|timeout|connection`，并排除 `rate_limit`。

这些字段不应被当作运维控制面。删除兼容字段前需要考虑已有私有配置能否继续解析。

## 热重载边界

配置 watcher 监听文件写入并重新解析。当前热生效：

- `routing.aliases/chains/catch_all/default_strategy`
- 数据库中的 Gateway Key 重载
- provider 的计费来源、Responses 能力及数据库模型/价格快照
- quota worker 参数和启停
- 后续 Provider Key CRUD、route order CRUD、中转站 CRUD 各有自己的热更新路径

需要重启：database、HTTP server、静态目录、access log、usage collector、provider
实例/endpoint/timeout、`retry.relay_first_byte_timeout`，以及管理员认证开关和参数。配置
watcher 不更新 Fingerprint；
`fingerprint.enabled` 可通过管理 API 临时热切换，改 `canonical_device_id` 必须重启。

## 数据库表

启动时 GORM `AutoMigrate` 当前模型：

| 表 | 用途 |
|---|---|
| `providers` | 历史/数据库 Provider 元数据 |
| `provider_models` | vendor 级模型与每百万 token 三档价格 |
| `provider_model_faces` | 注册面到模型的归属与上游顺序 |
| `relay_stations` | 动态中转站配置和 key JSON |
| `model_aliases` | 数据库 alias 结构（当前路由主要读 YAML） |
| `provider_api_keys` | 上游 key、协议限制、计费层和启用状态 |
| `usage_records` | 请求 token、成本、TTFT、延迟和结果 |
| `routing_configs` | 历史路由配置结构 |
| `gateway_keys` | 客户端 Gateway Key、绑定、白名单、限流 |
| `access_logs` | 请求 metadata；body 存文件 |
| `mimo_quota_cookie` | MiMo 控制台 cookie 单行记录 |
| `route_order` | provider/key 顺序改写 |
| `admin_users` | 管理员账号和锁定状态 |
| `admin_sessions` | 管理员 session |

`key_hash` 字段名不代表加密：`provider_api_keys.key_hash` 和 `gateway_keys.key_hash` 当前
都保存可用原值。数据库备份、SQL 导出和文件权限必须按密钥材料处理。

AutoMigrate 适合新增表/列，不负责可靠删除旧列或约束。做 schema 减法前先备份，分别在
SQLite/PostgreSQL 验证 DDL，再启动应用确认迁移通过。

## 运行时快照

优雅关停会把 key 的 `COOLING`、`QUOTA_EXCEEDED`、余额和计数写到
`key-state.json`。SQLite 使用 DSN 所在目录；PostgreSQL 使用当前工作目录。它不是主
数据库备份，只用于跨重启恢复瞬时调度状态。
