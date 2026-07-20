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
| `/providers` | Provider 列表 + KeyPool + CB | `/api/v1/providers` |
| `/keys` | Gateway Key 列表(脱敏) | `/api/v1/keys` |
| `/routing` | 别名路由规则 | `/api/v1/routing` |
| `/usage` | 用量查询 + 聚合 | `/api/v1/usage`, `/api/v1/usage/aggregate` |
