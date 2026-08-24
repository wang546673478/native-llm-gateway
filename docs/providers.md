# Provider 厂商目录

> 当前内置的所有厂商。每个条目:**注册名(协议面) / billing / 余额恢复模式 / 文档来源 / 已知坑**。
>
> 新增/更新厂商见 `docs/provider厂商定制包指南.md` Step 0-6。

---

## 总览(2026-08)

| 厂商 | 注册名 | 协议 | billing | 余额恢复 | Responses API | 文档 |
|---|---|---|---|---|---|---|
| **deepseek** | `deepseek` | openai | api | poll | ✅ | `provider/deepseek/deepseek.go` |
| | `deepseek-anthropic` | anthropic | api | poll | ✅ | `provider/deepseek/anthropic.go` |
| **MiniMax** | `minimax` | anthropic | **token_plan** | poll | ✅ | `provider/minimax/minimax.go` |
| | `minimax-openai` | openai | **token_plan** | poll | ✅ | `provider/minimax/openai.go` |
| **MiMo**(小米) | `mimo` | openai | api | probe | ✅ | `provider/mimo/mimo.go` |
| | `mimo-token-plan` | openai | **token_plan** | probe | ✅ | `provider/mimo/mimo.go`(同 vendor) |
| | `mimo-anthropic` | anthropic | api | probe | ❌ | `provider/mimo/anthropic.go` |
| | `mimo-token-plan-anthropic` | anthropic | **token_plan** | probe | ❌ | `provider/mimo/anthropic.go`(同 vendor) |
| **Right Code** | `rightapi-grok` | openai | api | - | ✅ | config only |
| | `rightapi-gemini` | openai | api | - | ❌ | config only |
| | `rightapi-claude` | anthropic | api | - | ❌ | config only |
| | `rightapi-claude-aws` | anthropic | api | - | ❌ | config only |
| **TokenMarket** | `tokenmarket` | openai | api | - | ❌ | config only(中转站) |

> **kimi 已删除**(2026-08)。需要时按 `docs/provider厂商定制包指南.md` 加回。
> **glm / qwen / gemini 已删除**(2026-08-20,历史用量 glm 53 次、qwen/gemini 0 次)。需要时按 `docs/provider厂商定制包指南.md` 加回。
> **Right Code / TokenMarket** 为中转站,采用配置接入(无专属厂商包)。

---

## 1. deepseek

- **官方文档**:<https://api-docs.deepseek.com>
- **OpenAI 面**: `POST {endpoint}/chat/completions`(注意无 `/v1` 前缀)
- **Anthropic 面**: `POST {endpoint}/anthropic/v1/messages`
- **Endpoint**: `https://api.deepseek.com`

### 关键事实

1. **thinking 默认 enabled**;`reasoning_effort` ∈ `low | high | max`(medium/xhigh 映射 high)
2. **响应** `choices[].message.reasoning_content`;流式 `delta.reasoning_content`
3. **usage**:
   - `completion_tokens_details.reasoning_tokens` 计思维链
   - `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`(自动 KV cache,**缓存价仅 2%~0.8% 未命中价**)
4. **带 tools + thinking**:必须逐轮回传 `reasoning_content`,否则 400(跨厂商续接时网关自动剥离)
5. **Responses API**:`/v1/responses` 目前**只支持 `deepseek-v4-flash`(不支持 v4-pro)** — Codex 走 deepseek 时白名单放 v4-flash
6. **峰谷定价**(预告):高峰(北京 9-12 / 14-18 点)2 倍价

### 已弃用模型

- `deepseek-chat` / `deepseek-reasoner`(2026/07/24 弃用),老用户配置仍可用,建议尽快迁到 v4

### 已知坑

- **跨厂商 reasoning 回带**:Codex 从 MiniMax 切 deepseek 会因 MiniMax 的 `encrypted_content` 被 DeepSeek 拒收 → 网关 `stripResponsesReasoning` 自动剥离 + 注入 `effort=none`

---

## 2. MiniMax(稀宇科技)

- **官方文档**:<https://platform.minimaxi.com/docs/api-reference/api-overview>
- **Anthropic 面**(推荐): `POST https://api.minimaxi.com/anthropic/v1/messages`
- **OpenAI 面**: `POST https://api.minimaxi.com/v1/chat/completions`
- **Endpoint**: `https://api.minimaxi.com`(Anthropic 兼容,自动加 `/anthropic`)

### 当前模型(2026-07)

- `MiniMax-M3` — 1M tokens,旗舰
- `MiniMax-M2.7` / `MiniMax-M2.7-highspeed` — 204,800
- `MiniMax-M2.5` / `MiniMax-M2.5-highspeed` — 204,800
- `MiniMax-M2.1` / `MiniMax-M2.1-highspeed` — 204,800(早期稳定版)
- `MiniMax-M2` — 204,800

### M3 专属参数(`extra_body` 传)

```json
{
  "thinking": {"type": "adaptive" | "disabled"},   // M2.x 不可关闭
  "reasoning_split": true,                          // 把思考分到 reasoning_details
  "service_tier": "standard" | "priority"           // priority 1.5x 价格,优先准入
}
```

### 关键事实

1. **token_plan billing**:走套餐(`MiniMax` 默认 `billing_source: token_plan`),额度耗尽自动降档到 api 层(`minimax-openai` 同 vendor)
2. **错误藏在 HTTP 200 body**(踩坑 #1):`{"base_resp":{"status_code":1008|2056}}` 1008=余额不足,2056=超套餐
3. **Anthropic 面把套餐耗尽报成 HTTP 429**(踩坑 #14):与 openai 面的 200+base_resp 不同,关键词表必须含 "token plan / 用量上限 / 超套餐"
4. **余额接口**:官方 `token_plan/remains` 端点(未文档化,在 `www.minimaxi.com` 而非 chat host)
5. **Responses API**:`/v1/responses` 支持(注册名 `minimax`,虽然默认 anthropic 面)

### 当前 2 把 key 状态(实测 2026-08-08)

- `id=7 key-1`:ACTIVE,remaining=77%(健康)
- `id=8 weige`:QUOTA_EXCEEDED,remaining=1%(被 IsPolledAndExhausted 跳过)

### 已知坑

- 早期字段名猜错 → 余额端点要直连上游实测一次,别信文档示例

---

## 3. MiMo(小米)

- **官方文档**:<https://mimo.mi.com/docs/zh-CN/quick-start/summary/welcome>
- **两套端点/两套 key**:
  - **按量**: `https://api.xiaomimimo.com/v1`(`sk-xxx`,`billing=api`)
  - **Token Plan**: `https://token-plan-cn.xiaomimimo.com/v1`(`tp-xxx`,`billing=token_plan`)

### 当前模型

- `mimo-v2.5-pro` — 1M 上下文 / 128K 输出(旗舰)
- `mimo-v2.5` — 1M / 128K(便宜)

> mimo-v2-pro / mimo-v2-omni / mimo-v2-flash 等 v2 系列已于 **2026-06-30 弃用**,勿配置。

### 关键事实

1. **无官方余额 API**(踩坑 #19):只有控制台页面 → 用未文档化端点 `GET platform.xiaomimimo.com/api/v1/tokenPlan/usage`(套餐)+ `/api/v1/balance`(按量),鉴权是**账号登录 cookie** 而非 API key
2. **cookie 约 1 天过期**:过期 401 → 轮询退化保守(不标耗尽),错误码兜底不变
3. **cookie 存放**:config `quota_cookie` 或管理 API `POST /api/v1/providers/mimo/quota-cookie`(热注入),不放 key 上(账号级凭据)
4. **percent 字段是「已用比例」不是「剩余」**:实测 `used=0/limit=11B` 时 `percent=0.00`(用了 0%),判 HasQuota 必须用 `(limit-used)/limit`
5. **混层共享池**:同 vendor `mimo`(api) + `mimo-token-plan`(token_plan) 共享一个 pool,balancer 内必须按 `k.BillingSource` 分支端点(token_plan key → usage 端点,api key → balance 端点),不能在注册名上分
6. **思考模式**:
   - Chat 面:非标 `thinking={"type":"enabled|disabled"}`(extra_body)
   - Responses 面:标准 `reasoning={"effort":"none|low|medium|high"}`,none=关
7. **错误码**(实测):402 = 按量余额不足;429 = 限流 或 套餐额度耗尽(双义,body 区分信号官方未文档化);421 = 内容过滤;403 = 区域/风控;400 = 含「thinking 模式下 reasoning_content 未回传」
8. **套餐条款(用户已知悉)**:Token Plan 配额仅允许在编程工具中使用,禁止以 API 调用形式用于自动化脚本和自定义应用后端;夜间消耗 0.8x

### 定价(国内 ¥/M tokens)

- `mimo-v2.5-pro`:cache 命中 ¥0.025 / 未命中 ¥3.00 / 输出 ¥6.00
- `mimo-v2.5`:cache 命中 ¥0.02 / 未命中 ¥1.00 / 输出 ¥2.00

### 已知坑

- 套餐没开用却被查询为 0 — `percent` 字段语义反了,必须用 items.plan_total_token 重新算

---

## 4. 厂商注册方式(代码)

每个厂商包靠 `init()` 自注册到 `provider.Default()` Registry:

```go
// provider/minimax/minimax.go
func init() {
    provider.RegisterGlobalWithProtocolVendor("minimax", New, provider.ProtocolAnthropic, "minimax")
    provider.RegisterGlobalWithProtocolVendor("minimax-openai", NewOpenAI, provider.ProtocolOpenAI, "minimax")
}
```

**`RegisterGlobalWithProtocolVendor(注册名, 工厂, 协议, 厂商)` 第 4 个参数 vendor 决定**:
- `VendorFor(注册名)` 归一(key 绑定 / 白名单 / access log 都按厂商)
- 同 vendor 共享 key 池(`server.buildKeyPools` 按 vendor 复用)

**还要在 `provider/builtin/builtin.go` 加 blank import**(不要往 `cmd/gateway/main.go` 加):

```go
_ "github.com/wang546673478/native-llm-gateway/internal/provider/minimax"  // 触发 init() 注册
```

> 漏加 → Go 不编译该包 → `init()` 不跑 → `/api/v1/providers` 死活不出现它(踩坑 #10 的常见原因)
> (2026-08-20 起只剩 3 个厂商的 blank import;gemini/qwen/glm 已随包删除)

---

## 5. 共享 key 池 vs 独立池

| 厂商 | 池类型 | 原因 |
|---|---|---|
| deepseek | 共享(`deepseek` + `deepseek-anthropic`) | 同 vendor 协议面 |
| MiniMax | 共享(`minimax` + `minimax-openai`) | 同 vendor 协议面 |
| MiMo | 共享(`mimo` + `mimo-anthropic` + 各自 token_plan) | 同 vendor 协议面 + tier 互斥 |

> 共享池意味着:同一把 key 既能给 anthropic 协议面用,也能给 openai 协议面用 — key 的 `Protocols` 字段标记可用协议(空 = 全部)。

---

## 6. 余额恢复模式决策表

| 模式 | 何时用 | 行为 |
|---|---|---|
| **poll** | 厂商有官方余额 API(deepseek / MiniMax) | 标 QUOTA_EXCEEDED,quotacheck 轮询恢复;连续 2 轮读到 0 才确认耗尽(防瞬态 0 误杀,踩坑 #9 的姊妹) |
| **probe** | 厂商无官方余额 API(mimo) | 不永久标记,每次请求重探;充值即恢复(代价:每次请求先打上游,毫秒级) |

判定逻辑在 `server.buildKeyPools`(`server.go:251` `vendorHasBalancer`):

```go
if !vendorHasBalancer(vendor) {
    poolCfg.QuotaRecovery = keypool.QuotaRecoveryProbe
}
```

---

## 7. 厂商接入清单(新增时)

新增厂商的 6 步见 `docs/provider厂商定制包指南.md`。关键节点:

1. **Step 0 调研**:必须直连上游打一次耗尽场景,记录真实 HTTP status + body(踩坑 #1 #14 教训)
2. **Step 3 注册**:`RegisterGlobalWithProtocolVendor` 第 4 参数 vendor 别填错
3. **Step 4 balancer**:有官方余额端点就写(每个注册名都要注册!)
4. **config 块**:`billing_source` 与 vendor 内其他块保持一致
5. **`provider/builtin/builtin.go`**:blank import 漏一行就完蛋
6. **测试**:`registry_test.go` 断言两个注册名 + Protocol/Vendor

---

## 8. TokenMarket 中转站

> ✅ **接入时间**: 2026-08-22  
> 🎯 **接入方式**: 配置接入(无厂商包代码)  
> 📚 **完整文档**: `docs/provider-tokenmarket.md`

### 简介

**TokenMarket** (https://tokenmarket.cheap) 是基于 **New-API** 开源项目搭建的 LLM API 聚合中转站。

### 核心特点

- ✅ **OpenAI 完全兼容** — 标准 `/v1/chat/completions` / `/v1/models` 格式
- ✅ **国内直连** — 无需科学上网
- ✅ **多厂商聚合** — GPT-4o、Claude、DeepSeek、国产模型一站式
- ✅ **按量计费** — 不同模型倍率 0.05-1.5x 官方价格
- ❌ **无 Responses API** — Codex 的 `/v1/responses` 请求不会路由到 TokenMarket

### 技术实现

TokenMarket 完全复用 `openai_compatible` 基础实现,无需编写专属厂商包:

```yaml
# config.yaml
tokenmarket:
  enabled: true
  billing_source: "api"
  endpoint: "https://tokenmarket.cheap/v1"
  protocol: "openai"          # 复用 openai_compatible
  timeout: 60s
  responses_api: false        # 不支持 Responses API
```

### 使用步骤

1. **获取 API Key**: 访问 https://tokenmarket.cheap 注册并充值
2. **添加 Key**: 前端「Provider Keys」页面添加 `sk-xxxxx`
3. **同步模型**: `curl -X POST http://localhost:8080/api/v1/providers/tokenmarket/sync-models`
4. **测试**: `./scripts/test-tokenmarket.sh`

### 路由集成

TokenMarket 已自动加入路由链:
- **协议过滤**: `/v1/chat/completions` 走 OpenAI 面(包括 tokenmarket)
- **Sticky 调度**: 优先用最高优先级可用 key
- **熔断保护**: 5xx/timeout 触发 per-key 熔断,自动切换

### 监控

- **Access Logs**: 筛选 `provider=tokenmarket`
- **Overview**: 查看 tokenmarket 的 key 状态(健康/冷却/熔断)
- **Usage**: 统计 token 消耗

### 已知限制

| 限制 | 说明 |
|------|------|
| ❌ Responses API | 不支持 `/v1/responses` |
| ⚠️ 余额查询 | 无官方 balancer,需在 TokenMarket 平台管理 |
| ⚠️ 中转站风险 | 可能跑路/变更政策,建议多备份 |
| ⚠️ 首字延迟 | 中转站特性,平均 10-30 秒 |

### 成本对比

| 模型 | 官方价格 | TokenMarket 倍率 | 实际价格 |
|------|----------|------------------|----------|
| gpt-4o | $5/1M | 0.8-1.2x | $4-6/1M |
| claude-3.5-sonnet | $3/1M | 0.9-1.5x | $2.7-4.5/1M |
| deepseek-chat | $0.14/1M | 0.05-0.3x | $0.007-0.042/1M |

### 完整文档

详见 `docs/provider-tokenmarket.md` 和 `docs/TOKENMARKET-INTEGRATION-SUMMARY.md`。
