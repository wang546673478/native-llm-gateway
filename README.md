# Native LLM Gateway

> 一个协议感知的 LLM Gateway,为 AI Agent(Claude Code、Codex、任意 OpenAI 兼容客户端)提供多 Provider 自动路由、API Key 池化、tier 计费(token_plan → api → free)和自动故障转移。

📘 **架构总览**:`docs/ARCHITECTURE.md` 一份图谱,新人 30 分钟上手
📘 **完整实现规格书 v2**:`Native LLM Gateway — 完整实现规格书 v2.md`(设计基线,差异看横幅)
📘 **Provider 厂商目录**:`docs/providers.md`(当前内置的所有厂商)
📘 **子系统详解**:`docs/subsystems.md`(quotacheck / auth / usage)
📘 **横切关注点**:`docs/cross-cutting.md`(accesslog / metrics / circuit)
📘 **配置参考**:`docs/config-reference.md`(config.yaml 完整字段)
📘 **前端**:`docs/frontend.md`(管理 UI)
📘 **运维**:`docs/operations.md`(部署 / 脚本 / 监控)
📘 **文档索引**:`docs/INDEX.md` 一站式目录
📕 **踩坑与排错**:`docs/踩坑与排错.md`
📗 **新增厂商指南**:`docs/provider厂商定制包指南.md`

---

## 核心设计:没有路由表

网关对客户端暴露的是**一条链,不是一张路由表**:

```
客户端发任何模型名(gpt-5 / claude-opus-5 / deepseek-v4-pro / 随便什么)
  → 模型名只是标签,不参与路由决策
  → 所有 enabled provider 自动参与(按请求路径选协议面)
  → 一层 token_plan(包月套餐,如 MiniMax)→ 二层 api(按量)→ 三层 free
  → 额度耗尽自动降级,探测恢复自动切回
  → key 白名单决定「链上能用哪些真实模型」
```

| 概念 | 说明 |
|---|---|
| **catch_all 自动模式** | `routing.catch_all: {}`(空规则)= 所有 provider 参与,无路由表可维护;加 provider + key 即自动进链 |
| **tier 计费** | `billing_source: token_plan / api / free` 决定层级;token plan 额度耗尽自动切 api 层,恢复自动切回(实测双向验证) |
| **白名单 = 链上模型选择** | key 的「允许的模型」直接决定链上服务哪些真实模型(provider 声明过白名单模型就用白名单模型),UI 改即生效 |
| **alias 表已退役** | 不再需要为探测名配映射;fallback model 已移除 |
| **key 顺序黏性(sticky)** | 同 provider 多 key 默认按加入时间「先用尽一把再切下一把」(round_robin 轮换已下线);纯 429 同 key 重试 10 次仍 429 才 COOLING 切换 |
| **优先级改写** | Routing 页拖拽调 provider/key 顺序 → `PUT /routing/order` 写 route_order 表,热生效 |

### Key 调度:树状模型(2026-08-10)

调度是一棵树,**天然排序、先来先用**,两层可改:

```
Level 0 候选名单    = 所有有可用 key 的 provider,自动进链(UI 不展示)
Level 1 分层        = key 的 billing_source → token_plan / api 付费层
Level 2 层内 provider 顺序 = 默认按最早 key 加入时间；可拖拽改写
Level 3 provider 内 key 顺序 = 默认按加入时间，先用尽一把再切；可拖拽改写
```

- **天然**:加 provider = 加一把 key 即自动进链;加 key = 自动排到队尾;零配置。
- **sticky**:同一 provider 内固定用「最高优先级可用 key」(默认按加入时间/改写序)。高位 key 不可用(额度耗尽 / COOLING / 熔断 OPEN / 429×10)才推进到下一把;高位 key 一旦恢复(额度 poll 回 ACTIVE / 熔断 HALF_OPEN→CLOSED)自动回位到它。429 仍同 key 重试 10 次;connection/timeout/5xx 靠 per-key 熔断隔离+自愈(2026-08-10)。
- **改写**:Routing 页拖拽 provider/key 顺序 → 写 `route_order` 表(token_plan/api 各层独立),保存即热生效;不改写时用加入时间兜底。

## 客户端接入(三种全通)

| 客户端 | 路径 | 模型名 |
|---|---|---|
| Claude Code | `/v1/messages`(x-api-key) | 任意,走链 |
| Codex | `/responses` 或 `/v1/responses` | 任意,走链(原生透传,DeepSeek/MiniMax 官方支持) |
| 任意 OpenAI 兼容 | `/v1/chat/completions` | 任意,走链 |

```jsonc
// Claude Code: ~/.claude/settings.json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
    "ANTHROPIC_AUTH_TOKEN": "gw-xxxxxxxx",   // Gateway 颁发的 key
    "ANTHROPIC_MODEL": "claude-opus-5",      // 任意名字,网关会路由到真实模型
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}
```

```toml
# Codex: ~/.codex/config.toml
model = "gpt-5-codex"              # 任意名字
model_provider = "gateway"
[model_providers.gateway]
name = "gateway"
base_url = "http://127.0.0.1:8080" # Codex 会拼 /responses
wire_api = "responses"
```

> ⚠️ DeepSeek 的 `/v1/responses` 目前只支持 `deepseek-v4-flash`(不支持 v4-pro)——Codex 走 deepseek 时白名单需含 v4-flash。

## 内置 Provider(2026-08 现状)

| 厂商 | 注册名(协议面) | billing | 余额恢复 | Responses API |
|---|---|---|---|---|
| `deepseek` | `deepseek`(openai)+ `deepseek-anthropic`(anthropic) | api | poll(官方余额接口) | ✅(`responses_api: true`,仅 v4-flash) |
| `minimax` | `minimax`(anthropic)+ `minimax-openai`(openai) | **token_plan** | poll(`token_plan/remains`) | ✅ |
| `mimo` | `mimo`(openai,api)+ `mimo-token-plan`(openai,token_plan 套餐) + `mimo-anthropic`(anthropic,api) + `mimo-token-plan-anthropic`(anthropic,token_plan 套餐) | api + token_plan(两套端点两套 key) | probe(无官方余额接口,未文档化 cookie 端点探测) | ✅ |

> 同一厂商的多个注册名(协议面)共享同一 key 池;key 厂商级一份,协议由 key 的 Protocols 标记过滤。例外:mimo 两套端点用两套 key,按 per-key BillingSource 隔离(sk- key 只走 api 端点、tp- key 只走 token-plan 端点,交叉必 401)。kimi / glm / qwen / gemini 已删除(需要时按 `docs/provider厂商定制包指南.md` 加回)。
> 余额恢复:poll = 额度耗尽标 QUOTA_EXCEEDED,quotacheck 轮询余额,恢复自动回链;probe = 不永久标记,每次请求重新探测,充值即恢复。

## 管理 API(`/api/v1`)

| URL | 说明 |
|---|---|
| `GET /providers` | 按厂商聚合的 Provider 列表(共享 pool、每把 key 的熔断/额度状态) |
| `GET /providers/registered` | Registry 注册名列表(轻量,过滤下拉用) |
| `GET /providers/models` | 模型清单(按 vendor 分组,DB `provider_models`,含三档价格) |
| `POST /providers/sync-models` | `{vendor}` 触发上游 `/models` 同步(sort_order 记上游顺序,默认模型 = 首个) |
| `PUT /providers/models` | 保存某模型三档每百万价格(模型管理页手工定价) |
| `GET /routing` | catch_all 状态(自动模式 / 显式列表) |
| `GET/PUT /routing/order` | Level 2/3 优先级改写:provider 顺序 / 某 provider 内 key 顺序(路由页拖拽落库,热生效) |
| `GET /keys` | Gateway Key 管理(CRUD;白名单在此配置) |
| `GET /providers/:name/api-keys` | 厂商 key 池管理(CRUD) |
| `GET /access-logs` / `/:id/detail` / `/stats` | 接入日志(详情人类可读) |
| `GET /access-logs/export` | **JSONL 训练数据导出**(30 天保留,body 上限 16MB) |
| `GET /dashboard` | 总览(QuotaKnownSum 按厂商聚合) |
| `GET /usage*` | 用量统计(含 `/usage/by_model/:model_id/providers` 上游用量对比) |
| `GET /config/quota` | 配额探测配置(端点/间隔/轮询 vs 探测模式) |

## 快速开始(本地)

```bash
# 1. 配置(不入库,磁盘生效)
cp config.example.yaml config.yaml   # 按需改 provider;模型/价格在「模型管理页」配(存 DB,不在 config)

# 2. 后端
cd backend && go build -o bin/gateway ./cmd/gateway
./bin/gateway                        # 从仓库根目录运行(读 config.yaml)

# 3. 前端(开发)
cd frontend && npm run dev           # http://localhost:5173(代理 /api 到 8080)

# 4. 加 key + 配白名单(页面或 API)
curl -X POST http://localhost:8080/api/v1/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-key","allowed_models":["MiniMax-M3","deepseek-v4-flash"],"rpm":100,"tpm":500000}'
# 注意:创建响应里包含明文 key,只展示一次
```

## Docker 部署

镜像 `wuhuhhhh/native-llm-gateway`(Docker Hub)。**每次 push main 后 GitHub Actions 自动构建推送**(`latest` + commit sha 双 tag,可回滚),本地无需 Docker 即可获得新镜像,部署方 `docker pull` 更新。

### 快速部署(推荐:gateway + PostgreSQL)

**编排文件 `docker-compose.yml`**(仓库根目录,完整内容):

```yaml
services:
  gateway:
    image: wuhuhhhh/native-llm-gateway:latest
    container_name: llm-gateway
    ports:
      - "8080:8080"          # 前端 + API 都在这里
    volumes:
      - ./config.yaml:/app/config.yaml:ro   # 只读挂载真实配置(密钥不进镜像)
      - ./gateway-data:/app/data            # DB / key-state / access body 持久化
    depends_on:
      postgres:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8080/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    restart: unless-stopped

  postgres:
    image: postgres:16-alpine
    container_name: llm-gateway-postgres
    environment:
      POSTGRES_USER: gateway
      POSTGRES_PASSWORD: CHANGE_ME          # ← 改成你的密码
      POSTGRES_DB: gateway
    volumes:
      - ./pg-data:/var/lib/postgresql/data   # PG 数据持久化
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gateway -d gateway"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped
```

```bash
# 1. 准备 config — 用 docker 专用模板(容器路径已配好),只改密码和默认 key
cp config.docker.example.yaml config.yaml
#   ① database.dsn 里的 CHANGE_ME 改为你的密码(与 compose 的 POSTGRES_PASSWORD 一致)
#   ② auth.keys 的默认 dev-key(gw-key-dev-please-change-me)改掉或删除 — 代理端点
#      认证默认开启,不改的话任何人知道这个 key 都能免费调用

# 2. 改 docker-compose.yml 里 postgres 的 POSTGRES_PASSWORD(与 dsn 一致)

# 3. 启动
docker compose up -d
# 前端 + API 都在 http://<host>:8080
```

### 最小部署(单容器 + SQLite,无 PG)

```bash
docker run -d --name llm-gateway \
  -p 8080:8080 \
  -v $PWD/config.yaml:/app/config.yaml:ro \
  -v $PWD/gateway-data:/app/data \
  wuhuhhhh/native-llm-gateway:latest
# config.yaml 保持 database.driver: "sqlite",dsn 指向 /app/data/gateway.db
```

### 说明

- **前端托管**:镜像内单进程(gateway)直接托管构建产物 + SPA fallback,无 nginx
- **数据卷** `/app/data`:DB(dsn 指向这里时)、`key-state.json`(自动跟随 dsn 目录)、access body
- **config.yaml 更新**:改配置后 `docker compose up -d` 重建生效(healthcheck 通过才算就绪)
- **镜像更新**:`docker compose pull && docker compose up -d`(或 watchtower 自动拉取)
- **SQLite vs PostgreSQL**:access logs 每请求一条 + 8 索引,SQLite 单写者在请求量大时写锁排队(页面卡顿);PostgreSQL 并发写,`database.driver` 一键切换,代码层无差异(CI 每次提交跑 PG 集成测试)
- **本机主力已切 PG(2026-08-07)**:systemd 托管(`llm-gateway.service`,Restart=always)+ 每日 3:07 pg_dump 备份(保留 SQLite 归档 7 天后清理);`key-state.json` 在 PG 模式下落进程 cwd(仓库根)。运维:`sudo systemctl {restart|status} llm-gateway`、`./gateway-reload.sh`(编译 + systemctl restart)

## 目录结构(核心)

```
backend/internal/
├── provider/                 # Provider 接口 + 厂商包
│   ├── builtin/              # blank import 3 个厂商包(deepseek/mimo/minimax)→ 注册进默认 Registry
│   ├── deepseek/             # deepseek + deepseek-anthropic(双协议面,共享 pool)
│   ├── minimax/              # minimax + minimax-openai + token plan balancer
│   ├── mimo/                 # mimo / mimo-token-plan / mimo-anthropic / mimo-token-plan-anthropic(两套端点两套 key)
│   ├── google/               # Google 协议共享实现(当前无厂商消费者,暂留)
│   ├── openai_compatible/    # OpenAI 兼容共享实现(上游路径/错误分类/SSE)
│   ├── anthropic_compatible/ # Anthropic 兼容共享实现
│   └── registry.go manager.go
├── router/                   # catch_all 自动模式 + 白名单选择 + tier 拉平
├── proxy/                    # 代理引擎(failover / 白名单逐候选 / Responses 剥离)
├── keypool/                  # 厂商级 key 池(tier 桶 + 额度状态机 + per-key 熔断 + sticky 顺序黏性)
├── circuit/                  # per-key 熔断器(5xx/timeout/connection 只熔断该 key)
├── quotacheck/               # 余额轮询(标记 QUOTA_EXCEEDED / 恢复)
├── accesslog/                # 接入日志(body 文件 + 30 天保留 + 导出)
├── api/http/                 # 管理 API
└── server/                   # 服务编排(优雅关停写 key 状态快照)
```
运行产物:`logs/gateway.log`(追加模式,按天轮转,7 天自动清理);DB 目录下 `key-state.json`(优雅关停时的 key 状态快照,重启恢复 QUOTA_EXCEEDED/COOLING/余额)。

## 已知边界

- 同 tier 内多个 provider 顺序 = 默认按该 provider 最早 key 加入时间(先来优先),可用 Routing 页拖拽改写(route_order);无 key/provider 按名字兜底确定性
- **无终端禁用状态**:上游 auth 错误 → 该 key COOLING 5 分钟自动重试;400 invalid_request 只计数;5xx/timeout/connection → per-key 熔断器(只熔断这一把 key,不连坐同 provider 其他 key)
- 配额耗尽标记按厂商分两档:poll(有余额接口:deepseek/minimax)→ 标 QUOTA_EXCEEDED,quotacheck 轮询恢复;probe(无官方余额接口:mimo)→ 不永久标记,每次请求重新探测,恢复后自动可用
- 跨厂商切换时客户端回带的推理块会被网关剥离 + 强制 `effort=none`(DeepSeek 校验)
- provider 的 endpoint/protocol 改动需重载(用 `./gateway-reload.sh` 无感重载,自动编译+优雅排空);routing/key 热重载,模型清单与价格走 DB(`provider_models`,模型管理页改即生效,无需重载)。重载后 key 状态从 `key-state.json` 快照恢复(QUOTA_EXCEEDED/COOLING 不丢),无需重新 poll 确认

## 已知耦合点(2026-08 现状)

代码已能跑但**过度耦合** — 任何跨包改动(如 sticky session)预计 ~35 个文件,详见 `docs/ARCHITECTURE.md` §5 + §9 改动前检查清单。改进方向见文档末尾讨论。
