# CHANGELOG

> 项目的可读变更日志。仅记录**用户面** / **运维面** / **架构面**的改动。
>
> 完整 commit 历史看 `git log`。

---

## 2026-08-08 — 文档更新(本会话)

### 新增

- `docs/INDEX.md` — 文档一站式目录
- `docs/ARCHITECTURE.md` — 架构总览(包+职责+边界,新人 30 分钟上手)
- `docs/providers.md` — Provider 厂商目录(当前所有内置厂商)
- `docs/cross-cutting.md` — accesslog / metrics / circuit 横切关注点
- `docs/subsystems.md` — quotacheck / auth / usage 核心子系统
- `docs/config-reference.md` — config.yaml 完整字段 + 数据库 schema
- `docs/frontend.md` — 前端管理 UI
- `docs/operations.md` — 部署 / 脚本 / 监控

### 修改

- `README.md` — 顶部文档导航 + 厂商清单补充 MiMo 4 个注册名 + 末尾「已知耦合点」段

### 背景

代码已能跑、用户在使用,但**文档分散**且**新人接手成本高**(理解整个链路需 1-2 周)。
本轮按「每遍查代码 → 立刻写文档」节奏,10 遍循环覆盖:

1. 核心入口(server.go / main.go / cmd/gateway)
2. 所有 provider 包
3. accesslog / metrics / circuit
4. quotacheck / auth / usage
5. config / database / migrations
6. frontend
7. scripts / 部署 / 运维
8. 现有 docs 修正
9. 总索引 + superpowers 整理
10. CHANGELOG + 收尾

---

## 设计基线(2026-08 状态)

- **架构**:无路由表的 catch_all 自动模式,按 tier 自动降级
- **provider**:按厂商一组,协议面共享 key 池
- **错误**:无终端禁用状态,COOLING / QUOTA_EXCEEDED 都可恢复
- **熔断**:per-key 级(连坐问题已修)
- **配额**:poll(probe + 轮询)/ probe(每次重探)双档
- **key 调度**:RoundRobin(`key_rotation: "round_robin"`)
- **持久化**:key-state.json 优雅关停落盘,启动恢复
- **部署**:单 Docker 镜像 + 可选 PostgreSQL + systemd 托管

历史决策和实施细节见 `docs/superpowers/specs/` + `docs/superpowers/plans/`。

---

## 已知技术债(2026-08)

| 项 | 状态 | 文档 |
|---|---|---|
| 代码过度耦合(7+ 包跨调用) | 调研完成,未动手 | `docs/ARCHITECTURE.md` §5 + §9 |
| sticky session 改造 | 用户决定暂缓 | (讨论历史) |
| `key_rotation` 仅 round_robin,无 sticky 模式 | 暂不实现 | `docs/config-reference.md` §7 |
| 详情页流式 token 不显示 | 有意不修,看 Usage 页 | `docs/踩坑与排错.md` #17 |
| `api.md` 完整 HTTP API 文档 | 缺失,散落在 README | `docs/INDEX.md` §5 |

---

## 2026-08-20 — 模型/定价进 DB + 三厂商下线 + 网关全挂排障实录

### 新增

- **模型管理页**(`/models`):按 vendor 分组的模型清单、上游同步按钮、手工定价
- **DB 表 `provider_models` 成为模型清单与定价唯一真相源**(vendor 粒度、sort_order 保上游顺序,默认模型 = 上游首个)
- 管理 API:`GET /api/v1/providers/models` / `POST /api/v1/providers/sync-models` / `PUT /api/v1/providers/models`

### 变更

- **下线 gemini / qwen / glm 三个厂商**(包 + 注册 + config 模板同步删;glm 历史 53 次、qwen/gemini 0 次)。现存 deepseek / minimax / mimo 三厂商 8 个注册面
- config.yaml 删除 `providers.*.models` 段与 `default_model` 字段(改由 DB 提供)
- 前端 dist 需 `npm run build` 才含模型管理页(8080 托管 dist 而非 dev server)

### 修复(排障挖出,均含守卫测试)

- `ListModels` 硬编码 `/v1/models` → endpoint 已含版本前缀的厂商拼出 `/v1/v1/models`;按 `ResponsesPath` 惯例加 `ModelsPath`
- mimo openai 面双 `/v1` 的 `ChatPath`(该面历史 0 条成功记录,靠 anthropic 面掩盖)
- 默认模型按字典序取 → minimax 会从 `MiniMax-M3` 静默降到 `MiniMax-M2`;改按上游顺序
- `Provider.Models` 关联建跨命名空间外键(`vendor` → `providers.name`)→ 启动崩溃循环;移除
- 模型同步取 key 不按计费面 → mimo 的 `tp-` key 发到 api 端点 401;`ListModels` 改按本面 `BillingSource` 取 key

### 踩坑

- **#23:AutoMigrate 只加不删** —— 删结构体字段会在生产库留下 NOT NULL 死列,轻则 INSERT 全炸、重则启动崩溃循环。已进 CLAUDE.md 提交前自检清单
