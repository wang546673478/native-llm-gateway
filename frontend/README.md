# Native LLM Gateway — Frontend

Vue 3 + TypeScript + Vite + Pinia + Naive UI

## 开发

```bash
npm install
npm run dev          # 起在 :5173,代理 /api 到 :8080
```

## 生产构建

```bash
npm run build        # 输出到 dist/
npm run preview      # 本地预览生产构建
```

## 集成到 Gateway 二进制

可以让 Gateway 直接 serve 前端 dist(简化部署),后续阶段接入。

## 页面

| 路径 | 功能 | API |
|------|------|-----|
| `/overview` | 24h 总览 + 聚合 | `/api/v1/dashboard` |
| `/providers` | Provider 列表(按厂商聚合) | `/api/v1/providers` |
| `/provider-keys` | 厂商 key 池(状态/额度/per-key 熔断) | `/api/v1/providers/:name/api-keys` |
| `/keys` | Gateway Key 列表(脱敏) | `/api/v1/keys` |
| `/routing` | catch_all 自动模式状态 | `/api/v1/routing` |
| `/usage` | 用量查询 + 聚合 | `/api/v1/usage`, `/api/v1/usage/aggregate` |
| `/access-logs` | 接入日志 + 详情(人类可读/原始 JSON 切换) | `/api/v1/access-logs`, `/:id/detail` |

> 详情页的 token 用量行(输入/输出/缓存读/缓存写)只对**非流式**请求显示 —— 流式响应存的是 SSE 文本,前端无法整体 JSON.parse;流式请求的 token 数看用量页(`/usage`,数据来自内存解析的 usage_records,不受影响)。
