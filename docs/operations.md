# 部署与运维

本文描述仓库当前可用的启动、监控、备份和回滚路径。配置字段及热重载边界见
[`config-reference.md`](config-reference.md)，故障定位见
[`踩坑与排错.md`](踩坑与排错.md)。

## 运行前检查

最低依赖：

- 后端构建：Go 1.23+
- 前端构建：Node.js 20+ 和 npm
- 本地低负载：SQLite
- 生产：PostgreSQL 16+
- 容器部署：Docker Engine 与 Compose v2

准备配置：

```bash
# 本机
cp config.example.yaml config.yaml

# Docker
cp config.docker.example.yaml config.yaml
```

上线前至少完成以下项目：

1. 保持 `auth.enabled: true`，替换或删除模板里的 bootstrap Gateway Key。
2. 保持 `admin_auth.enabled: true`，显式填写 `session_ttl`、
   `max_login_attempts`、`login_ban_duration`。
3. PostgreSQL DSN 和 Compose 的 `POSTGRES_PASSWORD` 使用同一个非默认强密码。
4. 只启用实际使用的内置协议面；Provider key 在管理页面添加，不写入 YAML。
5. 确定是否开启 access log。body 文件可能含完整提示词、响应及客户端附带的敏感信息。
6. 限制数据库、`config.yaml`、备份目录和 access body 目录的文件权限。

首次启用管理员认证且数据库没有 root 用户时，服务会创建
`admin / Gateway@2026`，并把默认密码写入启动日志。首次登录后立即修改。

## 本地运行

构建并以前台进程运行：

```bash
npm --prefix frontend ci
npm --prefix frontend run build
make build
./bin/gateway --config ./config.yaml
```

默认地址：

| 入口 | 地址 |
|---|---|
| 管理页面 | `http://127.0.0.1:8080/` |
| 存活检查 | `GET /healthz` |
| 就绪检查 | `GET /readyz` |
| Prometheus | `GET /metrics` |

`/healthz` 只表示 HTTP 进程可响应；`/readyz` 会在 1 秒超时内 ping 数据库，负载均衡器
应以 `/readyz` 作为接流条件。

前端开发服务器：

```bash
npm --prefix frontend run dev -- --host 0.0.0.0 --port 5180
```

Vite 当前把 `/api` 代理到 `http://localhost:8080`。后端端口变化时同步修改
`frontend/vite.config.ts`。

### Makefile 边界

`make test`、`make vet`、`make build`、`make frontend` 可直接使用。当前进程管理目标存在
混合语义：`make start` 用 `nohup` 启动本地进程，而 `make stop` 和 `make status` 委托名为
`llm-gateway` 的 systemd 服务。没有安装该服务时，不要用这组命令管理同一个开发进程；
以前台运行和 Ctrl-C 结束最明确。

## Docker Compose

仓库的 `docker-compose.yml` 启动单体 gateway 镜像和 PostgreSQL：

```bash
cp config.docker.example.yaml config.yaml
# 同时修改 config.yaml 与 docker-compose.yml 中的数据库密码
# 替换 bootstrap Gateway Key
docker compose up -d
docker compose ps
docker compose logs -f gateway
```

持久化目录：

| 主机路径 | 容器用途 |
|---|---|
| `./config.yaml` | 只读挂载为 `/app/config.yaml` |
| `./gateway-data` | access body；当前不包含 PostgreSQL 模式的 `key-state.json` |
| `./pg-data` | PostgreSQL 数据目录 |

镜像内包含构建后的前端，由 Go 服务从 `/app/web/dist` 托管。Compose 健康检查访问
`/healthz`；业务就绪监控仍建议额外检查 `/readyz`。

更新镜像：

```bash
docker compose pull gateway
docker compose up -d gateway
curl -fsS http://127.0.0.1:8080/readyz
```

`main` 分支中影响应用或镜像的文件变更会触发 `.github/workflows/docker.yml`。流水线执行
后端构建、带临时 PostgreSQL 的测试和前端构建，再发布 Docker Hub 的 `latest` 与 commit
SHA tag。纯文档变更不会触发该 workflow。

## systemd

仓库当前没有可直接安装的 unit 文件，但部署脚本约定服务名是 `llm-gateway`，二进制是
仓库内 `bin/gateway`，且 unit 应配置自动重启。一个最小 unit 应具备：

```ini
[Unit]
Description=Native LLM Gateway
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/native-llm-gateway
ExecStart=/opt/native-llm-gateway/bin/gateway --config /opt/native-llm-gateway/config.yaml
Restart=always
RestartSec=3
TimeoutStopSec=45

[Install]
WantedBy=multi-user.target
```

按实际部署目录调整路径和运行用户。安装后：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now llm-gateway
systemctl status llm-gateway
journalctl -u llm-gateway -f
```

服务收到 SIGINT/SIGTERM 后，先停止 quota worker，再等待 HTTP 请求排空，随后停止并 flush
usage/access-log，最后保存 key 状态。超过 `server.shutdown_timeout` 的长流可能被中止。

## 配置与运行时更新

进程通过文件 watcher 监听启动时指定的 YAML。当前 watcher 可热更新路由规则、Gateway
Key 快照、部分 provider 元数据和 quota worker 参数。数据库、HTTP 监听、静态目录、
access log、usage collector、provider endpoint/timeout、fingerprint 配置值和管理员认证参数
仍需重启。完整矩阵见配置参考。

管理页面/API 的下列写操作有独立热更新路径：

- Gateway Key CRUD
- Provider Key CRUD
- provider/key 调度顺序保存
- 模型同步与价格保存
- MiMo quota cookie 更新
- 中转站创建、编辑、删除和 reload
- fingerprint `enabled` 开关（不写回 YAML）

文件 watcher 的热更新是部分更新，不能替代一次受控重启。容器挂载文件被编辑器原子替换时，
文件系统事件也可能因平台不同而不稳定；容器部署修改配置后执行 `docker compose up -d
gateway` 更可靠。

## 发布与回滚脚本

### `scripts/gateway-deploy.sh`

该脚本只适用于已安装 `llm-gateway` systemd 服务的本机部署：

```bash
./scripts/gateway-deploy.sh
```

它会依次执行后端全量测试、备份当前二进制、编译 `bin/gateway.new`、替换二进制、终止
当前 MainPID 让 `Restart=always` 拉起新进程，并等待 `/healthz`。新进程 30 秒内未就绪时
自动调用 rollback。可设置：

- `GATEWAY_PORT`：覆盖从 `config.yaml` 读取的端口。
- `SKIP_TEST=1`：跳过脚本内测试；只应在同一产物已完成验证时使用。

脚本不构建前端、不迁移外部 schema、不备份数据库，也不检查 `/readyz`。涉及这些内容时在
部署前单独完成。

### `scripts/gateway-backup.sh` 与 `gateway-rollback.sh`

backup 只复制 `bin/gateway`，不是数据备份。rollback 默认恢复 `bin/backups/` 最新文件，
并依赖 systemd 自动重启：

```bash
./scripts/gateway-rollback.sh --list
./scripts/gateway-rollback.sh
./scripts/gateway-rollback.sh bin/backups/gateway.YYYYMMDD-HHMMSS
```

backup 脚本按修改时间保留最近 5 份 `gateway.<timestamp>`，rollback 产生的
`gateway.pre-rollback.*` 不在自动轮换集合内。二进制备份仍不能替代数据库和配置备份。

### 其他脚本

| 脚本 | 当前用途与限制 |
|---|---|
| `gateway-health-check.sh` | systemd 主机诊断；默认配置路径写死为仓库开发路径，可用 `CONFIG_FILE`、`GATEWAY_URL` 覆盖 |
| `gateway-log-rotate.sh` | 轮转仓库 `logs/gateway.log` 并删除 7 天前归档；仓库没有自动调用它的 unit/cron |
| `gateway-ctl.sh` | 只管理 systemd 的 `llm-gateway`，不管理 `make start` 的 nohup 进程 |
| `pg-init.sh` | 本机创建 `gateway`/`gateway_test` 库并重置角色密码；默认写入机器专用路径，可用 `GATEWAY_DATA_DIR` 覆盖 |
| `sync-provider-models.sh` | 直接修改 PostgreSQL face 数据的维护工具；优先使用管理页面同步，执行前备份并显式设置 `DB_PASSWORD` |
| `orphan-*.sql` | 历史数据清理脚本，含固定的活面假设；不能直接用于未来生产清理，必须先审阅和备份 |

## 备份与恢复

仓库没有自动数据库备份计划。应由部署环境配置定时任务并验证恢复。

PostgreSQL 示例：

```bash
umask 077
pg_dump --format=custom "$DATABASE_URL" > gateway-$(date +%Y%m%d-%H%M%S).dump
pg_restore --clean --if-exists --dbname "$RESTORE_DATABASE_URL" gateway-YYYYMMDD-HHMMSS.dump
```

SQLite 在停机或确认 WAL 一致性的条件下备份，优先使用 SQLite backup 命令：

```bash
sqlite3 data/gateway.db '.backup gateway-backup.db'
```

数据库和备份含明文 Provider/Gateway key、relay key 与 session token，必须加密存储并限制
访问。恢复演练至少验证：管理员登录、Provider key、Gateway Key 绑定、模型/价格、路由顺序
和一条真实代理请求。

`key-state.json` 只保存瞬时 key 状态，不是业务数据备份：

- SQLite：位于数据库 DSN 同目录。
- PostgreSQL：位于进程工作目录。当前 Docker 工作目录是 `/app`，而 Compose 只挂载
  `/app/data`，所以 `/app/key-state.json` 不会随容器重建保留；这只影响瞬时调度状态。

## 监控

`/metrics` 当前始终注册在主 HTTP 端口，`metrics.enabled/path/port` 不控制它。主要指标：

| 指标 | 关注点 |
|---|---|
| `gateway_requests_total` | 按 provider/status/error_type 的请求与错误率 |
| `gateway_tokens_total` | 输入/输出 token 趋势 |
| `gateway_request_duration_seconds` | 延迟分布 |
| `gateway_stream_ttft_seconds` | relay 流首个 body/ping/data 延迟，按 provider/model/请求规模分层 |
| `gateway_relay_events_total` | relay 候选、首包超时、响应提交、流中断和透明性门禁事件 |
| `gateway_relay_active_upstreams` | 当前 relay 上游调用/流数量，用于发现连接未释放 |
| `gateway_quota_probe_total` | quota 恢复探测结果 |
| `gateway_quota_poll_total` | 余额轮询结果 |
| `gateway_quota_key_status_transitions_total` | key 状态变化 |
| `gateway_quota_pending_probes` | 等待恢复探测的 key 数 |

基础巡检：

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS http://127.0.0.1:8080/metrics | head
GATEWAY_URL=http://127.0.0.1:8080 \
CONFIG_FILE=/path/to/config.yaml \
./scripts/gateway-health-check.sh
```

建议对就绪失败、5xx/timeout/connection 错误率、持续增长的 pending probes、磁盘空间和
PostgreSQL 连接耗尽告警。不要只看最终 200：流式上游可在 HTTP 200 内发送结构化失败事件，
应结合 access log 的 `error_type` 和应用日志判断。

调整 `retry.relay_first_byte_timeout` 前，应分别查看冷/热请求以及各模型、请求规模的 TTFT
P50/P95/P99。默认 180s 是首轮保护值；预算到期只会切换尚未提交正文的 relay 候选，收到
ping/data 后该流已承诺且不再切换。修改该配置后需要重启 Gateway。

relay 灰度看板至少加入：

```promql
# 分 provider/请求规模/阶段的 1h TTFT P99
histogram_quantile(
  0.99,
  sum by (le, provider, model, request_size, phase) (
    rate(gateway_stream_ttft_seconds_bucket[1h])
  )
)

# headers 前与 headers 后正文静默的首包预算触发量
sum by (provider, stage) (
  increase(gateway_relay_events_total{event="first_byte_timeout"}[15m])
)

# 已提交流的中断原因
sum by (provider, stage) (
  increase(gateway_relay_events_total{event="stream_interrupted"}[15m])
)

# 当前活动上游；请求低谷仍持续不降需要排查泄漏
max by (provider) (gateway_relay_active_upstreams)
```

以下是发布硬门禁，任一结果大于 0 应立即停止灰度并回滚：

```promql
sum(increase(gateway_relay_events_total{event="body_mismatch"}[5m])) or vector(0)
sum(increase(gateway_relay_events_total{event="switch_after_response_committed"}[5m])) or vector(0)
```

取消后的额外候选数目前记录在 zap 的 `candidate chain canceled` 摘要中，字段必须保持
`post_cancel_candidate_count=0`。同一 trace 已取消后若仍出现新的候选日志，同样属于发布
阻断。`client_gone` 本身不能要求为 0：真实客户端取消或测首字时主动终止 curl 都会产生它。

## 日志与数据保留

- zap 日志实际写 stdout/stderr；`logging.output/file_path` 当前不生效。systemd 用
  journald，Docker 用容器日志驱动管理轮转。
- access-log metadata 在 `access_logs` 表；body 是 `body_dir` 下的独立 JSON 文件。
- access retention 默认/模板为 24 小时，只管理 access log；`usage.retention_days` 当前
  不会自动清理 `usage_records`。
- body 文件与数据库记录需要一起备份或一起清理，否则会出现孤儿文件/记录。

## 安全事件处理

发现仓库、日志、SQL 导出或聊天记录出现真实凭证时：

1. 立即在上游和数据库侧吊销/轮换，不要只删除文件。
2. 轮换 PostgreSQL 密码、Provider/Gateway key、relay key、MiMo cookie 和管理员 session。
3. 从当前工作树删除明文并检查 Git 历史、CI artifact、镜像 layer 与备份。
4. 用新凭证验证 `/readyz`、管理员登录和最小代理请求。

Git 历史重写会影响所有协作者，只有在明确协调后执行。
