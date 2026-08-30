# Native LLM Gateway

Native LLM Gateway 是面向 Claude Code、Codex 和 OpenAI 兼容客户端的协议感知 LLM
代理。它提供多 Provider 路由、上游 API key 池、分层 failover、用量计费、接入日志和 Web
管理控制台。

上游有两种来源：

- 内置厂商包：处理厂商专属端点、错误、余额和请求差异。当前内置 DeepSeek、MiniMax、MiMo。
- 数据库动态中转站：无需增加 Go 包即可配置 OpenAI 或 Anthropic 兼容上游。当前不支持
  Google relay。

## 文档

- [文档索引](docs/INDEX.md)
- [架构与请求流](docs/ARCHITECTURE.md)
- [配置、数据库与热重载](docs/config-reference.md)
- [HTTP API](docs/api-reference.md)
- [Provider 目录](docs/providers.md)
- [动态中转站](docs/relay-stations.md)
- [内置厂商开发](docs/provider厂商定制包指南.md)
- [前端管理台](docs/frontend.md)
- [部署与运维](docs/operations.md)
- [排错指南](docs/踩坑与排错.md)
- [变更记录](docs/CHANGELOG.md)

## 请求入口

| 客户端/协议 | Gateway 路径 | 上游协议面 |
|---|---|---|
| Claude Code / Anthropic Messages | `POST /v1/messages` | Anthropic |
| OpenAI Chat Completions | `POST /v1/chat/completions` | OpenAI |
| 兼容入口 | `POST /v1/completions` | 已注册，但当前没有 legacy Completions 专用转换 |
| Codex / OpenAI Responses | `POST /responses` 或 `/v1/responses` | 支持 Responses 的 OpenAI 面 |

请求路径决定协议。在推荐的 `routing.catch_all: {}` 模式下：

- 内置厂商把客户端 model 视为路由标签，再从 Gateway Key 白名单和数据库模型清单选择真实模型。
- Relay 候选始终保留客户端模型名；同步过模型时先按 face 清单过滤，未同步时按通配处理。
- Relay 当前跳过 Gateway Key 模型白名单校验；混合绑定时，白名单仍约束内置厂商候选。

`/v1/completions` 目前不会被协议检测器识别为 OpenAI，也不会把 legacy `prompt` body
转换为 Chat Completions；OpenAI 兼容基座会把所有非 Responses 请求发往配置的 Chat
路径。其他未知 `POST /v1/*` 同样只表示“进入代理”，不是任意上游路径透明转发。

## 路由与调度

自动路由是一棵三级有序树：

1. 计费层固定为 `token_plan -> api -> free`。
2. 层内 provider 顺序优先使用 `route_order`；没有改写时按最早 key 创建时间和名称排序。
3. provider 内 key 顺序优先使用 `route_order`；没有改写时按 key 创建时间排序。

默认 `key_rotation: sticky` 始终选择当前最高优先级的可用 key。它不记忆“上次成功 key”；
高位 key 进入冷却、额度耗尽或熔断时才使用下一把，高位恢复后自动回位。

错误处理由分类决定：

| 上游失败 | 行为 |
|---|---|
| HTTP 429 `rate_limit` | 首次失败后同 key 再重试最多 10 次，再冷却并换 key/候选 |
| 非 429 `rate_limit`，如 HTTP 403/200 内嵌错误 | 不进入同 key 循环，立即换 key/候选 |
| 401/普通 403 `auth` | 对应 key 固定冷却 5 分钟 |
| 确认额度耗尽 | 标记 `QUOTA_EXCEEDED`，由 poll/probe 控制恢复 |
| connection/timeout/5xx | 计入对应 key 的 circuit breaker，不影响同厂商其他 key |

`keypool.cooling_duration` 只控制普通 rate-limit 冷却，不覆盖 auth 的固定 5 分钟。

## 快速开始

需要 Go 1.23+、Node.js 20+；本地可用 SQLite，生产建议 PostgreSQL 16+。

```bash
cp config.example.yaml config.yaml
npm --prefix frontend ci
npm --prefix frontend run build
make build
./bin/gateway --config ./config.yaml
```

默认访问 `http://127.0.0.1:8080`，存活与就绪端点分别为 `/healthz`、`/readyz`。

暴露服务前必须检查：

1. 保持 `auth.enabled: true`，替换模板中的 bootstrap Gateway Key。
2. 保持 `admin_auth.enabled: true` 并显式填写三个认证参数。
3. 只启用需要的内置协议面，正确配置数据库。
4. 决定是否开启 access log；body 可能包含完整请求与响应。

首次启用管理员认证且数据库没有 root 用户时，会创建
`admin / Gateway@2026`。首次登录后立即修改密码。

进入管理台后完成运行时配置：

1. 在 Provider Keys 添加内置厂商凭证，或在 Relay Stations 创建中转站。
2. 在 Models 同步各厂商模型，并按需填写每百万 token 定价。
3. 创建 Gateway Key，配置 provider/key 绑定、模型白名单和限流。
4. 在 Routing 拖拽并保存 provider/key 顺序；不保存则使用创建时间顺序。

## 客户端示例

Claude Code `settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
    "ANTHROPIC_AUTH_TOKEN": "gw-your-key",
    "ANTHROPIC_MODEL": "your-routing-label"
  }
}
```

Codex `config.toml`：

```toml
model = "your-routing-label"
model_provider = "gateway"

[model_providers.gateway]
name = "gateway"
base_url = "http://127.0.0.1:8080"
env_key = "LLM_GATEWAY_KEY"
wire_api = "responses"
```

```bash
export LLM_GATEWAY_KEY='gw-your-key'
```

OpenAI Chat Completions：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer gw-your-key' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "your-routing-label",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

## Docker

仓库的 Compose 栈运行 gateway 与 PostgreSQL：

```bash
cp config.docker.example.yaml config.yaml
# 在 config.yaml 和 docker-compose.yml 使用同一个非默认数据库密码
# 替换 bootstrap Gateway Key
docker compose up -d
docker compose ps
```

镜像为 `wuhuhhhh/native-llm-gateway`。影响应用产物的 `main` push 会触发 GitHub Actions，
测试通过后发布 `latest` 和 commit SHA tag；纯文档提交不会触发镜像构建。

## 数据与安全

- `config.yaml` 是本地配置，不应提交。
- Provider key 和 Gateway Key 当前以明文保存在历史命名的
  `provider_api_keys.key_hash`、`gateway_keys.key_hash`。
- Relay key 同时以明文保存在 `relay_stations.keys` 和 `provider_api_keys`；当前管理列表也会
  返回 relay/Gateway key 明文。
- 管理员密码使用 bcrypt；管理员 session token 以明文保存在数据库。
- access body 文件可能包含提示词、响应和客户端附带的敏感字段。示例保留期为 24 小时，
  单个 body 上限 16 MiB。
- 数据库、备份、access body 和管理 API 都应按密钥材料限制访问。
- SQLite 适合本地和低负载；生产写并发使用 PostgreSQL。

## 验证

```bash
cd backend
go test -count=1 ./...
go vet ./...
go build ./...

cd ../frontend
npm run build
```

systemd 部署可使用 `scripts/gateway-deploy.sh` 完成后端测试、二进制备份、部署、健康检查和
失败回滚。该脚本有明确环境前提，使用前先读[运维文档](docs/operations.md)。
