# 核心子系统:quotacheck / auth / usage

---

## 1. quotacheck(配额恢复管理器)

### 1.1 职责

管理两条路径:
- **probe** — 主动查询(失败响应后由 proxy 调 `CheckQuota`)
- **poll** — 周期轮询(有余额接口的厂商)

### 1.2 模块结构

```
backend/internal/quotacheck/
├── manager.go    — 主协调:Start / pollAllBalancers / injectCallbacks
├── prober.go     — 单次 probe(同步)
├── default_prober.go
├── scheduler.go  — 探测调度(backoff + jitter)
└── balancer.go   — 全局 balancer Registry(quotacheck.RegisterBalancer)
```

### 1.3 两条路径

#### poll(周期轮询)

```
pollAllBalancers (poll interval)
   ↓
按 vendor pool 去重(同一 vendor 共享 pool)
   ↓
按 tier 桶:token_plan → api → free
   ↓
对每把 key:
   ├─ balancer.FetchBalance(ctx, baseURL, key)
   ├─ 更新 k.Remaining / k.LastPolledAt / k.QuotaKind
   ├─ 连续 2 轮读到 0 → ReportQuotaExceeded(标 QE)
   └─ 已是 QE 且 had quota → RestoreQuota(回 ACTIVE)
```

#### probe(主动探测)

```
proxy 在网络类/限流错误后调 CheckQuota(name, endpoint, key)
   ↓
balancer.FetchBalance(同 poll)
   ↓
返回 (HasQuota bool, error)
   ↓
proxy 据此决定是否降档
```

### 1.4 关键配置

```yaml
keypool:
  quota_enabled: true
  quota_probe_initial_delay: 10s
  quota_probe_max_backoff: 5m
  quota_probe_jitter_pct: 20
  quota_poll_interval: 30s
  quota_poll_jitter_pct: 20
  quota_http_timeout: 10s
  quota_user_agent: "NativeLLMGateway/1.0"
  quota_warn_threshold_pct: 20
```

### 1.5 状态机

```
ACTIVE ──(错误 / 探到 0)──> QUOTA_EXCEEDED
   ↑                              │
   │                              │ (RestoreQuota)
   └──────(余额恢复)──────────────┘
```

> **关键设计**:无终端禁用状态(踩坑 #2、#16)。所有失败映射到**可恢复状态**(COOLING / QUOTA_EXCEEDED),由冷却到期或 balancer poll 自动恢复。

### 1.6 连续 2 轮确认(踩坑 #9 教训)

MiniMax 余额 API 瞬态返回 0 → 旧逻辑直接标 QE → key 整晚趴着。修复:

```go
// P-quota-poll-guard: 连续 >=2 轮才确认耗尽标 QE
if k.QuotaZeroStreak >= 2 {
    k.Remaining = bal.Raw
    pool.ReportQuotaExceeded(k)
}
```

---

## 2. auth(客户端鉴权)

### 2.1 凭证的两层

```
┌─────────────────────────────────────────┐
│ 客户端                                    │
│  - 持有 gw-xxx(Gateway Key)              │
│  - 配 ANTHROPIC_AUTH_TOKEN=gw-xxx         │
└─────────────────────────────────────────┘
              ↓ X-Api-Key: gw-xxx
┌─────────────────────────────────────────┐
│ Gateway:auth 中间件                       │
│  - 哈希 key → 查 gateway_keys 表         │
│  - 通过 → 装入 c.Get("gateway_key")      │
│  - 失败 → 401                            │
└─────────────────────────────────────────┘
```

### 2.2 `auth.GatewayKey` 字段

```go
type GatewayKey struct {
    ID            uint
    Name          string
    KeyHash       string          // 哈希存
    Providers     []string        // 绑定的厂商(空 = 不限)
    ProviderKeyIDs []uint         // 绑定的 ProviderAPIKey.id(空 = 不限,P34)
    AllowedModels []string        // 白名单(["*"] = 不限)
    RPM, TPM      int             // 限流
}
```

### 2.3 关键设计

#### 2.3.1 Provider 绑定按厂商归一(踩坑 #12)

绑 `deepseek` 应该让 `deepseek-anthropic` 也通过:

- `CheckProvider` 内部对两边都 `VendorFor` 归一
- 创建/更新时存储归一为厂商名,**旧数据绑注册名也兼容**(无需迁移)

#### 2.3.2 ProviderKeyIDs 锁定凭证(P34)

- 空 = 不限制(用该 provider 的所有 key 池挑)
- 非空 = 只用 ID 在这个集合里的 Provider Key(精准锁定)
- 实现:`pool.AcquireFromIDs(allowedIDs, proto)` 内部按 ID 过滤

#### 2.3.3 白名单逐候选校验(踩坑 #3)

- 旧逻辑:客户端原始模型名校验 → 客户端发假名 → 403
- 新逻辑:`CheckAllowed` 改在路由后真实模型名校验
- 再升级:白名单参与候选模型选择(声明过的模型优先)

### 2.4 限流(RateLimit)

- `RateLimitMiddleware` 按 `RPM` / `TPM` 计数
- 配额扣减点:proxy.attemptOne 成功后调 `Authenticator.RecordUsage(tokenCount)`
- 超额 → 429

### 2.5 管理 API

| 路径 | 说明 |
|---|---|
| `GET /api/v1/keys` | 列表 |
| `POST /api/v1/keys` | 创建(响应含明文,**只展示一次**) |
| `PATCH /api/v1/keys/:id` | 改白名单 / 限流 |
| `DELETE /api/v1/keys/:id` | 删除 |
| `GET /api/v1/providers/:name/api-keys` | 厂商 key 池列表 |
| `POST /api/v1/providers/:name/api-keys` | 加厂商 key |
| `POST /api/v1/providers/mimo/quota-cookie` | MiMo 控制台 cookie(热注入) |

> CRUD 端点本身**不要求 auth.enabled** — 这样即使没启用 auth 也能管理 keys

---

## 3. usage(用量收集)

### 3.1 职责

异步收集每次请求的 token 用量 + cost + 延迟,批量落库。

### 3.2 模块

```
backend/internal/usage/
├── collector.go   — 异步 collector + 批量落库
├── repository.go  — DB 读写
└── adapter.go     — Adapter(注入到 proxy)
```

### 3.3 字段

```go
type Record struct {
    TraceID        string
    GatewayKeyID   string
    ProviderName   string
    ModelID        string
    Protocol       string
    BillingSource  string  // token_plan / api / free
    InputTokens    int
    OutputTokens   int
    TotalTokens    int
    Cost           float64
    LatencyMs      int64
    IsStream       bool
}
```

### 3.4 异步流

```
proxy.attemptOne 成功
   ↓
recordUsageWithTokens → usage.NewAdapter(usageC).Record(record)
   ↓
[cached_channel] → usageC.Start 协程
   ↓
[batch_size 累积] → 周期 flush → DB
```

### 3.5 Cost 计算

proxy.attemptOne 成功 → 从 `provider.ModelCost` 读取单 key 价格:

```go
cost = (inputTokens * CostPer1kInput + cacheReadTokens * CostPer1kCacheRead + cacheCreationTokens * CostPer1kCacheCreation + outputTokens * CostPer1kOutput) / 1000
```

### 3.6 启动 + 关闭

```go
// server.go
s.usageC.Start(ctx)              // 启动收集协程
// on shutdown
s.usageC.Stop()                  // flush 剩余,然后退出
```

### 3.7 页面

- `/api/v1/usage` — 时间窗口统计
- `/api/v1/usage/by_model/:model_id/providers` — 单 model 在各 provider 的用量对比
- 流式请求的 token 数**必须**看这里(access log 详情页不显示,踩坑 #17)

---

## 4. 三者的协作

```
                       ┌─ auth ────〉
                       │             proxy.handle
   Client gw-xxx ──── Gin ─────────── proxy.attemptOne
                       │             │
                       │             ├─ recordUsageWithTokens ── usage (async)
                       │             ├─ recordMetrics ── metrics
                       │             ├─ recordAccessLog ── accesslog (async)
                       │             └─ ReportError ── pool
                       │                              │
                       │                              ├─ breaker.RecordFailure
                       │                              └─ quota.CheckQuota
                       │                                             │
                       │                                             └─ quotacheck
                       └─ rateLimit (RPM/TPM) ── auth
```

启动顺序见 `docs/ARCHITECTURE.md` §6。
