# Frontend（管理 UI）

> Vue 3 + TypeScript + Vite + Pinia + Naive UI
>
> 生产入口：`http://<gateway>:8080/`。Gateway 可直接托管 `frontend/dist`，不需要 nginx。
>
> 本文记录当前代码行为。已知的前后端契约问题集中列在第 9 节，不能把它们当成已完成能力。

## 1. 技术栈与运行方式

| 层 | 当前实现 |
|---|---|
| 框架 | Vue 3 Composition API |
| 语言 | TypeScript |
| UI | Naive UI |
| 状态 | Pinia：`auth`、`health`、`providers` |
| 路由 | vue-router，history 模式 |
| HTTP | axios，`baseURL=/api/v1`，超时 10 秒 |
| 图表 | ECharts + vue-echarts |
| 拖拽 | vuedraggable |
| 构建 | Vite，产物在 `frontend/dist` |

开发模式：

```bash
cd frontend
npm install
npm run dev
```

Vite 固定监听 `5180`，并把 `/api`、`/healthz`、`/readyz` 代理到
`http://localhost:8080`。后端端口变化时要同步修改 `frontend/vite.config.ts`。

生产构建：

```bash
cd frontend
npm run build
```

构建命令先执行 `vue-tsc -b`，再执行 `vite build`。本地从仓库根目录启动 Gateway 时，
可将 `server.static_dir` 配为 `frontend/dist`；Docker 镜像使用 `/app/web/dist`。

## 2. 页面与导航

| 路径 | 视图 | 当前用途 |
|---|---|---|
| `/login` | `Login.vue` | 管理员登录 |
| `/` | 重定向 | 跳转到 `/overview` |
| `/overview` | `Overview.vue` | 24h 指标、模型用量、Key Pool、额度汇总、指纹开关 |
| `/providers` | `Providers.vue` | 内置厂商只读列表 |
| `/provider-keys` | `ProviderKeys.vue` | 内置厂商上游 Key 的查看、新增、删除 |
| `/keys` | `Keys.vue` | Gateway Key 管理及上游绑定 |
| `/relay-stations` | `RelayStations.vue` | 中转站 CRUD |
| `/routing` | `Routing.vue` | 查看 catch-all，并调整自动模式的调度顺序 |
| `/usage` | `Usage.vue` | 趋势、模型聚合、Provider 分布、请求明细 |
| `/access-logs` | `AccessLogs.vue` | 接入日志筛选、详情和 JSONL 导出 |
| `/inflight` | `Inflight.vue` | 当前活跃请求，1 秒轮询 |
| `/models` | `ModelManager.vue` | 模型同步、面归属、无归属清理、定价 |
| `/admin-users` | `AdminUsers.vue` | 管理员账户管理，仅 root 可进入 |

主布局还提供：

- 亮色/暗色主题切换；偏好保存在浏览器本地。
- `/healthz` 健康状态，10 秒检查一次。
- 当前用户名和登出按钮。
- root 用户才显示“管理员用户”菜单，路由守卫也会再次校验角色。

## 3. 登录与会话

登录调用 `POST /api/v1/auth/login`。成功响应包含 session token，同时后端设置 HttpOnly
`session_token` cookie。前端把响应 token 保存为 localStorage 中的 `admin_token`，用它判断路由
是否已登录；管理 API 在浏览器中实际依靠 session cookie 通过后端认证。

当前 axios 拦截器还会发送 `Authorization: Bearer <token>`，但管理员认证中间件只读取：

1. `X-Admin-Token` header，适合 curl 或 API 调试工具。
2. `session_token` cookie，适合浏览器。

收到 401 时，前端删除本地 token 并跳转 `/login`。

`admin_auth.enabled: true` 时，首次启动会确保至少存在一个 root 账户。默认账户为
`admin` / `Gateway@2026`，部署后应立即重置密码。登录失败次数、封禁时间和 session 有效期由
`admin_auth` 配置控制。

当前限制：前端路由守卫无条件要求 `admin_token`。当 `admin_auth.enabled: false` 时，登录接口只
返回 `feature_disabled`，因此管理 API 虽未加管理员中间件，浏览器 UI 却无法进入。详见第 9 节。

## 4. 厂商、上游 Key 与 Gateway Key

### 4.1 厂商列表 `/providers`

该页按 vendor 聚合内置厂商，展示：

- 注册协议面及协议。
- 模型并集。
- Key Pool 的 active / total / cooling / disabled 数量。
- Circuit Breaker 状态和窗口内失败数。

中转站被明确过滤，不会出现在该页；中转站统一在 `/relay-stations` 管理。

### 4.2 上游 Key `/provider-keys`

该页一次加载并合并所有内置厂商的上游 Key，不是“先选厂商再查看”的详情页。中转站 Key 被
过滤，因为中转站的 Key 由 `/relay-stations` 中每行一个的 Key 列表维护。

列表展示 ID、vendor、名称、脱敏 Key、计费来源、运行时状态、创建时间和额度。状态呈现规则：

- 熔断打开时优先显示“熔断中”。
- `ACTIVE`、`COOLING`、`QUOTA_EXCEEDED` 按各自状态显示。
- `enabled=false` 显示“已关闭”。
- `LIMITED` 是后端预留状态，前端没有专门中文文案。

额度颜色阈值来自 `GET /api/v1/config/quota`。百分比额度按绝对阈值判断，货币额度按同
provider、同计费层的最大已知余额相对判断。

当前页面只支持：

- 新增：选择 vendor、协议范围、名称、API Key、计费来源和初始启用状态。
- 删除。

页面没有编辑、启停切换或“手动标记 QUOTA_EXCEEDED”按钮，不能写成完整 CRUD。

### 4.3 Gateway Key `/keys`

Gateway Key 是客户端访问代理端点时使用的凭证。创建时由后端自动生成 `gw-...`，表单支持：

- 绑定一个或多个 vendor；空表示不限制 vendor，中转站也可绑定。
- 绑定具体 Provider Key ID；空表示使用所选 vendor 的整个 Key Pool。
- 设置模型白名单，`*` 表示所有模型。
- 按模型面归属分组选项，区分“仅此面”和“多面共有”。
- 表单可设置 RPM、TPM、默认模型和启用状态。

编辑以 Key 名称为 URL 标识，名称和密钥本身不可修改；可修改绑定、白名单、限流和启用状态。

这些控件不代表所有值都已形成运行时契约：`enabled=false` 当前不会阻止该 Gateway Key
认证，紧急吊销必须删除 Key；TPM 只记录实际消耗，不会拒绝请求；`default_model` 的 PUT
更新不持久化，Gateway Key CRUD 的全量内存重载还会丢失所有默认模型，直到配置热重载或
进程重启。RPM、provider/key 绑定和模型白名单仍会生效。

创建弹窗声明明文只展示一次，但当前 `GET /api/v1/keys` 实际仍返回明文，列表也提供点击复制。
这是已知安全契约问题。部署方必须把整个管理 UI 和 `/api/v1` 管理端点视为高敏感面，并使用
管理员认证、HTTPS 和网络访问控制；不要依据“一次展示”文案判断密钥已经不可再次读取。

## 5. 中转站与路由

### 5.1 中转站 `/relay-stations`

页面支持创建、编辑、删除和刷新列表。配置字段包括：

- 英文名称和显示名称。
- Base URL。
- 单协议或多协议模式。
- 主协议及多协议模式下的支持协议：OpenAI、Anthropic、Google。
- 超时，UI 允许 1 到 600 秒，新增表单默认 400 秒。
- 计费来源。
- API Keys，每行一个。
- 启用状态。

页面没有“热重载”按钮。后端在创建、更新、删除成功后自动调用中转站重载函数；另有
`POST /api/v1/relay-stations/reload` 管理端点，但当前页面不调用它。

中转站的 Key 列表是同步源：重载时按 Key 最后 8 个字符生成名称，并同步到
`provider_api_keys`；短于 8 个字符的值不会被同步。删除中转站还会清理其模型面归属、路由
顺序和相关上游 Key。

中转站 UI 的 `billing_source` 选项为 `token_plan`、`api`、`free`；动态加载时会登记到
face/vendor，并作为新同步 Provider Key 的默认值，空值默认 `api`。运行时 KeyPool 以每把
key 自身的 `BillingSource` 调度，因此已有同后缀 key 不会因站点 reload 自动改写 key 或计费层。
修改后需执行站点 reload；若要改变旧 key 的 tier，先显式更新或删除并重建，再从 Provider/
Access Log 核对候选层和实际 key。当前 Provider Keys 页面没有 relay key 的编辑入口，不要
直接在未备份的生产数据库中改值。

协议控件仍超出当前生产承诺范围：Google 虽可选择，但 relay 构造器会拒绝加载；`multi`
模式的 face protocol、pool/endpoint 回退和未知 path 门禁已有工作树实现及针对性测试，但
尚未完成真实 DB、热重载和并发生产矩阵验收。当前只应创建名称互不冲突的 `single` 站点，
并选择 OpenAI 或 Anthropic；同一上游的两个协议面应建成两个独立站点。完整限制见
[`relay-stations.md`](relay-stations.md)。

### 5.2 路由 `/routing`

该页不会编辑 `routing.catch_all` 或 `default_strategy`：

- 显式 catch-all 列表只读展示 Provider、Model、Priority、Weight。
- 自动模式（catch-all 的 Provider 列表为空）显示调度顺序树。
- 设计用途是调整同计费层的 provider 顺序和同 provider 内 Key 顺序。
- 保存通过 `PUT /api/v1/routing/order` 写 `route_order`，下一次调度热生效。

当前树只渲染 `token_plan` 和 `api` 两层；`free` Key 可以参与后端调度，但页面无法查看或调整
其顺序。拖拽组件目前还允许把 provider 或 Key 放入其他列表，但后端不会因此改变计费层或
Key 所属 provider；不要跨列表拖拽并保存。

## 6. 观测页面

### 6.1 Overview `/overview`

页面首次加载显示骨架，之后每 15 秒静默刷新 `/dashboard`。内容包括：

- 24h 总请求数、总 Token、错误数、总费用。
- 按 Model 的请求、Token、费用。
- 各 Key Pool 的总数、Active、Cooling、Disabled。
- 各 Pool 已轮询 Key 数和 QuotaKnownSum；百分比池不做可加总余额展示。
- 设备指纹归一化运行时开关及 canonical device ID。

指纹开关只热切换 `enabled`；canonical device ID 不能在页面修改。

### 6.2 Usage `/usage`

页面包含三块：

1. 请求趋势：从 `/usage` 最多取最近 1000 条，在前端分桶；跨度不超过 48 小时按小时，否则
   按天。它不是后端全量时间序列端点。
2. 按 Model 聚合：可输入 RFC3339 开始/结束时间，显示 Provider 分布、缓存输入、未缓存输入、
   输出、总 Token、错误、平均延迟、TPS 和平均 TTFT。
3. 最近请求：后端分页，显示 Provider、Model、协议、状态、延迟、Token、TPS、TTFT 和 Trace。

TPS 可切换为输出 Token/秒或总 Token/秒。缓存与未缓存输入的归一规则在后端
`internal/usage/tokensplit.go`，前端只展示后端结果。

流式响应的完整 body 是 SSE，Access Logs 无法整体 `JSON.parse`；流式请求的可靠 Token、TPS
和 TTFT 应查看 Usage。

### 6.3 Access Logs `/access-logs`

页面顶部三个数字是最近 24 小时的总数、错误数和活跃 Gateway Key 数；下方列表本身没有自动
附加 24h 起始时间，会查询数据库当前保留期内的全部记录并分页展示。

筛选维度：Trace ID、Gateway Key、vendor、Model 和多选状态桶。点击行打开详情抽屉，展示
metadata、请求/响应 body，并可在人类可读和原始 JSON 之间切换。流式 SSE 无法解析时回退显示
原始文本。

“导出 JSONL”复用当前筛选条件，页面固定请求最多 10000 条。底层 body 不是滚动 JSONL：每个
请求分别存 `*-req.json`、`*-resp.json`，单个 body 上限 16 MiB；JSONL/NDJSON 只是导出格式。
保留期由 `server.access_log.retention` 决定，不应在前端文档中固定写成 24 小时或 30 天。

### 6.4 Inflight `/inflight`

每秒调用 `GET /api/v1/inflight`，显示 Trace、请求模型、实际模型、Provider、Gateway Key、是否
流式和已耗时。接口返回 `started_at`，但当前表格不显示开始时间。

## 7. 模型与管理员

### 7.1 模型 `/models`

模型数据按 vendor 分卡片展示，支持：

- 单厂商同步：`POST /providers/sync-models`。
- 全部同步：`POST /providers/sync-all-models`；单个 vendor 失败不阻断其他结果。
- 按注册面筛选模型，展示一个模型所属的一个或多个面。
- 标记不属于任何现存面的模型，并经二次确认清理；清理会同时删除其手工价格。
- 编辑每百万 Token 的输入、缓存命中、输出三档价格，输入框失焦时保存。

同步只更新模型及面归属，不提供价格；三档价格需要本地维护，三项均为 0 时显示“未定价”。

### 7.2 管理员 `/admin-users`

菜单和路由仅 root 可见。当前确认可用的页面操作是：

- 创建 `admin` 或 `root` 用户。
- 重置任意列表用户的密码。
- 删除非 root 用户；UI 禁止删除所有 root 用户。

后端还支持 `readonly` 角色和当前用户修改密码端点，但 UI 没有对应选项或页面。

锁定状态、失败次数和解锁当前存在字段契约错误：前端读取 `locked` / `login_attempts` 并发送
`locked:false`，后端实际字段是 `locked_until` / `failed_attempts`，更新接口接受的是
`enabled`。因此不要把“查看锁定状态、解锁账户”写成当前可用功能。

## 8. 页面与 API 对照

下表路径均以 `/api/v1` 为前缀。

| 页面 | 当前调用 |
|---|---|
| Login | `POST /auth/login`、`GET /auth/me`、`POST /auth/logout` |
| Overview | `GET /dashboard`、`GET/PUT /fingerprint` |
| Providers | `GET /providers` |
| ProviderKeys | `GET /providers`、`GET/POST /providers/:name/api-keys`、`DELETE /providers/:name/api-keys/:id`、`GET /config/quota` |
| Keys | `GET/POST /keys`、`PUT/DELETE /keys/:name`、`GET /providers`、`GET /providers/:name/api-keys`、`GET /providers/models` |
| RelayStations | `GET/POST /relay-stations`、`PUT/DELETE /relay-stations/:id` |
| Routing | `GET /routing`、`GET /providers`、`GET/PUT /routing/order`、`GET /providers/:name/api-keys` |
| Usage | `GET /usage`、`GET /usage/aggregate`、`GET /usage/by_model/:model_id/providers` |
| AccessLogs | `GET /access-logs`、`GET /access-logs/stats`、`GET /access-logs/:id/detail`、`GET /access-logs/export`、`GET /keys`、`GET /providers`、`GET /providers/registered` |
| Inflight | `GET /inflight` |
| Models | `GET /providers/models`、`POST /providers/sync-models`、`POST /providers/sync-all-models`、`PUT /providers/models`、`POST /providers/models/prune` |
| AdminUsers | `GET/POST /admin-users`、`PUT/DELETE /admin-users/:id`、`POST /admin-users/:id/reset-password` |

## 9. 已知前端/契约问题

这些是当前代码事实，不是文档待办的推测：

1. `admin_auth.enabled=false` 时管理 API 不受管理员中间件保护，但前端仍要求登录，导致 UI
   无法进入。
2. axios 发送 `Authorization: Bearer`，管理员中间件不读取该 header；浏览器依靠 HttpOnly
   cookie，命令行应使用 `X-Admin-Token`。
3. Gateway Key 文案声明明文只展示一次，但 `GET /keys` 当前仍返回明文。
4. Gateway Key 的“禁用”只改变数据库/UI；运行时仍接受该 Key，可靠吊销方式是删除。
5. Gateway Key TPM 当前只记账不拒绝；`default_model` 更新不持久化，CRUD 重载还会丢失
   内存默认模型。
6. AdminUsers 的锁定字段和解锁请求与后端不匹配。
7. RelayStations 的计费来源后端运行时已支持 `token_plan|api|free`、station 默认层和新同步
   key；KeyPool 按每把 key 调度，旧 key 不会因重复同步自动改层，修改后需 reload 并核对
   候选顺序，必要时显式更新/重建旧 key。
8. RelayStations 仍提供 Google 选项，但后端构造器会拒绝加载。
9. RelayStations 的 `multi` 模式已有工作树 face/protocol/pool/path 修复和针对性测试，但尚未
   完成真实 DB/热重载/并发生产矩阵验收；生产仍建议使用 `single`。
10. Routing 页面没有 `free` 层，也不能编辑 catch-all 或默认策略。
11. ProviderKeys 页面没有编辑、启停或手动标记额度耗尽操作。
12. Access Logs 标题含“24h”，但只有统计徽标是 24h，列表查询范围由实际保留数据决定。
13. Routing 拖拽允许跨层或跨 provider 放置，但保存端点只改顺序，不改实际归属。

修复这些问题时，应同步更新本文件和 `frontend/README.md`，并以实际 API handler 与页面调用为准。
