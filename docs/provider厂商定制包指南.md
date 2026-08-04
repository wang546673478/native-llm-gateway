# Provider 厂商定制包指南

> 新增或更新一个厂商(如加回 kimi)的完整实操手册。
> 配套 skill:`.claude/skills/provider-vendor`(让 Claude Code 按本指南执行)。

## 概念模型(必须理解)

- **厂商(vendor)= 一个实体**(如 deepseek),UI 层只显示厂商名
- **注册名(registration name)= 协议面路由名**:`deepseek`(openai 面)、`deepseek-anthropic`(anthropic 面),同一厂商的多个注册名**共享同一个 key 池**
- **key 厂商级一份**:`provider_api_keys.provider_name` 存厂商名,`protocols` 列标记可用协议面(空 = 全部)
- **catch_all 自动模式**:只要注册了 + config 启用了,自动进链,无路由表

## 文件清单(以 deepseek 为模板)

```
backend/internal/provider/deepseek/
├── deepseek.go          # openai 面:继承 openai_compatible.Base
├── anthropic.go         # anthropic 面:继承 anthropic_compatible.Base
├── balancer.go          # 余额查询(quotacheck.RegisterBalancer)
├── registry_test.go     # 双协议面注册回归测试(必写,防误删注册名)
└── balancer_test.go     # 余额解析测试
```

## 分步指南

### Step 1:写 openai 面(`xxx.go`)

```go
// Package deepseek — 厂商包:openai 面 + anthropic 面共享 key 池
package deepseek

import (
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	name       = "deepseek"
	chatPath   = "/chat/completions" // 无 /v1 前缀时用这个;否则默认 /v1/chat/completions
	responsesPath = "/v1/responses"  // 原生支持 Responses API 才配;endpoint 已含 /v1 则写 "/responses"
)

type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

func New(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("%s requires protocol=openai, got %q", name, cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%s endpoint is required", name)
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:          name,
			Endpoint:      cfg.Endpoint,
			Timeout:       cfg.Timeout,
			ChatPath:      chatPath,
			ResponsesPath: responsesPath, // 不支持 Responses API 的厂商不要设(用默认也发不过去,链上会被 responses_api 过滤)
			StreamUsage:   true,          // 流式末尾带 usage,网关才能记账
			Pool:          toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}
```

### Step 2:写 anthropic 面(可选,`anthropic.go`)

```go
const anthropicName = "deepseek-anthropic"

func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	// 校验 protocol=anthropic + endpoint
	return &AnthropicProvider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     anthropicName,
			Endpoint: cfg.Endpoint, // 通常 = openai 面的 endpoint + /anthropic
			Timeout:  cfg.Timeout,
			Pool:     toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}
```

### Step 3:注册(核心不变量)

```go
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)                    // vendor = 厂商名
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name) // 同一 vendor
}
```

> `RegisterGlobalWithProtocolVendor(注册名, 工厂, 协议, 厂商)` — vendor 参数决定:
> - `VendorFor(注册名)` 归一(key 绑定/白名单/access log 都按厂商)
> - 同 vendor 共享 key 池(server.buildKeyPools 按 vendor 复用)

### Step 4:余额查询(可选,`balancer.go`)

```go
func init() {
	quotacheck.RegisterBalancer("deepseek", b)          // 每个注册名都要注册!
	quotacheck.RegisterBalancer("deepseek-anthropic", b)
}
```

Balancer 接口:`FetchBalance(ctx, baseURL, key) (*Balance, error)`,返回 `{Raw float64, HasQuota bool, Kind "percent"|"currency"}`。
- **token_plan 厂商必须有**(percent 或金额),否则额度耗尽永不标记、永不降级
- 实测过 MiniMax 的 `token_plan/remains` 是**未文档化端点**,不稳定要降级到错误码驱动(HTTP 200 + base_resp 1008/2056 → 见踩坑 #1)

### Step 5:config.yaml 加块

```yaml
kimi:
  enabled: true
  billing_source: "api"          # token_plan / api / free — 决定 tier 层级
  endpoint: "https://api.kimi.com"
  protocol: "openai"             # 该块的协议面
  timeout: 60s
  default_model: "kimi-k3"       # 可选:catch_all 自动模式用它承接;缺省 = models 第一个
  responses_api: false           # 原生支持 /v1/responses 才 true(deepseek/minimax 已标)
  models:
    - id: "kimi-k3"
      cost_per_1k_input: 0.001
      cost_per_1k_output: 0.002
      cost_per_1k_cache_read: 0.0
```

> anthropic 面要单独一个块(`kimi-anthropic` 或 `kimi` + `kimi-openai`,协议对应)。**同一厂商的所有块 `billing_source` 保持一致**(共享 pool 按 tier 桶)。

### Step 6:测试与验证

1. `registry_test.go`:断言两个注册名 + 各自的 Protocol/Vendor(复制 deepseek/registry_test.go 改名字)
2. `go build ./... && go test ./...`
3. 重启网关 → `/api/v1/providers` 出现新厂商(按 vendor 聚合)
4. 页面加 key(选厂商 → 协议)
5. E2E:发任意模型名 → 走链命中新厂商;access log 显示厂商名 + 实际使用模型

## 常见坑(详细见 docs/踩坑与排错.md)

| 坑 | 要点 |
|---|---|
| 上游错误藏在 HTTP 200 body | 用 `ParseMiniMaxBaseResp` 同款机制解析,别只认状态码 |
| 400 invalid_request 不禁用 key | `ReportError` 只对 auth 禁用(已修,别改回去) |
| Responses API 支持矩阵 | 支持的厂商标 `responses_api: true` + `ResponsesPath`;`/responses` 请求链上只走支持的 |
| 跨厂商推理块 | 客户端回带上家 reasoning → 网关自动剥离 + effort=none,厂商包无需处理 |
| 注册名都要注册 balancer | 漏一个 → 该协议面额度永不标记 |
| vendor 参数别填错 | 填错 → key 池不共享 / 绑定归一失效 / access log 厂商名错乱 |
