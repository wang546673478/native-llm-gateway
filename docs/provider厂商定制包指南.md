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

### Step 0:调研官方文档(先做,防返工)

**官方文档是唯一权威来源** — 用户只提供官方文档 URL,不依赖搜索镜像/二手信息。

**0.1 确认厂商**:请求未点明厂商时(如「加一个新厂商」)先问清是哪家;已点明(如「加 kimi」)跳过。

**0.2 要官方文档 URL**:
- 用户给的必须是 URL;不是 URL(如「去搜 XX 官网」)→ 要求重新提供
- URL 拉取失败(404 / 非官方域名)→ 请用户换一个

**0.3 遍历官方文档站**(只在用户给的站内):
- 入口:优先拉 `/llms.txt` 全量索引(抓不到就抓首页)
- WebSearch 限定站内定位章节
- **必须 `grep -i "anthropic\|claude"` 索引** —— anthropic 兼容面常藏在「Claude API 兼容」章节(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,漏查导致第一版只有 openai 面)
- llms.txt + 首页都失败或站内定位不到 → 回 0.2 请用户换 URL,不要自行出站找替代来源

**0.4 提取 6 类信息**(对话内速查表,每项标消费方):

| # | 提取项 | 消费方 |
|---|--------|--------|
| ① 协议面 | openai/anthropic base URL、Responses 支持与路径(端点是否已含 `/v1`)、鉴权方式 | ChatPath/ResponsesPath、config 块、`responses_api` |
| ② 模型与能力 | 真实模型 ID、上下文窗口(512k 悬崖)、思考模式(默认开/关、reasoning_effort、thinking 参数名)、工具调用(带 tools 的回传要求)、流式格式、JSON output 触发条件 | 包代码 + DB `provider_models`(靠上游同步拉取,排序保上游顺序) |
| ③ 定价 | input/output 单价(**单位换算**:元/M → 元/百万 token;美元单价还要按汇率换算)、缓存 read/creation 有无与数值、缓存计费语义(`prompt_tokens` 含不含 cached)、峰谷价 | DB `provider_models` 三档 `cost_per_million_input/cache_read/output`(模型管理页手工填) |
| ④ 余额 | 官方余额 API 端点与响应字段;没有 → 替代方案(如未文档化 token_plan);额度错误藏 200 body? | `balancer.go`、`RegisterBalancer` |
| ⑤ 定制特性 | 响应包裹格式(base_resp)、reasoning 字段名与回传规则、缓存机制差异、厂商专属参数(service_tier 等)、429 语义(套餐耗尽 vs 真限流) | 包 header 注释 |
| ⑥ 入口 | `/llms.txt` 索引、模型表/定价表/协议章节各自 URL | 遍历路线 |

**0.5 差异标注**:官方文档与现有 config/文档冲突(如 MiniMax 旧域名)时,标出差异再动,不静默覆盖。

**完成度标准**(全部满足 = 调研完,进入 Step 1):

| 项 | 标准 |
|---|---|
| 协议 | 每个面 base URL + 路径确认,能写出 ChatPath/ResponsesPath |
| 模型 | ≥1 个真实可用模型 ID(靠上游同步拉取后写进 DB `provider_models`) |
| 价格 | input/output 单价确认;文档没给 → 显式标「无定价 → cost 缺省 0」,不是跳过 |
| 特性 | 能写出完整 header 注释清单 |
| 余额 | 有官方 API / 无(用替代)/ 文档未提及 —— 三选一显式结论 |
| 未知项 | 标「文档未提及」≠「没有」,写代码时按最保守处理 |

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

**关键:还要在 `internal/provider/builtin/builtin.go` 加 blank import** — Go 只编译被 import 的包,
厂商包靠 `init()` 自注册;不加这行,新厂商不会进二进制,`/api/v1/providers` 里
死活不出现它。在现有 blank import 后面加一行:

```go
_ "github.com/wang546673478/native-llm-gateway/internal/provider/kimi" // 触发 init() 注册(示例;glm/qwen/gemini 已于 2026-08-20 删除)
```

### Step 4:余额查询(可选,`balancer.go`)

```go
func init() {
	quotacheck.RegisterBalancer("deepseek", b)          // 每个注册名都要注册!
	quotacheck.RegisterBalancer("deepseek-anthropic", b)
}
```

Balancer 接口:`FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (Balance, error)`,返回 `Balance{Raw float64, HasQuota bool, Source string, Kind "percent"|"currency"}`。
- **token_plan 厂商必须有**(percent 或金额),否则额度耗尽永不标记、永不降级
- 有余额查询的厂商一律写 balancer。现存三厂商(deepseek / minimax / mimo)**全部有 balancer**:
  - deepseek / minimax 有官方余额端点(API key 鉴权)
  - mimo 无官方 API,走**控制台未文档化端点 cookie 鉴权**(见下一条)—— 也是 balancer,不是「不写」
  - 「无 balancer → probe 模式(每次请求重新探测,不永久标耗尽)」只作为未来加「真无余额端点」厂商的兜底路径,当前没有厂商走它(glm/qwen/gemini 已删除)
- balancer 会被请求路径的主动查额度复用(quotacheck.CheckQuota):网络类错误后由网关统一调用,厂商包无需另写查询入口
- 实测过 MiniMax 的 `token_plan/remains` 是**未文档化端点**(quota host 与 chat host 不同,`www.minimaxi.com`),且早期猜的字段名都不对;稳定兜底仍是错误码驱动(HTTP 200 + base_resp 1008/2056 → 见踩坑 #1)——balancer 拿不到数就靠错误码降级,两条路都写着
- **控制台 cookie 鉴权模式**(MiMo 先例,见踩坑 #19):厂商无官方余额 API 但控制台有未文档化端点(社区逆向),鉴权是账号登录 cookie(约 1 天过期)而非 API key。实现要点:① cookie 放 config `quota_cookie` + 管理 API `POST /api/v1/providers/mimo/quota-cookie` 热更新(验证 → DB 持久化 → 注入),不放 key 上;② poll 按 vendor pool 去重,balancer 内按 `k.BillingSource` 分支端点(token_plan key → 套餐端点,api key → 余额端点),不能在注册名上分;③ cookie 过期 401 → 轮询退化保守(不标耗尽),错误码兜底不变
- (glm 已随包删除,其 monitor 端点案例仅作历史参考)

### Step 5:config.yaml 加块

```yaml
kimi:
  enabled: true
  billing_source: "api"          # token_plan / api / free — 决定 tier 层级
  endpoint: "https://api.kimi.com"   # 已含 /v1 就不该在 ModelsPath 再拼(踩坑:mimo 双 /v1)
  protocol: "openai"             # 该块的协议面
  timeout: 60s
  responses_api: false           # 原生支持 /v1/responses 才 true(deepseek/minimax 已标)
```

> ⚠️ **没有 `models:` 段、没有 `default_model`、没有 `cost_per_1k_*` 了**(2026-08-20 起)——
> 模型清单与定价的唯一真相源是 DB 表 `provider_models`,靠「上游同步」拉取、靠「模型管理页」手工定价。
> config 里只负责「启不启用、走哪个端点、计费层级」这类面级配置。

> anthropic 面要单独一个块(`kimi-anthropic` 或 `kimi` + `kimi-openai`,协议对应)。**同一厂商的所有块 `billing_source` 保持一致**(共享 pool 按 tier 桶)。

### Step 6:测试与验证

1. `registry_test.go`:断言两个注册名 + 各自的 Protocol/Vendor(复制 deepseek/registry_test.go 改名字)
2. `go build ./... && go test ./...`
3. `./gateway-reload.sh` 无感重载(make build + `sudo systemctl restart` + health check,**需要 sudo**;无 sudo 时手动 `make build` + `kill -TERM <pid>` 靠 Restart=always 拉起)→ `/api/v1/providers` 出现新厂商(按 vendor 聚合)
4. 页面加 key(选厂商 → 协议)
5. **同步模型**(必须,否则新厂商路由会 no_route):模型管理页点该厂商「同步」,或「全部同步」`POST /api/v1/providers/sync-all-models`——从上游 `/models` 端点拉模型 id 写进 `provider_models`;模型没进 DB 时,gateway key 白名单命不中 → 无候选 → 503 no_route
6. **拖拽排序**(可选):Routing 页「调度顺序树」里把新厂商拖到你想要的优先级,点「保存排序」→ 写入 `route_order` 表 + 热生效。不拖的话新厂商走默认序「最早 key 加入时间」,且**有改写的厂商恒排在无改写的新厂商之前**(即新厂商通常垫底),但仍参与路由
7. E2E:发任意模型名 → 走链命中新厂商;access log 显示厂商名 + 实际使用模型

## 常见坑(详细见 docs/踩坑与排错.md)

| 坑 | 要点 |
|---|---|
| 上游错误藏在 HTTP 200 body | 用 `ParseMiniMaxBaseResp` 同款机制解析,别只认状态码 |
| 错误分类别禁用 key | **无终端禁用状态**:auth → COOLING 5 分钟自动重试;400 invalid_request 只计数;5xx/timeout/connection → per-key 熔断(只熔断该 key)。别把「禁用」逻辑加回去 |
| 429 冷却标错 key | `req.Key` 已由路由层 acquire,Provider 层必须复用它发请求,不能内部二次 acquire(双 acquire 会把 429 冷却标到没发过请求的 healthy key 上) |
| Responses API 支持矩阵 | 支持的厂商标 `responses_api: true` + `ResponsesPath`;`/responses` 请求链上只走支持的 |
| 跨厂商推理块 | 客户端回带上家 reasoning → 网关自动剥离 + effort=none,厂商包无需处理 |
| 注册名都要注册 balancer | 漏一个 → 该协议面额度永不标记 |
| vendor 参数别填错 | 填错 → key 池不共享 / 绑定归一失效 / access log 厂商名错乱 |
| 注册了但新厂商不生效 | 忘了 `internal/provider/builtin/builtin.go` 的 blank import —— Go 只编译被 import 的包,`init()` 不跑,不进二进制 |
