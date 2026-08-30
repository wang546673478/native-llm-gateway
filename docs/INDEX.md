# 文档索引

这里仅保留与当前代码一致的长期文档。已完成实施计划、一次性事故报告、问题交接和生产数据
快照留在 Git 历史，不继续作为活跃文档维护。

## 从这里开始

| 文档 | 内容 |
|---|---|
| [README](../README.md) | 项目能力、快速开始、客户端示例和安全边界 |
| [架构](ARCHITECTURE.md) | 启动顺序、包边界、请求流、路由、重试和关停 |
| [配置参考](config-reference.md) | YAML 字段、数据库表、热重载和配置孤岛 |
| [HTTP API](api-reference.md) | 认证方式、代理入口和管理端点 |
| [部署与运维](operations.md) | 本地、Docker、systemd、监控、备份、发布和回滚 |
| [排错指南](踩坑与排错.md) | 按症状定位认证、路由、key、relay、流式和数据问题 |

## 按任务阅读

| 任务 | 文档 |
|---|---|
| 理解路由、failover 或 sticky 调度 | [架构](ARCHITECTURE.md)、[横切机制](cross-cutting.md)、[排错](踩坑与排错.md) |
| 配置服务、数据库或热重载 | [配置参考](config-reference.md) |
| 调用或维护 HTTP API | [HTTP API](api-reference.md) |
| 查看内置厂商差异 | [Provider 目录](providers.md) |
| 新增或修改内置厂商 | [厂商定制包指南](provider厂商定制包指南.md) |
| 无代码接入兼容上游 | [动态中转站](relay-stations.md)、[前端](frontend.md) |
| 修改认证、Key Pool、quota 或 Usage | [核心子系统](subsystems.md) |
| 修改 access log、metrics、circuit 或 fingerprint | [横切机制](cross-cutting.md) |
| 修改管理控制台 | [前端](frontend.md)、[前端包 README](../frontend/README.md) |
| 部署、监控、备份或恢复 | [部署与运维](operations.md) |
| 查看重要行为变化 | [变更记录](CHANGELOG.md) |

## 活跃文档清单

### 项目入口

- [README](../README.md)
- [协作说明](../CLAUDE.md)
- [前端包 README](../frontend/README.md)
- [Provider vendor skill](../.claude/skills/provider-vendor/SKILL.md)

### 架构与运行时

- [架构](ARCHITECTURE.md)
- [配置参考](config-reference.md)
- [HTTP API](api-reference.md)
- [核心子系统](subsystems.md)
- [横切机制](cross-cutting.md)

### Provider 与管理台

- [Provider 目录](providers.md)
- [厂商定制包指南](provider厂商定制包指南.md)
- [动态中转站](relay-stations.md)
- [前端](frontend.md)

### 运维与历史

- [部署与运维](operations.md)
- [排错指南](踩坑与排错.md)
- [变更记录](CHANGELOG.md)

## 权威顺序

文档与实现冲突时，按以下顺序核对：

1. `backend/internal/`、`backend/cmd/`、`frontend/src/` 的运行时代码和测试。
2. `backend/internal/config/`、数据库模型和两个 example YAML。
3. 本索引列出的长期文档。
4. Git 历史中的设计背景。

Provider key、Gateway Key、模型、价格、中转站和 `route_order` 是数据库运行时数据。不要把
生产清单、真实凭证、进程 ID 或一次性清理结果复制到长期文档。

## 维护规则

- 用户可见行为、API、配置、schema、部署或 UI 改变时，在同一变更中更新对应文档。
- 示例只使用明确假值，不包含真实 key、cookie、DSN、session 或用户数据。
- 一个事实只保留一个详细权威解释，其他文档使用链接。
- 事故中的可复用结论进入排错指南；临时交接在问题关闭后删除。
- 已完成工作进入变更记录和 Git 历史；执行计划不长期留在 `docs/`。
- 重命名或删除文档后检查所有本地 Markdown 链接。
