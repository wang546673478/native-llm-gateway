# Native LLM Gateway

> 一个协议感知的、插件化的 LLM Gateway,为 AI Agent(Claude Code、Codex、Cline、Continue)提供多 Provider 路由、API Key 池化、Token 计费和自动故障转移。

📘 **完整实现规格书**:`Native LLM Gateway — 完整实现规格书 v2.md`

---

## 为什么需要这个 Gateway

AI Agent 场景下,单一的 LLM Provider 已经无法满足生产需求:

- **多 Provider 路由**:不同模型擅长不同任务(代码、推理、长文本),需要按模型名路由
- **Key 池化**:单一 Provider 的多个 Key 需要轮询、限速、隔离
- **故障转移**:某个 Provider 抖动时不能阻塞 Agent 主流程
- **统一鉴权**:给客户端发 Gateway Key,真实 Provider Key 由 Gateway 持有
- **可观测性**:每次请求的 trace、Usage、延迟、错误必须可查

**Native LLM Gateway** 在保留 Provider 原始协议语义的前提下,把上述控制面能力集中到一个 Gateway。

---

## 核心设计原则

| # | 原则 | 一句话 |
|---|------|--------|
| 1 | **协议感知的透明代理** | 只读取 body 提取路由元数据(model、stream、tools),请求/响应原样透传 |
| 2 | **Provider 即插件** | 统一接口,新增 Provider 不需要改 Gateway 核心代码 |
| 3 | **Gateway 只管控制面** | 路由/调度/限流/统计/认证/故障恢复归 Gateway;协议细节归 Provider |
| 4 | **所有资源可观测** | trace-id 贯穿全链路,Provider 健康、Key 状态、Usage 必须可查 |
| 5 | **所有行为可配置** | 路由策略、Key Pool、Circuit Breaker 参数可配置,支持热加载 |
| 6 | **流式响应的安全边界** | 流式已经开始发送数据后,中途失败不做 failover(避免重复/不一致) |

> ❌ Gateway **不会**做的事情:转换消息结构、映射 Provider 特有字段、统一错误格式、修改 body 任何字段。

---

## 协议配对

Gateway **不做跨协议桥接**。客户端发什么协议,就路由到声明该协议的 Provider。

| 客户端协议 | 可路由到的 Provider |
|-----------|-------------------|
| Anthropic (`/v1/messages`) | MiniMax、Kimi、GLM(protocol=anthropic) |
| OpenAI (`/v1/chat/completions`) | DeepSeek、Qwen、MiniMax、Kimi(protocol=openai) |
| Google (`/v1/generate`) | Gemini(protocol=google) |

---

## 内置 Provider

- DeepSeek
- MiniMax
- GLM(智谱)
- Qwen(通义千问)
- Kimi(月之暗面)
- Gemini
- OpenAI 兼容(通用 Provider 插件,适配任意 OpenAI 兼容 API)

新增 Provider 只需实现 `provider.Provider` 接口 + 在 `registry` 注册,Gateway 核心代码无需改动。

---

## 技术栈

### Backend

| 组件 | 选型 |
|------|------|
| 语言 | Go >= 1.23 |
| HTTP 框架 | Gin v1.10+ |
| ORM | GORM v1.25+ |
| 数据库 | SQLite(开发)/ PostgreSQL 16+(生产) |
| 缓存 | Redis 7+ |
| 日志 | Uber Zap |
| 指标 | Prometheus |
| 配置 | Viper |
| CLI | Cobra |
| 迁移 | golang-migrate |

### Frontend

| 组件 | 选型 |
|------|------|
| 框架 | Vue 3.4+(Composition API) |
| 语言 | TypeScript 5+ |
| 构建 | Vite 5+ |
| 状态管理 | Pinia |
| UI 库 | Naive UI |
| 图表 | ECharts 5+ |
| HTTP | Axios |
| 路由 | Vue Router 4 |

### 基础设施

- Docker + Docker Compose
- Prometheus + Grafana

---

## 目录结构

```
llm-gateway/
├── backend/
│   ├── cmd/gateway/main.go          # 入口
│   ├── internal/
│   │   ├── api/http/                # HTTP 处理器 + 中间件
│   │   ├── api/proxy/               # 核心代理引擎(流式 + 非流式)
│   │   ├── router/                  # 路由引擎 + 别名解析
│   │   ├── policy/                  # 路由策略(优先级/权重/成本/健康)
│   │   ├── provider/                # Provider 接口 + 各 Provider 实现
│   │   ├── keypool/                 # API Key 池
│   │   ├── circuit/                 # Circuit Breaker
│   │   ├── usage/                   # Usage 收集与查询
│   │   ├── token/                   # Token 计数
│   │   ├── metrics/                 # Prometheus 指标
│   │   ├── auth/                    # 客户端认证
│   │   ├── config/                  # 配置加载
│   │   ├── database/                # DB 初始化 + GORM 模型
│   │   └── server/                  # 服务编排
│   ├── plugins/                     # 第三方 Provider 插件
│   ├── migrations/                  # SQL 迁移脚本
│   └── docs/                        # ARCHITECTURE.md / API.md / CHANGELOG.md
├── frontend/                        # Vue 3 + TS + Vite
├── prometheus/
├── grafana/
├── config.example.yaml
└── docker-compose.yml
```

---

## 快速开始

> 实现完成后,典型使用流程:

```bash
# 1. 复制配置模板并按需修改
cp config.example.yaml config.yaml

# 2. 启动服务
docker compose up -d

# 3. 把 Gateway 当成 Provider 用
# 客户端(Claude Code / Codex / Cline 等)配置:
#   base_url: http://localhost:8080
#   api_key:  gw-key-xxxx        # 由 Gateway 颁发,非 Provider 真实 Key
#   model:    coding-model       # 由 Gateway 的别名表路由到真实模型
```

> 完整安装、配置、API、部署、测试细节见 [`Native LLM Gateway — 完整实现规格书 v2.md`](./Native%20LLM%20Gateway%20%E2%80%94%20%E5%AE%8C%E6%95%B4%E5%AE%9E%E7%8E%B0%E8%A7%84%E6%A0%BC%E4%B9%A6%20v2.md)。

---

## 项目状态

🚧 **规格已冻结,实现待启动**。本仓库当前只包含完整实现规格书;Backend / Frontend / Provider 插件代码将在规格冻结后按阶段实现。

实现阶段请遵循规格书定义的目录结构、接口签名和错误处理规则。任何代码变更必须同步更新:

- `backend/docs/ARCHITECTURE.md`
- `backend/docs/API.md`
- `backend/docs/CHANGELOG.md`

---

## 路线图(规格书层面)

完整 Roadmap、模块优先级、验收标准见规格书对应章节。

---

## 许可证

待定(建议 Apache-2.0 或 MIT,实现阶段确认)。
