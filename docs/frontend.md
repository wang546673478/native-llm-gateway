# Frontend(管理 UI)

> Vue 3 + TypeScript + Naive UI + Vite
>
> **进入路径**:`http://<gateway>:8080/`(镜像内 gateway 进程直接托管构建产物,无 nginx)

---

## 1. 技术栈

| 层 | 选型 |
|---|---|
| 框架 | Vue 3 (Composition API) |
| 语言 | TypeScript |
| UI | Naive UI(`n-data-table` / `n-card` / `n-tag` / `n-grid`) |
| 状态 | Pinia (`stores/health.ts`) |
| 路由 | vue-router (history 模式) |
| 构建 | Vite |
| HTTP | 同目录 `api/client.ts` 封装 `fetch` |

---

## 2. 页面结构

| 路径 | 视图 | 用途 |
|---|---|---|
| `/` | (redirect)→ `/overview` | — |
| `/overview` | `Overview.vue` | 总览:24h 请求数 / Token / 错误 / 费用 + 按 Model 用量 |
| `/providers` | `Providers.vue` | 厂商列表(按 vendor 聚合,展示协议面) |
| `/provider-keys` | `ProviderKeys.vue` | 厂商 key 池管理(CRUD) |
| `/keys` | `Keys.vue` | Gateway Key 管理(CRUD,白名单,Provider绑定) |
| `/routing` | `Routing.vue` | 路由配置(默认策略 / catch_all / 链) |
| `/usage` | `Usage.vue` | 用量统计含 `/usage/by_model/:model_id/providers` |
| `/access-logs` | `AccessLogs.vue` | 接入日志 + 详情 + 导出 JSONL + 故障排查 |
| `/models` | `ModelManager.vue` | 模型管理(上游同步 + 手工定价,按 vendor 分组) |
| `/relay-stations` | `RelayStations.vue` | 中转站配置(CRUD + 热重载,2026-08-22) |
| `/inflight` | `Inflight.vue` | 活跃请求快照(1s 轮询,2026-08) |

---

## 3. 关键页面

### 3.1 Overview

- 总请求数(24h) / 总 Token / 错误数 / 总费用
- 按 Model 用量卡片(每张卡显示一个 Model 的用量,2026-08 改:不再按 provider 归类)

### 3.2 Providers

- 一行 = 一个 vendor
- 多协议面(`deepseek` + `deepseek-anthropic`)显示在 name 列表

### 3.3 ProviderKeys

- 选厂商 → 看 key 池
- 单 key 状态:**ACTIVE / COOLING / QUOTA_EXCEEDED**(实时从 Pool)
- 剩余余额 / LastPolledAt(实时)
- 手动标记 QUOTA_EXCEEDED: `POST /api/v1/providers/:name/api-keys/:id/mark-quota-exceeded`

### 3.4 Keys(Gateway Key)

- 客户端用 key 管理
- 创建响应**含明文**,只展示一次
- 白名单 `allowed_models` 是路由生效关键

### 3.5 Routing

- catch_all 状态:**自动模式 / 显式列表**
- 显式列表配 `providers` 数组(按 name 排序确定)
- 配置 `default_strategy`(`priority` / `weighted` / `cost`)

### 3.6 Usage

- 时间窗口切换
- 按 Model 折线 / 按 Provider 折线
- **流式请求的 token 数必须看这里**(access log 详情页不显示,踩坑 #17)

### 3.7 AccessLogs

- 24h 列表 + 详情
- 筛选:Trace ID / Gateway Key / Provider / Status / 模型名
- 详情页:响应 body 可读(JSON 解析后展示)
- **底层数据**:DB metadata + body 文件(jsonl)
- **导出 JSONL**: `/api/v1/access-logs/export` — 30 天保留,body 上限 16MB

### 3.7.1 详情行解析(踩坑 #17)

- 非流式:`JSON.parse(body)` 读 `usage` → 显示 cache_read / cache_creation / input / output
- 流式:body 是 SSE 拼接 → parse 失败 → 显示空 → **改去 Usage 页**

### 3.8 Models(模型管理)

- 按 vendor 分组的模型清单(来源:DB `provider_models`)
- 「上游同步」按钮(单厂商):`POST /api/v1/providers/sync-models {vendor}` — 拉上游
  `/models` 端点填模型 id(sort_order 保上游顺序,默认模型 = 上游首个)
- 「全部同步」按钮(顶部,2026-08-21 加):`POST /api/v1/providers/sync-all-models`
  — 动态算所有 vendor 逐个同步,单个失败不中断,最后汇总提示失败的 vendor
- 手工定价:`PUT /api/v1/providers/models` 三档每百万价格(input / cache_read / output)
- 注意:同步只带 model id 不带价格,同步后价格需手工填;未定价模型 cost 记 0

### 3.9 RelayStations(中转站配置,2026-08-22)

- 中转站 = 纯透传代理,无需编写代码,只需配置 URL + 协议
- **CRUD**:添加 / 编辑 / 删除中转站(存 `relay_stations` 表)
- **协议模式**:
  - `single`:单协议(如 tokenmarket 只提供 openai 面)
  - `multi`:多协议(如 rightapi 按后缀拆分 openai / anthropic / google 三个面)
- **热重载**:编辑后点「热重载」按钮 → `POST /api/v1/relay-stations/reload`
  → 后端重新从 DB 读取并注册所有启用的中转站(旧的先删除)
- **自动同步 keys**:每次加载/重载时,中转站的 keys 字段(JSON 数组)会自动
  同步到 `provider_api_keys` 表(增删双向同步,以 relay_stations.keys 为准)
- **中转站直通模式**(P-relay-passthrough,2026-08-25):
  - Gateway Key 绑定的 Providers **全是**中转站时 → 跳过白名单选择逻辑,直接透传
    客户端请求的模型名(不替换为 default_model)
  - 混合绑定(中转站 + 普通厂商)时 → 中转站也按普通路由走(使用 default_model)

### 3.10 Inflight(活跃请求快照,2026-08)

- 1 秒轮询 `GET /api/v1/inflight`
- 显示正在处理的请求:trace_id / 客户端模型 / 实际 provider+model / 开始时间
- 流式请求可持续数分钟,非流式一般秒级

### 3.7.2 Key 名字("key-1" / "weige")

- DB 存 ID 数字,前端 fallback 显示 ID
- ProviderKeyName 是在 `provider_api_keys.name` 字段
- 改名字 → 历史记录同步显示

---

## 4. 状态管理

只有一个 Pinia store:**`stores/health.ts`**(后端健康 + 启动时间)。

---

## 5. 开发模式

```bash
cd frontend
npm install
npm run dev              # 启动 Vite dev server,默认 :5180
```

vite proxy 配置:**跟实际后端端口走**(`memory/feedback-vite-proxy-port.md` 踩坑)。后端如果不在 8080,`vite.config.ts` 的 proxy target 要跟着改。

---

## 6. 构建

```bash
npm run build            # 输出 dist/
```

`dist/` 由 gateway 进程托管(Go 静态文件 + SPA fallback 到 `index.html`)。

---

## 7. 已知坑

- **页面全空白**(踩坑 #7):后端返回 `null` 数组 + 模板 `.length` → TypeError → 渲染期崩溃整个应用
  - 修复:后端 `append([]T{})` 保证返回 `[]`,前端 `?? []` 容错
- **vite dev 端口冲突**(踩坑 #13):`pgrep -af vite` 看现有实例
- **流式请求 token 在详情页看不到**:去 Usage 页

---

## 8. 与后端 API 的对应

| 页面 | 主要 API |
|---|---|
| Overview | `GET /api/v1/dashboard` |
| Providers | `GET /api/v1/providers` |
| ProviderKeys | `GET /api/v1/providers/:name/api-keys` `POST /api/v1/providers/:name/api-keys` |
| Keys | `GET /api/v1/keys` `POST /api/v1/keys` `PATCH /api/v1/keys/:id` |
| Routing | `GET /api/v1/routing` `PUT /api/v1/routing` |
| Usage | `GET /api/v1/usage` `GET /api/v1/usage/by_model/:model/providers` |
| AccessLogs | `GET /api/v1/access-logs` `GET /api/v1/access-logs/:id/detail` `GET /api/v1/access-logs/export` |
| Models | `GET /api/v1/providers/models` `POST /api/v1/providers/sync-models` `POST /api/v1/providers/sync-all-models` `PUT /api/v1/providers/models` |
| RelayStations | `GET /api/v1/relay-stations` `POST /api/v1/relay-stations` `PUT /api/v1/relay-stations/:id` `DELETE /api/v1/relay-stations/:id` `POST /api/v1/relay-stations/reload` |
| Inflight | `GET /api/v1/inflight` |

详细 API 文档见 `docs/api.md` / `docs/http-api.md`(若存在)。
