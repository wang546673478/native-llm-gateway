# Native LLM Gateway — 完整实现规格书 v2.0

> 本文档是面向 AI Coding Agent（Claude Code / Codex / Cline）的完整实现规格书。
> 任何修改必须在完成所有步骤后更新 CHANGELOG.md、ARCHITECTURE.md 和 API.md。

---

# 第一部分：项目定义

## 1.1 一句话定义

```
一个协议感知的、插件化的 LLM Gateway，
为 AI Agent（Claude Code、Codex、Cline、Continue）提供
多 Provider 路由、API Key 池化、Token 计费和自动故障转移。
```

## 1.2 核心设计原则（不可违反）

### 原则 1：协议感知的透明代理

Gateway 的定位既不是 Nginx 式的字节搬运工，也不是 LiteLLM 式的协议转换器，而是一个**协议感知的透明代理**。

三种模式的精确区分：

```
模式 A — Nginx 式反向代理（不是我们）
  不看 body，不做路由，不知道协议语义
  URL 路径 → 后端，纯字节搬运
  问题：无法根据模型名路由、无法做 failover

模式 B — 协议转换网关（不是我们，如 LiteLLM）
  完整解析请求体，把 Anthropic 格式转成 OpenAI 格式
  问题：转换过程中丢失语义、Provider 特性无法表达、调试困难

模式 C — 协议感知的透明代理（我们）
  读取 body 但只提取路由元数据（model、stream、tools 等标志位）
  请求体原封不动透传给 Provider
  响应体原封不动透传给客户端
  路由和故障转移在元数据层完成
```

**Gateway 允许做的事情：**

| 操作 | 示例 | 原因 |
|------|------|------|
| 读取 body 提取 model 字段 | `{"model": "coding-model"}` → `"coding-model"` | 路由需要 |
| 读取 body 提取 stream 字段 | `{"stream": true}` | 决定走流式/非流式路径 |
| 读取请求路径判断协议 | `/v1/messages` → anthropic | 协议匹配需要 |
| 重写 Authorization header | `Bearer gw-key-xxx` → `Bearer sk-provider-xxx` | 认证替换 |
| 注入 X-Request-Id header | 添加 `X-Request-Id: trace-abc` | 链路追踪 |
| 删除/覆盖 hop-by-hop headers | 删除原始 Connection header | HTTP 规范要求 |

**Gateway 禁止做的事情：**

| 操作 | 示例 | 原因 |
|------|------|------|
| 修改 body 中任何字段 | 把 `max_tokens` 改成 `maxTokens` | 零转换原则 |
| 转换消息结构 | Anthropic `content` 数组 → OpenAI `content` 字符串 | 会丢失语义 |
| 映射 Provider 特有字段 | Anthropic `system` 顶层字段 → OpenAI `messages[0]` | Provider 差异由 Provider 处理 |
| 统一错误格式 | 把 MiniMax 的错误体转换成标准格式 | 客户端期望看到 Provider 原始错误 |
| 解析或修改响应体 | 修改 `usage` 字段名 | 响应原样回传 |

**Provider 职责边界：**

每个 Provider 插件了解自己和标准协议的差异，在 SendRequest 内部自行处理。Gateway 核心对此完全无感。

```
MiniMax Provider 知道：
  - MiniMax 的 anthropic 兼容接口需要哪些额外 header
  - MiniMax 的 usage 字段可能叫什么名字
  - MiniMax 的错误体长什么样

这些差异由 MiniMax Provider 的 SendRequest() 内部处理
Gateway 核心代码不知道、也不需要知道
```

### 原则 2：Provider 即插件

- 所有 Provider 实现统一接口
- 禁止在 Router、Policy、API 层出现 provider-specific 的 if/switch
- 新增 Provider 只需实现接口 + 注册，无需修改 Gateway 核心代码

### 原则 3：Gateway 只管控制面

- Gateway 负责：路由、调度、限流、统计、认证、故障恢复
- Provider 负责：API 调用、协议细节、Usage 解析、Error 分类
- 禁止在 Gateway 核心代码中出现 Provider 业务逻辑

### 原则 4：所有资源可观测

- 每个请求必须可追踪（trace-id 贯穿全链路）
- 每个 Provider 的健康状态、延迟、错误率必须可查询
- 每个 Key 的状态、用量必须可查询

### 原则 5：所有行为可配置

- 路由策略可配置
- Key Pool 行为可配置
- Circuit Breaker 参数可配置
- 添加/移除 Provider 或 Key 支持热加载（不重启）

### 原则 6：流式响应的安全边界

- 流式请求一旦开始向客户端发送数据，中途失败不做 failover
- failover 只发生在 Provider.SendStreamRequest 调用失败时
- 原因：客户端已收到部分数据，重试会导致重复或不一致
- 流式中途失败时，发送协议对应的 error event 给客户端

## 1.3 协议配对矩阵

```
规则：客户端发送的协议决定路由目标，Gateway 不做跨协议桥接

客户端协议            可路由到的 Provider
─────────────────────────────────────────
Anthropic (v1/messages)   → MiniMax, Kimi, GLM（protocol=anthropic）
OpenAI (v1/chat/comp.)    → DeepSeek, Qwen, MiniMax, Kimi（protocol=openai）
Google (v1/generate)      → Gemini（protocol=google）

配置方式：每个 Provider 声明自己的 protocol 字段
路由逻辑：根据请求 endpoint 匹配协议，只路由到对应协议的 Provider
```

---

# 第二部分：技术栈

## 2.1 Backend

```
语言:           Go >= 1.23
HTTP 框架:      Gin v1.10+
数据库 ORM:     GORM v1.25+
数据库:         SQLite（开发/默认）, PostgreSQL 16+（生产）
缓存:           Redis 7+
日志:           Zap (go.uber.org/zap)
指标:           Prometheus (prometheus/client_golang)
配置:           Viper (spf13/viper)
CLI:            Cobra (spf13/cobra)
迁移:           golang-migrate/migrate
UUID:           google/uuid
```

## 2.2 Frontend

```
框架:           Vue 3.4+ (Composition API)
语言:           TypeScript 5+
构建:           Vite 5+
状态管理:       Pinia
UI 库:          Naive UI
图表:           ECharts 5+
HTTP:           Axios
路由:           Vue Router 4
```

## 2.3 基础设施

```
容器:           Docker + Docker Compose
监控:           Prometheus + Grafana
```

---

# 第三部分：目录结构

```
llm-gateway/
├── backend/
│   ├── cmd/
│   │   └── gateway/
│   │       └── main.go                  # 入口：初始化配置、启动 Gateway
│   ├── internal/
│   │   ├── api/
│   │   │   ├── http/
│   │   │   │   ├── handler/             # HTTP 处理器
│   │   │   │   │   ├── provider.go      # Provider CRUD
│   │   │   │   │   ├── key.go           # Key 管理
│   │   │   │   │   ├── usage.go         # 用量查询
│   │   │   │   │   ├── routing.go       # 路由规则管理
│   │   │   │   │   ├── dashboard.go     # Dashboard 数据聚合
│   │   │   │   │   ├── config.go        # 配置热重载
│   │   │   │   │   └── health.go        # 健康检查
│   │   │   │   ├── middleware/          # 中间件
│   │   │   │   │   ├── auth.go          # 客户端认证
│   │   │   │   │   ├── ratelimit.go     # 客户端限流
│   │   │   │   │   ├── logger.go        # 请求日志
│   │   │   │   │   ├── trace.go         # 链路追踪
│   │   │   │   │   └── cors.go          # CORS
│   │   │   │   └── router.go            # 路由注册
│   │   │   └── proxy/
│   │   │       ├── proxy.go             # 核心代理引擎
│   │   │       ├── stream.go            # 流式响应处理
│   │   │       └── buffer.go            # 非流式响应缓冲
│   │   ├── router/
│   │   │   ├── router.go                # 路由引擎
│   │   │   ├── model_alias.go           # 模型别名解析
│   │   │   └── policy.go                # 路由策略接口
│   │   ├── policy/
│   │   │   ├── priority.go              # 优先级策略
│   │   │   ├── weight.go                # 权重策略
│   │   │   ├── cost.go                  # 成本策略
│   │   │   └── health.go                # 健康感知策略
│   │   ├── provider/
│   │   │   ├── provider.go              # Provider 接口定义
│   │   │   ├── manager.go               # Provider 生命周期管理
│   │   │   ├── registry.go              # Provider 注册表
│   │   │   ├── deepseek/
│   │   │   │   ├── deepseek.go
│   │   │   │   └── config.go
│   │   │   ├── minimax/
│   │   │   │   ├── minimax.go
│   │   │   │   └── config.go
│   │   │   ├── glm/
│   │   │   │   ├── glm.go
│   │   │   │   └── config.go
│   │   │   ├── qwen/
│   │   │   │   ├── qwen.go
│   │   │   │   └── config.go
│   │   │   ├── kimi/
│   │   │   │   ├── kimi.go
│   │   │   │   └── config.go
│   │   │   ├── gemini/
│   │   │   │   ├── gemini.go
│   │   │   │   └── config.go
│   │   │   └── openai_compatible/       # 通用 OpenAI 兼容 Provider
│   │   │       ├── openai_compatible.go
│   │   │       └── config.go
│   │   ├── keypool/
│   │   │   ├── pool.go                  # Key Pool 管理器
│   │   │   ├── key.go                   # Key 实体和状态机
│   │   │   └── scheduler.go             # Key 调度（轮询、选择）
│   │   ├── circuit/
│   │   │   └── breaker.go               # Circuit Breaker 实现
│   │   ├── usage/
│   │   │   ├── collector.go             # Usage 数据收集
│   │   │   ├── recorder.go              # Usage 持久化
│   │   │   └── query.go                 # Usage 查询
│   │   ├── token/
│   │   │   └── counter.go               # Token 本地计数（审计用）
│   │   ├── metrics/
│   │   │   ├── collector.go             # 指标收集
│   │   │   └── prometheus.go            # Prometheus 指标定义
│   │   ├── auth/
│   │   │   ├── authenticator.go         # 客户端认证逻辑
│   │   │   └── apikey.go                # Gateway API Key 管理
│   │   ├── config/
│   │   │   └── config.go                # 配置加载和验证
│   │   ├── database/
│   │   │   ├── database.go              # 数据库初始化
│   │   │   └── models.go               # GORM 模型定义
│   │   └── server/
│   │       └── server.go                # Gateway 服务编排
│   ├── plugins/                         # 第三方 Provider 插件目录（预留）
│   ├── migrations/
│   │   ├── 001_init_providers.up.sql
│   │   ├── 001_init_providers.down.sql
│   │   ├── 002_init_keys.up.sql
│   │   ├── 002_init_keys.down.sql
│   │   ├── 003_init_usage.up.sql
│   │   ├── 003_init_usage.down.sql
│   │   ├── 004_init_routing.up.sql
│   │   ├── 004_init_routing.down.sql
│   │   ├── 005_init_auth.up.sql
│   │   └── 005_init_auth.down.sql
│   ├── docs/
│   │   ├── ARCHITECTURE.md
│   │   ├── API.md
│   │   └── CHANGELOG.md
│   ├── go.mod
│   ├── go.sum
│   └── Makefile
├── frontend/
│   ├── src/
│   │   ├── views/
│   │   │   ├── Overview.vue
│   │   │   ├── Providers.vue
│   │   │   ├── Keys.vue
│   │   │   ├── Routing.vue
│   │   │   ├── Usage.vue
│   │   │   └── Settings.vue
│   │   ├── components/
│   │   │   ├── StatusBadge.vue
│   │   │   ├── UsageChart.vue
│   │   │   ├── LatencyChart.vue
│   │   │   └── ProviderCard.vue
│   │   ├── stores/
│   │   │   ├── provider.ts
│   │   │   ├── usage.ts
│   │   │   └── routing.ts
│   │   ├── api/
│   │   │   └── client.ts
│   │   ├── router/
│   │   │   └── index.ts
│   │   ├── App.vue
│   │   └── main.ts
│   ├── index.html
│   ├── vite.config.ts
│   ├── tsconfig.json
│   ├── package.json
│   └── .env
├── docker-compose.yml
├── docker-compose.dev.yml
├── prometheus/
│   └── prometheus.yml
├── grafana/
│   └── provisioning/
│       └── dashboards/
│           └── gateway.json
├── config.example.yaml
├── README.md
├── CLAUDE.md
└── LICENSE
```

---

# 第四部分：配置文件

## 4.1 config.yaml 完整规格

```yaml
# config.yaml — Gateway 完整配置

server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 120s       # 流式响应需要较长写超时
  idle_timeout: 120s
  shutdown_timeout: 30s

database:
  driver: "sqlite"          # sqlite | postgres
  dsn: "data/gateway.db"    # SQLite 路径或 PostgreSQL DSN
  max_open_conns: 25
  max_idle_conns: 10
  conn_max_lifetime: 300s

redis:
  enabled: false
  addr: "localhost:6379"
  password: ""
  db: 0
  pool_size: 10

# 客户端认证（Gateway 自身的认证）
auth:
  enabled: true
  keys:
    - name: "claude-code-dev"
      key: "gw-key-xxxx"
      allowed_models: ["*"]
      rate_limit:
        rpm: 100
        tpm: 500000
    - name: "cline-prod"
      key: "gw-key-yyyy"
      allowed_models: ["coding-model", "chat-model"]
      rate_limit:
        rpm: 60
        tpm: 300000

# Provider 配置
providers:
  deepseek:
    enabled: true
    endpoint: "https://api.deepseek.com"
    protocol: "openai"                 # openai | anthropic | google
    timeout: 60s
    models:
      - id: "deepseek-chat"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.0014
        cost_per_1k_output: 0.0028
      - id: "deepseek-coder"
        aliases: ["coding-model"]
        cost_per_1k_input: 0.0014
        cost_per_1k_output: 0.0028
    keys:
      - name: "deepseek-key-1"
        key: "sk-xxx1"
      - name: "deepseek-key-2"
        key: "sk-xxx2"
    circuit_breaker:
      failure_threshold: 5
      failure_window: 60s
      open_timeout: 30s
      half_open_requests: 1
      countable_errors: ["5xx", "timeout", "connection_error"]
      excluded_errors: ["429"]

  minimax:
    enabled: true
    endpoint: "https://api.minimax.chat"
    protocol: "anthropic"
    timeout: 90s
    models:
      - id: "MiniMax-Text-01"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.001
        cost_per_1k_output: 0.002
    keys:
      - name: "minimax-key-1"
        key: "sk-mm-xxx1"
    circuit_breaker:
      failure_threshold: 5
      failure_window: 60s
      open_timeout: 30s
      half_open_requests: 1

  glm:
    enabled: true
    endpoint: "https://open.bigmodel.cn/api/paas/v4"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "glm-4-plus"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.005
        cost_per_1k_output: 0.01
    keys:
      - name: "glm-key-1"
        key: "xxx"

  qwen:
    enabled: true
    endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "qwen-coder-plus"
        aliases: ["coding-model"]
        cost_per_1k_input: 0.003
        cost_per_1k_output: 0.006
    keys:
      - name: "qwen-key-1"
        key: "sk-xxx"

  kimi:
    enabled: true
    endpoint: "https://api.moonshot.cn/v1"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "moonshot-v1-128k"
        aliases: ["coding-model", "long-context"]
        cost_per_1k_input: 0.012
        cost_per_1k_output: 0.012
    keys:
      - name: "kimi-key-1"
        key: "sk-xxx"

  gemini:
    enabled: false
    endpoint: "https://generativelanguage.googleapis.com/v1beta"
    protocol: "google"
    timeout: 60s
    models:
      - id: "gemini-2.0-flash"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.0001
        cost_per_1k_output: 0.0004
    keys:
      - name: "gemini-key-1"
        key: "xxx"

# 路由规则
routing:
  aliases:
    coding-model:
      strategy: "priority"
      providers:
        - name: "minimax"
          model: "MiniMax-Text-01"
          priority: 1
        - name: "deepseek"
          model: "deepseek-coder"
          priority: 2
        - name: "glm"
          model: "glm-4-plus"
          priority: 3
    chat-model:
      strategy: "weight"
      providers:
        - name: "deepseek"
          model: "deepseek-chat"
          weight: 70
        - name: "glm"
          model: "glm-4-plus"
          weight: 30
    long-context:
      strategy: "priority"
      providers:
        - name: "kimi"
          model: "moonshot-v1-128k"
          priority: 1

  default_strategy: "priority"

# Key Pool 配置
keypool:
  cooling_duration: 60s
  max_cooling_count: 5
  health_check_interval: 30s
  key_rotation: "round_robin"        # round_robin | least_used | random

# 超时与重试
timeouts:
  server_read: 30s
  server_write: 120s
  server_idle: 120s
  provider_default: 60s
  request_total: 300s                # 全局请求最大时间（含 failover）

retry:
  enabled: true
  max_attempts: 3                    # 最大 failover 次数
  no_failover_on:                    # 以下错误不触发 failover
    - "invalid_request"              # 400
    - "auth"                         # 401/403
  failover_on:                       # 以下错误触发 failover
    - "rate_limit"                   # 429
    - "server_error"                 # 5xx
    - "timeout"
    - "connection"

# 日志
logging:
  level: "info"                      # debug | info | warn | error
  format: "json"                     # json | console
  output: "stdout"                   # stdout | file
  file_path: "logs/gateway.log"

# 指标
metrics:
  enabled: true
  path: "/metrics"
  port: 9090

# Usage
usage:
  flush_interval: 10s
  batch_size: 100
  retention_days: 90
```

---

# 第五部分：Go 接口定义

## 5.1 Provider 接口

```go
// backend/internal/provider/provider.go

package provider

import (
    "context"
    "net/http"
    "time"
)

// Protocol 协议类型
type Protocol string

const (
    ProtocolOpenAI    Protocol = "openai"
    ProtocolAnthropic Protocol = "anthropic"
    ProtocolGoogle    Protocol = "google"
)

// Request 是 Gateway 收到的原始请求的包装
// 注意：Body 是原始字节，不做任何解析或转换
type Request struct {
    Method    string
    Path      string          // 原始请求路径，如 /v1/messages
    Headers   http.Header     // 原始 headers
    Body      []byte          // 原始请求体，不做解析
    Model     string          // 解析后的目标模型 ID（非别名）
    IsStream  bool            // 是否流式请求
    GatewayKeyID string       // Gateway 客户端的 key ID
    TraceID   string          // 链路追踪 ID
}

// Response 是 Provider 返回的包装
type Response struct {
    StatusCode int
    Headers    http.Header
    Body       []byte         // 原始响应体，不做修改
    Usage      *Usage
}

// StreamChunk 是流式响应的一行
type StreamChunk struct {
    Data []byte               // SSE data 行的原始内容
    Err  error                // io.EOF 表示结束
}

// Usage 是从 Provider 响应中提取的用量信息
type Usage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    RawUsage         map[string]interface{}  // 原始 usage 数据
}

// Provider 是所有 LLM Provider 必须实现的接口
type Provider interface {
    Name() string
    Protocol() Protocol
    Models() []string

    // SendRequest 发送非流式请求
    // 实现要求：
    //   1. 从 Key Pool 获取 Key，构造 Provider 原生 HTTP 请求
    //   2. 设置正确的认证 header（各 Provider 不同）
    //   3. Body 原样透传，不修改
    //   4. 从响应体中解析 Usage 信息
    //   5. 根据状态码判断错误类型
    SendRequest(ctx context.Context, req *Request) (*Response, error)

    // SendStreamRequest 发送流式请求
    // 实现要求：
    //   1. 构造 Provider 原生 HTTP 请求，启用 stream
    //   2. 返回 channel 逐步推送 SSE chunk
    //   3. 不同协议的 SSE 格式不同，由 Provider 自行解析
    //   4. 输出统一的 StreamChunk 格式给 Gateway
    //   5. 流结束后关闭 channel
    SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error)

    // HealthCheck 执行健康检查
    HealthCheck(ctx context.Context) error

    // Close 清理资源
    Close() error
}

// ProviderError 是 Provider 返回的结构化错误
type ProviderError struct {
    ProviderName string
    StatusCode   int
    ErrorType    ErrorType
    Message      string
    RetryAfter   time.Duration
    RawError     []byte
}

func (e *ProviderError) Error() string {
    return e.Message
}

// ErrorType 错误分类
type ErrorType string

const (
    ErrorTypeRateLimit      ErrorType = "rate_limit"       // 429
    ErrorTypeAuth           ErrorType = "auth"             // 401, 403
    ErrorTypeInvalidRequest ErrorType = "invalid_request"  // 400
    ErrorTypeServerError    ErrorType = "server_error"     // 500+
    ErrorTypeTimeout        ErrorType = "timeout"
    ErrorTypeConnection     ErrorType = "connection"
    ErrorTypeModelNotFound  ErrorType = "model_not_found"  // 404
)

// ClassifyError 根据 HTTP 状态码分类错误
func ClassifyError(statusCode int) ErrorType {
    switch {
    case statusCode == 429:
        return ErrorTypeRateLimit
    case statusCode == 401 || statusCode == 403:
        return ErrorTypeAuth
    case statusCode == 400:
        return ErrorTypeInvalidRequest
    case statusCode == 404:
        return ErrorTypeModelNotFound
    case statusCode >= 500:
        return ErrorTypeServerError
    default:
        return ErrorTypeServerError
    }
}
```

## 5.2 Provider Manager

```go
// backend/internal/provider/manager.go

package provider

import "context"

type Manager struct {
    providers map[string]Provider
    registry  *Registry
}

func NewManager(registry *Registry) *Manager

// LoadFromConfig 从配置加载所有 enabled 的 Provider
// 流程：
//   1. 遍历 config.providers
//   2. enabled=false 的跳过
//   3. 从 Registry 获取 Provider 工厂函数
//   4. 创建 Provider 实例
//   5. 执行 HealthCheck
//   6. 注册到 providers map
func (m *Manager) LoadFromConfig(ctx context.Context, cfg *Config) error

func (m *Manager) Get(name string) (Provider, bool)
func (m *Manager) GetAll() map[string]Provider
func (m *Manager) GetByProtocol(protocol Protocol) []Provider

// Reload 热加载配置（添加/移除 Provider）
func (m *Manager) Reload(ctx context.Context, cfg *Config) error

func (m *Manager) Close() error
```

## 5.3 Provider Registry

```go
// backend/internal/provider/registry.go

package provider

type Factory func(config ProviderConfig) (Provider, error)

type Registry struct {
    factories map[string]Factory
}

func NewRegistry() *Registry
func (r *Registry) Register(name string, factory Factory)
func (r *Registry) Create(name string, config ProviderConfig) (Provider, error)
func (r *Registry) ListRegistered() []string
```

## 5.4 Key Pool

```go
// backend/internal/keypool/key.go

package keypool

import "time"

type KeyStatus string

const (
    KeyStatusActive   KeyStatus = "ACTIVE"
    KeyStatusCooling  KeyStatus = "COOLING"
    KeyStatusLimited  KeyStatus = "LIMITED"
    KeyStatusDisabled KeyStatus = "DISABLED"
)

type Key struct {
    ID            string
    ProviderName  string
    Name          string
    Key           string       // 加密存储，运行时解密
    Status        KeyStatus
    CoolingUntil  time.Time
    CoolingCount  int
    TotalRequests int64
    TotalTokens   int64
    ErrorCount    int
    LastUsedAt    time.Time
    LastErrorAt   time.Time
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

// Pool 管理一个 Provider 下的所有 Key
type Pool struct {
    ProviderName string
    keys         []*Key
    scheduler    Scheduler
}

func NewPool(providerName string, keys []*Key, strategy string) *Pool

// Acquire 获取一个可用的 Key
//   1. Scheduler 选择一个 Key
//   2. 如果 Key 是 COOLING 且 cooling_until < now，恢复为 ACTIVE
//   3. 如果所有 Key 都不可用，返回 ErrNoAvailableKey
func (p *Pool) Acquire() (*Key, error)

func (p *Pool) ReportSuccess(key *Key)

// ReportRateLimit 报告 429
//   1. 设置 status = COOLING
//   2. 设置 cooling_until = now + cooling_duration（或 Retry-After）
//   3. cooling_count++
//   4. 如果 cooling_count > max_cooling_count，设置 DISABLED
func (p *Pool) ReportRateLimit(key *Key, retryAfter time.Duration)

func (p *Pool) ReportError(key *Key, err *ProviderError)

type PoolStatus struct {
    ProviderName string
    TotalKeys    int
    ActiveKeys   int
    CoolingKeys  int
    DisabledKeys int
}

func (p *Pool) Status() PoolStatus

// Scheduler Key 选择策略接口
type Scheduler interface {
    Select(keys []*Key) (*Key, error)
}

type RoundRobinScheduler struct{ current int }
type LeastUsedScheduler struct{}
type RandomScheduler struct{}
```

## 5.5 Router

```go
// backend/internal/router/router.go

package router

import (
    "context"
    "llm-gateway/internal/provider"
    "llm-gateway/internal/keypool"
)

type RouteResult struct {
    ProviderName string
    ModelID      string       // 真实模型 ID（非别名）
    Key          *keypool.Key
    Endpoint     string
}

type Router struct {
    manager    *provider.Manager
    keyPools   map[string]*keypool.Pool
    aliases    map[string]*AliasConfig
    policies   map[string]Policy
    breakerMap map[string]*circuit.Breaker
    maxAttempts int
}

func NewRouter(manager *provider.Manager, pools map[string]*keypool.Pool, cfg *RoutingConfig) *Router

// Route 根据请求选择 Provider、模型和 Key
// 逻辑：
//   1. 解析请求中的模型名
//   2. 检查是否是别名 → 获取路由配置
//   3. 确定请求协议（根据请求路径判断）
//   4. 过滤：只保留协议匹配 + Circuit Breaker 未 OPEN 的 Provider
//   5. 使用 Policy 选择 Provider
//   6. 从 Key Pool 获取 Key
//   7. 返回 RouteResult
//   8. 如果当前 Provider 失败，调用 Next() 获取下一个候选
func (r *Router) Route(ctx context.Context, req *provider.Request) (*RouteIterator, error)

// RouteIterator 支持 failover 的迭代器
type RouteIterator struct {
    candidates []ProviderRoute
    current    int
    pools      map[string]*keypool.Pool
    breakers   map[string]*circuit.Breaker
}

// Next 返回下一个可用的 Provider 路由
// 如果所有候选都不可用，返回 nil, ErrNoAvailableProvider
func (it *RouteIterator) Next() (*RouteResult, error)

type AliasConfig struct {
    Strategy  string
    Providers []ProviderRoute
}

type ProviderRoute struct {
    Name     string
    Model    string
    Priority int
    Weight   int
}

type Policy interface {
    Select(candidates []ProviderRoute, healthStatus map[string]bool) (*ProviderRoute, error)
}
```

## 5.6 Circuit Breaker

```go
// backend/internal/circuit/breaker.go

package circuit

import "time"

type State string

const (
    StateClosed   State = "CLOSED"
    StateOpen     State = "OPEN"
    StateHalfOpen State = "HALF_OPEN"
)

type BreakerConfig struct {
    FailureThreshold  int
    FailureWindow     time.Duration
    OpenTimeout       time.Duration
    HalfOpenRequests  int
    CountableErrors   []string
    ExcludedErrors    []string
}

type Breaker struct {
    name         string
    config       BreakerConfig
    state        State
    failures     []time.Time       // 滑动窗口内的失败记录
    successCount int               // Half-Open 期间的成功计数
    openedAt     time.Time
    mu           sync.RWMutex
}

func NewBreaker(name string, config BreakerConfig) *Breaker

// Allow 检查是否允许请求通过
// CLOSED → 允许
// OPEN → 检查是否超时，超时则转 HALF_OPEN
// HALF_OPEN → 允许 half_open_requests 个
func (b *Breaker) Allow() bool

func (b *Breaker) RecordSuccess()

// RecordFailure 记录失败
// 检查错误类型是否应该计数 → 记录到滑动窗口 → 清理窗口外记录
// 如果窗口内失败数 >= threshold → 转 OPEN
func (b *Breaker) RecordFailure(errType string)

func (b *Breaker) State() State
func (b *Breaker) Reset()    // 手动恢复
```

## 5.7 Proxy Engine

```go
// backend/internal/api/proxy/proxy.go

package proxy

import (
    "context"
    "llm-gateway/internal/provider"
    "llm-gateway/internal/router"
    "llm-gateway/internal/usage"
    "llm-gateway/internal/metrics"
    "llm-gateway/internal/keypool"
)

type Engine struct {
    router    *router.Router
    collector *usage.Collector
    metrics   *metrics.Collector
}

func NewEngine(r *router.Router, c *usage.Collector, m *metrics.Collector) *Engine

// HandleRequest 处理代理请求（非流式）
// 完整流程见第九部分
func (e *Engine) HandleRequest(c *gin.Context)

// HandleStreamRequest 处理流式代理请求
// 完整流程见第九部分
func (e *Engine) HandleStreamRequest(c *gin.Context)

// determineProtocol 根据请求路径判断协议
func determineProtocol(path string) provider.Protocol {
    switch {
    case strings.HasPrefix(path, "/v1/messages"):
        return provider.ProtocolAnthropic
    case strings.HasPrefix(path, "/v1/chat/completions"):
        return provider.ProtocolOpenAI
    case strings.HasPrefix(path, "/v1beta/"):
        return provider.ProtocolGoogle
    default:
        return provider.ProtocolOpenAI
    }
}
```

## 5.8 Usage Collector

```go
// backend/internal/usage/collector.go

package usage

import (
    "context"
    "time"
)

type Record struct {
    ID              string
    TraceID         string
    GatewayKeyID    string
    ProviderName    string
    ModelID         string
    Protocol        string
    InputTokens     int
    OutputTokens    int
    TotalTokens     int
    Cost            float64
    LatencyMs       int64
    IsStream        bool
    StatusCode      int
    ErrorType       string
    CreatedAt       time.Time
}

type Repository interface {
    BatchCreate(ctx context.Context, records []*Record) error
    Query(ctx context.Context, filter QueryFilter) ([]*Record, error)
    Aggregate(ctx context.Context, filter QueryFilter) (*AggregateResult, error)
}

type QueryFilter struct {
    StartTime    time.Time
    EndTime      time.Time
    ProviderName string
    ModelID      string
    GatewayKeyID string
    Limit        int
    Offset       int
}

type AggregateResult struct {
    TotalRequests  int64
    TotalTokens    int64
    TotalCost      float64
    AvgLatencyMs   float64
    ErrorCount     int64
    ByProvider     map[string]*AggregateResult
    ByModel        map[string]*AggregateResult
}

type Collector struct {
    records   chan *Record
    batch     []*Record
    batchSize int
    flushInt  time.Duration
    repo      Repository
    mu        sync.Mutex
}

func NewCollector(repo Repository, batchSize int, flushInterval time.Duration) *Collector

// Start 启动后台批量写入协程
func (c *Collector) Start(ctx context.Context)

// Record 异步记录一条用量（非阻塞）
func (c *Collector) Record(r *Record)
```

## 5.9 Auth

```go
// backend/internal/auth/authenticator.go

package auth

import "net/http"

type Authenticator struct {
    keys    map[string]*GatewayKey
    enabled bool
}

type GatewayKey struct {
    Name          string
    Key           string       // hash 存储
    AllowedModels []string
    RateLimit     RateLimitConfig
}

type RateLimitConfig struct {
    RPM int
    TPM int
}

// Authenticate 验证请求
//   1. 从 Authorization header 提取 Bearer token
//   2. 查找 GatewayKey（对比 hash）
//   3. 检查 allowed_models
//   4. 检查 rate limit
//   5. 返回 GatewayKey 或 error
func (a *Authenticator) Authenticate(r *http.Request) (*GatewayKey, error)
```

---

# 第六部分：HTTP API 规格

## 6.1 代理端点

这些是客户端（Claude Code 等）实际调用的端点，Gateway 原样透传到对应 Provider。

```
POST /v1/chat/completions     # OpenAI 协议
POST /v1/messages              # Anthropic 协议
POST /v1/generate              # Google 协议
POST /v1beta/models/{model}:*  # Google 协议（原生路径）

请求头:
  Authorization: Bearer <gateway-api-key>
  Content-Type: application/json

Gateway 行为:
  1. 验证 Gateway API Key
  2. 解析模型名（可能包含别名）
  3. 路由到目标 Provider
  4. 替换 Authorization 为目标 Provider 的 Key
  5. 原样透传请求体
  6. 原样透传响应体（或流式 chunk）
```

## 6.2 管理 API — Provider

```
GET  /api/v1/providers
  返回所有 Provider 及其状态

Response 200:
{
  "providers": [
    {
      "name": "deepseek",
      "protocol": "openai",
      "endpoint": "https://api.deepseek.com",
      "enabled": true,
      "health": {
        "status": "healthy",
        "last_check": "2025-01-01T00:00:00Z",
        "latency_ms": 230
      },
      "circuit_breaker": {
        "state": "CLOSED",
        "failure_count": 0
      },
      "models": [
        {
          "id": "deepseek-coder",
          "aliases": ["coding-model"],
          "cost_per_1k_input": 0.0014,
          "cost_per_1k_output": 0.0028
        }
      ],
      "key_pool": {
        "total": 3,
        "active": 2,
        "cooling": 1,
        "disabled": 0
      },
      "stats": {
        "total_requests": 1234,
        "total_tokens": 567890,
        "avg_latency_ms": 250,
        "error_rate": 0.02
      }
    }
  ]
}

GET    /api/v1/providers/:name           # 单个详情
POST   /api/v1/providers                 # 创建（热加载）
PUT    /api/v1/providers/:name           # 更新（热加载）
DELETE /api/v1/providers/:name           # 删除（热加载）
POST   /api/v1/providers/:name/health    # 手动健康检查
```

## 6.3 管理 API — Keys

```
GET  /api/v1/providers/:provider/keys
Response 200:
{
  "keys": [
    {
      "id": "key-001",
      "name": "deepseek-key-1",
      "status": "ACTIVE",
      "total_requests": 500,
      "total_tokens": 234567,
      "cooling_count": 2,
      "error_count": 0,
      "last_used_at": "2025-01-01T00:00:00Z",
      "last_error_at": null
    }
  ]
}

POST   /api/v1/providers/:provider/keys           # 添加
PUT    /api/v1/providers/:provider/keys/:id        # 更新
DELETE /api/v1/providers/:provider/keys/:id        # 删除
PUT    /api/v1/providers/:provider/keys/:id/status # 更新状态
```

## 6.4 管理 API — Routing

```
GET  /api/v1/routing/aliases
Response 200:
{
  "aliases": {
    "coding-model": {
      "strategy": "priority",
      "providers": [
        {"name": "minimax", "model": "MiniMax-Text-01", "priority": 1},
        {"name": "deepseek", "model": "deepseek-coder", "priority": 2}
      ]
    }
  }
}

PUT    /api/v1/routing/aliases/:alias   # 更新
POST   /api/v1/routing/aliases          # 创建
DELETE /api/v1/routing/aliases/:alias   # 删除
```

## 6.5 管理 API — Usage

```
GET /api/v1/usage?start_time=&end_time=&provider=&model=&group_by=provider|model|hour&limit=&offset=

Response 200:
{
  "records": [...],
  "aggregate": {
    "total_requests": 1000,
    "total_tokens": 500000,
    "total_cost": 12.50,
    "avg_latency_ms": 280,
    "error_count": 15
  },
  "by_provider": {
    "deepseek": {
      "total_requests": 600,
      "total_tokens": 300000,
      "total_cost": 5.00
    }
  }
}

GET /api/v1/usage/aggregate             # 仅聚合数据
```

## 6.6 管理 API — Dashboard

```
GET /api/v1/dashboard/overview

Response 200:
{
  "period": "24h",
  "total_requests": 5000,
  "total_tokens": 2500000,
  "total_cost": 45.00,
  "avg_latency_ms": 260,
  "error_rate": 0.012,
  "active_providers": 4,
  "total_providers": 5,
  "active_keys": 8,
  "requests_per_hour": [
    {"hour": "2025-01-01T00:00:00Z", "count": 200}
  ],
  "tokens_per_hour": [...],
  "cost_per_hour": [...]
}
```

## 6.7 管理 API — System

```
GET  /api/v1/health
Response 200:
{
  "status": "ok",
  "version": "1.0.0",
  "uptime_seconds": 86400,
  "providers": {
    "deepseek": "healthy",
    "minimax": "healthy",
    "glm": "unhealthy"
  }
}

PUT  /api/v1/config/reload              # 热重载配置

GET  /metrics                           # Prometheus 指标
```

## 6.8 API 端点速查表

```
=== 代理端点（客户端调用） ===

POST /v1/chat/completions              OpenAI 协议
POST /v1/messages                       Anthropic 协议
POST /v1beta/models/{model}:*           Google 协议

=== 管理 API ===

GET    /api/v1/providers
GET    /api/v1/providers/:name
POST   /api/v1/providers
PUT    /api/v1/providers/:name
DELETE /api/v1/providers/:name
POST   /api/v1/providers/:name/health

GET    /api/v1/providers/:provider/keys
POST   /api/v1/providers/:provider/keys
PUT    /api/v1/providers/:provider/keys/:id
DELETE /api/v1/providers/:provider/keys/:id
PUT    /api/v1/providers/:provider/keys/:id/status

GET    /api/v1/routing/aliases
PUT    /api/v1/routing/aliases/:alias
POST   /api/v1/routing/aliases
DELETE /api/v1/routing/aliases/:alias

GET    /api/v1/usage
GET    /api/v1/usage/aggregate

GET    /api/v1/dashboard/overview

GET    /api/v1/health
PUT    /api/v1/config/reload
GET    /metrics
```

---

# 第七部分：数据库模型

## 7.1 Migration 001 — Providers

```sql
-- migrations/001_init_providers.up.sql

CREATE TABLE IF NOT EXISTS providers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL UNIQUE,
    protocol        TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS provider_models (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_name       TEXT NOT NULL REFERENCES providers(name),
    model_id            TEXT NOT NULL,
    cost_per_1k_input   REAL NOT NULL DEFAULT 0,
    cost_per_1k_output  REAL NOT NULL DEFAULT 0,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider_name, model_id)
);

CREATE TABLE IF NOT EXISTS model_aliases (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    alias           TEXT NOT NULL,
    provider_name   TEXT NOT NULL REFERENCES providers(name),
    model_id        TEXT NOT NULL REFERENCES provider_models(model_id),
    priority        INTEGER NOT NULL DEFAULT 0,
    weight          INTEGER NOT NULL DEFAULT 0,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(alias, provider_name, model_id)
);
```

## 7.2 Migration 002 — Keys

```sql
-- migrations/002_init_keys.up.sql

CREATE TABLE IF NOT EXISTS api_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_name   TEXT NOT NULL REFERENCES providers(name),
    name            TEXT NOT NULL,
    key_encrypted   TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'ACTIVE',
    cooling_until   DATETIME,
    cooling_count   INTEGER NOT NULL DEFAULT 0,
    total_requests  INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    error_count     INTEGER NOT NULL DEFAULT 0,
    last_used_at    DATETIME,
    last_error_at   DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider_name, name)
);
```

## 7.3 Migration 003 — Usage

```sql
-- migrations/003_init_usage.up.sql

CREATE TABLE IF NOT EXISTS usage_records (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL,
    gateway_key_id  TEXT,
    provider_name   TEXT NOT NULL,
    model_id        TEXT NOT NULL,
    protocol        TEXT NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    cost            REAL NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    is_stream       BOOLEAN NOT NULL DEFAULT FALSE,
    status_code     INTEGER,
    error_type      TEXT,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_usage_created_at ON usage_records(created_at);
CREATE INDEX idx_usage_provider ON usage_records(provider_name);
CREATE INDEX idx_usage_model ON usage_records(model_id);
CREATE INDEX idx_usage_trace ON usage_records(trace_id);
```

## 7.4 Migration 004 — Routing

```sql
-- migrations/004_init_routing.up.sql

CREATE TABLE IF NOT EXISTS routing_configs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    alias           TEXT NOT NULL UNIQUE,
    strategy        TEXT NOT NULL DEFAULT 'priority',
    config_json     TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

## 7.5 Migration 005 — Auth

```sql
-- migrations/005_init_auth.up.sql

CREATE TABLE IF NOT EXISTS gateway_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL UNIQUE,
    key_hash        TEXT NOT NULL UNIQUE,
    allowed_models  TEXT NOT NULL DEFAULT '["*"]',
    rpm             INTEGER NOT NULL DEFAULT 100,
    tpm             INTEGER NOT NULL DEFAULT 500000,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

---

# 第八部分：Provider 实现规范

## 8.0 总体规范

每个 Provider 插件必须回答以下问题并在代码中体现：

```
1. 我使用什么协议？（openai / anthropic / google）
2. 我的 endpoint 是什么？
3. 我的认证方式是什么？（Bearer token / x-api-key / query param）
4. 请求体需要原样透传，但 header 需要怎么处理？
5. 流式响应的格式是什么？（SSE data 行结构、结束标志）
6. Usage 信息从响应体的哪个字段提取？字段名是什么？
7. 错误响应体的结构是什么？怎么判断错误类型？
8. 健康检查怎么做？
9. 我和标准协议有什么已知差异？
```

## 8.1 Provider 差异对比表

```
┌──────────────┬────────────────┬────────────────┬─────────────────┐
│              │ OpenAI Compat  │ Anthropic      │ Google          │
│              │ (DeepSeek等)   │ (MiniMax等)    │ (Gemini)        │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ 认证 Header  │ Authorization  │ x-api-key      │ URL ?key=       │
│              │ Bearer {key}   │ {key}          │ {key}           │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ 端点路径     │ /v1/chat/      │ /v1/messages   │ /models/{m}:    │
│              │ completions    │                │ generateContent │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ SSE 格式     │ data: {...}    │ event: xxx     │ data: {...}     │
│              │ data: [DONE]   │ data: {...}    │                 │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ Usage 字段   │ usage.         │ usage.         │ usageMetadata.  │
│              │ prompt_tokens  │ input_tokens   │ promptTokenCount│
│              │ completion_    │ output_tokens  │ candidatesToken │
│              │ tokens         │                │ Count           │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ 错误格式     │ {"error":      │ {"type":"err", │ {"error":       │
│              │  {"message":..}}│  {"type":..,   │  {"message":..}}│
│              │                │   "message":..}}│                │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ 以上差异由   │ OpenAICompat   │ MiniMax        │ Gemini          │
│ 谁处理       │ Provider       │ Provider       │ Provider        │
├──────────────┼────────────────┼────────────────┼─────────────────┤
│ Gateway      │ 不感知         │ 不感知         │ 不感知          │
│ 核心是否感知 │                │                │                 │
└──────────────┴────────────────┴────────────────┴─────────────────┘
```

## 8.2 OpenAI 兼容 Provider（DeepSeek / GLM / Qwen / Kimi）

```go
// backend/internal/provider/openai_compatible/openai_compatible.go

/*
适用 Provider:
  - DeepSeek:  endpoint = https://api.deepseek.com
  - GLM:       endpoint = https://open.bigmodel.cn/api/paas/v4
  - Qwen:      endpoint = https://dashscope.aliyuncs.com/compatible-mode/v1
  - Kimi:      endpoint = https://api.moonshot.cn/v1

协议: OpenAI Chat Completions API
认证: Authorization: Bearer {api_key}
端点: POST /v1/chat/completions

各 Provider 的已知差异:
  DeepSeek:
    - 基本完全兼容 OpenAI 格式
    - 支持 stream_options.include_usage

  GLM (智谱):
    - 基本兼容 OpenAI 格式
    - 某些模型不支持 function_call
    - 错误格式与 OpenAI 一致

  Qwen (通义千问):
    - 通过 DashScope 的 OpenAI 兼容模式接入
    - 支持 stream_options.include_usage
    - 部分参数支持范围与 OpenAI 不同

  Kimi (Moonshot):
    - 基本兼容 OpenAI 格式
    - 支持 128K 上下文

以上差异由各 Provider 在 SendRequest 中自行处理，Gateway 核心不感知。
*/
package openai_compatible

type OpenAICompatible struct {
    name   string
    config Config
    client *http.Client
    pool   *keypool.Pool
}

type Config struct {
    Name         string
    Endpoint     string
    Timeout      time.Duration
    ExtraHeaders map[string]string // Provider 可注入额外 header
}

func (p *OpenAICompatible) Name() string   { return p.name }
func (p *OpenAICompatible) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

// createHTTPRequest 构造目标 Provider 的 HTTP 请求
func (p *OpenAICompatible) createHTTPRequest(ctx context.Context, req *provider.Request, key *keypool.Key) (*http.Request, error) {
    url := p.config.Endpoint + req.Path

    httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req.Body))
    if err != nil {
        return nil, err
    }

    // 认证
    httpReq.Header.Set("Authorization", "Bearer "+key.Key)
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("X-Request-Id", req.TraceID)

    // Provider 额外 header
    for k, v := range p.config.ExtraHeaders {
        httpReq.Header.Set(k, v)
    }

    // 注意：不修改 req.Body
    return httpReq, nil
}

func (p *OpenAICompatible) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
    key, err := p.pool.Acquire()
    if err != nil {
        return nil, err
    }

    httpReq, err := p.createHTTPRequest(ctx, req, key)
    if err != nil {
        return nil, err
    }

    resp, err := p.client.Do(httpReq)
    if err != nil {
        return nil, &provider.ProviderError{
            ProviderName: p.name,
            ErrorType:    provider.ErrorTypeConnection,
            Message:      err.Error(),
        }
    }
    defer resp.Body.Close()

    bodyBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    if resp.StatusCode != http.StatusOK {
        return nil, p.classifyError(resp.StatusCode, bodyBytes)
    }

    usage := p.extractUsage(bodyBytes)

    return &provider.Response{
        StatusCode: resp.StatusCode,
        Headers:    resp.Header,
        Body:       bodyBytes, // 原样
        Usage:      usage,
    }, nil
}

func (p *OpenAICompatible) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
    key, err := p.pool.Acquire()
    if err != nil {
        return nil, nil, err
    }

    httpReq, err := p.createHTTPRequest(ctx, req, key)
    if err != nil {
        return nil, nil, err
    }

    resp, err := p.client.Do(httpReq)
    if err != nil {
        return nil, nil, &provider.ProviderError{
            ProviderName: p.name,
            ErrorType:    provider.ErrorTypeConnection,
            Message:      err.Error(),
        }
    }

    if resp.StatusCode != http.StatusOK {
        bodyBytes, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        return nil, nil, p.classifyError(resp.StatusCode, bodyBytes)
    }

    chunkChan := make(chan *provider.StreamChunk, 32)

    go func() {
        defer close(chunkChan)
        defer resp.Body.Close()

        scanner := bufio.NewScanner(resp.Body)
        for scanner.Scan() {
            line := scanner.Bytes()

            if !bytes.HasPrefix(line, []byte("data: ")) {
                continue
            }

            data := bytes.TrimPrefix(line, []byte("data: "))

            if string(data) == "[DONE]" {
                break
            }

            chunkChan <- &provider.StreamChunk{Data: data, Err: nil}
        }

        if err := scanner.Err(); err != nil {
            chunkChan <- &provider.StreamChunk{Data: nil, Err: err}
        }
    }()

    return chunkChan, &provider.Response{
        StatusCode: resp.StatusCode,
        Headers:    resp.Header,
    }, nil
}

// extractUsage OpenAI 格式:
//
//	{"usage": {"prompt_tokens": N, "completion_tokens": N, "total_tokens": N}}
func (p *OpenAICompatible) extractUsage(body []byte) *provider.Usage {
    var resp struct {
        Usage *struct {
            PromptTokens     int `json:"prompt_tokens"`
            CompletionTokens int `json:"completion_tokens"`
            TotalTokens      int `json:"total_tokens"`
        } `json:"usage"`
    }
    if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
        return nil
    }
    return &provider.Usage{
        PromptTokens:     resp.Usage.PromptTokens,
        CompletionTokens: resp.Usage.CompletionTokens,
        TotalTokens:      resp.Usage.TotalTokens,
    }
}

// classifyError OpenAI 错误格式:
//
//	{"error": {"message": "...", "type": "..."}}
func (p *OpenAICompatible) classifyError(statusCode int, body []byte) *provider.ProviderError {
    var errResp struct {
        Error struct {
            Message string `json:"message"`
            Type    string `json:"type"`
        } `json:"error"`
    }
    json.Unmarshal(body, &errResp)

    return &provider.ProviderError{
        ProviderName: p.name,
        StatusCode:   statusCode,
        ErrorType:    provider.ClassifyError(statusCode),
        Message:      errResp.Error.Message,
        RawError:     body,
    }
}

func (p *OpenAICompatible) HealthCheck(ctx context.Context) error {
    req, _ := http.NewRequestWithContext(ctx, "GET", p.config.Endpoint+"/models", nil)
    resp, err := p.client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("health check failed: %d", resp.StatusCode)
    }
    return nil
}

func (p *OpenAICompatible) Close() error {
    p.client.CloseIdleConnections()
    return nil
}
```

## 8.3 Anthropic 兼容 Provider（MiniMax）

```go
// backend/internal/provider/minimax/minimax.go

/*
Provider: MiniMax
Protocol: anthropic
Endpoint: https://api.minimax.chat

认证方式:
  x-api-key: {api_key}
  anthropic-version: 2023-06-01

端点:
  POST /v1/messages

已知与 Anthropic 标准的差异:
  - MiniMax 的 anthropic 兼容接口是模仿实现，不是原生 Anthropic API
  - 可能存在以下差异:
    a. 某些 Anthropic 特有参数可能不支持（如 top_k）
    b. 错误响应体格式可能与 Anthropic 标准略有不同
    c. 流式事件类型可能不完全一致
    d. usage 字段名或结构可能有细微差别
  - 这些差异由本 Provider 在 SendRequest 中自行处理
  - Gateway 核心代码对此完全无感

Anthropic 请求格式:
  POST /v1/messages
  Content-Type: application/json
  x-api-key: {key}
  anthropic-version: 2023-06-01

  {
    "model": "MiniMax-Text-01",
    "max_tokens": 1024,
    "system": "You are helpful",
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }

Anthropic 响应格式:
  {
    "id": "msg_xxx",
    "type": "message",
    "role": "assistant",
    "content": [{"type": "text", "text": "Hi!"}],
    "usage": {"input_tokens": 10, "output_tokens": 5}
  }

Anthropic 流式事件:
  event: message_start       (包含 usage.input_tokens)
  event: content_block_start
  event: content_block_delta  (包含 text delta)
  event: content_block_stop
  event: message_delta        (包含 usage.output_tokens 和 stop_reason)
  event: message_stop
*/
package minimax

type MiniMax struct {
    name   string
    config Config
    client *http.Client
    pool   *keypool.Pool
}

type Config struct {
    Name         string
    Endpoint     string
    Timeout      time.Duration
    ExtraHeaders map[string]string
}

func (p *MiniMax) createHTTPRequest(ctx context.Context, req *provider.Request, key *keypool.Key) (*http.Request, error) {
    url := p.config.Endpoint + req.Path // /v1/messages

    httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req.Body))
    if err != nil {
        return nil, err
    }

    // Anthropic 协议认证方式（不是 Bearer token）
    httpReq.Header.Set("x-api-key", key.Key)
    httpReq.Header.Set("anthropic-version", "2023-06-01")
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("X-Request-Id", req.TraceID)

    for k, v := range p.config.ExtraHeaders {
        httpReq.Header.Set(k, v)
    }

    return httpReq, nil
}

func (p *MiniMax) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
    key, err := p.pool.Acquire()
    if err != nil {
        return nil, err
    }

    httpReq, err := p.createHTTPRequest(ctx, req, key)
    if err != nil {
        return nil, err
    }

    resp, err := p.client.Do(httpReq)
    if err != nil {
        return nil, &provider.ProviderError{
            ProviderName: p.name,
            ErrorType:    provider.ErrorTypeConnection,
            Message:      err.Error(),
        }
    }
    defer resp.Body.Close()

    bodyBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    if resp.StatusCode != http.StatusOK {
        return nil, p.classifyError(resp.StatusCode, bodyBytes)
    }

    usage := p.extractUsage(bodyBytes)

    return &provider.Response{
        StatusCode: resp.StatusCode,
        Headers:    resp.Header,
        Body:       bodyBytes,
        Usage:      usage,
    }, nil
}

// extractUsage Anthropic 格式:
//
//	{"usage": {"input_tokens": N, "output_tokens": N}}
func (p *MiniMax) extractUsage(body []byte) *provider.Usage {
    var resp struct {
        Usage *struct {
            InputTokens  int `json:"input_tokens"`
            OutputTokens int `json:"output_tokens"`
        } `json:"usage"`
    }
    if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
        return nil
    }
    return &provider.Usage{
        PromptTokens:     resp.Usage.InputTokens,
        CompletionTokens: resp.Usage.OutputTokens,
        TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
    }
}

// classifyError Anthropic 错误格式:
//
//	{"type": "error", "error": {"type": "overloaded_error", "message": "..."}}
func (p *MiniMax) classifyError(statusCode int, body []byte) *provider.ProviderError {
    var errResp struct {
        Type  string `json:"type"`
        Error *struct {
            Type    string `json:"type"`
            Message string `json:"message"`
        } `json:"error"`
    }
    json.Unmarshal(body, &errResp)

    msg := ""
    if errResp.Error != nil {
        msg = errResp.Error.Message
    }

    return &provider.ProviderError{
        ProviderName: p.name,
        StatusCode:   statusCode,
        ErrorType:    provider.ClassifyError(statusCode),
        Message:      msg,
        RawError:     body,
    }
}

func (p *MiniMax) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
    // Anthropic 流式格式与 OpenAI 不同:
    //   OpenAI:   "data: {...}" / "data: [DONE]"
    //   Anthropic: "event: xxx\ndata: {...}\n\n"
    //
    // Provider 自行解析 Anthropic SSE 格式
    // 输出统一的 StreamChunk 给 Gateway

    // 解析逻辑:
    //   读取 "event: xxx" 行（记录事件类型）
    //   读取 "data: {...}" 行
    //   将 data 内容放入 StreamChunk.Data
    //   遇到 event: message_stop 时关闭 channel
    //
    // Usage 提取:
    //   event: message_start → {"usage": {"input_tokens": N}}
    //   event: message_delta → {"usage": {"output_tokens": N}}
    //   流结束后合并这两个 usage
}

func (p *MiniMax) HealthCheck(ctx context.Context) error {
    // 轻量级请求测试连通性
}

func (p *MiniMax) Close() error {
    p.client.CloseIdleConnections()
    return nil
}
```

## 8.4 Google 兼容 Provider（Gemini）

```go
// backend/internal/provider/gemini/gemini.go

/*
Provider: Gemini
Protocol: google
Endpoint: https://generativelanguage.googleapis.com/v1beta

认证方式:
  Google API 使用 API Key 作为 URL 查询参数（不是 header）
  POST ...?key={api_key}

端点:
  非流式: POST /v1beta/models/{model}:generateContent?key={key}
  流式:   POST /v1beta/models/{model}:streamGenerateContent?key={key}&alt=sse

已知差异:
  - API Key 在 URL 中，不在 header 中
  - 请求体结构与 OpenAI/Anthropic 完全不同
  - 响应体结构也不同
  - "零转换" 原则意味着客户端必须发 Google 格式的请求
  - Gateway 不会把 OpenAI 格式的请求转换成 Google 格式
*/
package gemini

func (p *Gemini) createHTTPRequest(ctx context.Context, req *provider.Request, key *keypool.Key) (*http.Request, error) {
    // Google API 特殊：API Key 在 URL 查询参数中
    url := p.config.Endpoint + req.Path + "?key=" + key.Key

    httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req.Body))
    if err != nil {
        return nil, err
    }

    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("X-Request-Id", req.TraceID)
    // 不设置 Authorization header，Key 在 URL 中

    return httpReq, nil
}

// extractUsage Google 格式:
//
//	{"usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": N, "totalTokenCount": N}}
func (p *Gemini) extractUsage(body []byte) *provider.Usage {
    var resp struct {
        UsageMetadata *struct {
            PromptTokenCount     int `json:"promptTokenCount"`
            CandidatesTokenCount int `json:"candidatesTokenCount"`
            TotalTokenCount      int `json:"totalTokenCount"`
        } `json:"usageMetadata"`
    }
    if err := json.Unmarshal(body, &resp); err != nil || resp.UsageMetadata == nil {
        return nil
    }
    return &provider.Usage{
        PromptTokens:     resp.UsageMetadata.PromptTokenCount,
        CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
        TotalTokens:      resp.UsageMetadata.TotalTokenCount,
    }
}
```

---

# 第九部分：Proxy 流程详解

## 9.1 非流式请求完整流程

```
客户端 POST /v1/chat/completions
     |
     v
[Middleware 链]
  1. Trace: 生成或提取 X-Request-Id
  2. Logger: 记录请求开始
  3. Auth: 验证 Gateway API Key
  4. RateLimit: 检查客户端 RPM/TPM
     |
     v
[Proxy Engine.HandleRequest]
  |
  1. 读取请求 Body 到内存
  2. 解析模型名（从 JSON body 的 model 字段提取）
  3. 解析是否 stream（从 JSON body 的 stream 字段提取）
  |
  4. 调用 Router.Route(ctx, req)
     |
     4a. 模型名 → 别名查找
     4b. 确定协议（/v1/messages → anthropic, /v1/chat/completions → openai）
     4c. 获取别名配置下的 Provider 列表
     4d. 过滤：协议不匹配的排除 / Circuit Breaker OPEN 的排除 / Key Pool 无可用 Key 的排除
     4e. 调用 Policy.Select 获取最佳 Provider
     4f. 从 Key Pool 获取 Key
     4g. 返回 RouteIterator（支持 failover 迭代）
     |
     4h. 如果 RouteIterator 为空 → 返回 503
  |
  5. 设置请求 headers:
     - Authorization: Bearer {key}
     - X-Request-Id: {trace_id}
     - 删除原始 Authorization
  |
  6. 调用 Provider.SendRequest(ctx, req)
     |
     成功 (status 2xx):
       7a. Key Pool.ReportSuccess(key)
       7b. Circuit Breaker.RecordSuccess()
       7c. 构造 Usage Record:
           - trace_id, provider_name, model_id
           - input_tokens = Provider.Usage.PromptTokens
           - output_tokens = Provider.Usage.CompletionTokens
           - cost = input_tokens * cost_per_1k_input / 1000
                   + output_tokens * cost_per_1k_output / 1000
           - latency_ms = 请求耗时
       7d. Usage Collector.Record(record)  // 异步
       7e. Metrics.Record(...)
       7f. 将 Provider Response 原样写回客户端:
           - 复制 status code
           - 复制 headers（删除 hop-by-hop headers）
           - 复制 body
     |
     失败:
       8a. 解析 ProviderError
       8b. 如果是 rate_limit:
           - Key Pool.ReportRateLimit(key)
           - 先尝试同 Provider 其他 Key
           - 如果都 429，Iterator.Next() 下一个 Provider
       8c. 如果是 server_error/timeout/connection:
           - Circuit Breaker.RecordFailure(error_type)
           - Iterator.Next() 下一个 Provider
       8d. 如果是 invalid_request (400):
           - 直接返回客户端，不做 failover
       8e. 如果所有 Provider 都失败:
           - 返回 502 Bad Gateway
           - 响应体格式与原始协议一致:
             OpenAI: {"error": {"message": "...", "type": "gateway_error"}}
             Anthropic: {"type": "error", "error": {"type": "overloaded", "message": "..."}}
```

## 9.2 流式请求完整流程

```
客户端 POST /v1/chat/completions (stream: true)
     |
     v
[Middleware 链 — 同上]
     |
     v
[Proxy Engine.HandleStreamRequest]
  |
  1-5 同非流式
  |
  6. 调用 Provider.SendStreamRequest(ctx, req)
     |
     调用失败（流未开始）:
       → 可以 failover，Iterator.Next() 下一个 Provider
       → 如果所有 Provider 都失败，返回 502
     |
     成功: 获取到 (<-chan *StreamChunk, *Response, error)
     |
  7. 设置 SSE response headers:
     Content-Type: text/event-stream
     Cache-Control: no-cache
     Connection: keep-alive
     X-Request-Id: {trace_id}
  |
  8. 启动 goroutine 读取 channel:
     for chunk := range streamChan {
         if chunk.Err != nil {
             // 流中途错误 — 不可 failover
             // 记录 error metrics
             // 向客户端发送 error event
             break
         }
         // 写入: "data: {chunk.Data}\n\n"
         c.Writer.Write(data)
         c.Writer.Flush()
     }
  |
  9. 流结束后:
     - 从 Response 中提取 Usage
     - 记录 Usage 和 Metrics
     - Key Pool.ReportSuccess(key)
     - Circuit Breaker.RecordSuccess()
  |
  重要规则:
    流式请求一旦开始发送数据给客户端，中途失败不做 failover。
    原因：客户端已经收到了部分响应，重试会导致重复或不一致。
    Failover 只发生在 Provider.SendStreamRequest 调用失败时（流还未开始）。
```

---

# 第十部分：超时与重试策略

```yaml
# config.yaml 中的超时和重试配置

timeouts:
  # Gateway server 超时
  server_read: 30s          # 读取客户端请求的超时
  server_write: 120s        # 写响应的超时（流式需要较长）
  server_idle: 120s         # 空闲连接超时

  # Gateway 到 Provider 的超时
  provider_default: 60s     # 默认 Provider 超时
  # 每个 Provider 可单独覆盖:
  # providers.deepseek.timeout: 60s
  # providers.minimax.timeout: 90s

  # 全局请求超时（从接收请求到返回响应的最大时间）
  # 即使 failover 会尝试多个 Provider，总时间不能超过此值
  request_total: 300s

retry:
  # 是否启用 failover
  enabled: true

  # 最大 failover 次数
  max_attempts: 3

  # 以下错误类型不触发 failover（直接返回客户端）
  no_failover_on:
    - "invalid_request"     # 400: 客户端请求格式错误
    - "auth"                # 401/403: Gateway 自身的 key 问题

  # 以下错误类型触发 failover
  failover_on:
    - "rate_limit"          # 429: 先尝试同 Provider 其他 Key，再 failover
    - "server_error"        # 5xx: 直接 failover
    - "timeout"             # 超时: 直接 failover
    - "connection"          # 连接失败: 直接 failover
```

---

# 第十一部分：链路追踪

```
每条请求必须有唯一的 trace-id，贯穿整个链路:

客户端请求
  → Gateway 生成 trace-id（如果客户端没提供）
  → 日志中所有该请求的条目都带此 trace-id
  → 转发给 Provider 时在 header 中携带
  → Usage 记录中包含此 trace-id
  → 响应 header 中返回此 trace-id

Trace ID 格式:
  UUID v4: "550e8400-e29b-41d4-a716-446655440000"

Header 名称:
  发送: X-Request-Id: {trace-id}
  接收: 支持 X-Request-Id, X-Trace-Id, Traceparent

日志格式（每条日志都包含 trace_id）:
  {
    "level": "info",
    "msg": "request completed",
    "trace_id": "550e8400-...",
    "provider": "deepseek",
    "model": "deepseek-coder",
    "status": 200,
    "latency_ms": 230,
    "input_tokens": 150,
    "output_tokens": 500
  }

客户端可见:
  响应 header 中返回 X-Request-Id，方便客户端关联日志
```

---

# 第十二部分：配置热更新

```
支持热更新的配置:
  - providers (添加/移除/启用/禁用 Provider)
  - providers[].keys (添加/移除 Key)
  - routing.aliases (修改路由规则)
  - keypool.* (Key Pool 参数)
  - auth.keys (Gateway 认证 Key)
  - circuit_breaker.* (熔断器参数)

不支持热更新的配置（需要重启）:
  - server.* (监听端口等)
  - database.* (数据库连接)
  - redis.* (Redis 连接)

热更新方式:
  1. API: PUT /api/v1/config/reload
  2. Signal: kill -SIGHUP {pid}

热更新流程:
  1. 读取新配置
  2. 验证配置格式
  3. 对比差异:
     - 新增 Provider → 创建实例，HealthCheck，注册
     - 移除 Provider → 标记为 draining，等待进行中的请求完成，Close
     - 新增 Key → 加密存储，加入 Pool
     - 移除 Key → 标记为 DISABLED
     - 路由规则变更 → 原子替换 Router 配置
  4. 记录日志
```

---

# 第十三部分：Prometheus 指标

```go
// backend/internal/metrics/prometheus.go

package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
    RequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "gateway",
            Name:      "requests_total",
            Help:      "Total number of requests",
        },
        []string{"provider", "model", "status", "protocol"},
    )

    RequestDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "gateway",
            Name:      "request_duration_seconds",
            Help:      "Request duration in seconds",
            Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
        },
        []string{"provider", "model"},
    )

    TokensTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "gateway",
            Name:      "tokens_total",
            Help:      "Total tokens processed",
        },
        []string{"provider", "model", "type"}, // type: input|output
    )

    CostTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "gateway",
            Name:      "cost_total",
            Help:      "Total cost in USD",
        },
        []string{"provider", "model"},
    )

    KeyPoolActive = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "gateway",
            Name:      "keypool_active_keys",
            Help:      "Number of active keys",
        },
        []string{"provider"},
    )

    CircuitBreakerState = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "gateway",
            Name:      "circuit_breaker_state",
            Help:      "Circuit breaker state (0=closed, 1=open, 2=half_open)",
        },
        []string{"provider"},
    )

    FailoversTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "gateway",
            Name:      "failovers_total",
            Help:      "Total number of failovers",
        },
        []string{"from_provider", "to_provider"},
    )
)
```

---

# 第十四部分：Docker 配置

## 14.1 docker-compose.yml

```yaml
services:
  gateway:
    build:
      context: ./backend
      dockerfile: Dockerfile
    ports:
      - "8080:8080"
      - "9090:9090"
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - gateway-data:/app/data
    environment:
      - GATEWAY_CONFIG=/app/config.yaml
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    restart: unless-stopped

  frontend:
    build:
      context: ./frontend
      dockerfile: Dockerfile
    ports:
      - "3000:80"
    depends_on:
      - gateway
    restart: unless-stopped

  postgres:
    image: postgres:16-alpine
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: gateway
      POSTGRES_USER: gateway
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-gateway_dev_password}
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gateway"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:latest
    ports:
      - "9091:9090"
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus
    restart: unless-stopped

  grafana:
    image: grafana/grafana:latest
    ports:
      - "3001:3000"
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD:-admin}
    volumes:
      - grafana-data:/var/lib/grafana
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
    depends_on:
      - prometheus
    restart: unless-stopped

volumes:
  gateway-data:
  postgres-data:
  redis-data:
  prometheus-data:
  grafana-data:
```

## 14.2 docker-compose.dev.yml

```yaml
services:
  gateway:
    build:
      context: ./backend
      dockerfile: Dockerfile.dev
    ports:
      - "8080:8080"
      - "9090:9090"
    volumes:
      - ./backend:/app
      - ./config.yaml:/app/config.yaml
    environment:
      - GATEWAY_CONFIG=/app/config.yaml
      - GIN_MODE=debug
```

## 14.3 Prometheus 配置

```yaml
# prometheus/prometheus.yml

global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'gateway'
    static_configs:
      - targets: ['gateway:9090']
    metrics_path: /metrics
```

---

# 第十五部分：Makefile

```makefile
# backend/Makefile

.PHONY: build run dev test lint migrate docker

build:
	go build -o bin/gateway ./cmd/gateway

run: build
	./bin/gateway --config ../config.yaml

dev:
	air -c .air.toml

test:
	go test ./... -v -race -coverprofile=coverage.out

lint:
	golangci-lint run ./...

migrate-up:
	migrate -path ./migrations -database "$(DB_DSN)" up

migrate-down:
	migrate -path ./migrations -database "$(DB_DSN)" down

docker:
	docker compose -f ../docker-compose.yml up -d

docker-dev:
	docker compose -f ../docker-compose.dev.yml up -d

docker-down:
	docker compose -f ../docker-compose.yml down
```

---

# 第十六部分：前端 Dashboard 详细设计

## 16.1 Overview 页面

```
┌─────────────────────────────────────────────────────────────┐
│  LLM Gateway                                     v1.0.0    │
├────────┬────────────┬─────────┬──────────┬────────┬─────────┤
│ Overview│ Providers │  Keys   │ Routing  │ Usage  │Settings │
├────────┴────────────┴─────────┴──────────┴────────┴─────────┤
│                                                             │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐      │
│  │ Requests │ │  Tokens  │ │   Cost   │ │ Latency  │      │
│  │  12,345  │ │  2.5M    │ │ $45.20   │ │  280ms   │      │
│  │ +12% ↑   │ │ +8% ↑    │ │ -3% ↓    │ │ +5% ↑    │      │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘      │
│                                                             │
│  ┌─────────────────────────┐ ┌─────────────────────────┐   │
│  │ Requests Over Time      │ │ Token Usage Over Time   │   │
│  │ [折线图 - ECharts]      │ │ [面积图 - ECharts]      │   │
│  │ 24h / 7d / 30d 切换    │ │ 24h / 7d / 30d 切换    │   │
│  └─────────────────────────┘ └─────────────────────────┘   │
│                                                             │
│  ┌─────────────────────────┐ ┌─────────────────────────┐   │
│  │ Provider Status         │ │ Error Rate              │   │
│  │ [状态卡片列表]           │ │ [柱状图 - 按 Provider]  │   │
│  │                         │ │                         │   │
│  │ DeepSeek  ● Healthy     │ │                         │   │
│  │ MiniMax   ● Healthy     │ │                         │   │
│  │ GLM       ● Unhealthy   │ │                         │   │
│  └─────────────────────────┘ └─────────────────────────┘   │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## 16.2 Providers 页面

```
┌─────────────────────────────────────────────────────────────┐
│ Providers                                             [+Add]│
├─────────────────────────────────────────────────────────────┤
│                                                             │
│ ┌─ DeepSeek ────────────────────────────────────────────┐  │
│ │ Protocol: openai    Endpoint: api.deepseek.com        │  │
│ │ Status: ● Healthy    Circuit: CLOSED                   │  │
│ │                                                       │  │
│ │ Keys: ██████░░ 4/6 Active    (1 Cooling, 1 Disabled) │  │
│ │                                                       │  │
│ │ Models:                                               │  │
│ │   deepseek-coder  [coding-model]  $0.0014/1K in      │  │
│ │   deepseek-chat   [chat-model]    $0.0014/1K in      │  │
│ │                                                       │  │
│ │ Stats (24h):                                          │  │
│ │   Requests: 3,456  Tokens: 1.2M  Latency: 230ms     │  │
│ │   Errors: 12 (0.35%)                                  │  │
│ │                                                       │  │
│ │ [Health Check] [View Keys] [Edit] [Disable]           │  │
│ └───────────────────────────────────────────────────────┘  │
│                                                             │
│ ┌─ MiniMax ─────────────────────────────────────────────┐  │
│ │ ...                                                   │  │
│ └───────────────────────────────────────────────────────┘  │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## 16.3 Keys 页面

```
┌─────────────────────────────────────────────────────────────┐
│ Keys                              Provider: [All ▼]  [+Add]│
├──────┬──────────┬──────────┬────────┬────────┬──────────────┤
│ Name │ Provider │ Status   │ Reqs   │ Tokens │ Last Used    │
├──────┼──────────┼──────────┼────────┼────────┼──────────────┤
│ dk-1 │ DeepSeek │ ● ACTIVE │ 3,456  │ 1.2M   │ 2 min ago   │
│ dk-2 │ DeepSeek │ ● ACTIVE │ 2,100  │ 800K   │ 5 min ago   │
│ dk-3 │ DeepSeek │ ○ COOL   │ 500    │ 200K   │ 30 min ago  │
│ mm-1 │ MiniMax  │ ● ACTIVE │ 1,200  │ 500K   │ 1 min ago   │
│ mm-2 │ MiniMax  │ ✕ DIS    │ 0      │ 0      │ never       │
├──────┴──────────┴──────────┴────────┴────────┴──────────────┤
│                                                             │
│ Key Detail Panel (点击行展开):                               │
│ ┌───────────────────────────────────────────────────────┐  │
│ │ Name: dk-3                                            │  │
│ │ Key: sk-xxxx...xxxx (masked)                          │  │
│ │ Status: COOLING                                       │  │
│ │ Cooling Until: 2025-01-01 00:05:00                    │  │
│ │ Cooling Count: 3 / 5                                  │  │
│ │ Error Count: 0                                        │  │
│ │ Total Requests: 500                                   │  │
│ │ Total Tokens: 200,000                                 │  │
│ │                                                       │  │
│ │ [Force Active] [Disable] [Delete]                     │  │
│ └───────────────────────────────────────────────────────┘  │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## 16.4 Routing 页面

```
┌─────────────────────────────────────────────────────────────┐
│ Routing Rules                                       [+Add]  │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│ ┌─ coding-model ──────────── Strategy: [Priority ▼] ──────┐│
│ │                                                         ││
│ │  # │ Provider  │ Model          │ Priority │ Status     ││
│ │  1 │ MiniMax   │ MiniMax-Text-01│    1     │ ● Healthy  ││
│ │  2 │ DeepSeek  │ deepseek-coder │    2     │ ● Healthy  ││
│ │  3 │ GLM       │ glm-4-plus     │    3     │ ○ Unhealthy││
│ │                                                         ││
│ │ Failover Flow:                                          ││
│ │  MiniMax ──fail──▶ DeepSeek ──fail──▶ GLM ──fail──▶ 502││
│ │                                                         ││
│ │ [Edit] [Delete] [Test]                                  ││
│ └─────────────────────────────────────────────────────────┘│
│                                                             │
│ ┌─ chat-model ─────────────── Strategy: [Weight ▼] ───────┐│
│ │                                                         ││
│ │  # │ Provider  │ Model         │ Weight │ Status        ││
│ │  1 │ DeepSeek  │ deepseek-chat │  70%   │ ● Healthy     ││
│ │  2 │ GLM       │ glm-4-plus    │  30%   │ ○ Unhealthy   ││
│ │                                                         ││
│ │ [Edit] [Delete] [Test]                                  ││
│ └─────────────────────────────────────────────────────────┘│
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## 16.5 Usage 页面

```
┌─────────────────────────────────────────────────────────────┐
│ Usage Statistics       Period: [24h ▼]  Provider: [All ▼]  │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Cost Breakdown                                       │  │
│  │ [饼图: DeepSeek 40%, MiniMax 35%, GLM 25%]         │  │
│  └──────────────────────────────────────────────────────┘  │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Token Usage Timeline                                 │  │
│  │ [堆叠面积图: input tokens / output tokens]            │  │
│  └──────────────────────────────────────────────────────┘  │
│                                                             │
│  Detailed Records:                                          │
│  ┌───────┬──────────┬─────────────┬───────┬───────┬──────┐ │
│  │ Time  │ Provider │ Model       │ In    │ Out   │ Cost │ │
│  ├───────┼──────────┼─────────────┼───────┼───────┼──────┤ │
│  │ 00:05 │ DeepSeek │ deepseek-.. │ 1,200 │ 3,400 │$0.01│ │
│  │ 00:04 │ MiniMax  │ MiniMax-..  │ 800   │ 2,100 │$0.01│ │
│  │ 00:03 │ DeepSeek │ deepseek-.. │ 500   │ 1,500 │$0.01│ │
│  └───────┴──────────┴─────────────┴───────┴───────┴──────┘ │
│                                                             │
│  [Export CSV] [Export JSON]                                 │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

---

# 第十七部分：CLAUDE.md（AI Agent 指令）

```markdown
# CLAUDE.md — AI Coding Agent 指令

## 必读

在执行任何修改前，你必须：
1. 阅读本文件 (CLAUDE.md)
2. 阅读 config.example.yaml 了解配置格式
3. 阅读 internal/provider/provider.go 了解 Provider 接口
4. 检查现有代码结构，保持模块边界

## 核心规则

### 禁止
- 禁止在 Router/Policy/API 层写 provider-specific 的 if/switch
- 禁止修改 Provider 的请求体或响应体（零转换原则）
- 禁止在 Gateway 核心代码中硬编码 Provider 信息
- 禁止使用 Inter/Roboto/Arial 作为前端主要字体
- 禁止使用紫色渐变配白色的 AI 通用设计

### 必须
- 所有新增 Provider 必须实现 provider.Provider 接口
- 所有新增 Provider 必须在 Registry 中注册
- 所有新增功能必须更新 ARCHITECTURE.md 和 API.md
- 所有新增功能必须包含测试
- 所有数据库变更必须有 migration 文件
- Key 的存储必须加密（AES-256-GCM）
- 请求追踪 ID (trace-id) 必须贯穿全链路

### 协议零转换规则
- Gateway 是协议感知的透明代理
- 请求体（body）原样透传，不做任何字段映射
- 响应体原样透传
- Gateway 只重写 header（Authorization、X-Request-Id）
- 模型名映射属于路由层面，不属于协议转换
- Provider 之间的协议差异由各自的 Provider 插件处理
- Gateway 核心代码不感知任何 Provider 的协议细节

### 流式响应规则
- 流式请求一旦开始发送数据给客户端，中途失败不做 failover
- 流式 failover 只发生在 Provider.SendStreamRequest 调用失败时
- 每个 SSE chunk 必须立即 Flush，不允许缓冲

### 错误处理规则
- 429 (Rate Limit) → Key Pool 处理，尝试同 Provider 的其他 Key
- 如果同 Provider 所有 Key 都 429 → 尝试下一个 Provider
- 5xx (Server Error) → Circuit Breaker 处理，尝试下一个 Provider
- 401/403 (Auth Error) → 标记 Key 为 DISABLED，尝试下一个 Provider
- 400 (Bad Request) → 直接返回客户端，不做 failover
- 502 (All Providers Failed) → 所有 Provider 都失败时返回

### 前端规则
- 使用 Naive UI 组件库
- 使用 TypeScript，禁止 any
- 使用 Composition API (setup script)
- 图表使用 ECharts
- 暗色主题为主

## 修改完成后
必须更新：
- CHANGELOG.md: 添加变更记录
- ARCHITECTURE.md: 如果涉及架构变更
- API.md: 如果涉及 API 变更
```

---

# 第十八部分：Phase 1 实施计划

## 实施顺序（严格按序）

```
Phase 1.1 — 基础骨架
  ├── 初始化 Go 项目 (go mod init)
  ├── 目录结构创建
  ├── 配置加载 (config.yaml → Viper)
  ├── 数据库初始化 (GORM + SQLite)
  ├── 所有 migration 文件
  ├── HTTP Server 启动 (Gin)
  ├── 健康检查端点 (GET /api/v1/health)
  └── 日志初始化 (Zap)

Phase 1.2 — Provider 接口 + 第一个 Provider
  ├── Provider 接口定义 (provider.go)
  ├── ProviderError 和 ErrorType 定义
  ├── Provider Registry (registry.go)
  ├── Provider Manager (manager.go)
  ├── OpenAI Compatible 通用实现 (openai_compatible.go)
  │   ├── createHTTPRequest
  │   ├── extractUsage (OpenAI 格式)
  │   ├── classifyError (OpenAI 格式)
  │   ├── SendRequest
  │   ├── SendStreamRequest (SSE: data: {...} / data: [DONE])
  │   └── HealthCheck
  ├── DeepSeek Provider（直接使用 OpenAI Compatible）
  ├── Provider 配置加载
  └── 测试：直接调用 DeepSeek API（非流式 + 流式）

Phase 1.3 — Key Pool
  ├── Key 实体和状态机 (key.go)
  ├── Key Pool 管理器 (pool.go)
  ├── 轮询调度器 (scheduler.go)
  ├── Key 加密存储
  ├── Key CRUD API (handler/key.go)
  └── 测试：多 Key 轮询和故障切换

Phase 1.4 — Router + Policy
  ├── 路由引擎 (router.go)
  ├── RouteIterator（支持 failover 迭代）
  ├── 模型别名解析 (model_alias.go)
  ├── 协议匹配逻辑 (determineProtocol)
  ├── Priority 策略 (priority.go)
  ├── Weight 策略 (weight.go)
  └── 测试：模型别名解析和策略选择

Phase 1.5 — Proxy Engine
  ├── 非流式代理 (proxy.go)
  │   ├── 完整请求链路
  │   ├── Failover 逻辑
  │   └── 响应原样回传
  ├── 流式代理 (stream.go)
  │   ├── SSE 流转发
  │   ├── 流开始前 failover
  │   ├── 流中途失败处理（不 failover）
  │   └── 客户端断开处理
  ├── Auth 中间件
  ├── 请求日志中间件
  ├── Trace 中间件 (X-Request-Id)
  └── 测试：完整请求链路（非流式 + 流式）

Phase 1.6 — Circuit Breaker
  ├── 熔断器实现 (breaker.go)
  │   ├── 滑动窗口
  │   ├── 三状态状态机
  │   └── 错误类型过滤
  ├── 集成到 Router
  └── 测试：熔断和恢复

Phase 1.7 — Usage + Metrics
  ├── Usage Collector (collector.go)
  ├── Usage Repository (GORM 实现)
  ├── Usage API (handler/usage.go)
  ├── Prometheus 指标注册
  └── 测试：Usage 记录和查询

Phase 1.8 — 更多 Provider
  ├── Anthropic 兼容 Provider 基础实现
  │   ├── createHTTPRequest (x-api-key header)
  │   ├── extractUsage (Anthropic 格式)
  │   ├── classifyError (Anthropic 格式)
  │   └── SendStreamRequest (SSE: event/data 格式)
  ├── MiniMax Provider（使用 Anthropic 兼容基础）
  ├── GLM Provider（使用 OpenAI 兼容基础）
  ├── Qwen Provider（使用 OpenAI 兼容基础）
  ├── Kimi Provider（使用 OpenAI 兼容基础）
  └── 测试：多 Provider 切换 + 协议匹配

Phase 1.9 — 前端基础
  ├── Vue3 项目初始化 (Vite + TS)
  ├── Naive UI 集成
  ├── 暗色主题配置
  ├── Overview 页面
  ├── Providers 页面
  ├── Keys 页面
  ├── Usage 页面
  └── Routing 页面

Phase 1.10 — Docker + 文档
  ├── Dockerfile (backend)
  ├── Dockerfile (frontend)
  ├── docker-compose.yml
  ├── docker-compose.dev.yml
  ├── prometheus.yml
  ├── ARCHITECTURE.md（包含 Provider 差异对比表）
  ├── API.md
  ├── CHANGELOG.md
  └── README.md（包含协议配对矩阵说明）
```

---

# 第十九部分：Testing 规范

```
单元测试:
  - 每个 Policy 策略的 Select 逻辑
  - Key Pool 的状态转换（ACTIVE → COOLING → ACTIVE / DISABLED）
  - Circuit Breaker 的状态转换（CLOSED → OPEN → HALF_OPEN → CLOSED）
  - 模型别名解析
  - Token 计数
  - 成本计算
  - determineProtocol 函数

集成测试:
  - 完整请求链路（使用 mock Provider）
  - Failover 流程（第一个 Provider 失败 → 切换到第二个）
  - 流式请求链路
  - 流式中途失败处理
  - Auth 中间件
  - Rate Limit 中间件

端到端测试:
  - 启动 Gateway
  - 发送真实请求到 DeepSeek（CI 中用 mock）
  - 验证 Usage 记录
  - 验证 Prometheus 指标

测试文件命名:
  backend/internal/keypool/pool.go      → pool_test.go
  backend/internal/router/router.go     → router_test.go
  backend/internal/circuit/breaker.go   → breaker_test.go
  backend/internal/provider/openai_compatible/openai_compatible.go → openai_compatible_test.go
```

---

# 第二十部分：安全要求

```
1. Key 存储加密
   - Provider API Key 使用 AES-256-GCM 加密后存储
   - 加密密钥从环境变量 GATEWAY_ENCRYPTION_KEY 获取
   - 启动时解密到内存，运行时不再访问加密存储

2. Gateway API Key
   - 客户端认证用的 Gateway Key 存储 hash（bcrypt）
   - 传输时使用 Bearer token 格式

3. 日志脱敏
   - 日志中不记录 API Key 明文
   - 日志中不记录请求体中的敏感字段
   - API Key 在日志中显示为 sk-xxx...xxx（只保留前 4 后 4 位）

4. 网络安全
   - Gateway 和 Provider 之间强制 HTTPS
   - 生产环境 Gateway 前应有 TLS 终结（Nginx/Caddy）
```

---

# 附录 A：快速参考卡片

```
Gateway 本质 = 协议感知的透明代理（不是 Nginx，不是 LiteLLM）

一句话原则:
  读 body → 只提取 model/stream → 路由 → body 原样转发

三大职责边界:
  Gateway:  路由 / 调度 / 限流 / 统计 / 认证 / 故障恢复
  Provider: API 调用 / 协议细节 / Usage 解析 / Error 分类
  Client:   发送原始协议格式的请求

核心抽象:
  Provider 接口 → 各 Provider 实现
  Key Pool      → 管理 API Key 生命周期
  Router        → 模型别名 → Provider 选择
  Circuit Breaker → Provider 健康状态
  Usage Collector → 异步批量记录

请求协议对应关系:
  /v1/messages           → Anthropic → MiniMax, Kimi, GLM
  /v1/chat/completions   → OpenAI    → DeepSeek, Qwen, MiniMax, Kimi, GLM
  /v1beta/models/*       → Google    → Gemini

Failover 规则:
  429 → 同 Provider 换 Key → 换 Provider
  5xx → 换 Provider
  400 → 不 failover，直接返回客户端
  流式中途失败 → 不 failover，记录错误
```

以下是所有需要变更的部分，标注了「替换」或「新增」，可以直接覆盖到原文档对应位置。

---

# 变更 1：替换原文档 2.1 Backend 技术栈

## 2.1 Backend

```
语言:           Go >= 1.23
HTTP 框架:      Gin v1.10+
数据库 ORM:     GORM v1.25+
数据库:         SQLite（开发/默认）, PostgreSQL 16+（生产）
缓存:           Redis 7+（可选，多实例部署时使用）
日志:           Zap (go.uber.org/zap)
指标:           Prometheus (prometheus/client_golang)
配置:           Viper (spf13/viper)
CLI:            Cobra (spf13/cobra)
迁移:           golang-migrate/migrate
UUID:           google/uuid
```

---

# 变更 2：替换原文档 4.1 config.yaml 中 redis 和相关部分

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 120s
  idle_timeout: 120s
  shutdown_timeout: 30s

database:
  driver: "sqlite"
  dsn: "data/gateway.db"
  max_open_conns: 25
  max_idle_conns: 10
  conn_max_lifetime: 300s

# 状态存储（客户端限流、Key Pool 状态、Circuit Breaker 状态）
# mode: memory  → 单实例，所有状态存内存（默认，无需 Redis）
# mode: redis   → 多实例，所有状态存 Redis（需要 Redis 服务）
state_store:
  mode: "memory"              # memory | redis
  redis:
    addr: "localhost:6379"
    password: ""
    db: 0
    pool_size: 10
    dial_timeout: 5s
    read_timeout: 3s
    write_timeout: 3s

auth:
  enabled: true
  keys:
    - name: "claude-code-dev"
      key: "gw-key-xxxx"
      allowed_models: ["*"]
      rate_limit:
        rpm: 100
        tpm: 500000
    - name: "cline-prod"
      key: "gw-key-yyyy"
      allowed_models: ["coding-model", "chat-model"]
      rate_limit:
        rpm: 60
        tpm: 300000

providers:
  deepseek:
    enabled: true
    endpoint: "https://api.deepseek.com"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "deepseek-chat"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.0014
        cost_per_1k_output: 0.0028
      - id: "deepseek-coder"
        aliases: ["coding-model"]
        cost_per_1k_input: 0.0014
        cost_per_1k_output: 0.0028
    keys:
      - name: "deepseek-key-1"
        key: "sk-xxx1"
      - name: "deepseek-key-2"
        key: "sk-xxx2"
    circuit_breaker:
      failure_threshold: 5
      failure_window: 60s
      open_timeout: 30s
      half_open_requests: 1
      countable_errors: ["5xx", "timeout", "connection_error"]
      excluded_errors: ["429"]

  minimax:
    enabled: true
    endpoint: "https://api.minimax.chat"
    protocol: "anthropic"
    timeout: 90s
    models:
      - id: "MiniMax-Text-01"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.001
        cost_per_1k_output: 0.002
    keys:
      - name: "minimax-key-1"
        key: "sk-mm-xxx1"
    circuit_breaker:
      failure_threshold: 5
      failure_window: 60s
      open_timeout: 30s
      half_open_requests: 1

  glm:
    enabled: true
    endpoint: "https://open.bigmodel.cn/api/paas/v4"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "glm-4-plus"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.005
        cost_per_1k_output: 0.01
    keys:
      - name: "glm-key-1"
        key: "xxx"

  qwen:
    enabled: true
    endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "qwen-coder-plus"
        aliases: ["coding-model"]
        cost_per_1k_input: 0.003
        cost_per_1k_output: 0.006
    keys:
      - name: "qwen-key-1"
        key: "sk-xxx"

  kimi:
    enabled: true
    endpoint: "https://api.moonshot.cn/v1"
    protocol: "openai"
    timeout: 60s
    models:
      - id: "moonshot-v1-128k"
        aliases: ["coding-model", "long-context"]
        cost_per_1k_input: 0.012
        cost_per_1k_output: 0.012
    keys:
      - name: "kimi-key-1"
        key: "sk-xxx"

  gemini:
    enabled: false
    endpoint: "https://generativelanguage.googleapis.com/v1beta"
    protocol: "google"
    timeout: 60s
    models:
      - id: "gemini-2.0-flash"
        aliases: ["coding-model", "chat-model"]
        cost_per_1k_input: 0.0001
        cost_per_1k_output: 0.0004
    keys:
      - name: "gemini-key-1"
        key: "xxx"

routing:
  aliases:
    coding-model:
      strategy: "priority"
      providers:
        - name: "minimax"
          model: "MiniMax-Text-01"
          priority: 1
        - name: "deepseek"
          model: "deepseek-coder"
          priority: 2
        - name: "glm"
          model: "glm-4-plus"
          priority: 3
    chat-model:
      strategy: "weight"
      providers:
        - name: "deepseek"
          model: "deepseek-chat"
          weight: 70
        - name: "glm"
          model: "glm-4-plus"
          weight: 30
    long-context:
      strategy: "priority"
      providers:
        - name: "kimi"
          model: "moonshot-v1-128k"
          priority: 1
  default_strategy: "priority"

keypool:
  cooling_duration: 60s
  max_cooling_count: 5
  health_check_interval: 30s
  key_rotation: "round_robin"

timeouts:
  server_read: 30s
  server_write: 120s
  server_idle: 120s
  provider_default: 60s
  request_total: 300s

retry:
  enabled: true
  max_attempts: 3
  no_failover_on:
    - "invalid_request"
    - "auth"
  failover_on:
    - "rate_limit"
    - "server_error"
    - "timeout"
    - "connection"

logging:
  level: "info"
  format: "json"
  output: "stdout"
  file_path: "logs/gateway.log"

metrics:
  enabled: true
  path: "/metrics"
  port: 9090

usage:
  flush_interval: 10s
  batch_size: 100
  retention_days: 90
```

---

# 变更 3：在第五部分末尾新增 5.10 State Store

## 5.10 State Store（状态存储抽象）

```go
// backend/internal/state/store.go

/*
State Store 解决的核心问题：

  多个 Gateway 实例之间共享运行时状态。
  包括：客户端限流计数、Key Pool 状态、Circuit Breaker 状态。

两种实现：
  MemoryStore — 单实例部署时使用，所有状态存内存，不需要 Redis
  RedisStore  — 多实例部署时使用，所有状态存 Redis

选择逻辑：
  config.state_store.mode = "memory" → NewMemoryStore()
  config.state_store.mode = "redis"  → NewRedisStore(redisClient)

所有依赖运行时状态的模块通过 Store 接口访问，不直接依赖 Redis。
*/
package state

import "time"

// Store 是运行时状态存储的抽象接口
type Store interface {
    // === 客户端限流 ===

    // IncrCounter 递增计数器并设置过期时间
    // 用途：RPM/TPM 限流
    // key 示例："ratelimit:client-name:rpm:202501010005"
    // 返回递增后的值
    IncrCounter(ctx context.Context, key string, window time.Duration) (int64, error)

    // GetCounter 获取计数器当前值
    GetCounter(ctx context.Context, key string) (int64, error)

    // === Key Pool 状态 ===

    // SetKeyState 设置 Key 的状态
    // 用途：标记 Key 为 COOLING / ACTIVE / DISABLED
    // 多实例场景下，实例 A 标记 Key 为 COOLING，实例 B 能立即感知
    SetKeyState(ctx context.Context, providerName, keyID string, state KeyState, ttl time.Duration) error

    // GetKeyState 获取 Key 的状态
    GetKeyState(ctx context.Context, providerName, keyID string) (KeyState, error)

    // DeleteKeyState 删除 Key 状态记录
    DeleteKeyState(ctx context.Context, providerName, keyID string) error

    // === Circuit Breaker 状态 ===

    // SetBreakerState 设置熔断器状态
    // 用途：多实例共享熔断器状态
    // 实例 A 发现 Provider 连续失败打开熔断器，实例 B 能立即感知
    SetBreakerState(ctx context.Context, providerName string, state BreakerState, ttl time.Duration) error

    // GetBreakerState 获取熔断器状态
    GetBreakerState(ctx context.Context, providerName string) (BreakerState, error)

    // RecordBreakerFailure 记录一次失败（原子递增）
    // 返回当前窗口内的失败总数
    RecordBreakerFailure(ctx context.Context, providerName string, window time.Duration) (int64, error)

    // ResetBreakerFailures 重置失败计数
    ResetBreakerFailures(ctx context.Context, providerName string) error

    // === 通用 ===

    // Ping 检查存储连通性
    Ping(ctx context.Context) error

    // Close 关闭连接
    Close() error
}

type KeyState string

const (
    KeyStateActive   KeyState = "ACTIVE"
    KeyStateCooling  KeyState = "COOLING"
    KeyStateDisabled KeyState = "DISABLED"
)

type BreakerState string

const (
    BreakerStateClosed   BreakerState = "CLOSED"
    BreakerStateOpen     BreakerState = "OPEN"
    BreakerStateHalfOpen BreakerState = "HALF_OPEN"
)
```

```go
// backend/internal/state/memory.go

/*
MemoryStore 实现

单实例部署时使用。
所有状态存在进程内存中，不需要任何外部依赖。

实现细节：
  - 计数器：sync.Map + 带 TTL 的 entry
  - Key 状态：sync.Map
  - Circuit Breaker 状态：sync.Map + 带 TTL 的 entry
  - TTL 清理：后台 goroutine 定期扫描过期 entry
*/
package state

type MemoryStore struct {
    counters   sync.Map  // key → *counterEntry
    keyStates  sync.Map  // "provider:keyID" → *stateEntry
    breakerStates sync.Map // providerName → *breakerEntry
    stopClean  chan struct{}
}

type counterEntry struct {
    value     int64
    expiresAt time.Time
}

type stateEntry struct {
    state     string
    expiresAt time.Time
}

func NewMemoryStore() *MemoryStore

// IncrCounter 内存实现
func (s *MemoryStore) IncrCounter(ctx context.Context, key string, window time.Duration) (int64, error) {
    // 1. 从 counters map 获取或创建 entry
    // 2. 如果 entry 已过期，重置为 0
    // 3. 原子递增
    // 4. 设置过期时间 = now + window
    // 5. 返回递增后的值
}

// SetKeyState 内存实现
func (s *MemoryStore) SetKeyState(ctx context.Context, providerName, keyID string, state KeyState, ttl time.Duration) error {
    // 直接写入 sync.Map
    // key = "providerName:keyID"
    // 如果 ttl > 0，设置过期时间
}

// GetKeyState 内存实现
func (s *MemoryStore) GetKeyState(ctx context.Context, providerName, keyID string) (KeyState, error) {
    // 从 sync.Map 读取
    // 如果已过期，返回默认值 ACTIVE
}

// SetBreakerState 内存实现
func (s *MemoryStore) SetBreakerState(ctx context.Context, providerName string, state BreakerState, ttl time.Duration) error

// GetBreakerState 内存实现
func (s *MemoryStore) GetBreakerState(ctx context.Context, providerName string) (BreakerState, error)

// RecordBreakerFailure 内存实现
func (s *MemoryStore) RecordBreakerFailure(ctx context.Context, providerName string, window time.Duration) (int64, error) {
    // 1. 获取 breakerEntry
    // 2. 在滑动窗口内追加失败记录
    // 3. 清理窗口外的记录
    // 4. 返回窗口内失败总数
}

func (s *MemoryStore) Ping(ctx context.Context) error { return nil }

func (s *MemoryStore) Close() error {
    close(s.stopClean)
    return nil
}

// startCleaner 后台清理过期 entry
func (s *MemoryStore) startCleaner() {
    // 每 10 秒扫描一次所有 map，删除过期 entry
}
```

```go
// backend/internal/state/redis.go

/*
RedisStore 实现

多实例部署时使用。
所有状态存在 Redis 中，多个 Gateway 实例共享。

Redis 数据结构：
  计数器:     STRING key → INCR + EXPIRE（TTL = 窗口大小）
  Key 状态:   STRING "gw:keystate:{provider}:{keyID}" → state string + TTL
  熔断器状态: STRING "gw:breaker:{provider}" → state string + TTL
  熔断器计数: STRING "gw:breaker:failures:{provider}" → INCR + EXPIRE

Redis 优势：
  - INCR 是原子操作，多实例并发安全
  - TTL 自动过期，不需要手动清理
  - 所有实例看到同一份数据

连接管理：
  - 使用连接池（pool_size 配置）
  - 断线自动重连
  - Ping 检查连通性
*/
package state

import "github.com/redis/go-redis/v9"

type RedisStore struct {
    client *redis.Client
}

func NewRedisStore(client *redis.Client) *RedisStore

// IncrCounter Redis 实现
func (s *RedisStore) IncrCounter(ctx context.Context, key string, window time.Duration) (int64, error) {
    // 1. INCR key
    // 2. 如果返回值为 1（第一次），设置 EXPIRE key window
    // 3. 返回递增后的值
    pipe := s.client.Pipeline()
    incrCmd := pipe.Incr(ctx, key)
    pipe.ExpireNX(ctx, key, window)
    _, err := pipe.Exec(ctx)
    if err != nil {
        return 0, err
    }
    return incrCmd.Val(), nil
}

// SetKeyState Redis 实现
func (s *RedisStore) SetKeyState(ctx context.Context, providerName, keyID string, state KeyState, ttl time.Duration) error {
    key := fmt.Sprintf("gw:keystate:%s:%s", providerName, keyID)
    if ttl > 0 {
        return s.client.Set(ctx, key, string(state), ttl).Err()
    }
    return s.client.Set(ctx, key, string(state), 0).Err()
}

// GetKeyState Redis 实现
func (s *RedisStore) GetKeyState(ctx context.Context, providerName, keyID string) (KeyState, error) {
    key := fmt.Sprintf("gw:keystate:%s:%s", providerName, keyID)
    val, err := s.client.Get(ctx, key).Result()
    if err == redis.Nil {
        return KeyStateActive, nil  // 默认 ACTIVE
    }
    if err != nil {
        return KeyStateActive, err
    }
    return KeyState(val), nil
}

// SetBreakerState Redis 实现
func (s *RedisStore) SetBreakerState(ctx context.Context, providerName string, state BreakerState, ttl time.Duration) error {
    key := fmt.Sprintf("gw:breaker:%s", providerName)
    if ttl > 0 {
        return s.client.Set(ctx, key, string(state), ttl).Err()
    }
    return s.client.Set(ctx, key, string(state), 0).Err()
}

// GetBreakerState Redis 实现
func (s *RedisStore) GetBreakerState(ctx context.Context, providerName string) (BreakerState, error) {
    key := fmt.Sprintf("gw:breaker:%s", providerName)
    val, err := s.client.Get(ctx, key).Result()
    if err == redis.Nil {
        return BreakerStateClosed, nil  // 默认 CLOSED
    }
    if err != nil {
        return BreakerStateClosed, err
    }
    return BreakerState(val), nil
}

// RecordBreakerFailure Redis 实现
func (s *RedisStore) RecordBreakerFailure(ctx context.Context, providerName string, window time.Duration) (int64, error) {
    key := fmt.Sprintf("gw:breaker:failures:%s", providerName)
    pipe := s.client.Pipeline()
    incrCmd := pipe.Incr(ctx, key)
    pipe.ExpireNX(ctx, key, window)
    _, err := pipe.Exec(ctx)
    if err != nil {
        return 0, err
    }
    return incrCmd.Val(), nil
}

// ResetBreakerFailures Redis 实现
func (s *RedisStore) ResetBreakerFailures(ctx context.Context, providerName string) error {
    key := fmt.Sprintf("gw:breaker:failures:%s", providerName)
    return s.client.Del(ctx, key).Err()
}

func (s *RedisStore) Ping(ctx context.Context) error {
    return s.client.Ping(ctx).Err()
}

func (s *RedisStore) Close() error {
    return s.client.Close()
}
```

---

# 变更 4：替换原文档 5.4 Key Pool（修改为使用 State Store）

## 5.4 Key Pool

```go
// backend/internal/keypool/key.go

package keypool

import (
    "time"
    "llm-gateway/internal/state"
)

// KeyStatus Key 状态（与 state.KeyState 保持语义一致）
type KeyStatus string

const (
    KeyStatusActive   KeyStatus = "ACTIVE"
    KeyStatusCooling  KeyStatus = "COOLING"
    KeyStatusLimited  KeyStatus = "LIMITED"
    KeyStatusDisabled KeyStatus = "DISABLED"
)

// Key 代表一个 API Key
type Key struct {
    ID            string
    ProviderName  string
    Name          string
    Key           string       // 加密存储，运行时解密
    Status        KeyStatus    // 内存中的状态副本
    CoolingUntil  time.Time
    CoolingCount  int
    TotalRequests int64
    TotalTokens   int64
    ErrorCount    int
    LastUsedAt    time.Time
    LastErrorAt   time.Time
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

// Pool 管理一个 Provider 下的所有 Key
type Pool struct {
    ProviderName string
    keys         []*Key
    scheduler    Scheduler
    store        state.Store    // 状态存储抽象（Memory 或 Redis）
}

func NewPool(providerName string, keys []*Key, strategy string, store state.Store) *Pool

// Acquire 获取一个可用的 Key
// 逻辑：
//   1. Scheduler 选择一个候选 Key
//   2. 从 State Store 查询 Key 的实时状态
//      （多实例场景下，其他实例可能已将该 Key 标记为 COOLING）
//   3. 如果 Key 是 COOLING 且 cooling_until < now，恢复为 ACTIVE
//   4. 如果 Key 是 COOLING 或 DISABLED，跳过，选下一个
//   5. 如果所有 Key 都不可用，返回 ErrNoAvailableKey
func (p *Pool) Acquire() (*Key, error)

func (p *Pool) ReportSuccess(key *Key)

// ReportRateLimit 报告 429
// 逻辑：
//   1. 通过 State Store 设置 key 状态为 COOLING（所有实例可见）
//   2. cooling_count++（持久化到数据库）
//   3. 如果 cooling_count > max_cooling_count，通过 State Store 设置 DISABLED
func (p *Pool) ReportRateLimit(key *Key, retryAfter time.Duration)

func (p *Pool) ReportError(key *Key, err *ProviderError)

type PoolStatus struct {
    ProviderName string
    TotalKeys    int
    ActiveKeys   int
    CoolingKeys  int
    DisabledKeys int
}

func (p *Pool) Status() PoolStatus

// Scheduler Key 选择策略接口
type Scheduler interface {
    Select(keys []*Key) (*Key, error)
}

type RoundRobinScheduler struct{ current int }
type LeastUsedScheduler struct{}
type RandomScheduler struct{}
```

---

# 变更 5：替换原文档 5.6 Circuit Breaker（修改为使用 State Store）

## 5.6 Circuit Breaker

```go
// backend/internal/circuit/breaker.go

package circuit

import (
    "time"
    "llm-gateway/internal/state"
)

type BreakerConfig struct {
    FailureThreshold  int
    FailureWindow     time.Duration
    OpenTimeout       time.Duration
    HalfOpenRequests  int
    CountableErrors   []string
    ExcludedErrors    []string
}

// Breaker Circuit Breaker 实现
// 状态通过 State Store 存储，支持多实例共享
type Breaker struct {
    name         string
    config       BreakerConfig
    store        state.Store
    mu           sync.RWMutex
}

func NewBreaker(name string, config BreakerConfig, store state.Store) *Breaker

// Allow 检查是否允许请求通过
// 从 State Store 读取当前状态：
//   CLOSED   → 允许
//   OPEN     → 检查是否超时（需要本地记录 openedAt），超时则转 HALF_OPEN
//   HALF_OPEN → 允许 half_open_requests 个
func (b *Breaker) Allow() bool

// RecordSuccess 记录成功
// 如果当前是 HALF_OPEN → 递增本地 successCount
// 如果 successCount >= half_open_requests → 通过 State Store 转 CLOSED，重置失败计数
func (b *Breaker) RecordSuccess()

// RecordFailure 记录失败
// 逻辑：
//   1. 检查错误类型是否应该计数（排除 429 等）
//   2. 通过 State Store.RecordBreakerFailure 记录失败
//   3. 如果窗口内失败数 >= failure_threshold → 通过 State Store 转 OPEN
func (b *Breaker) RecordFailure(errType string)

func (b *Breaker) State() state.BreakerState

// Reset 手动恢复（通过 State Store 转 CLOSED，重置失败计数）
func (b *Breaker) Reset()
```

---

# 变更 6：替换原文档 5.9 Auth（修改为使用 State Store）

## 5.9 Auth

```go
// backend/internal/auth/authenticator.go

package auth

import (
    "net/http"
    "time"
    "llm-gateway/internal/state"
)

type Authenticator struct {
    keys    map[string]*GatewayKey
    store   state.Store     // 用于限流计数器
    enabled bool
}

type GatewayKey struct {
    Name          string
    Key           string       // hash 存储
    AllowedModels []string
    RateLimit     RateLimitConfig
}

type RateLimitConfig struct {
    RPM int
    TPM int
}

// Authenticate 验证请求
// 逻辑：
//   1. 从 Authorization header 提取 Bearer token
//   2. 查找 GatewayKey（对比 hash）
//   3. 检查 allowed_models
//   4. 限流检查：
//      a. 构造限流 key: "ratelimit:{name}:rpm:{minute_bucket}"
//      b. 调用 store.IncrCounter(key, 1*time.Minute)
//      c. 如果返回值 > rpm，返回 429
//      d. TPM 同理
//   5. 返回 GatewayKey 或 error
func (a *Authenticator) Authenticate(r *http.Request) (*GatewayKey, error)
```

---

# 变更 7：新增目录结构中的 state 包

在 `internal/` 目录下新增：

```
   ├── state/                       # 状态存储抽象
   │   ├── store.go                 # Store 接口定义
   │   ├── memory.go                # MemoryStore 实现（单实例）
   │   └── redis.go                 # RedisStore 实现（多实例）
```

完整 internal 目录更新为：

```
 internal/
   ├── api/
   │   ├── http/
   │   │   ├── handler/
   │   │   │   ├── provider.go
   │   │   │   ├── key.go
   │   │   │   ├── usage.go
   │   │   │   ├── routing.go
   │   │   │   ├── dashboard.go
   │   │   │   ├── config.go
   │   │   │   └── health.go
   │   │   ├── middleware/
   │   │   │   ├── auth.go
   │   │   │   ├── ratelimit.go
   │   │   │   ├── logger.go
   │   │   │   ├── trace.go
   │   │   │   └── cors.go
   │   │   └── router.go
   │   └── proxy/
   │       ├── proxy.go
   │       ├── stream.go
   │       └── buffer.go
   ├── router/
   │   ├── router.go
   │   ├── model_alias.go
   │   └── policy.go
   ├── policy/
   │   ├── priority.go
   │   ├── weight.go
   │   ├── cost.go
   │   └── health.go
   ├── provider/
   │   ├── provider.go
   │   ├── manager.go
   │   ├── registry.go
   │   ├── deepseek/
   │   │   ├── deepseek.go
   │   │   └── config.go
   │   ├── minimax/
   │   │   ├── minimax.go
   │   │   └── config.go
   │   ├── glm/
   │   │   ├── glm.go
   │   │   └── config.go
   │   ├── qwen/
   │   │   ├── qwen.go
   │   │   └── config.go
   │   ├── kimi/
   │   │   ├── kimi.go
   │   │   └── config.go
   │   ├── gemini/
   │   │   ├── gemini.go
   │   │   └── config.go
   │   └── openai_compatible/
   │       ├── openai_compatible.go
   │       └── config.go
   ├── keypool/
   │   ├── pool.go
   │   ├── key.go
   │   └── scheduler.go
   ├── circuit/
   │   └── breaker.go
   ├── usage/
   │   ├── collector.go
   │   ├── recorder.go
   │   └── query.go
   ├── token/
   │   └── counter.go
   ├── metrics/
   │   ├── collector.go
   │   └── prometheus.go
   ├── auth/
   │   ├── authenticator.go
   │   └── apikey.go
   ├── state/                       # ← 新增
   │   ├── store.go                 # Store 接口
   │   ├── memory.go                # 内存实现
   │   └── redis.go                 # Redis 实现
   ├── config/
   │   └── config.go
   ├── database/
   │   ├── database.go
   │   └── models.go
   └── server/
       └── server.go
```

---

# 变更 8：替换原文档第十四部分 Docker 配置

## 14.1 docker-compose.yml（默认，无 Redis）

```yaml
# docker-compose.yml — 默认部署（单实例，不需要 Redis）

services:
  gateway:
    build:
      context: ./backend
      dockerfile: Dockerfile
    ports:
      - "8080:8080"
      - "9090:9090"
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - gateway-data:/app/data
    environment:
      - GATEWAY_CONFIG=/app/config.yaml
    depends_on:
      postgres:
        condition: service_healthy
    restart: unless-stopped

  frontend:
    build:
      context: ./frontend
      dockerfile: Dockerfile
    ports:
      - "3000:80"
    depends_on:
      - gateway
    restart: unless-stopped

  postgres:
    image: postgres:16-alpine
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: gateway
      POSTGRES_USER: gateway
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-gateway_dev_password}
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gateway"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:latest
    ports:
      - "9091:9090"
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus
    restart: unless-stopped

  grafana:
    image: grafana/grafana:latest
    ports:
      - "3001:3000"
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD:-admin}
    volumes:
      - grafana-data:/var/lib/grafana
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
    depends_on:
      - prometheus
    restart: unless-stopped

volumes:
  gateway-data:
  postgres-data:
  prometheus-data:
  grafana-data:
```

## 14.2 docker-compose.dev.yml（开发环境）

```yaml
# docker-compose.dev.yml — 开发环境用 SQLite，无外部依赖

services:
  gateway:
    build:
      context: ./backend
      dockerfile: Dockerfile.dev
    ports:
      - "8080:8080"
      - "9090:9090"
    volumes:
      - ./backend:/app
      - ./config.yaml:/app/config.yaml
    environment:
      - GATEWAY_CONFIG=/app/config.yaml
      - GIN_MODE=debug
```

## 14.3 docker-compose.scale.yml（多实例部署）

```yaml
# docker-compose.scale.yml — 多实例部署，需要 Redis
# 使用方式: docker compose -f docker-compose.yml -f docker-compose.scale.yml up -d

services:
  gateway:
    deploy:
      replicas: 3
    environment:
      - GATEWAY_CONFIG=/app/config.yaml
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 5s
      retries: 5
    restart: unless-stopped

  # 多实例时需要 Nginx 做负载均衡
  nginx:
    image: nginx:alpine
    ports:
      - "443:443"
      - "80:80"
    volumes:
      - ./nginx/nginx.conf:/etc/nginx/nginx.conf:ro
    depends_on:
      - gateway
    restart: unless-stopped

volumes:
  redis-data:
```

多实例部署时，config.yaml 中需要：

```yaml
state_store:
  mode: "redis"            # ← 改为 redis
  redis:
    addr: "redis:6379"     # Docker 内部网络地址
    password: ""
    db: 0
    pool_size: 10
```

## 14.4 Prometheus 配置

```yaml
# prometheus/prometheus.yml

global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'gateway'
    static_configs:
      - targets: ['gateway:9090']
    metrics_path: /metrics
```

---

# 变更 9：替换原文档 1.2 原则 5，更新为包含 State Store 说明

### 原则 5：所有行为可配置，可选依赖最小化

- 路由策略可配置
- Key Pool 行为可配置
- Circuit Breaker 参数可配置
- 添加/移除 Provider 或 Key 支持热加载（不重启）
- **外部依赖可选：Redis 只在多实例部署时需要，单实例零外部依赖**

```
部署模式 vs 外部依赖:

┌──────────────────┬──────────┬───────┬──────────┐
│ 部署模式          │ Database │ Redis │ 适用场景  │
├──────────────────┼──────────┼───────┼──────────┤
│ 单实例 + SQLite  │ SQLite   │ 不需要 │ 开发/测试 │
│ 单实例 + PG      │ Postgres │ 不需要 │ 小型生产  │
│ 多实例 + PG      │ Postgres │ 需要   │ 企业生产  │
└──────────────────┴──────────┴───────┴──────────┘

状态存储策略:
  state_store.mode = "memory"
    → 客户端限流、Key Pool 状态、Circuit Breaker 状态全存内存
    → 零外部依赖
    → 仅限单实例

  state_store.mode = "redis"
    → 以上状态存 Redis
    → 多实例共享
    → 需要 Redis 服务
```

---

# 变更 10：替换原文档第十八部分 Phase 1.1

```
Phase 1.1 — 基础骨架
  ├── 初始化 Go 项目 (go mod init)
  ├── 目录结构创建
  ├── 配置加载 (config.yaml → Viper)
  ├── 数据库初始化 (GORM + SQLite)
  ├── 所有 migration 文件
  ├── State Store 接口定义 + MemoryStore 实现
  ├── HTTP Server 启动 (Gin)
  ├── 健康检查端点 (GET /api/v1/health)
  └── 日志初始化 (Zap)
```

Phase 1.3（Key Pool）更新为：

```
Phase 1.3 — Key Pool
  ├── Key 实体和状态机 (key.go)
  ├── Key Pool 管理器 (pool.go)
  │   └── 通过 State Store 接口读写 Key 状态
  ├── 轮询调度器 (scheduler.go)
  ├── Key 加密存储
  ├── Key CRUD API (handler/key.go)
  └── 测试：多 Key 轮询和故障切换
```

Phase 1.5（Proxy Engine）中 Auth 更新为：

```
Phase 1.5 — Proxy Engine
  ├── 非流式代理 (proxy.go)
  ├── 流式代理 (stream.go)
  ├── Auth 中间件
  │   └── 通过 State Store 接口做限流计数
  ├── 请求日志中间件
  ├── Trace 中间件 (X-Request-Id)
  └── 测试：完整请求链路（非流式 + 流式）
```

Phase 1.6（Circuit Breaker）更新为：

```
Phase 1.6 — Circuit Breaker
  ├── 熔断器实现 (breaker.go)
  │   └── 通过 State Store 接口读写熔断状态
  ├── 集成到 Router
  └── 测试：熔断和恢复
```

在 Phase 1.10 后追加：

```
Phase 1.11 — Redis 适配（可选，在需要多实例时执行）
  ├── RedisStore 实现 (state/redis.go)
  ├── 集成测试：双实例 + Redis 共享状态
  ├── docker-compose.scale.yml
  └── 文档更新
```

---

# 变更 11：快速参考卡片更新

替换附录 A 的快速参考卡片：

```
Gateway 本质 = 协议感知的透明代理（不是 Nginx，不是 LiteLLM）

一句话原则:
  读 body → 只提取 model/stream → 路由 → body 原样转发

三大职责边界:
  Gateway:  路由 / 调度 / 限流 / 统计 / 认证 / 故障恢复
  Provider: API 调用 / 协议细节 / Usage 解析 / Error 分类
  Client:   发送原始协议格式的请求

核心抽象:
  Provider 接口   → 各 Provider 实现
  State Store     → 运行时状态存储（Memory / Redis）
  Key Pool        → 管理 API Key 生命周期
  Router          → 模型别名 → Provider 选择
  Circuit Breaker → Provider 健康状态
  Usage Collector → 异步批量记录

部署模式:
  单实例 → SQLite + MemoryStore，零外部依赖
  多实例 → PostgreSQL + RedisStore + Nginx

请求协议对应关系:
  /v1/messages           → Anthropic → MiniMax, Kimi, GLM
  /v1/chat/completions   → OpenAI    → DeepSeek, Qwen, MiniMax, Kimi, GLM
  /v1beta/models/*       → Google    → Gemini

Failover 规则:
  429 → 同 Provider 换 Key → 换 Provider
  5xx → 换 Provider
  400 → 不 failover，直接返回客户端
  流式中途失败 → 不 failover，记录错误
```

---

以上就是全部变更。核心改动总结：

1. **Redis 从必选项变为可选项**——单实例不需要，多实例才需要
2. **新增 State Store 抽象层**——`MemoryStore`（内存）和 `RedisStore`（Redis）两种实现，通过配置切换
3. **Key Pool / Circuit Breaker / Auth 的限流**全部通过 State Store 接口访问状态，不再直接依赖具体存储
4. **Docker Compose 拆分**——默认文件不含 Redis，scale 文件按需叠加
5. **Phase 1 不依赖 Redis**——先用 MemoryStore 跑通全部功能，Phase 1.11 再做 Redis 适配