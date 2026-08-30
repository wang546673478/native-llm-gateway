# 横切机制

本文说明贯穿请求主链的 trace、活跃请求、接入日志、Prometheus 指标、per-key Circuit Breaker 和设备指纹归一化。

## 1. Trace ID

Proxy 优先接受客户端的 `X-Request-Id`；没有时生成 UUID。该值会：

- 写回客户端响应头。
- 覆盖发往上游请求的 `X-Request-Id`。
- 作为 Usage、AccessLog 和 Inflight 的关联键。
- 作为 access body 文件名的一部分。

一次请求内的所有候选尝试共用同一个 trace ID，因此一个 trace 可能对应多条 access-log metadata：每个失败候选一条，最终结果再一条。

当前没有 OpenTelemetry span 或跨进程 trace store。`X-Request-Id` 是字符串关联键，不表达父子 span、候选顺序或多实例拓扑。

当前安全边界：客户端提供的 `X-Request-Id` 没有长度和字符归一化；`BodyFileWriter.Write` 又直接将它拼入相对文件名，写入路径没有执行与读取路径相同的 containment 校验。启用 body 采集时，不应把该头视为可信文件名。读取接口本身会拒绝绝对路径和越界路径。

## 2. Inflight 活跃请求

`inflight.Registry` 是进程内、并发安全的 `trace_id -> Snapshot` map。Proxy 在请求 body、模型和 Gateway Key 已解析后写入，在统一 defer 中删除，因此成功、失败和 panic 恢复路径最终都会清理。

快照字段包括：

- trace ID、开始时间和实时计算的 elapsed milliseconds。
- 客户端原始模型和当前候选最终模型。
- Provider 注册名。
- Gateway Key 名称。
- 是否流式。

`GET /api/v1/inflight` 返回按开始时间升序的值拷贝。数据只存在于本进程，请求结束立即消失；重启、多实例和历史查询都不共享。

当前更新时机有一个限制：`FinalModel` 在尝试候选前更新，但 `ProviderName` 是一次 `attemptOne` 返回后才写入。第一次上游请求执行期间 Provider 可能仍为空；failover 的下一次请求执行期间也可能暂时显示上一个已失败 Provider。因此该字段不是严格实时的“当前 socket 正在连接谁”。

## 3. AccessLog

AccessLog 将 metadata 和 body 分开保存：

```text
请求/响应 body -> 同步写文件
metadata       -> 非阻塞内存 channel -> 批量写 access_logs
```

`server.access_log.enabled=false` 时 Recorder 是 no-op。列表、详情和导出接口返回 `access_log_disabled`，统计接口返回零值。

### 3.1 Metadata

一条最终 metadata 包含：

- trace、UTC 创建时间、method、path、client IP 和 user agent。
- Gateway Key ID。
- requested/final model、vendor、Provider Key ID 和协议面。
- stream、HTTP status、error type、latency。
- request/response body 的相对路径和大小。

Gateway Key 名称和 Provider Key 名称不落在 access row；查询时按 ID 读取当前名称。因此 Key 改名会同步改变历史日志的展示名称，删除 Key 后名称可能为空。

Provider 名在 AccessLog 中按 vendor 归一，协议注册面另存在 `protocol` 字段。每次 `attemptOne` 失败都会克隆当前 entry 并异步写一条失败记录；最终 defer 再写最终成功或最后失败记录。失败克隆不附响应 body。

流式 HTTP headers 一旦提交，状态码通常保持 200。中途失败依靠 `error_type` 表达，不能只用 `status_code >= 400` 判断流式错误。

### 3.2 Body 文件

文件按 UTC 日期分目录，每个请求最多有两个文件：

```text
YYYY-MM-DD/<trace-id>-req.json
YYYY-MM-DD/<trace-id>-resp.json
```

数据库保存相对于 `body_dir` 的路径，不保存 body 本身。请求 body 在 alias、候选模型和 fingerprint 改写前写入；`/responses` 的 reasoning 清理已经发生。非流式响应保存上游返回 body。

单文件上限为 16 MiB：

- 普通同步写入超过上限时截断为 16 MiB，并使用 `.truncated.json` 后缀。
- 流式响应先在内存中累计，最多 16 MiB；全进程最多同时为 1000 个流保留累计槽。超过并发槽上限时只写 metadata，不保存流响应 body。

当前流式截断标记有实现缺口：累加器在超过 16 MiB 时只保留前 16 MiB，但 `finalizeStream` 没有把内部 `truncated` 标记传给文件写入器；文件写入器看到的长度恰好等于上限，可能仍使用普通 `.json` 后缀。详情和导出的 `resp_body_trunc` 因而可能对超限流返回 false。

Body 写文件与转发主路径同步，但失败只记 warning，不阻止代理响应。文件权限当前为 `0644`，body 可能包含 prompt、工具参数和其他敏感内容，部署时必须限制 `body_dir` 及宿主机访问权限。

### 3.3 Metadata buffer

Metadata buffer 默认容量 10000、batch 100、flush interval 1 秒，配置为零时使用这些兜底值。

- `Push` 永不阻塞；channel 满或 Recorder 已关闭时丢整条 metadata 并写 warning。
- 批量 INSERT 最多重试 3 次，并从已成功的偏移继续，避免重试导致重复行。
- 正常关停先排空 HTTP，再关闭 buffer；Close 会 drain 已接收的全部 entry。
- 排空超时后仍结束的请求可能在 Recorder 关闭后被丢弃。

### 3.4 Retention 和导出

Retention 默认 24 小时。worker 启动后立即执行一次，之后每 5 分钟执行；每轮最多处理 1000 条过期记录，同时删除对应 body 文件和这批数据库行。

磁盘存储不是滚动 `.jsonl`。`GET /api/v1/access-logs/export` 才会按过滤条件即时生成 NDJSON，默认最多 10000 条、上限 50000 条，并尽量嵌入仍存在的 request/response body。

## 4. Prometheus Metrics

Collector 使用独立 registry。`GET /metrics` 当前始终注册在主 HTTP 端口和固定路径；`metrics.enabled`、`metrics.path`、`metrics.port` 没有运行时消费者。

实际指标及 labels：

| 指标 | 类型 | Labels |
| --- | --- | --- |
| `gateway_requests_total` | Counter | `provider`, `status`, `is_stream`, `error_type` |
| `gateway_tokens_total` | Counter | `provider`, `type` (`input`/`output`) |
| `gateway_request_duration_seconds` | Histogram | `provider`, `is_stream` |
| `gateway_quota_probe_total` | Counter | `provider`, `result` |
| `gateway_quota_poll_total` | Counter | `provider`, `result` |
| `gateway_quota_key_status_transitions_total` | Counter | `provider`, `from`, `to` |
| `gateway_quota_pending_probes` | Gauge | 无 |

请求计数和 latency 在每次实际 Provider 尝试后记录，不是每个客户端请求只记一次。一个发生 failover 的 trace 会增加多个 request samples。Token 只在最终成功且上游提供 usage 时记录。

常见 `result` 值：

- probe：`restored`、`still_exhausted`、`auth_failed`、`transport_error`。
- poll：`ok`、`exhausted`、`restored`、`transport_error`。

当前没有 Gateway Key、具体 Provider Key、队列丢弃数、AccessLog 写入失败、Circuit 状态和 Inflight 数量的 Prometheus 指标。

## 5. Per-key Circuit Breaker

Circuit Breaker 位于 Key Pool 内，每把上游 Key 独立持有；一把 Key OPEN 不会连带同 vendor 的其他 Key。只有 Provider 配置的 `failure_threshold > 0` 时才创建 Breaker。

状态机为：

```text
CLOSED --窗口内失败达到阈值--> OPEN
OPEN   --open_timeout 到期且再次 Acquire--> HALF_OPEN
HALF_OPEN --试探全部成功--> CLOSED
HALF_OPEN --任一计数错误--> OPEN
```

Pool 在 Acquire 时调用 `Allow`，OPEN Key 被过滤。成功调用 `RecordSuccess`；`server_error`、`timeout`、`connection` 调用 `RecordFailure`。429、quota、auth 和 invalid request 不计入熔断。

状态转换写 zap info 日志；Provider Key 列表 API通过 Pool 快照返回 `circuit_open` 和 `circuit_state`。当前没有单独的 circuit metrics 或持久化；重建 Pool/重启后 Breaker 回到 CLOSED。

配置限制：`circuit.New` 虽然根据 `countable_errors` 和 `excluded_errors` 构造了局部 map，但没有把它们保存到 Breaker；`shouldCount` 始终读取包级默认 map。因此这两个列表当前不生效，实际计数集合固定为 `server_error`、`timeout`、`connection`，固定排除 `rate_limit`。

## 6. 设备指纹归一化

Fingerprint sanitizer 在请求 body 已解析 alias、但尚未构建上游 Provider 请求时执行。默认开关语义是：

- `fingerprint.enabled` 未配置时开启。
- 显式 `false` 时关闭。
- `canonical_device_id` 非空时使用配置值；为空时启动时生成 64 字符随机 hex，只在本进程内保持稳定，不落盘。

启动时还采集一次 Gateway 环境快照：`runtime.GOOS`、`$SHELL`（空时 `bash`）和 `uname -r`（失败时回退 GOOS）。

Sanitize 只定位以下形态：

1. `metadata.user_id` 必须是一个 JSON 字符串；其内部对象存在 `device_id` 时，替换为 canonical device ID。
2. `system` 必须是数组；每个对象的 `text` 包含 `# Environment` 时，将以 ` - Platform:`、` - Shell:`、` - OS Version:` 开头的行替换为 Gateway 快照值。

它不主动改写 messages、tools、thinking 或 primary working directory。非法 JSON 原样返回。

当前字节级行为需要注意：对任何合法的顶层 JSON object，即使没有命中指纹字段，函数仍会 `json.Marshal` 后返回，所以空白、对象 key 顺序和转义形式可能变化；“无目标字段”只保证语义不变，不保证 body 原始字节不变。

管理 API：

- `GET /api/v1/fingerprint` 返回运行时 enabled 和 canonical ID。
- `PUT /api/v1/fingerprint` 只原子切换 enabled，下一次请求立即生效。

PUT 不持久化配置、不生成新 ID，也不重新采集环境。配置文件 watcher 当前不更新 Fingerprint；canonical ID 变化需要重启。

## 7. 观测口径对齐

同一次客户端请求在各子系统中的计数单位不同：

| 子系统 | 一次 failover 请求如何记录 |
| --- | --- |
| Inflight | 始终一条临时快照 |
| AccessLog | 每个失败尝试一条，另加最终结果一条 |
| Metrics | 每个实际 Provider 尝试一组 request/latency 指标 |
| Usage | 只记录最终成功响应或已经提交的流 |

Provider 名称口径也不完全相同：AccessLog 将注册面归一为 vendor，并单独记录协议；Usage 和普通请求 Metrics 使用实际 Provider 注册名；Quota poll 常使用共享 Pool 的 vendor 名，probe 使用调度项的 Provider 名。跨数据源聚合时必须先确定是按 vendor 还是按注册面分析。

错误判断应优先组合 `trace_id`、`error_type`、status、Provider Key ID 和候选失败行。尤其对流式响应，HTTP 200 不等于完整成功；对 failover 请求，Usage 的一条成功记录也不表示之前没有失败尝试。
