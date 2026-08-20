# 实时活跃请求(in-flight)设计

> 日期:2026-08-20
> 状态:已确认(待实现计划)
> 目标:提供一个**只读、实时**的「正在跑的对话」视图 —— 谁在打、什么 model、走哪家 provider、哪个 gateway key、流式/非流式、已跑多久。

---

## 1. 背景与动机

Gateway 当前对每条请求的生命周期是「结尾才留痕」:

- 请求进来 → `handle()` 建内存 `AccessEntry`(不落库)
- 处理(可能流式跑很久)→ 结束 → `defer` 收尾 → `RecordAsync(entry)` 异步落库

因此「此刻正在跑什么」没有任何可观测窗口。用户想要一个**进行时**视图,区别于现有的 AccessLogs 页(完成时)。

## 2. 目标 / 非目标

**目标**

- 实时列出「正在执行」的请求:谁、什么 model、哪家 provider、哪个 key、流式与否、已耗时。
- 纯内存态,**请求结束即从列表消失**,不保留任何历史。

**非目标(明确不做)**

- ❌ 不实时更新 output token —— 沿用「已耗时」维度,不改流循环(二期再议,届时在 `doStream` 循环里旁路刷 token)。
- ❌ 不做对话内容旁路(「看正在生成的字」一类)。
- ❌ 不做 Redis —— 只把快照封装成窄接口,未来替换为 Redis 实现即可(多实例场景)。
- ❌ 不动 access log / usage 的任何字段与落库时序。

## 3. 语义定案(与用户确认)

- inflight = **现在进行时**,只活在内存;**结束即消,不留历史、不做灰显缓存**。
- 正常结束 → `defer` 里 `Delete` → 从列表消失(事后去 AccessLogs 页查)。
- 进程崩溃/重启 → 内存 map 归零 → 列表清空,无僵尸、无残留存档。
- AccessLogs(现有)= 完成时;inflight(新)= 进行时。两页职责互补,不重叠。

## 4. 架构

### 4.1 新包 `internal/inflight`

只做一件事:维护 `trace_id → 请求状态` 的并发安全内存 map。

```go
package inflight

// Snapshot 一条活跃请求的只读快照
type Snapshot struct {
    TraceID        string
    StartedAt      time.Time // 请求开始(handler 返回时现算 elapsed_ms = now - StartedAt)
    Model          string    // alias 解析后的真实 model
    ProviderName   string    // 当前正在打的 vendor,随 failover 实时更新
    GatewayKeyName string
    IsStream       bool
}

// Registry 并发安全的内存快照表(窄接口,未来可替换为 Redis 实现)
type Registry struct {
    mu sync.RWMutex
    m  map[string]*Snapshot
}

func (r *Registry) Put(s *Snapshot)                       // 请求开始
func (r *Registry) SetProvider(traceID, provider string)  // failover 途中 provider 变化
func (r *Registry) Delete(traceID string)                 // 请求结束(defer 收尾)
func (r *Registry) Snapshot() []*Snapshot                 // 只读列表,按 StartedAt 有序
```

**为什么是窄接口**:`Registry` 只暴露 4 个方法。未来把 `m map` 换成 `redis.Client`,proxy 一行不改。符合低耦合第一原则,给「多实例 Redis」留干净替换点。

### 4.2 proxy 层:3 个旁路插入点(不改现有逻辑)

`Registry` 通过 `proxy.Config.Inflight *inflight.Registry` 注入(与 `AccessLog` / `FingerprintSanitizer` 同构),nil = 不启用。

| 位置 | 动作 | 对现有逻辑 |
|------|------|-----------|
| `handle()` body/alias 解析完成、路由之前 | `inflight.Put(...)`(此时 model / is_stream 已确定,provider 留空待 SetProvider) | 新增一行,不改 |
| `attemptOne` 已有 `*outProviderName = result.ProviderName` 处 | `inflight.SetProvider(req.TraceID, result.ProviderName)` 紧贴该赋值 | 新增一行,不改 |
| `handle()` defer 里 `RecordAsync(entry)` 旁边 | `inflight.Delete(traceID)` | 新增一行,不改 |

**关键:SetProvider 读取的是现有变量 `result.ProviderName`** —— 展示的「当前 provider」与网关实际正在打的**是同一个值**,failover 切换时列表实时跟着变(deepseek 挂 → 立刻显示转到 minimax)。

**Put 时机选择**:放在 body 解析完成、路由之前,而非入口建 entry 时 —— 这样 model(alias 解析后)和 is_stream(body `stream` 字段)都已确定,避免额外的 `SetModel`/`SetIsStream` 方法,接口更收敛。

### 4.3 API:只读端点

`GET /api/v1/inflight` → 200:

```json
{
  "requests": [
    {
      "trace_id": "...",
      "started_at": "RFC3339",
      "model": "...",
      "provider_name": "...",
      "gateway_key_name": "...",
      "is_stream": true,
      "elapsed_ms": 12345
    }
  ]
}
```

- `elapsed_ms` 由 handler 返回时 `now - StartedAt` 现算(不存,快照只存 `StartedAt`)。
- handler 通过**回调注入**拿 `Registry.Snapshot()`,与 `FingerprintGet` 同模式(server 顶层传闭包),handler 不 import inflight 包。

### 4.4 前端:独立 `/inflight` 页面

- 新路由 `/inflight` + 侧边栏入口(选择独立页,因为活跃请求是可变列表,塞进 Overview 会让总览页臃肿)。
- 表头:Trace / Model / Provider / Gateway Key / 流式 / 已耗时。
- 轮询间隔 **1s**(`setInterval` + `onUnmounted` 清理)。空列表显示「当前无活跃请求」。
- 不缓存历史、不做灰显 —— 与第 3 节语义一致。

## 5. 并发 / 清理 / 泄漏

- `Registry` 用 `sync.RWMutex`,读多写少;`SetProvider` 只在 provider 变时写一次,不每 chunk 写(热路径成本纳秒级)。
- **防泄漏**:`Delete` 挂在 `handle()` 的 defer(与 `RecordAsync` 同个 defer),成功/失败/panic 必经。唯一例外是进程崩溃 → 内存态自然清空,无僵尸。
- 预留一个「`StartedAt` 超时惰性清理」兜底(默认不启用),先靠 defer 保证配对。

## 6. 边界与后续

- 实时 output token(需碰 `doStream` 循环)→ 二期,旁路刷 `SetTokens`。
- Redis 多实例 → 替换 `Registry` 实现即可。
