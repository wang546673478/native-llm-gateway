# HTTP API 参考

路由以 `backend/internal/server/server.go`、`handler/admin.go`、`auth/*_handler.go` 为准。
管理前端通过同源 `/api/v1` 调用这些端点。

## 三类认证

| 范围 | 凭证 |
|---|---|
| 代理端点 | `auth.enabled=true` 时接受 `Authorization: Bearer <gateway-key>` 或 `x-api-key: <gateway-key>` |
| 管理 API | `admin_auth.enabled=true` 时接受 `X-Admin-Token: <session>` 或 HttpOnly `session_token` cookie |
| 登录 | `POST /api/v1/auth/login` 不要求已有 session |

代理 key 和管理员 session 完全独立。管理员中间件当前不读取
`Authorization: Bearer`；命令行调用管理 API 时应使用 `X-Admin-Token`。关闭
`admin_auth` 后管理 API 不挂认证中间件，但当前 SPA 路由仍要求登录，因而浏览器 UI
并不支持这一模式。

## 公共与代理端点

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 进程存活和版本 |
| `GET` | `/readyz` | 1 秒内完成数据库 ping 才返回 200 |
| `GET` | `/metrics` | Prometheus 文本；当前始终在主监听端口注册 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions |
| `POST` | `/v1/completions` | 已注册兼容入口；当前没有 legacy Completions 专用转换 |
| `POST` | `/responses` | Codex 常用的无 `/v1` Responses 路径 |
| `POST` | `/v1/responses` | OpenAI Responses |
| `POST` | `/v1/messages` | Anthropic Messages |

其他 `POST /v1/*` 也会进入通用代理，但这不是任意路径透明转发。当前协议实现会重建
OpenAI Chat/Responses 或 Anthropic Messages 的上游路径；`/v1/completions`、
`/v1/embeddings` 等路径没有专用 body/path adapter，不能仅因入口被接收就视为已支持。
请求体必须是 JSON 且含 `model`；`stream` 决定流式处理。网关保留上游状态、主要响应头
和 body，同时移除 hop-by-hop header，并返回/沿用 `X-Request-Id` 作为 trace id。

## 管理员会话

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/v1/auth/login` | body `{username,password}`，返回 token 并设置 cookie |
| `GET` | `/api/v1/auth/me` | 当前用户 |
| `POST` | `/api/v1/auth/logout` | 删除当前 session 并清 cookie |
| `POST` | `/api/v1/auth/change-password` | body `{old_password,new_password}`，新密码至少 8 位 |

首次启用且数据库没有 root 时会创建 `admin / Gateway@2026`。登录连续失败达到
`max_login_attempts` 后按 `login_ban_duration` 锁定。

## Provider、模型和上游 Key

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/providers` | 按 vendor 聚合注册面、模型、池和熔断状态 |
| `GET` | `/api/v1/providers/registered` | 注册面轻量列表 |
| `GET` | `/api/v1/providers/:name` | 单注册面详情 |
| `GET` | `/api/v1/providers/:name/api-keys` | 上游 key 列表及运行时状态/余额 |
| `POST` | `/api/v1/providers/:name/api-keys` | 新增上游 key |
| `DELETE` | `/api/v1/providers/:name/api-keys/:id` | 删除并热重建共享池 |
| `POST` | `/api/v1/providers/:name/api-keys/:id/mark-quota-exceeded` | 调试用，强制标记额度耗尽 |
| `POST` | `/api/v1/providers/:name/api-keys/:id/diagnose` | 管理员单 key、单次、只读诊断；不重试、不切换、不改变池状态 |
| `GET` | `/api/v1/providers/models` | vendor 模型、价格和面归属 |
| `POST` | `/api/v1/providers/sync-models` | body `{vendor}`，同步单厂商 |
| `POST` | `/api/v1/providers/sync-all-models` | 同步全部 vendor，逐项返回成功/错误 |
| `PUT` | `/api/v1/providers/models` | 保存三档每百万 token 价格 |
| `POST` | `/api/v1/providers/models/prune` | body `{vendor}`，删除有归属数据时的无归属模型 |
| `GET` | `/api/v1/providers/mimo/quota-cookie` | 只返回 MiMo cookie 是否已配置及更新时间 |
| `POST` | `/api/v1/providers/mimo/quota-cookie` | body `{cookie}`，校验、持久化并热注入 |

新增上游 key body：

```json
{
  "name": "primary",
  "key": "sk-...",
  "enabled": true,
  "billing_source": "api",
  "protocols": "openai,anthropic"
}
```

`billing_source` 只接受 `token_plan|api|free`，`protocols` 为空表示全部协议面。注册面
名会先归一到 vendor；动态中转站 key 必须在中转站 API/页面维护，Provider Key API 会
拒绝 relay。

单 key 诊断请求 body：

```json
{
  "protocol": "anthropic",
  "path": "/v1/messages",
  "model": "claude-opus-5"
}
```

`protocol` 和 `path` 可省略，省略时使用注册面的默认协议和路径；显式值必须是该注册面
支持的协议入口。诊断只对实现 `KeyDiagnoser` 的注册面可用；不支持时返回 HTTP 503
`{"error":"diagnostic_unavailable"}`。成功响应只包含 provider/key ID、协议、HTTP
状态、可达性、错误分类和耗时等 metadata，不返回上游 body、headers 或任何 secret；诊断
会完整读取上游响应后才结束，避免因探针主动断流制造 `client_gone`。

价格保存 body：

```json
{
  "vendor": "deepseek",
  "model_id": "deepseek-v4-flash",
  "cost_per_million_input": 2,
  "cost_per_million_cache_read": 0.2,
  "cost_per_million_output": 8
}
```

## Gateway Key

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/keys` | 列出客户端 key、绑定、白名单和限流 |
| `POST` | `/api/v1/keys` | 自动生成 `gw-...` key |
| `PUT` | `/api/v1/keys/:name` | 按名称接收绑定、模型、默认模型、RPM/TPM、启用状态更新；当前限制见下文 |
| `DELETE` | `/api/v1/keys/:name` | 按名称删除 |

创建示例：

```json
{
  "name": "claude-code",
  "providers": ["deepseek", "minimax"],
  "provider_key_ids": [3, 7],
  "allowed_models": ["deepseek-v4-flash", "MiniMax-M3"],
  "default_model": "deepseek-v4-flash",
  "rpm": 100,
  "tpm": 500000,
  "enabled": true
}
```

空 `providers` / `provider_key_ids` 表示不限制。`allowed_models` 省略时创建为 `["*"]`。
当前实现的 `GET /keys` 仍返回 `key` 原值，虽然类型注释声称列表应脱敏；在修复前应把
所有管理 API 访问视为可读取 Gateway Key 的高权限访问。

Gateway Key 的三个字段目前不能按 API 表面契约依赖：

- `enabled=false` 会写入数据库并在列表中显示，但加载与 CRUD 重载都没有过滤该字段，
  该 Key 仍可通过代理认证。需要立即吊销时应删除 Key，而不是只禁用。
- `tpm` 会保存并累计成功请求的实际 token，但代理入口没有调用 TPM 拒绝检查；当前只有
  RPM 会拒绝超限请求。
- `default_model` 在创建时会入库，但 CRUD 的内存重载没有复制它；PUT 虽接收并回显该字段，
  Repository 又没有持久化更新。任意 Gateway Key CRUD 还会让全部 Key 的内存默认模型丢失，
  直到配置热重载或进程重启重新从数据库加载。

## 路由与顺序

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/routing` | 当前 aliases 与只读 `catch_all` |
| `GET` | `/api/v1/routing/order` | 查询 Level 2/3 顺序改写 |
| `PUT` | `/api/v1/routing/order` | 整体替换一个计费层/作用域的顺序并热生效 |

查询参数：`scope=provider|key`、`provider=<vendor>`（key scope 必填语义）、
`billing_source=token_plan|api|free`。更新 body：

```json
{
  "scope": "key",
  "provider": "deepseek",
  "billing_source": "api",
  "order": ["primary", "backup"]
}
```

## 中转站

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/relay-stations` | 所有数据库中转站 |
| `POST` | `/api/v1/relay-stations` | 创建并自动 reload |
| `PUT` | `/api/v1/relay-stations/:id` | 更新并自动 reload |
| `DELETE` | `/api/v1/relay-stations/:id` | 删除并清面归属、顺序、上游 key，再 reload |
| `POST` | `/api/v1/relay-stations/reload` | 手工重新加载全部中转站 |

`keys` 和 `supported_protocols` 在数据库/API 模型中都是 JSON 字符串，不是直接数组：

```json
{
  "name": "example-relay",
  "display_name": "Example Relay",
  "base_url": "https://relay.example.com/v1",
  "protocol_mode": "single",
  "primary_protocol": "openai",
  "supported_protocols": "[\"openai\"]",
  "keys": "[\"sk-...\"]",
  "enabled": true,
  "timeout_seconds": 400,
  "billing_source": "api"
}
```

当前 relay 实现支持 OpenAI 和 Anthropic；选择 Google 会在加载时报不支持。详情见
[`relay-stations.md`](relay-stations.md)。

## 用量、日志和运行状态

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/dashboard` | 最近 24h 聚合、模型、计费层、key pool |
| `GET` | `/api/v1/usage` | 明细；支持 `start,end,provider,model,gateway_key,limit,offset` |
| `GET` | `/api/v1/usage/aggregate` | 模型聚合；支持时间、provider、gateway key |
| `GET` | `/api/v1/usage/by_model/:model_id/providers` | 某模型的 provider 分布 |
| `GET` | `/api/v1/access-logs` | 接入日志筛选和分页 |
| `GET` | `/api/v1/access-logs/stats` | 24h 总量/错误与 active key 数 |
| `GET` | `/api/v1/access-logs/:id/detail` | metadata 与请求/响应 body |
| `GET` | `/api/v1/access-logs/export` | NDJSON 导出，默认 10000、上限 50000 条 |
| `GET` | `/api/v1/inflight` | 当前内存中的活跃请求快照 |
| `GET` | `/api/v1/config/quota` | 当前额度告警阈值 |
| `GET` | `/api/v1/fingerprint` | 指纹归一化开关和本进程 canonical id |
| `PUT` | `/api/v1/fingerprint` | body `{enabled}`，只热切开关 |

Access log 筛选支持 `start,end,gateway_key,provider,model,trace_id,error_type,status,limit,offset`。
`status` 接受后端定义的状态桶；非法值返回 400。body 已被 retention 清理时，详情/导出
仍可返回 metadata，但对应 body 字段为空。

## 管理员用户（root）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/admin-users` | 用户列表 |
| `POST` | `/api/v1/admin-users` | 创建；role 为 `root|admin|readonly` |
| `PUT` | `/api/v1/admin-users/:id` | body 可含 `role`、`enabled` |
| `DELETE` | `/api/v1/admin-users/:id` | 删除用户及其 session |
| `POST` | `/api/v1/admin-users/:id/reset-password` | body `{new_password}`，至少 8 位 |

这些端点在后端由 root role 保护。当前前端解锁按钮发送的 `locked` 字段与后端契约不
一致，不能把该按钮当作可靠的解锁手段；API 也没有直接清空 `locked_until` 的字段。
