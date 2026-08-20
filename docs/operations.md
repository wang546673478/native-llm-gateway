# 部署 / 运维 / 脚本

---

## 1. 三种部署方式

### 1.1 Docker(推荐)

镜像 `wuhuhhhh/native-llm-gateway`(Docker Hub),**每次 push main 后 GitHub Actions 自动构建推送**(`latest` + commit sha 双 tag,可回滚)。

#### 1.1.1 快速部署(gateway + PostgreSQL)

```yaml
# docker-compose.yml(仓库根目录)
services:
  gateway:
    image: wuhuhhhh/native-llm-gateway:latest
    container_name: llm-gateway
    ports: ["8080:8080"]
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - ./gateway-data:/app/data
    depends_on:
      postgres: { condition: service_healthy }
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
      POSTGRES_PASSWORD: CHANGE_ME
      POSTGRES_DB: gateway
    volumes: [./pg-data:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gateway -d gateway"]
      interval: 10s
      timeout: 5s
      retries: 5
    restart: unless-stopped
```

```bash
cp config.docker.example.yaml config.yaml
# ① 改 database.dsn 的 CHANGE_ME 密码
# ② 改 auth.keys 的默认 dev-key
docker compose up -d
```

#### 1.1.2 最小部署(单容器 + SQLite)

```bash
docker run -d --name llm-gateway \
  -p 8080:8080 \
  -v $PWD/config.yaml:/app/config.yaml:ro \
  -v $PWD/gateway-data:/app/data \
  wuhuhhhh/native-llm-gateway:latest
```

### 1.2 本机 systemd(systemd 托管,2026-08-07 起)

适合单机长期运行,配置文件 + systemd unit 模板见 `scripts/llm-gateway.service`(若有)。

```bash
sudo systemctl status llm-gateway
sudo systemctl restart llm-gateway
```

**`gateway-ctl.sh`**:被 Makefile 调用的 helper,2026-08-07 起 systemd 托管,stop/status 委托 `systemctl`,不再直接 kill(裸 kill 会被 Restart=always 拉起)。

### 1.3 本机裸跑(开发)

```bash
make build                          # 编译到 bin/gateway
make start                          # 后台启动(写 PID 到 /tmp/gateway.pid)
make status                         # 进程 + 端口 + /healthz
make logs                           # tail -f /tmp/gateway.log
make stop                           # 优雅停止
make restart                        # stop + start
make test                           # 跑所有单元测试
make all                            # build + test + vet
```

---

## 2. 关键脚本

### 2.1 `gateway-reload.sh`(无感重载)

```bash
./gateway-reload.sh
```

**3 步**:
1. 编译新二进制到 `bin/gateway.new`
2. `mv` 替换为 `bin/gateway`
3. `sudo systemctl restart llm-gateway`(systemd 完成 SIGTERM 优雅排空 + 新进程接管)

**适用场景**:
- 加了厂商包
- 改了 provider 代码
- 改了 proxy / router / pool 等运行时逻辑

**已知代价**:
- 重载会重置内存状态(熔断器 / 配额标记)
- 从 `key-state.json` 快照恢复 QE/COOLING/余额
- 排空窗口(`shutdown_timeout`,默认 30s)内未结束的长流会被掐断

### 2.2 `gateway-log-rotate.sh`(日志轮转)

按天轮转 + 清理 7 天前归档。被两处调用:
- systemd `llm-gateway.service` 的 `ExecStartPre`(每次启动前)
- `gateway-reload.sh`(重载前)

幂等:同一天重复执行无操作。

### 2.3 `pg-init.sh`(PG 初始化)

```bash
sudo bash scripts/pg-init.sh
```

- 随机 16 字节 hex 密码
- 写入 `/home/hhhh/llm-gateway-data/pg-password.txt`(0600)
- 建 `gateway` 角色 + `gateway` 库 + `gateway_test` 库
- 幂等:重复运行重置密码、保留已建库

### 2.4 `Makefile`

```bash
make help          # 默认目标,显示用法
make build         # 编译到 bin/gateway
make start|stop|restart|status|logs
make test          # 跑所有单元测试
make test-verbose  # 详细输出
make vet           # go vet
make all           # build + test + vet
make frontend      # 构建前端生产版本
make frontend-dev  # vite dev server :5180
make clean         # 清构建产物 + 临时数据
```

可覆盖变量:
- `CONFIG=...`(默认 `$(pwd)/config.yaml`)
- `PORT=...`(默认 8080)
- `LOG=...`、`PIDFILE=...`、`DB_PATH=...`

---

## 3. 数据持久化

### 3.1 目录约定

| 路径 | 内容 |
|---|---|
| `gateway-data/` | DB / key-state.json / access body |
| `logs/` | gateway.log(按天轮转,7 天清理) |
| `pg-data/` | PG only,PG 数据目录 |

### 3.2 key-state.json 位置

- **SQLite**:`$(dirname $dsn)/key-state.json`(= `/tmp/gateway-data/` 同目录)
- **PostgreSQL**:`./key-state.json`(cwd;systemd 下即仓库根,持久)

### 3.3 备份

- PG 模式:每日 3:07 `pg_dump` 备份(保留 SQLite 归档 7 天后清理)
- SQLite 模式:不自动备份,建议 `cp /tmp/gateway-data/gateway.db{,.bak}`

---

## 4. 监控告警

### 4.1 Prometheus 抓取

```
GET /metrics    # Prometheus 格式
```

### 4.2 关键指标

| 指标 | 告警阈值 |
|---|---|
| `gateway_requests_total{error_type="..."}` | 错误率 > 5% |
| `gateway_quota_key_status_transitions_total{from,to}` | 大量 ACTIVE → QUOTA_EXCEEDED |
| `gateway_quota_pending_probes` | 持续 > 100(队列堵塞) |
| 进程内存 | > 1GB(可能是 access log buffer 积压) |
| 进程 UP | 进程 down |

### 4.3 /healthz 和 /readyz

- `GET /healthz` — 进程级,返回 200 = 在跑
- `GET /readyz` — 进程级 + DB ping;DB 不可用返回 503

---

## 5. 故障排查三板斧

### 5.1 access logs 定层

```
GET /api/v1/access-logs?limit=N
```

状态码 / error_type / provider / 延迟一眼定位:
- 401/403 → 客户端鉴权
- 503 → 路由无候选
- 5xx + provider_name → 上游错误

### 5.2 gateway 日志定因

```
grep "trace_id=xxx" logs/gateway.log
```

白名单跳过 / failover / 熔断转移 / poll 错误都有行。

### 5.3 直连上游对照

从 DB 取真实 key(`provider_api_keys.key_hash` 存原值)直接 curl 上游端点,排除网关层干扰。

**「网关 200 空流」和「上游就没回内容」必须分开定位**。

---

## 6. 升级路径

### 6.1 升级前检查

```bash
git pull
git log --oneline -10   # 看最近改了什么
make test               # 跑测试
./gateway-reload.sh     # 重载
```

### 6.2 schema 变更

GORM AutoMigrate 自动处理**新增字段**(改 `database/models.go` struct + gorm tag)。

**删字段 / 删关联必须手工执行**(AutoMigrate 只加不删,踩坑 #23):

```sql
-- 先确认空表(有行则先备份),再删
ALTER TABLE <表> DROP COLUMN <列>;
ALTER TABLE <表> DROP CONSTRAINT <约束名>;
```

删除后立刻重启验证 —— AutoMigrate 失败是致命错误会中止启动,漂移一定会以
「起不来」的形式暴露。`migrations/00X.up.sql` 机制已废弃(2026-08-20 决定:
保持 AutoMigrate 现状,靠 CLAUDE.md 提交前自检清单补「减法无人负责」的缺口)。

---

## 7. 常见故障

| 故障 | 定位 | 修复 |
|---|---|---|
| 500 错误,网页报失败 | 进程没在跑(`make status`) | `make start` 或 systemd 启动 |
| 401 客户端 | Gateway Key 没创建 / 被删 / hash 不匹配 | `GET /api/v1/keys` 检查 |
| 403 model_not_allowed | 白名单不含客户端发的模型 | 改 key 的 allowed_models |
| 503 no_route | catch_all 配置空了 / 所有 provider 禁用 | `GET /api/v1/routing` 检查 |
| 整链掉到 deepseek | 配额耗尽 / 熔断 | `GET /api/v1/providers` 看 key 状态 |
| 启动后 key 状态全 ACTIVE | `key-state.json` 未恢复 | 检查 `keyStateSnapshotPath` 路径 |
| 流式 token 不显示 | 详情页只解析非流式 body | 改去 Usage 页 |
| 网页全空白 | 后端返回 null + 模板 .length → TypeError | 后端 append([]Ts{}) / 前端 ?? [] |
