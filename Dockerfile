# Native LLM Gateway — 单镜像构建
# 多阶段:前端(node)构建 → 后端(golang)构建 → 精简运行时(alpine)
# 运行时单进程:gateway 直接托管前端静态文件(方案 B),无 nginx
#
# 部署要点(详见 README「Docker 部署」):
#   - config.yaml 含真实凭据,不进镜像 — 部署时挂载覆盖 /app/config.yaml
#   - 数据(DB / key-state.json / access body)在 /app/data,挂 volume
#   - 数据库 driver 由挂载的 config 决定:sqlite 或 postgres

# ---- 阶段 1:构建前端 ----
FROM node:20-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# vue-tsc 类型检查 + vite build → dist/
RUN npm run build

# ---- 阶段 2:构建后端 ----
FROM golang:1.23-alpine AS backend
WORKDIR /src
# 依赖分层缓存:只 COPY go.mod/go.sum 先下载
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# CGO_ENABLED=0:sqlite(glebarez 纯 Go)与 postgres 驱动都不需要 CGO
RUN CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway

# ---- 阶段 3:运行时 ----
FROM alpine:3.20
# ca-certificates:上游 https 请求;tzdata:时区数据
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=backend /out/gateway /app/gateway
# 前端构建产物 — 运行时由 config 的 server.static_dir 指向 /app/web/dist
COPY --from=frontend /src/frontend/dist /app/web/dist
# 示例配置(不含真实凭据);部署时挂载自己的 config.yaml 覆盖
COPY config.example.yaml /app/config.example.yaml
ENV GIN_MODE=release
# 数据目录:DB(dsn 指向 /app/data/gateway.db,key-state.json 快照自动跟随)+ access body
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/gateway", "-c", "/app/config.yaml"]
