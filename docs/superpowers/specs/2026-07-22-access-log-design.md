# Access Logs — 设计文档

**日期**: 2026-07-22
**作者**: Claude
**目标版本**: Native LLM Gateway 后续 release

---

## 0. Context & 动机

Gateway 当前可观测性只有:
1. `UsageRecord` — 聚合过的用量数字,无双击查看明细能力
2. `gateway.log`(zap 文件) — 没有结构化查询 / UI 入口

Claude Code 用户接入 Gateway 出问题(连接中断、403、stream 卡死)时,管理员只能 ssh 上服务器 `tail -f gateway.log`,体验等同于黑盒。

**目标**:
- 管理员在 Web UI 能看到每一次客户端请求的元数据 + 原始 body
- 支持按 key / model / provider / 状态 / 时间窗过滤
- 默认 24 小时保留,长期不爆磁盘
- 主路径零阻塞

---

## 1. 数据模型

### 1.1 DB 表 `access_logs`

只存 metadata。Body **不入 DB**,文件路径冗余存以便详情查询。

| 字段 | 类型 | 索引 | 说明 |
|---|---|---|---|
| `id` | uint PK | pk | |
| `trace_id` | string | index | 与 `X-Request-Id` 一致 |
| `created_at` | time | index | UTC |
| `gateway_key_id` | string | index | auth middleware 写入的 key.id;空 = 未认证 |
| `gateway_key_name` | string | — | 冗余(UI 直接展示) |
| `method` | string | — | HTTP method |
| `path` | string | — | `/v1/messages` / `/v1/chat/completions` 等(**不含 query string**,防止泄漏 key/apikey) |
| `client_ip` | string | — | `c.ClientIP()` |
| `user_agent` | string | — | |
| `requested_model` | string | index | 客户端原始 model 名 |
| `final_model` | string | index | alias/fallback 后的 model |
| `provider_name` | string | index | 空 = 没路由成功 |
| `protocol` | string | — | anthropic / openai |
| `is_stream` | bool | — | |
| `status_code` | int | index | 返回给客户端的 HTTP 状态 |
| `error_type` | string | index | 见 §1.2 |
| `latency_ms` | int | — | 全链路时长 |
| `req_body_path` | string | — | e.g. `2026-07-22/abc-req.json` |
| `req_body_size` | int | — | bytes |
| `resp_body_path` | string | — | |
| `resp_body_size` | int | — | |

### 1.2 error_type 枚举

- `ok` — 成功
- `auth_failed` — token 无效 / 缺失
- `no_route` — 路由解析失败
- `model_not_allowed` — 白名单拒绝
- `key_provider_mismatch` — key 不允许该 provider
- `upstream_4xx` — 上游 4xx(除 429)
- `upstream_429` — 限流
- `upstream_5xx` — 上游 5xx
- `connection_error` — 网络层失败
- `timeout` — 上下文超时
- `unknown` — 兜底

---

## 2. 组件划分

新包 `internal/accesslog`,5 个文件,各自清晰:

```
internal/accesslog/
├── entry.go         # AccessEntry 结构 + JSON 序列化
├── recorder.go      # 中间件接口:RecordAsync(AccessEntry)
├── buffer.go        # chan + 批量 flush worker
├── store.go         # DB 读写:List/Count/Get
├── bodyfile.go      # .jsonl 文件读/写/轮转
└── retention.go     # 24h+ 清理 goroutine
```

外部注入:
- `proxy.Engine.Config` 加 `Recorder *accesslog.Recorder`
- `server.New()` 构造 Recorder 并注入
- `database.Init()` `AutoMigrate` 加上 `AccessLog` 表

---

## 3. 写入流程

### 3.1 接入点(响应式记录)

`proxy.handle(c, isStream)` 在入口创建 `AccessEntry`,`defer` 在出口 finalize:

```go
e := &accesslog.AccessEntry{
    TraceID:        traceID,
    Method:         c.Request.Method,
    Path:           c.Request.URL.Path,
    ClientIP:       c.ClientIP(),
    UserAgent:      c.Request.UserAgent(),
    RequestedModel: model,
    IsStream:       isStream,
    ReqBodyPath:    accesslog.BodyFilePath(traceID, "req"),
    LatencyStart:   time.Now(),
    GatewayKeyID:   c.GetString("gateway_key_id"),
    GatewayKeyName: authn.GetKeyName(c),
}
// BodyFile 在调用上下文中同步写请求 body(必须同步 — Body 来自中间件层)
_ = bodyFile.Write(traceID, "req", body)

// defer:
e.StatusCode = c.Writer.Status()
e.ErrorType  = classify(e.StatusCode, e.ProviderName)
e.LatencyMs = int(time.Since(e.LatencyStart) / time.Millisecond)
e.FinalModel = req.Model
e.ProviderName = ...        // 由路由结果回填
e.ProviderName := routed.ProviderName

// 响应 body 同样同步写
if !isStream {
    _ = bodyFile.Write(traceID, "resp", capturedRespBody)
} else {
    // 流式 chunk 累积在 buffer,message_stop 后 flush
    _ = bodyFile.Write(traceID, "resp", streamBuffer.Bytes())
}
e.RespBodyPath = accesslog.BodyFilePath(traceID, "resp")
e.ReqBodySize = len(body)
e.RespBodySize = capturedSize

e.Recorder.RecordAsync(e)  // 异步,零阻塞
```

**流式响应特殊处理**:
- 每个 chunk 转发时 append 到 stream-specific buffer(单独的 `bytes.Buffer`,独立于 entry chan,容量上限 **8 MB**,超过截断 — 文件名后缀变 `.truncated.json`)
- `message_stop` 后一次性写文件
- 客户端断开 / 错误 / 超时时 defer 写已累积部分(可能不完整,文件名后缀 `.truncated.json` 标记)

> 注:`streamBuffer` 与 `RecordAsync` 的 entry `chan` **不是同一个**:entry chan 满了丢整条记录,streamBuffer 满了截断 body 但仍记录 metadata。

### 3.2 RecordAsync 内部

```go
func (r *Recorder) RecordAsync(e *AccessEntry) {
    select {
    case r.buffer <- e:
    default:
        // chan 满 = 丢,zap Warn,**不阻塞主路径**
        r.logger.Warn("accesslog buffer full, dropping entry", zap.String("trace_id", e.TraceID))
    }
}
```

Worker goroutine:
- ticker 每 1s 或 batch 满 100 → flush
- 一次 `INSERT INTO access_logs VALUES (...), (...), ...` 批量写
- 写失败 → 重试 3 次 → 仍败 → zap Error

### 3.3 流式响应 body 累积上限(streamBuffer)

`streamBuffer` 是**独立于** `RecordAsync` entry chan 的 in-flight accumulator,每个 SSE 流式响应一个 buffer:

| 上限 | 值 | 行为 |
|---|---|---|
| 单条流式响应 body | 8 MB | 超过后丢弃新 chunk,文件名后缀 `.truncated.json` |
| 全局并发流式响应 | 1000 | 超出直接不开启 buffer,只记 metadata(响应 body path 标 `.truncated.json`) |

8 MB 足够绝大多数 SSE 输出(token 成本约 $0.01/MB 的 4o 输出);超过一般是客户端 bug 或滥用。

### 3.4 配置

`config.example.yaml`:

```yaml
server:
  access_log:
    enabled: true
    retention: 24h
    buffer_size: 10000
    flush_interval: 1s
    body_dir: ./data/access/
```

`false → 整体跳过`,proxy 中 `if !cfg.AccessLog.Enabled { Recorder = nil }`,中间件 no-op。

---

## 4. 查询 API

### 4.1 `GET /api/v1/access-logs`

```
?start=&end=             # RFC3339
&gateway_key=prod-a      # 按 name
&provider=minimax
&model=MiniMax-M3
&status=ok,4xx,5xx,auth_failed,no_route,model_not_allowed,key_provider_mismatch,upstream_4xx,upstream_429,upstream_5xx,connection_error,timeout,unknown
  # 逗号多选(F9 决议),允许同时筛多个分类
&limit=20&offset=0
```

返回:
```json
{
  "records": [<AccessLog>...],
  "total": 1234,
  "limit": 20,
  "offset": 0
}
```

**status 参数是面向用户的语义过滤**(预飞 F9 决议 — 扩展枚举为完整 error_type 列表)：
- `ok` = `status_code < 400`
- `4xx` = `400 <= status_code < 500,error_type=''`
- `5xx` = `status_code >= 500,error_type=''`
- 其他枚举按 `error_type` 精确过滤(见 §1.2)
- 多值用半角逗号分隔,如 `status=4xx,auth_failed`
- 其他枚举按 `error_type` 精确过滤

### 4.2 `GET /api/v1/access-logs/:id/detail`

返回 metadata + body 原文(raw string,非 base64 —— 预飞 F3 决议):

```json
{
  "metadata": { ... },
  "req_body":  "{\"model\":\"...\"}",
  "resp_body": "{\"content\":[...]}",
  "req_body_trunc":  false,
  "resp_body_trunc": false
}
```

如果 body 文件被 retention 清理掉了,字段为 `null` / `""`,前端显示 "已过期"。trunc 字段由文件名后缀 `.truncated.json` 推断(§1.1 决议:不存 DB 列)。

### 4.3 `GET /api/v1/access-logs/stats`

```json
{ "total_24h": 1234, "errors_24h": 56, "active_keys": 5 }
```

---

## 5. UI — AccessLogs.vue

```
┌──────────────────────────────────────────────────────────┐
│ Access Logs (24h)  [1234 总 / 56 错 / 5 活跃key]  [刷新] │
├──────────────────────────────────────────────────────────┤
│ [时间窗] [Key ▼] [Provider ▼] [Model ▼] [状态 ▼] [查询] │
├──────────────────────────────────────────────────────────┤
│ Time      Status  Key       Model       Provider  Latency │
│ 12:01:23  ✓200    prod-a    MiniMax-M3   minimax    123ms │
│ 12:00:58  ✗403    prod-a    bad-model                 2ms  │
│ 12:00:14  ✓200    dev       claude-so..  minimax    204ms │
│ ...                                                          │
├──────────────────────────────────────────────────────────┤
│            < 1 2 3 ... 51 >      [20条/页 ▼]              │
└──────────────────────────────────────────────────────────┘
点击行 → 右抽屉:
┌────────────────────────────┐
│ traceID: abc-123  [关闭 X] │
│ 入站时间: 12:01:23  123ms  │
├────────────────────────────┤
│ ▼ 请求头                    │
│   authorization: gw-...    │
│   anthropic-version: 2023..│
│ ▼ 请求体                    │
│   {"model": "claude-...",   │
│    "messages": [...]}      │
│ ▼ 响应 (200, 204ms)         │
│   {"content": [{"text":..}]│
└────────────────────────────┘
```

- 复制按钮:每个 body 段有一键复制
- 状态用色块:✓200 = 绿 / ✗4xx = 橙 / ✗5xx = 红 / 403 = 红
- 分页复用 Usage.vue 模式(Naive UI 的 `<n-data-table :pagination>`)
- 路由:`/access-logs`

---

## 6. 清理与保留

### 6.1 Retention worker

`retention.go` 启动一个 goroutine,每 5 分钟跑一次:

```sql
SELECT id, trace_id FROM access_logs
WHERE created_at < datetime('now', '-24 hours')
LIMIT 1000;
```

对每行:
- 构造 `body_dir/{YYYY-MM-DD}/{trace_id}-req.json` 路径
- `os.Remove()` 删文件(找不到也跳过)
- DELETE FROM access_logs WHERE id IN (...)

**单轮限制 1000 条**避免 lock。

### 6.2 性能预算

| 维度 | 上限 | 设计保证 |
|---|---|---|
| 主路径阻塞 | 0 ms | chan + 满则丢 |
| Body 写盘 | 异步 worker | buffer 批量 |
| DB 写入 | 100/s 可承受 | body 不进 DB + batch insert |
| 24h 磁盘 | ~4 GB @ 5req/s × 100KB | retention 24h |
| 内存 | < 100 MB | chan cap 10000 |

### 6.3 错误兜底

- `RecordAsync` chan 满 → zap Warn,**永不阻塞**
- Body 文件写失败 → entry 仍入 DB,path 空字符串(UI 显示"不可用")
- DB 写失败 → 3 次重试 → 仍败 → zap Error(运维可查)
- retention 失败 → zap Error,下个 tick 重试

---

## 7. 与现有模块的关系

| 现有模块 | 影响 | 说明 |
|---|---|---|
| `proxy.Engine` | 加 Recv 字段 | no-op if nil |
| `usage.UsageRecord` | 不改 | 仅 metadata 维度重合;Detail 看 access_log |
| `UsageRecord.TraceID` | 复用 | access_log 也带 trace_id,可以 join |
| `database/models.go` | +AccessLog | AutoMigrate 自动建表 |
| `server.New()` | 初始化 Recorder | lifecyc.go.Close() 里调 Recorder.Close |
| `router` | 不改 | |
| `auth` | 不改 | |
| `Usage.vue` | 不改 | |

新增迁移 +1 个新 GORM 表 `access_logs`,其他零修改。

---

## 8. 不在本次设计范围(YAGNI)

- 实时 SSE 推送刷新(用户没要求)
- 跨实例集中日志(SQLite 单实例够用)
- 手动 cleanup 按钮(retention 自动)
- 按 token / cost 过滤(有 UsageRecord 已经覆盖)
- 详情对比(diff 两请求)
- 导出 / 下载(access_logs 已经在文件系统)

---

## 9. 实施拆解(为 writing-plans 准备)

1. **migration**:`database/models.go` 加 `AccessLog` struct
2. **新包 `internal/accesslog/`**:6 个文件全部建好,带单元测试
3. **`server.New()` 注入**:Recorder 实例化 + 关闭时 Close
4. **`proxy.handle` 接入**:defer + Entry 填写 + 调用 RecordAsync
5. **路由**:`api/http/handler/admin.go` +3 个 endpoint
6. **`config.example.yaml`**:加 access_log 段
7. **前端 `AccessLogs.vue`**:页 + 抽屉
8. **接入 `App.vue` 菜单**:n-menu 加一项
9. **E2E 验证**:本地 curl + 浏览器复现

---

## 10. 关键风险与缓解

| 风险 | 缓解 |
|---|---|
| 高并发时 chan 满丢日志 | 监控 + 调大 cap + 加 metric |
| 大量 SSE chunk 撑爆内存 | 流式累积有上限(单独 chan),超过截断写 |
| retention 阻塞主路径 | worker 异步 + 单轮 1000 上限 |
| SQLite INSERT 锁 | batch insert + 事务 |
| body 文件权限泄露 | 仅 `data/access/` 目录,应用进程读写 |

---

## 11. 成功标准

- Claude Code 接入 Gateway 出问题时,管理员 30s 内能在 UI 看到该请求的完整 body、错误类型、路由 trace
- 24 小时后数据自动清理,磁盘不增长
- 主路径 P99 延迟增加 < 5 ms(只多一次 `select { case ... default: }`)
- 前端新页面能翻页、过滤、查看 body 详情
