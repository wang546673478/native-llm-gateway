# 文档索引(INDEX)

> 项目的所有文档快速定位。一站式目录。

---

## 0. 入口

| 文档 | 用途 | 何时读 |
|---|---|---|
| `README.md` | 项目门面 + 快速开始 | 第一次接触 |
| `docs/INDEX.md`(本文件) | 文档目录 | 找文档时 |
| `docs/ARCHITECTURE.md` | 架构总览(包+职责+边界) | 第一次上手改代码 |
| `Native LLM Gateway — 完整实现规格书 v2.md` | 设计基线 | 理解设计意图 |

---

## 1. 按角色

### 1.1 新人(第一次接触 / 准备上手)

1. `README.md` — 项目门面
2. `docs/ARCHITECTURE.md` — 30 分钟看包结构
3. `docs/providers.md` — 了解这是给谁用的
4. `docs/operations.md` — 怎么 run

### 1.2 改 provider 代码(加厂商 / 改端点)

1. `docs/provider厂商定制包指南.md` — 6 步实操
2. `docs/providers.md` — 对照现有厂商
3. `docs/踩坑与排错.md` — 已知坑
4. `docs/ARCHITECTURE.md` §6 启动顺序

### 1.3 改 keypool / proxy / router(核心链路)

1. `docs/ARCHITECTURE.md` §4 一次请求完整路径
2. `docs/cross-cutting.md` — 跨包关注点
3. `docs/subsystems.md` — 配额 / auth / usage
4. `docs/踩坑与排错.md` — #15 双 acquire / #16 熔断 / #14 配额分类

### 1.4 改 frontend

1. `docs/frontend.md` — 页面 + API 映射
2. `docs/踩坑与排错.md` — #7 页面全空白 / #17 流式 token

### 1.5 部署 / 运维

1. `docs/operations.md` — 部署方式 + 脚本 + 监控
2. `README.md` Docker 章节
3. `docs/踩坑与排错.md` — #8 进程没在跑 / #21 SQLite→PG / #22 切库后快照

### 1.6 排查故障

1. `docs/operations.md` §5 三板斧
2. `docs/踩坑与排错.md` — 22 个实战坑

---

## 2. 按子系统

| 子系统 | 文档 |
|---|---|
| 启动 / 编排 / 优雅关停 | `docs/ARCHITECTURE.md` §1 + §6 |
| 路由 | `docs/ARCHITECTURE.md` §4 + `docs/config-reference.md` §6 |
| 代理引擎 | `docs/ARCHITECTURE.md` §4 |
| Key 池 | `docs/ARCHITECTURE.md` §3 + `docs/cross-cutting.md` §3(熔断) |
| 配额 | `docs/subsystems.md` §1 |
| 鉴权 | `docs/subsystems.md` §2 |
| 用量 | `docs/subsystems.md` §3 |
| 接入日志 | `docs/cross-cutting.md` §1 |
| 指标 | `docs/cross-cutting.md` §2 |
| 熔断 | `docs/cross-cutting.md` §3 |
| 前端 | `docs/frontend.md` |
| 配置 | `docs/config-reference.md` |
| 数据库 | `docs/config-reference.md` §11 |
| 迁移 | `docs/config-reference.md` §12 |
| 部署 | `docs/operations.md` §1 |
| 脚本 | `docs/operations.md` §2 |
| 监控 | `docs/operations.md` §4 |
| 故障排查 | `docs/operations.md` §5 + `docs/踩坑与排错.md` |

---

## 3. 全部文档清单

### 3.1 用户向

| 文件 | 行数 | 状态 |
|---|---|---|
| `README.md` | ~230 | 最新 |
| `docs/INDEX.md` | 本文件 | 新建 |
| `docs/ARCHITECTURE.md` | ~200 | 新建 |
| `docs/providers.md` | ~200 | 新建 |
| `docs/cross-cutting.md` | ~150 | 新建 |
| `docs/subsystems.md` | ~150 | 新建 |
| `docs/config-reference.md` | ~200 | 新建 |
| `docs/frontend.md` | ~150 | 新建 |
| `docs/operations.md` | ~200 | 新建 |
| `docs/踩坑与排错.md` | 294 | 完整 |
| `docs/provider厂商定制包指南.md` | 209 | 完整 |

### 3.2 设计 + 规格

| 文件 | 用途 |
|---|---|
| `Native LLM Gateway — 完整实现规格书 v2.md` | 设计基线 v2(4244 行) |

### 3.3 内部(specs + plans)

> superpowers 工作流产出,记录重大重构的设计和实施历史

#### 3.3.1 Specs(设计)

| 文件 | 用途 |
|---|---|
| `docs/superpowers/specs/2026-07-22-access-log-design.md` | 接入日志设计 |
| `docs/superpowers/specs/2026-08-04-provider-quota-display-design.md` | 配额展示设计 |
| `docs/superpowers/specs/2026-08-04-provider-quota-polling-design.md` | 配额轮询设计 |
| `docs/superpowers/specs/2026-08-04-provider-vendor-restructure-design.md` | 厂商改组(kimi 删除等) |
| `docs/superpowers/specs/2026-08-06-provider-vendor-step0-design.md` | 厂商指南 Step 0 |
| `docs/superpowers/specs/2026-08-06-tier-failover-design.md` | tier 降级设计 |

#### 3.3.2 Plans(实施)

| 文件 | 用途 |
|---|---|
| `docs/superpowers/plans/2026-07-22-access-log.md` | 接入日志实施 |
| `docs/superpowers/plans/2026-08-04-provider-quota-display.md` | 配额展示实施 |
| `docs/superpowers/plans/2026-08-04-provider-quota-polling.md` | 配额轮询实施 |
| `docs/superpowers/plans/2026-08-04-provider-vendor-restructure.md` | 厂商改组实施 |
| `docs/superpowers/plans/2026-08-06-provider-vendor-step0.md` | 厂商指南 Step 0 实施 |
| `docs/superpowers/plans/2026-08-06-tier-failover.md` | tier 降级实施 |

> 这些是 superpowers 工作流(规格书 → 实施计划 → 落地)的产物,保留作为历史决策记录

---

## 4. 文档维护规则

### 4.1 改动任何代码后,必须同步检查

- [ ] `README.md` — 快速开始是否过时
- [ ] `docs/ARCHITECTURE.md` — 包结构是否变
- [ ] `docs/providers.md` — 厂商清单是否准
- [ ] `docs/config-reference.md` — 新字段是否补
- [ ] `docs/踩坑与排错.md` — 新坑是否补
- [ ] `docs/INDEX.md` — 是否要新增/改名

### 4.2 文档版本

- 文档**无版本号**(用 commit history 追踪)
- 重大改动建议在 commit message 写 `docs(...)`

### 4.3 文档同步到代码的检查清单

```bash
# 列出所有 *.md 文件
find . -name "*.md" -not -path "./node_modules/*" -not -path "./backend/*"

# 找引用了旧术语的文档(示例)
grep -rn "TODO.*docs" docs/
```

---

## 5. 缺失/未来文档

- [ ] `docs/api.md` — 完整 HTTP API 文档(目前散落在 README)
- [ ] `docs/refactoring-considered.md` — 重构方案 + 优先级
- [ ] `docs/key-rotation-strategy.md` — Key 调度策略详解
- [ ] `docs/cross-vendor-reasoning.md` — 跨厂商 reasoning 回带处理
- [ ] `docs/troubleshooting-cookbook.md` — 故障排查手册(基于踩坑 #1-22)
