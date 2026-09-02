# Native LLM Gateway 协作说明

本文件面向在仓库中修改代码的开发者和编码 Agent。功能说明从
[`README.md`](README.md) 开始，长期文档索引见 [`docs/INDEX.md`](docs/INDEX.md)。

## 基本原则

- 先读调用链和测试，再修改行为；不要根据旧事故记录猜测当前实现。
- 保持包边界清晰。跨包协作优先使用窄接口、回调或显式参数。
- 数据结构、API、配置或用户可见行为改变时，同步更新对应文档和测试。
- 不提交真实 API key、Gateway Key、cookie、DSN、session token 或生产数据。
- 工作区可能包含用户未提交的改动；只处理任务范围内的文件，不覆盖或回退无关改动。

## 当前系统

- 后端：Go 1.23、Gin、GORM，支持 SQLite 和 PostgreSQL。
- 前端：Vue 3、TypeScript、Vite、Naive UI、Pinia。
- 内置厂商：DeepSeek、MiniMax、MiMo；一个厂商可注册多个协议面并共享 key 池。
- 动态中转站：保存在 `relay_stations`，当前只支持 OpenAI 与 Anthropic 兼容协议。
- 对外代理路径：`/v1/chat/completions`、`/v1/messages`、`/v1/completions`、
  `/responses`、`/v1/responses`。
- 管理 API：同源 `/api/v1`；健康检查 `/healthz`、`/readyz`；指标 `/metrics`。

## 目录与职责

```text
backend/cmd/gateway/              进程入口和顶层装配
backend/internal/api/http/        管理 API handler 与 middleware
backend/internal/auth/            Gateway Key、Provider Key 和代理鉴权
backend/internal/adminauth/       管理员、session 和登录限制
backend/internal/config/          YAML 加载、校验和文件 watcher
backend/internal/database/        GORM 模型、数据库初始化和 Store
backend/internal/provider/        协议接口、兼容基类、内置厂商和动态中转站
backend/internal/router/          候选收集、协议过滤、分层和路由迭代
backend/internal/proxy/           请求执行、重试、failover、流式输出和记账
backend/internal/keypool/         key 状态、调度器和 per-key 熔断适配
backend/internal/quotacheck/      额度轮询与恢复探测
backend/internal/accesslog/       请求元数据、body 文件和保留期
backend/internal/usage/           异步用量写入
backend/internal/fingerprint/     请求设备指纹归一化
backend/internal/metrics/         Prometheus 指标
frontend/src/                     管理控制台
scripts/                          部署、回滚、健康检查和受控数据维护
docs/                             当前长期文档
```

完整边界见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。

## 必须保持的运行时不变量

### 数据权威

- Provider key 只从数据库 `provider_api_keys` 构建运行时池。YAML 中的
  `providers.<name>.keys` 只是兼容字段，不是运行时权威。
- Gateway Key 以 `gateway_keys` 为权威；`auth.keys` 只用于启动补种缺失记录。
- 模型和定价以 `provider_models` 为权威；协议面归属保存在
  `provider_model_faces`。不要把模型或价格重新塞回 YAML。
- Provider/key 的人工顺序保存在 `route_order`。增删 provider、key 或中转站时要同步
  处理关联顺序记录。
- 历史字段名 `key_hash` 当前保存的是可用明文，不是 hash。API、日志和测试输出不得泄露。

### 路由与调度

- 候选先按 `token_plan -> api -> free` 分层，层内再应用 provider 和 key 顺序。
- 默认 `sticky` 调度器无内部游标，始终选择当前最高优先级的可用 key。高位 key 恢复后
  自动回位，不存在“从上次成功 key 继续”的记忆。
- Gateway Key 的 provider 绑定必须在路由阶段过滤，禁止先 acquire 不允许的 key 再由
  proxy 否决。
- `AllowedModels` 需要同时正确处理客户端请求名与最终上游模型名。
- 路由已 acquire 的 key 必须通过 `req.Key` 传给 Provider。Provider 不得再次 acquire，
  否则错误状态可能记到没有真正发请求的 key 上。
- Relay 候选始终按客户端模型名筛选并透传，与纯 relay 或混合绑定无关；当前 relay 候选
  还会跳过 Gateway Key 模型白名单。混合绑定时白名单仍约束普通厂商候选。
- 每个候选必须从不可变客户端快照独立派生。Relay body 与入站 body 逐字节一致，跳过
  reasoning/model/thinking/fingerprint/stream-options 改写；内置厂商继续在自己的副本适配。

### 错误、重试与状态

- 只有 HTTP 429 的 `rate_limit` 才允许在首次失败后同 key 重试，当前最多再重试 10 次。
- HTTP 403、HTTP 200 内嵌错误等非 429 `rate_limit` 必须立即换 key/候选，不能进入
  同 key 循环。
- `auth` 错误让对应 key 固定冷却 5 分钟；不受 `keypool.cooling_duration` 控制。
- `server_error`、`timeout`、`connection` 只计入对应 key 的熔断器，不连坐整个厂商。
- `quota_exceeded` 的恢复由 quota poll/probe 管理；不要引入无法自动恢复的永久禁用状态。
- 内置厂商的 HTTP 200 不一定成功，兼容基类会识别专属结构化错误；Relay 透明模式保持
  HTTP 200、headers 和 body 原样，不应用厂商错误适配。
- Relay 首个原始 body 字节（包括 SSE PING）提交后禁止 failover；中途错误只记录并关闭，
  不注入 Gateway SSE event。提交前的首包预算超时仍可切换。
- `client_disconnected` 必须终止全候选链，不得上报 key 失败、冷却或继续请求其他候选。
- 候选耗尽且最后一个 Relay 错误有 HTTP response 时返回其原始 status/header/body；只有
  没有 HTTP response 的 transport 失败才生成 Gateway 502/504。
- 同一次上游失败只能上报 key 状态一次，避免重复增加冷却和错误计数。

### URL 与协议

- OpenAI URL 归一化由 `openai_compatible` 的路径构造负责。
- Anthropic URL 归一化由 `anthropic_compatible.buildMessagesURL` 负责。
- `relay` 包没有通用 `normalizeEndpoint`。新增 URL 规则应放在对应协议实现并覆盖
  base URL 带/不带 `/v1`、尾斜杠以及完整资源路径的测试。
- 新内置厂商必须在 `backend/internal/provider/builtin/builtin.go` blank import，确保
  `init()` 注册进入二进制。

### 记录与安全

- access log 的 metadata 在数据库，request/response body 是每请求一个 JSON 文件；
  JSONL 只用于导出。单个 body 上限 16 MiB。
- access log 请求文件保存客户端原始 body；Relay 上行与其一致，内置候选上行可能不同。
- 流式 token 以 Usage 记录为准，不能假定 access-log 详情一定有最终 token。
- 请求 body、管理 session、Provider/Gateway key 都是敏感数据；测试夹具只用明确假值。
- SQLite 适合本地和低负载；生产写并发使用 PostgreSQL。

## 开发流程

1. 用 `rg` 定位入口、接口、调用方和相关测试。
2. 确认配置来源、数据库来源和热重载边界，不要制造第二份权威数据。
3. 先补或更新能锁定行为的测试，再做最小范围实现。
4. API 类型改变时同时检查 handler、`frontend/src/api/client.ts` 和相关 view。
5. 配置字段改变时同步检查 `config.go`、两个 example YAML、配置参考和热重载代码。
6. 数据库结构改变时更新 GORM 模型并在 SQLite/PostgreSQL 上验证。AutoMigrate 不可靠地
   删除旧列或约束，做 schema 减法需要显式迁移和备份方案。
7. 新增错误类型时同步检查写入方、管理 API 过滤白名单、前端筛选项和指标标签。
8. 完成后检查文档链接、示例凭据和 `git diff --check`。

## 验证

```bash
# 后端
cd backend
go test -count=1 ./...
go vet ./...
go build ./...

# 前端（build 已包含 vue-tsc）
cd ../frontend
npm ci
npm run build
```

也可在仓库根目录运行：

```bash
make test
make vet
make build
make frontend
```

涉及共享并发状态时，对相关包补跑 `go test -race`。涉及 PostgreSQL 分支时设置独立的
`PG_TEST_DSN`；测试会修改目标 schema，禁止指向生产库。

## 文档维护

- 架构与请求流：`docs/ARCHITECTURE.md`
- 配置、数据库和热重载：`docs/config-reference.md`
- HTTP 端点：`docs/api-reference.md`
- Provider 与中转站：`docs/providers.md`、中转站指南、厂商定制包指南
- 子系统和横切能力：`docs/subsystems.md`、`docs/cross-cutting.md`
- 前端契约：`docs/frontend.md`
- 部署与排障：`docs/operations.md`、`docs/踩坑与排错.md`

长期文档只描述当前行为。已完成计划、一次性事故交接、生产数据快照和逐 commit 工作日志
留在 Git 历史，不继续放在活跃文档目录。

## 当前已知限制

不要用文档掩盖代码中的真实缺口。当前已确认的配置孤岛和前后端契约问题分别记录在
[`docs/config-reference.md`](docs/config-reference.md) 与
[`docs/frontend.md`](docs/frontend.md)。修改相关模块前先读对应“当前限制”章节，并在修复后
同时更新代码、测试和文档。
