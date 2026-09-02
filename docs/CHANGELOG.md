# 变更记录

这里只记录对用户、运维和架构有重要影响的变化。完整实现历史以 `git log` 为准。

## 待发布

### Relay 透明透传与流可靠性

- Relay 候选改为从不可变客户端快照派生，请求 body 不再经过 reasoning、model、thinking、
  fingerprint 或 stream usage 改写；raw query 和安全的端到端多值 headers 保持不变。
- Relay SSE 改为原始字节通道，PING/注释/data 不再重建；收到任意正文即提交上游
  status/headers，提交后断流不注入 Gateway error event，也不再切换候选。
- 增加可配置的 relay 首包预算、TTFT 分层、生命周期事件和活动上游指标；客户端取消后立即
  停止全候选链，不再制造后续 `connection`/502。
- Relay 成功及最终 HTTP 错误响应保留上游 status、端到端 headers 和 body；无 HTTP
  response 的 transport 错误才生成 Gateway 错误。

### 文档

- 按当前架构重写文档集，覆盖配置、API、Provider、中转站、前端、运维和排错。
- 删除重复事故报告、过期 TokenMarket 接入说明和旧 v2 规格书；需要保留验证证据的实施
  计划可作为故障记录留存。
- 用数据库动态中转站指南替代特定中转商文档，并将两个示例配置收敛到当前内置厂商。

### 运维与安全

- 删除模型 face 同步脚本内置的 PostgreSQL 密码，执行时必须显式提供 `DB_PASSWORD`。
- 修正二进制备份轮换逻辑，保留最新 5 份时间戳备份，不再错误保留最旧文件。

## 2026-08-30

### 重试与 Key 调度

- 区分 HTTP 429 与非 429 `rate_limit`。只有 429 会在同一 Key 上重复重试；HTTP 403
  及其他非 429 限流立即回到 failover。
- 避免 Provider 与 Proxy 针对同一次上游失败重复上报 KeyPool，防止冷却次数和错误计数翻倍。
- 增加流式和非流式 403/429 重试矩阵的回归测试。

### Relay URL

- OpenAI/Anthropic 兼容端点现在兼容带或不带末尾 `/v1`、带或不带尾部斜杠的 base URL。
- 增加 Chat Completions、Responses 和 Messages 的中转站级 URL 归一化请求测试。
- Gateway Key 的 Provider 绑定同时匹配注册 face 和 vendor，使 vendor 绑定覆盖其兼容协议面。

### 运维

- 增加 `scripts/gateway-health-check.sh`，检查 systemd、进程、健康端点、数据库、指标、磁盘和近期日志。

## 2026-08-28

### Relay 数据生命周期

- 删除中转站时级联清理模型 face 归属、`route_order` 和 Provider Key 行，同时覆盖 face 名与站点名。
- 删除 Provider Key 时同步清理 Key 级 `route_order`。
- 增加带守卫的孤儿数据预览/清理脚本，以及 relay、路由顺序、Key 和模型 face 清理回归测试。

### Proxy 与管理端

- `retry.max_attempts: 0` 改为根据当前路由候选数计算预算，不再套用固定上限。
- 增加仅 root 可用的管理员密码重置。
- 修复流式 Usage 数据竞争，并扩充路由和 failover 回归测试。

### 前端

- 增加浅色/深色/跟随系统主题、共享视觉变量、首次加载骨架、通用统计卡片和 Usage 趋势图。
- 将登录页和管理员用户页的独立样式统一到管理控制台设计中。

## 2026-08-27

### 管理员认证

- 增加数据库管理员用户与 Session、登录限速、角色校验、密码修改和管理 API 中间件。
- 增加登录页和仅 root 可见的管理员管理页面。

### 流式可靠性

- 在响应字节发给客户端前检测早期流错误，使这类失败仍可参与 failover。
- 按请求体大小增加动态流式空闲超时，并补充针对性 failover 测试。

## 2026-08-26

### 流式与关停

- 识别 HTTP 200 SSE 流中的结构化上游错误。输出前发现的错误参与 failover；中途错误会暴露给客户端，但不会重放已经发送的内容。
- 修正关停顺序，等待在途请求结束后再关闭 Usage 和 Access Log collector。
- 提高中转站超时和写超时，适配长时间上游请求。

### Usage 计费

- 统一历史输入 token 聚合口径，停止对 OpenAI 缓存输入重复计费。

## 2026-08-24 至 2026-08-25

### 动态中转站

- 稳定中转站创建、编辑、热重载和模型同步流程。
- 增加 relay 透传、模型优先筛选、协议 face 模型归属及 relay 候选之间的 failover 连续性。
- 将 Gateway Key 的 Provider 过滤前移到 Router，禁止的 Provider 不再获取 Key 或污染调度状态。

## 2026-08-20 至 2026-08-21

### 模型与定价

- 将模型清单和每百万 token 定价迁移到 `provider_models`，支持手工同步上游和维护价格。
- 增加 `provider_model_faces`，让多端点中转站按协议面拥有不同模型，同时复用 vendor 级价格。
- 增加全部同步、过期模型裁剪、face 感知界面，以及按上游模型顺序确定默认模型。

### 在途请求可见性

- 增加内存在途请求注册表、管理 API 和每秒轮询的管理页面。

## 2026-08-18

### 请求指纹归一化

- 转发前可选归一化 Claude Code 的设备 ID、平台、shell 和操作系统元数据，同时保留工作目录和对话内容。
- 增加对应配置和管理控制台开关。

## 2026-08-10

### 分层路由与 Sticky Key

- 引入 `token_plan -> api -> free` 分层候选树。
- 增加持久化到 `route_order` 的 Provider 与 Key 顺序改写。
- Sticky 调度始终选择当前最高优先级可用 Key；高位不可用时自动后退，恢复后自动回位。
- 对短暂 HTTP 429 增加同 Key 重试。

## 2026-08-04 至 2026-08-09

### Provider 与配额架构

- 按 vendor 归组协议注册面，并共享 vendor 级 KeyPool。
- 增加配额轮询/探测、余额显示、可恢复配额状态和每 Key 熔断器。
- 删除终态 Key 禁用，并修复多项路由、热重载、数据库、并发和可观测性一致性问题。

### 部署与持久化

- 增加 PostgreSQL、Docker 镜像/Compose 部署、面向 systemd 的进程管理、Access Log 持久化和 Key 状态快照。

更早的实现细节可通过 `git log` 查阅。
