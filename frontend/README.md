# Native LLM Gateway Frontend

Vue 3 + TypeScript + Vite + Pinia + Naive UI 管理后台。

完整的页面行为、API 映射和已知契约问题见
[`docs/frontend.md`](../docs/frontend.md)。本文件只保留前端开发入口，避免维护两份功能说明。

## 开发

先启动后端并监听 `8080`，再运行：

```bash
cd frontend
npm install
npm run dev
```

开发地址：`http://localhost:5180`。

`vite.config.ts` 将 `/api`、`/healthz`、`/readyz` 代理到 `http://localhost:8080`。后端端口变化
时必须同步修改代理目标。

## 构建

```bash
cd frontend
npm run build
npm run preview
```

`npm run build` 执行 TypeScript 检查并输出 `dist/`。Gateway 已支持直接托管该目录并对前端
history 路由回退到 `index.html`；Docker 镜像也会在构建阶段生成并复制这份产物，不需要 nginx。

## 当前路由

| 路径 | 页面 |
|---|---|
| `/login` | 管理员登录 |
| `/overview` | 总览、Key Pool、额度、指纹开关 |
| `/providers` | 内置厂商列表 |
| `/provider-keys` | 内置厂商上游 Key |
| `/keys` | Gateway Key |
| `/relay-stations` | 中转站 |
| `/routing` | catch-all 状态和调度顺序 |
| `/usage` | 用量趋势、聚合和明细 |
| `/access-logs` | 接入日志、详情和导出 |
| `/inflight` | 活跃请求 |
| `/models` | 模型同步、归属和定价 |
| `/admin-users` | 管理员用户，仅 root |

## 代码入口

| 位置 | 职责 |
|---|---|
| `src/router/index.ts` | 页面路由和登录/root 守卫 |
| `src/api/client.ts` | axios 客户端、类型和管理 API |
| `src/stores/` | auth、health、providers 状态 |
| `src/views/` | 页面实现 |
| `src/components/` | 统计卡、趋势图、骨架屏、主题切换 |
| `src/styles/tokens.css` | 亮/暗主题语义变量 |

## 当前注意事项

- `admin_auth.enabled=false` 时，前端仍会跳转登录页，而登录接口返回功能未启用；UI 当前无法
  在这种配置下使用。
- Gateway Key 弹窗写“只展示一次”，但当前列表接口仍返回明文。管理后台必须按高敏感系统
  保护。
- AdminUsers 的锁定状态、失败次数和解锁请求字段与后端不一致。
- 中转站计费来源当前不会正确传递到同步生成的 Provider Key，实际仍按 `api`。
- 中转站页面仍可选择 Google，但后端会拒绝加载；`multi` 模式的跨协议运行也不可靠。
- Routing 只显示 `token_plan`、`api` 两层，不能编辑 catch-all，也不显示 `free` 层。
- Routing 拖拽应限制在原层、原 provider 内；当前 UI 允许跨列表放置，但后端不会改变归属。
- Provider Keys 目前只支持新增和删除，不支持编辑或手动标记额度耗尽。
