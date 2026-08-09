# 横切关注点:accesslog / metrics / circuit

---

## 1. accesslog(接入日志)

### 1.1 设计目标

让管理员能排查请求链路:**谁打哪个 provider、用哪把 key、延迟多少、token 多少、响应是什么**。

### 1.2 模块结构

```
backend/internal/accesslog/
├── entry.go       — AccessEntry 结构(每条记录的字段)
├── recorder.go    — Recorder 门面(异步 buffer + 周期 flush)
├── buffer.go      — 内存 channel buffer
├── store.go       — DB 读写 + 现查 name(fillKeyNames / fillGatewayKeyNames)
├── bodyfile.go    — body 写文件(SQLite 单行过大,body 落 jsonl 文件)
└── retention.go   — 30 天保留清理
```

### 1.3 关键字段

```go
type AccessEntry struct {
    ID            uint
    TraceID       string    // 与 X-Request-Id 一致
    CreatedAt     time.Time
    GatewayKeyID  string    // 落库的稳定身份
    ProviderName  string
    ProviderKeyID string    // 落库,实际发请求的上游 key ID
    Method, Path  string
    ClientIP, UserAgent string
    RequestedModel string   // 客户端发的模型名(标签)
    FinalModel    string    // 路由后实际模型
    Protocol      string
    IsStream      bool
    StatusCode    int
    ErrorType     string
    LatencyMs     int
    ReqBodyPath, RespBodyPath string  // 相对 body_dir
    ReqBodySize, RespBodySize int
}
```

### 1.4 关键设计

#### 1.4.1 ProviderKeyName / GatewayKeyName 不落库

- DB 只存 ID(数字)
- 列表/详情查询时按 ID **现查** `provider_api_keys.name` / `gateway_keys.name` 填充展示
- 改名字后历史记录**同步显示新名字**(用户改名 → 历史记录名字跟着变,不需要迁移)
- 删 key → 查不到 → 保持空(前端回退显示 ID)

理由:名字是「当前身份」,快照旧名字会带来一致性负担。

#### 1.4.2 ProviderKeyID = 失败的最后尝试

- 成功 = 最终成功的 key
- 失败 = 最后尝试的 key(排查「切到哪把 key」用)

#### 1.4.3 Body 落文件不落 DB

- DB 只存 metadata
- body 走 `.jsonl` 滚动(每个请求 1 个文件,16MB 上限)
- `body_path` 存**相对路径**(避免重启后失效位置)

### 1.5 30 天保留

`retention.go`:`Start(ctx)` 启动后台 goroutine,定期清理 30 天前的 entry + 关联 body 文件。

### 1.6 详情页 token 用量行(踩坑 #17)

- 非流式:响应 body 是完整 JSON,前端 `JSON.parse` 后读 `usage` 字段 ✓
- 流式:响应 body 是 SSE 拼接文本,parse 失败 → 什么都不显示
- 流式要看 **用量页**(`/api/v1/usage`),那里从内存解析的 usage_records 落库

---

## 2. metrics(Prometheus 指标)

### 2.1 暴露端点

`GET /metrics` — Prometheus 格式(`promhttp.Handler`)

### 2.2 指标清单

| 指标 | 类型 | Labels | 含义 |
|---|---|---|---|
| `gateway_requests_total` | Counter | `provider, status, is_stream, error_type` | 总请求数 |
| `gateway_tokens_total` | Counter | `provider, type` | token 数(type=input/output) |
| `gateway_request_duration_seconds` | Histogram | `provider, is_stream` | 请求延迟 |
| `gateway_quota_probe_total` | Counter | `provider, result` | 探测配额(由 quotacheck emit) |
| `gateway_quota_poll_total` | Counter | `provider, result` | 轮询配额 |
| `gateway_quota_key_status_transitions_total` | Counter | `provider, from, to` | key 状态转移(如 ACTIVE → QUOTA_EXCEEDED) |
| `gateway_quota_pending_probes` | Gauge | — | 当前待探测 key 数 |

### 2.3 Adapter 模式

`metrics.NewAdapter(metricsC)` 提供给 proxy 调用,Adapter 不感知具体指标名,只暴露 `recordUsage` / `recordMetrics` 方法。**测试时可以注入 mock adapter**。

---

## 3. circuit(熔断器)

### 3.1 状态机

```
CLOSED ──(失败计数 ≥ 阈值)──> OPEN
   ↑                              │
   │                              │ (OpenTimeout 后)
   │                              ↓
   └──(试探全成功)── HALF_OPEN ──(任一失败)──→ OPEN
```

| 状态 | 行为 |
|---|---|
| **CLOSED** | 正常处理请求 |
| **OPEN** | 直接拒绝请求(Acquire 跳过) |
| **HALF_OPEN** | 放行最多 N 个试探请求;全部成功 → CLOSED;任一失败 → OPEN |

### 3.2 per-key(2026-08-06 起,踩坑 #16)

- **熔断器挂到 keypool 而不是 provider**(`pool.go:88` `breakerFor`)
- 5xx / timeout / connection 触发 `RecordFailure`
- 429(rate_limit) / quota / auth / invalid_request **不计入**

### 3.3 配置(`config.circuit_breaker`)

```yaml
providers:
  minimax:
    circuit_breaker:
      failure_threshold: 5      # 5 个失败
      failure_window: 60s       # 60s 窗口内
      open_timeout: 60s         # OPEN 持续 60s
      half_open_requests: 2     # 试探 2 个
      countable_errors: [...]   # 计入熔断的错误类型
      excluded_errors: [...]    # 排除
```

`failure_threshold <= 0` = 不启用(测试场景)。

### 3.4 转移日志

`pool.go:88` `breaker.SetLogger`:熔断状态 CLOSED → OPEN → HALF_OPEN 全程打印,排查「某把 key 怎么被熔断的」用。

### 3.5 为什么不能 provider 级?

- 旧版(`provider-level`)一把 key 5xx 连坐整 provider,healthy key 一起被跳过
- 2026-08-06 实测:weige 出问题,key-1 一起被跳过,全链掉 deepseek
- 修复后:每把 key 独立熔断,同 provider 其他 key 照常参与调度

---

## 4. 三者的协作

```
proxy.attemptOne
   ↓
[记录开始时间]
   ↓
provider.SendRequest
   ↓
[成功] → recordUsageWithTokens + recordMetrics
   ↓
[失败] → ReportError(key, errType)
        ↓
        pool.ReportError
        ├─ auth → COOLING 5min
        ├─ invalid_request → 只计数
        ├─ quota_exceeded → 标 QUOTA_EXCEEDED + callback
        └─ server_error/timeout/connection → breaker.RecordFailure
   ↓
[写 access log] async via Recorder
   ↓
   ├─ Buffer → 周期 flush → DB
   ├─ body 写文件
   └─ 30 天保留清理
```

**关键不变量**:
- melt 记录失败次数(per-key),超出阈值 → OPEN → 跳过这把 key
- access log 失败信息(ProviderKeyID = 最后尝试)反映真实失败链路
- metrics 标签粒度 = provider,key 维度不进 metrics label(cardinality 爆炸)
