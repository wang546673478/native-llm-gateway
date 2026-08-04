# Provider 按厂商建模 — 定制包 + UI 按厂商 + key 厂商级

**日期**: 2026-08-04
**作者**: Claude
**目标版本**: Native LLM Gateway 下一版
**代号**: P-provider-vendor

---

## 0. Context & 动机

### 0.1 现状问题

1. **目录结构按协议拆包,冗余**:`deepseek/`(openai)与 `deepseek_anthropic/`(anthropic)、`minimax/`(anthropic)与 `minimax_openai/`(openai)同一家厂商拆成两个包,各包是 `openai_compatible.Base` / `anthropic_compatible.Base` 的薄包装(差异仅 endpoint / chat_path / stream_usage / 默认模型)。
2. **UI 层把协议变体当厂商展示**:Providers 页和 ProviderKeys 页把 `deepseek` / `deepseek-anthropic` 作为两个独立 provider 展示,用户心智中它们是同一家厂商。
3. **key 按 provider 名存两份**:同一把 API key(物理上两个协议端点通用)在 `deepseek` 和 `deepseek-anthropic` 下各存一行,冗余。
4. **glm/kimi 为死代码**:无可用 API key,无法测试。

### 0.2 用户决策(2026-08-04 确认)

| # | 决策 |
|---|---|
| 1 | **定制包方案**:保留每家一个包,目录按厂商合并;不用配置化零代码方案(可排查性优先) |
| 2 | **注册名 / config.yaml / 对外 API 路径全部不动**(`deepseek` / `deepseek-anthropic` / `minimax` / `minimax-openai` 照旧;路由对外行为不变,内部数据结构按 §4.4 增补协议字段) |
| 3 | **UI 按厂商显示**:Providers 页一行一个厂商;ProviderKeys 添加时选厂商 → 勾协议(默认全勾) |
| 4 | **key 厂商级一份**:DB 加 `protocols` 列 + 启动迁移 + 同厂商共享 pool + 取 key 按协议过滤 |
| 5 | **删除 glm/kimi** 四个包 + config 条目 + glm balancer 注册 |
| 6 | **特性落地**:把 2026-08-04 全量文档调研(DeepSeek 19 页 / MiniMax 23 页)发现的两个真实缺口补上;需要新端点的特性(C 类)YAGNI 不做 |

---

## 1. 设计原则

1. **厂商(vendor)是 UI 层概念,注册名是路由层概念**。同一厂商的多个注册名共享:模型列表(各自 config 条目仍独立)、key 池、额度 balancer。
2. **零行为变化优先**:注册名、config.yaml 的 provider 条目结构、路由逻辑不变;唯一行为变化是 key 按协议过滤(poll 去重)和 UI 展示。
3. **特性由客户端触发,网关透传即支持**;网关只在"必须读懂响应"的触点写代码(usage 解析、错误识别)。

---

## 2. 概念模型

```
厂商 vendor(UI 层,如 deepseek)
├── 注册名 name(路由层):"deepseek"(openai 协议) / "deepseek-anthropic"(anthropic 协议)
├── key 池:厂商级一份,每把 key 标记可用协议(protocols,空 = 全部)
├── 额度 balancer:按注册名注册,同一实例
└── 模型:各注册名 config 条目独立配置(现状,不动)
```

---

## 3. 数据模型

### 3.1 `database.ProviderAPIKey` 加 `Protocols` 列

```go
type ProviderAPIKey struct {
    // ... 既有字段 ...
    // P-provider-vendor: key 可用的协议列表,逗号分隔("openai,anthropic");空 = 全部协议
    Protocols string `gorm:"column:protocols;default:''" json:"protocols"`
}
```

语义:**限制性** — 非空时该 key 只用于列出的协议;空 = 全部。同一把 key 物理上两端点通用,protocols 只是用户限制。

### 3.2 `keypool.Key` 加运行时字段(不落 DB,同 `Remaining`)

```go
type Key struct {
    // ... 既有字段 ...
    Protocols string // P-provider-vendor: 从 DB ProviderAPIKey.Protocols 读入;空 = 全部
}
```

### 3.3 `provider.Registry` 加 vendor 元数据

```go
// Registry 内部新增:
// vendors map[string]string // name → vendor(默认 = name 本身)

// 新增注册 API(deepseek 包 init 里用):
func RegisterGlobalWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string)
```

`ListRegisteredProtocols()` 扩展为返回 `map[string]RegisteredInfo`,其中:

```go
type RegisteredInfo struct {
    Protocol Protocol
    Vendor   string
}
```

(旧签名保留兼容或直接替换,由实现决定;调用方只有 admin.go `listRegisteredProviders`。)

---

## 4. 组件改动(后端)

### 4.1 目录合并(纯搬文件 + 改名)

```
deepseek/                  minimax/
├── deepseek.go  (openai)  ├── minimax.go   (anthropic,现状)
├── anthropic.go (新增)     ├── openai.go    (新增,从 minimax_openai 搬)
├── balancer.go            ├── balancer.go
```

- `deepseek_anthropic/deepseek_anthropic.go` → `deepseek/anthropic.go`:包名改 `deepseek`,`New` 改名 `NewAnthropic`,`name` 常量保留 `"deepseek-anthropic"`;`init()` 合并到 `deepseek.go` 的 init(注册两个名字 + vendor)
- `minimax_openai/minimax_openai.go` → `minimax/openai.go`:包名改 `minimax`,`New` 改名 `NewOpenAI`,name 常量保留 `"minimax-openai"`
- 注册:`RegisterGlobalWithProtocolVendor("deepseek", New, ProtocolOpenAI, "deepseek")` + `("deepseek-anthropic", NewAnthropic, ProtocolAnthropic, "deepseek")`;minimax 同理(vendor = "minimax")
- `keypool` import 里的 `toPool` 函数:deepseek 包现有一个 `toPool`,anthropic.go 搬入后**重复定义会编译失败** — 合并时保留一个 `toPool`(deepseek.go 里已有),anthropic.go 删掉自己的

### 4.2 DB 迁移(启动时执行)

启动迁移函数(放在现有 AutoMigrate 之后):

```sql
-- 协议变体并入厂商名,并标协议
UPDATE provider_api_keys SET provider_name='deepseek', protocols='anthropic'
  WHERE provider_name='deepseek-anthropic';
UPDATE provider_api_keys SET provider_name='minimax', protocols='openai'
  WHERE provider_name='minimax-openai';
```

- **只迁移变体注册名**(deepseek-anthropic / minimax-openai),不碰主条目:主条目的旧 key 保持 protocols 为空 = 全部协议(物理上同一 key 两端点通用;且避免每次重启把用户后来新加的全协议 key 覆盖成单协议 — 2026-08-04 用户裁决)
- 幂等:变体行迁移后不再存在,重复执行影响 0 行,无副作用
- glm/kimi 的 key 行**不迁移**(包已删,行保留无害)
- 迁移后 `deepseek` 池内可能同一把 key 两行(主条目一行空、变体行标 anthropic)— 不去重,行为正确,用户可在 UI 调整

### 4.3 key pool 共享(厂商级一个池)

`server.go buildKeyPools`:遍历 `cfg.Providers` 时,按 vendor 去重 — vendor 已有 pool 则复用(两个注册名共享同一个 `*keypool.Pool`):

```go
// 伪码
vendorPools := map[string]*keypool.Pool{}
for name, p := range cfg.Providers {
    if !p.Enabled { continue }
    vendor := registry.VendorFor(name) // 新增方法
    pool, ok := vendorPools[vendor]
    if !ok {
        pool = buildOnePool(ctx, vendor, ...) // pool 名用 vendor
        vendorPools[vendor] = pool
    }
    out[name] = pool // 两个注册名 → 同一 pool
}
```

- `injectPools` 不变(按 name 注入,同一 pool SetPool 两次,幂等)
- `buildOnePool` 读 DB 时用 vendor 名查(迁移后 key 都在 vendor 名下)

### 4.4 取 key 按协议过滤

- `router.ProviderRoute` 加 `Protocol provider.Protocol` 字段(`routeDirectModelWithOpts` 构建 candidates 时从 manager 查实例协议填入)
- `RouteIterator` 透传协议;取 key 时过滤:`key.Protocols == "" || key.Protocols 包含当前协议`
- 过滤实现位置:keypool 的 Acquire 路径加协议参数(推荐),或 router 层取到后校验再换下一个(由 plan 定,语义不变)
- **fallback 语义**:过滤后无匹配 key 的 tier → 按现状兜底(继续下一个 tier / 下一个 candidate,同现状 `AcquireFromTier` 拿不到 key 就 continue)

### 4.5 quotacheck 轮询去重

`pollAllBalancers` 按 `m.pools.Get()` 遍历,共享 pool 后同一 `*keypool.Pool` 会被两个注册名各 poll 一次(两次相同的 HTTP 余额查询)。改为按 pool 指针去重:

```go
seen := map[*keypool.Pool]bool{}
for providerName, pool := range m.pools.Get() {
    if seen[pool] { continue }
    seen[pool] = true
    balancer := LookupBalancer(providerName) // 第一次遇到的名字查 balancer(同名注册,任意)
    ...
}
```

### 4.6 `auth/provider_keys_handler.go`

- `ProviderKeyView` 加 `Protocols string \`json:"protocols"\``;create 请求体接受 `protocols` 字段(默认空 = 全部)
- `toProviderKeyViewFromPool` 从 live `*keypool.Key` 透传 Protocols
- **List 过滤**:provider name 列表按 vendor 聚合时,前端传什么 name 就查什么(现状不变);UI 层选厂商+协议 → 具体注册名

### 4.7 特性落地(2026-08-04 文档调研的两个真实缺口)

**B1. `openai_compatible` usage 补 `prompt_tokens_details.cached_tokens`**

现状:只解析 DeepSeek 风格的 `prompt_cache_hit_tokens`。MiniMax(及 OpenAI 标准)返回 `prompt_tokens_details.cached_tokens`,未解析 → 缓存命中不计缓存价。

```go
// parseOpenAIUsage 的 Usage 结构体加:
PromptTokensDetails *struct {
    CachedTokens int `json:"cached_tokens"`
} `json:"prompt_tokens_details"`
// 解析时:CacheReadTokens = PromptCacheHitTokens + PromptTokensDetails.CachedTokens
```

(DeepSeek 的 prompt_tokens = hit + miss,缓存价按 hit;MiniMax 的 prompt_tokens 不含 cached_tokens、cached_tokens 按缓存价 — 两种语义下 `CacheReadTokens = hit + cached` 都正确,`RawUsage` 同时记录两个字段。)

**B2. config.yaml 补 MiniMax 缓存价**

文档(2026-08-04):M3 缓存读 0.42 元/M(无主动缓存写价);M2.7 缓存读 0.42 / 写 2.625;M2.5/2.1 读 0.21 / 写 2.625。

```yaml
# minimax 各模型加:
cost_per_1k_cache_read: 0.00042      # 0.42 元/M
cost_per_1k_cache_creation: 0.002625 # 2.625 元/M(M3 不加 creation,M2.7 加)
```

(M2.5/M2.1 的 read 是 0.00021。)

**B3. 包注释补官方特性清单**

deepseek / minimax 包 header 注释补全(排查时的权威参考):
- deepseek:thinking(默认 enabled + reasoning_effort)、JSON output(prompt 须含 "json" 字样)、tool calls(带 tools 时必须回传 reasoning_content 否则 400)、KV cache 自动开启、Anthropic 模式未知模型名静默映射 flash、流式 keep-alive 行、峰谷定价预告
- minimax:thinking 默认值分裂(anthropic 关 / openai 开)、`<think>` 标签内嵌 content 须原样回传、双缓存机制(主动 cache_control 仅 M2.x / 自动缓存 M3+)、`service_tier` priority 1.5x、`reasoning_split`、无官方余额 API(用 token_plan/remains 未文档化端点)

### 4.8 删除清单

| 删除 | 连带 |
|---|---|
| `provider/glm/` `glm_anthropic/` `kimi/` `kimi_anthropic/` 四包 | glm balancer 注册(`quotacheck.RegisterBalancer("glm", ...)` / `("glm-anthropic", ...)`)随之消失 |
| config.yaml 中 glm / glm-anthropic / kimi / kimi-anthropic 四个条目 | — |
| `deepseek_anthropic/` `minimax_openai/` 两个旧包(内容搬入新位置) | — |
| 注释里的陈旧引用 | `authenticator.go` / `database/models.go` 注释中 glm/kimi 示例;`router.go` P36 注释的"minimax 和 minimax-openai"表述更新为"同厂商两个协议" |

---

## 5. 前端改动

### 5.1 `frontend/src/api/client.ts`

- `ProviderInfo` 加 `vendor: string`、`protocols: string[]`(后端返回)
- `ProviderKeyView` 加 `protocols: string`

### 5.2 `backend/internal/api/http/handler/admin.go` — `listProviders`(GET /api/v1/providers)

返回结构固定为按 vendor 维度(前端两页一起改,无需向后兼容):

```json
{
  "vendors": [
    {
      "vendor": "deepseek",
      "names": [
        {"name": "deepseek",         "protocol": "openai"},
        {"name": "deepseek-anthropic","protocol": "anthropic"}
      ],
      "models": ["deepseek-v4-flash", "deepseek-v4-pro"],
      "key_pool": {...},
      "circuit_breaker": {...}
    }
  ]
}
```

聚合规则:按 vendor 分组(`vendor = a.Registry.VendorFor(name)`);`names` 为该 vendor 下全部已加载注册名及其协议;`models` 为并集去重;`key_pool` / `circuit_breaker` 取该 vendor 第一个注册名的(共享 pool 时状态相同)。单协议厂商(如 qwen)vendor = name,`names` 单元素。

**`listRegisteredProviders`(GET /api/v1/providers/registered)保持原样不动** — AccessLogs.vue 仍按平铺 `providers` 结构消费。

### 5.3 `frontend/src/views/Providers.vue`

- 一行一个厂商(vendor):name 列显示厂商名;protocol 列显示该厂商下协议列表(多标签);models 列并集
- `api.providers()` 消费新的 vendors 结构

### 5.4 `frontend/src/views/ProviderKeys.vue`

- 添加表单:`Provider` 下拉改为两级 — 厂商下拉(唯一 vendor)→ 协议多选(默认全勾,来自该 vendor 的 names 列表)→ 填 key
- **提交:一把 key 创建一条(厂商级一份)** — 目标注册名 = 该 vendor 的第一个注册名(pool 共享,另一协议面的请求同样能取到);`protocols` 字段 = 勾选列表(全勾 → 空 = 全部;勾选子集 → 逗号分隔)。不创建两条
- 列表:Provider 列显示注册名(同 vendor 的 key 因共享池而 provider_name 相同,天然相邻);keys 加载循环 vendor.names 展开后的注册名列表拉取(与现状循环 provider 等价)
- 编辑/删除:按行内注册名操作,不变

---

## 6. 测试

| 测试 | 内容 |
|---|---|
| 迁移测试 | 启动迁移函数:deepseek-anthropic 行 → deepseek+anthropic;minimax-openai → minimax+openai;幂等(跑两遍结果相同) |
| key 过滤测试 | keypool:Protocols="" 全部可用;Protocols="openai" 只被 openai 请求取到;无匹配时按现状兜底 continue |
| vendor 注册测试 | registry:RegisterGlobalWithProtocolVendor 后 ListRegisteredInfo 返回 vendor;未声明 vendor 的旧注册默认 vendor=name |
| usage 解析测试 | `parseOpenAIUsage`:`prompt_tokens_details.cached_tokens` 解析进 CacheReadTokens(与 prompt_cache_hit_tokens 并存) |
| 既有测试 | deepseek/minimax balancer 测试、openai_compatible 测试保持绿(注册名不变) |
| 构建 | `go build ./...` + `go test ./...` |

前端无单测基建,手动验证:Providers 页按厂商一行、ProviderKeys 添加选厂商勾协议、同一 key 两协议都能路由。

---

## 7. 成功标准

- 目录:`internal/provider/` 下 deepseek/minimax 各一个包,glm/kimi/deepseek_anthropic/minimax_openai 消失
- 注册名、config.yaml provider 条目结构、路由、对外 API 路径全部不变
- 启动后 DB 迁移完成:deepseek 池的 key 带协议标记;deepseek-anthropic 请求能取到 key
- ProviderKeys 页:添加时选厂商 → 勾协议 → 填 key;同一把 key 两协议都能用(实测 openai + anthropic 各发一次请求)
- Providers 页:一行一个厂商,协议列表显示
- pollAllBalancers:同一 pool 每轮只 poll 一次
- 新特性:`prompt_tokens_details.cached_tokens` 计入缓存价;minimax 缓存价生效;包注释含官方特性清单
- `go build ./...` + `go test ./...` 通过;e2e 启动验证

---

## 8. 不在本次范围(YAGNI)

- **配置化零代码加厂商**(用户选定制包,否决)
- **C 类新端点**:FIM(`/beta/completions`)、Chat Prefix、strict tools(`/beta` base_url)、Responses API、count_tokens — 无客户端,不做
- **glm/kimi 重接**:git 历史完整保留,以后有 key 再抄回
- **峰谷定价支持**(deepseek 高峰 2 倍价预告)— 未生效,生效后再配
- **Responses API 路由**、**模型名校验**(deepseek Anthropic 模式未知模型静默映射 flash)— 观察项,不在本期
- key 迁移去重(同一把 key 两行)— 不去重,行为正确

---

## 9. 预估 diff

- 后端:~350 行改动,~16 文件(2 新文件、6 删除、registry/DB/keypool/router/server/quotacheck/auth handler 小改、迁移函数)
- 前端:~120 行改动,3 文件(Providers.vue / ProviderKeys.vue / client.ts)+ admin.go 聚合逻辑
- config.yaml:删 4 条目 + minimax 补缓存价
- 1 次启动迁移,无配置结构变更
