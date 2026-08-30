# Provider 厂商定制包指南

本文用于新增或修改**内置厂商 Go 包**。如果上游只是标准 OpenAI 或 Anthropic
兼容代理，优先使用 [动态中转站](relay-stations.md)，不要增加代码包。

配套执行清单位于 `.claude/skills/provider-vendor/SKILL.md`。

## 何时需要定制包

满足任一条件时才增加内置包：

- 标准兼容基座无法表达请求 URL 或鉴权方式；
- 需要改写请求体、响应体或专属 header；
- 业务错误藏在 HTTP 200 body 或 SSE 事件中，通用分类器不能正确识别；
- 上游 usage 字段不是 OpenAI/Anthropic 标准形状；
- 有专属余额/套餐查询，需要注册 `quotacheck.Balancer`；
- 同一厂商有多个端点或协议面，需要共享 vendor 级 key 池。

仅仅更换 base URL、添加 key 或使用标准模型列表，不是写定制包的理由。

## 运行时概念

- **vendor** 是厂商级身份，例如 `mimo`。
- **face** 是注册名和路由面，例如 `mimo`、`mimo-anthropic`、
  `mimo-token-plan`。
- `RegisterGlobalWithProtocolVendor(face, factory, protocol, vendor)` 建立 face 到
  vendor 的映射。同 vendor 的 face 共用 KeyPool。
- 上游 key 存在 `provider_api_keys`，`provider_name` 使用 vendor；每把 key 自带
  `billing_source` 和可选 `protocols`。
- face 的 endpoint、protocol、timeout、`billing_source` 和 Responses 能力来自
  `config.yaml`。模型与定价不在配置文件中。
- 模型定价按 `(vendor, model_id)` 存在 `provider_models`；模型归属按
  `(face, model_id)` 存在 `provider_model_faces`。

## 第一步：调查上游契约

优先以厂商官方文档和可复现的直连请求为依据，至少记录：

| 项目 | 必须确认的内容 |
|---|---|
| 协议 | OpenAI Chat、OpenAI Responses、Anthropic Messages、Google 中哪些可用 |
| URL | base URL 是否已含 `/v1`；chat、responses、models 的完整路径 |
| 鉴权 | Bearer、`x-api-key`、`api-key`、query key 或其他方式 |
| 模型 | 真实模型 ID、模型列表端点及返回格式 |
| 请求 | thinking、tools、stream、JSON mode 等非标准参数或回传约束 |
| 响应 | usage、缓存 token、reasoning、流式事件和错误包裹格式 |
| 错误 | 400/401/403/402/429/5xx 的真实 status、body 和 `Retry-After` |
| 额度 | 是否有稳定余额端点、鉴权方式、单位和恢复条件 |
| 定价 | input/cache read/output 的币种和每百万 token 单位 |

必须分别实测正常、流式、鉴权失败、限流和额度耗尽。不能把“文档未提及”写成
“不支持”，也不能只根据 HTTP status 推断业务错误。

## 第二步：选择基础实现

### OpenAI 兼容面

标准实现位于 `backend/internal/provider/openai_compatible/`。最小 wrapper 可以使用嵌入：

```go
package acme

import (
    "fmt"

    "github.com/wang546673478/native-llm-gateway/internal/provider"
    "github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const openAIName = "acme"

type OpenAIProvider struct {
    *openai_compatible.Base
}

func NewOpenAI(cfg provider.ProviderConfig) (provider.Provider, error) {
    if cfg.Protocol != provider.ProtocolOpenAI {
        return nil, fmt.Errorf("%s requires protocol=openai, got %q", openAIName, cfg.Protocol)
    }
    if cfg.Endpoint == "" {
        return nil, fmt.Errorf("%s endpoint is required", openAIName)
    }
    return &OpenAIProvider{Base: openai_compatible.NewBase(openai_compatible.Config{
        Name:          openAIName,
        Endpoint:      cfg.Endpoint,
        Timeout:       cfg.Timeout,
        Pool:          cfg.Pool,
        BillingSource: cfg.BillingSource,
        ChatPath:      "/chat/completions", // endpoint 已含 /v1 时使用
        ResponsesPath: "/responses",        // endpoint 已含 /v1 时使用
        ModelsPath:    "/models",           // endpoint 已含 /v1 时使用
        StreamUsage:   true,
    })}, nil
}

func (p *OpenAIProvider) Name() string                { return openAIName }
func (p *OpenAIProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
```

`Base` 已提供 `SendRequest`、`SendStreamRequest`、`HealthCheck`、`ListModels`、
`SetPool` 和 `Close`。wrapper 只需补 `Name` 和 `Protocol`。

URL 规则必须用完整上游路径反推，不能凭感觉加 `/v1`：

| Endpoint 形状 | `ChatPath` | `ResponsesPath` | `ModelsPath` |
|---|---|---|---|
| `https://host`，标准 OpenAI | 留空（默认 `/v1/chat/completions`） | 留空（默认 `/v1/responses`） | 留空（默认 `/v1/models`） |
| `https://host/v1` | `/chat/completions` | `/responses` | `/models` |
| `https://host`，DeepSeek 风格 | `/chat/completions` | 按官方完整路径 | 按官方完整路径 |

请求和 `ListModels` 对 endpoint 恰好以 `/v1` 结尾的场景有去重保护，但健康检查直接拼
`ModelsPath`。因此内置包仍应显式写正确路径，并用 `httptest.Server` 锁定最终 URL。

`BillingSource` 不能遗漏。它让模型同步和健康检查从共享池中选择与该 endpoint 匹配的
计费层 key；代理请求本身始终优先使用路由层传入的 `req.Key`。

### Anthropic 兼容面

标准实现位于 `backend/internal/provider/anthropic_compatible/`：

```go
const anthropicName = "acme-anthropic"

type AnthropicProvider struct {
    *anthropic_compatible.Base
}

func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
    if cfg.Protocol != provider.ProtocolAnthropic {
        return nil, fmt.Errorf("%s requires protocol=anthropic, got %q", anthropicName, cfg.Protocol)
    }
    if cfg.Endpoint == "" {
        return nil, fmt.Errorf("%s endpoint is required", anthropicName)
    }
    return &AnthropicProvider{Base: anthropic_compatible.NewBase(
        anthropic_compatible.Config{
            Name:     anthropicName,
            Endpoint: cfg.Endpoint,
            Timeout:  cfg.Timeout,
            Pool:     cfg.Pool,
        },
    )}, nil
}

func (p *AnthropicProvider) Name() string { return anthropicName }
func (p *AnthropicProvider) Protocol() provider.Protocol {
    return provider.ProtocolAnthropic
}
```

Anthropic 基座把 endpoint 末尾的 `/` 去掉：末尾是 `/v1` 时拼 `/messages`，否则拼
`/v1/messages`。它使用 `x-api-key` 和固定 `anthropic-version: 2023-06-01`。

模型同步先尝试 `{endpoint}/api/models` 的 New API 形状，再尝试标准
`{endpoint}/v1/models`（endpoint 以 `/v1` 结尾时去重）。当前 Anthropic Config 没有
`BillingSource`；同 vendor 存在多个 Anthropic endpoint 且 key 与计费层绑定时，
`ListModels` 和健康检查无法按 tier 选 key，必须补齐基座能力或为该厂商实现专用逻辑。

### Google 面

`backend/internal/provider/google/` 保留了协议基座，但当前没有内置 Google 厂商，也没有
动态 relay 实现。新增 Google 厂商必须写 wrapper、注册、配置和全链测试；不能把 relay
页面出现 Google 选项当成已支持。

## 第三步：实现厂商差异

兼容基座是共享代码。只属于一家厂商的差异应优先放在该厂商 wrapper 或专用 Provider
中，避免让所有兼容上游都承担特殊规则。

- OpenAI 非标准 usage 可通过 `openai_compatible.Config.UsageParser` 注入。
- 请求体需要改写时，在 wrapper 的 `SendRequest`/`SendStreamRequest` 调用 Base 前处理
  副本；不要修改其他候选复用的原始状态。
- 非标准鉴权或 URL 无法由 Base 表达时，实现完整 `provider.Provider`，不要伪装成标准
  兼容面。
- 错误分类应返回 `provider.ProviderError`，保留 status、raw body、`RetryAfter` 和准确的
  `ErrorType`。
- 实际向 KeyPool 上报过错误时设置 `KeyPoolReported: true`，防止 Proxy 重复计数。
- 必须使用 `req.Key` 发请求。仅当它为 nil（健康检查等非路由调用）时才自行 acquire。
- HTTP 429 使用 `provider.ParseRetryAfter` 并上报 rate limit；401/403、额度耗尽、
  invalid request 和 5xx 不应混为一种错误。
- 流式响应要覆盖“开流前失败”和“SSE 内错误事件”两种形状。开流后才出现的错误无法
  向客户端改写已发送的 HTTP status。

如果新增的是一种可跨厂商复用的错误形状，才考虑扩展共享解析器，并为所有协议基座
增加回归测试。

## 第四步：注册 face 和装载包

```go
func init() {
    provider.RegisterGlobalWithProtocolVendor(
        openAIName, NewOpenAI, provider.ProtocolOpenAI, "acme",
    )
    provider.RegisterGlobalWithProtocolVendor(
        anthropicName, NewAnthropic, provider.ProtocolAnthropic, "acme",
    )
}
```

然后在 `backend/internal/provider/builtin/builtin.go` 增加：

```go
_ "github.com/wang546673478/native-llm-gateway/internal/provider/acme"
```

遗漏 blank import 时，Go 不会编译该包，`init()` 不执行，管理 API 也看不到厂商。
face 名必须全局唯一；vendor 必须在同一厂商的所有 face 中保持一致。

## 第五步：余额查询

只有上游存在可验证的额度端点时才实现 `quotacheck.Balancer`：

```go
func init() {
    b := newBalancer()
    quotacheck.RegisterBalancer(openAIName, b)
    quotacheck.RegisterBalancer(anthropicName, b)
}
```

共享池可能通过任意 face 查找 balancer，因此有余额能力时要为该 vendor 的每个 face
注册。`FetchBalance` 返回 `Raw`、`HasQuota`、`Source` 和 `Kind`（`percent` 或
`currency`），并区分“确认无额度”和“查询失败”。

- 有 balancer 的 vendor 使用 poll 恢复模式。
- 没有 balancer 的 vendor 使用 probe 模式，不要为了满足接口而伪造余额。
- 未公开控制台端点和短期 Cookie 是脆弱依赖，必须记录降级行为并测试 401/403/5xx。
- 同一 vendor 混合 `token_plan` 与 `api` key 时，应根据 `k.BillingSource` 选择额度
  端点，而不是根据随机选中的 face 名判断。

## 第六步：配置 face

```yaml
providers:
  acme:
    enabled: true
    endpoint: "https://api.example.com/v1"
    protocol: "openai"
    billing_source: "api"
    timeout: 60s
    responses_api: true
    circuit_breaker:
      failure_threshold: 5
      failure_window: 60s
      open_timeout: 30s
      half_open_requests: 1
```

每个 face 都要独立配置。`billing_source` 可以因 endpoint 而不同，MiMo 的按量与套餐面
就是现有先例；相同 endpoint/账户则应保持一致。`responses_api` 只给原生支持
Responses 的 OpenAI face 打开。

不要添加 `keys`、`models`、`default_model` 或旧的 `cost_per_1k_*`：

- key 通过 Provider Keys 页面或管理 API 写数据库；
- 模型通过上游同步写 `provider_models`/`provider_model_faces`；
- 三档价格在模型管理页按人民币每百万 token 手工维护。

## 第七步：测试和验证

至少增加以下测试：

- `registry_test.go`：每个 face 的 Protocol 和 Vendor；
- 构造测试：拒绝空 endpoint 和错误 protocol；
- URL 测试：endpoint 含/不含 `/v1`、尾斜杠、chat、responses、models；
- 非流式和流式 usage，特别是 cache token 不重复计费；
- 每一种专属错误形状，包括 HTTP 200 body 和 SSE 内错误；
- 多 key 下确认实际请求 key 与被上报的 key 相同；
- balancer 的正常、零额度、鉴权失败、解析失败和 5xx；
- 多 face/mixed tier 时模型同步使用正确 endpoint 和 key。

本地门禁：

```bash
cd backend
go test -count=1 ./internal/provider/acme ./internal/provider/...
go test -count=1 ./...
go vet ./...
```

部署新二进制后再完成运行验证：

1. `GET /api/v1/providers` 能看到 vendor 和全部 face。
2. 添加至少一把带正确 `billing_source`/`protocols` 的上游 key。
3. 在模型管理页同步该 vendor，确认 face 归属和模型列表。
4. 填写三档价格；未填价格的用量成本为 0。
5. 在 Routing 页保存所需顺序。未写 `route_order` 的新厂商按默认 key 创建时间排序，
   且排在已有显式改写之后。
6. 分别对每种协议做流式、非流式和 failover E2E，并用 Access Log 核对 vendor、
   protocol、provider key、最终模型和错误分类。

## 提交前清单

- [ ] 已证明动态中转站不足以表达该上游。
- [ ] 官方协议、URL、鉴权、模型、usage、错误和额度契约均有来源或直连证据。
- [ ] 每个 face 的 factory、Protocol、Vendor 和 `builtin` blank import 完整。
- [ ] Provider 复用 `req.Key`，没有发送后再次 acquire。
- [ ] endpoint 与三个 OpenAI path 或 Anthropic messages path 的组合有测试。
- [ ] mixed tier face 将 `BillingSource` 透传到支持它的基座。
- [ ] 有 balancer 时为所有 face 注册；无可靠端点时明确走 probe。
- [ ] config 只包含面级运行配置，没有旧模型、价格或 key 字段。
- [ ] 模型同步、价格、路由顺序和 E2E 已验证。
- [ ] Provider 包测试、后端全量测试和 `go vet` 全部通过。
