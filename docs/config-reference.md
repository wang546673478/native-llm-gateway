# 配置 / 数据库 / 迁移

---

## 1. config.yaml 完整结构

```yaml
server:        # HTTP 服务
database:      # SQLite / PostgreSQL
redis:         # 预留(暂未用)
auth:          # 客户端鉴权
providers:     # 厂商配置
routing:       # 路由规则
keypool:       # key 池调度
timeouts:      # 预留
retry:         # 失败重试
logging:       # 日志
metrics:       # Prometheus
usage:         # 用量批量写入
```

---

## 2. server

```yaml
server:
  host: 0.0.0.0
  port: 8080
  read_timeout: 60s
  write_timeout: 120s
  idle_timeout: 120s
  shutdown_timeout: 30s
  static_dir: ""              # 前端构建产物目录(留空则不托管)
  access_log:
    enabled: true
    body_dir: data/access-body
    buffer_size: 1000
    batch_size: 100
    flush_interval: 5s
    retention: 720h            # 30 天
```

---

## 3. database

### 3.1 SQLite(单机/演示)

```yaml
database:
  driver: "sqlite"
  dsn: "/tmp/gateway-data/gateway.db"
```

### 3.2 PostgreSQL(生产)

```yaml
database:
  driver: "postgres"
  dsn: "postgres://gateway:password@localhost:5432/gateway?sslmode=disable"
```

### 3.3 何时用哪个

| 场景 | 推荐 |
|---|---|
| 单机 / 演示 / 流量小 | SQLite(零部署) |
| 生产 / 高并发 / 大流量 | PostgreSQL(并发写零锁) |

> access logs 每请求一条 + 8 索引,SQLite 单写者在请求量大时写锁排队(页面卡顿);PostgreSQL 并发写,`database.driver` 一键切换,代码层无差异(CI 每次提交跑 PG 集成测试)

---

## 4. auth

```yaml
auth:
  enabled: true               # f1bf2d6 起默认 true
  keys:
    - name: "default"
      key: "gw-xxx-xxx"      # 启动时 seed 到 DB
      allowed_models: ["*"]
      rate_limit:
        rpm: 100
        tpm: 500000
```

**关键**:
- `enabled: true` → 代理端点**必须**带认证 key,否则 401
- `keys` 是**种子**(首次启动 seed 到 DB),之后通过管理 API 改
- 创建响应里**包含明文 key,只展示一次**

---

## 5. providers

每个厂商一个块:

```yaml
providers:
  minimax:
    enabled: true
    billing_source: "token_plan"  # token_plan / api / free
    endpoint: "https://api.minimaxi.com/anthropic"
    protocol: "anthropic"
    timeout: 60s
    default_model: "MiniMax-M3"   # catch_all 自动模式用它承接
    responses_api: true           # 原生支持 /v1/responses
    force_thinking_disabled: false
    circuit_breaker:
      failure_threshold: 5        # 0 = 不启用
      failure_window: 60s
      open_timeout: 60s
      half_open_requests: 2
    models:
      - id: "MiniMax-M3"
        cost_per_1k_input: 0.001
        cost_per_1k_output: 0.002
        cost_per_1k_cache_read: 0.0001
        cost_per_1k_cache_creation: 0.001
      - id: "MiniMax-M2.7"
        cost_per_1k_input: 0.0005
        cost_per_1k_output: 0.001
```

> `billing_source` 决定 tier 层级(`token_plan` 套餐耗尽自动降档到 `api`);同一厂商的所有块保持一致(共享 pool 按 tier 桶)

---

## 6. routing

### 6.1 catch_all 自动模式(推荐)

```yaml
routing:
  default_strategy: "priority"
  catch_all: {}                  # 自动模式:所有 enabled provider 参与
```

### 6.2 显式列表

```yaml
routing:
  aliases: {}                    # 显式 alias(仍生效,不是"已退役";catch_all 是默认路径,alias 命中优先)
  chains: {}                     # chain_ref(可选)
  catch_all:
    providers:
      - name: "minimax"
        model: "MiniMax-M3"
        priority: 100
      - name: "mimo"
        model: "mimo-v2.5-pro"
        priority: 108
```

### 6.3 显式 alias(可选)

```yaml
routing:
  aliases:
    "my-claude":
      strategy: "priority"
      providers:
        - name: "minimax"
          model: "MiniMax-M3"
```

> key 的 `allowed_models` 配 `"my-claude"` → 客户端发任意名字路由到 `MiniMax-M3`

---

## 7. keypool

```yaml
keypool:
  cooling_duration: 60s
  health_check_interval: 30s
  key_rotation: "round_robin"     # round_robin / least_used / random
  quota_enabled: true
  quota_probe_initial_delay: 10s
  quota_probe_max_backoff: 5m
  quota_probe_jitter_pct: 20
  quota_poll_interval: 30s
  quota_poll_jitter_pct: 20
  quota_http_timeout: 10s
  quota_user_agent: "NativeLLMGateway/1.0"
  quota_warn_threshold_pct: 20
```

---

## 8. retry

```yaml
retry:
  max_attempts: 3                # 同一请求最多尝试几个候选
```

---

## 9. logging

```yaml
logging:
  level: "info"                  # debug / info / warn / error
  format: "console"              # console / json
```

---

## 10. usage

```yaml
usage:
  batch_size: 100                # 累积多少条 flush
  flush_interval: 10s            # 周期 flush
```

---

## 11. 数据库 schema

### 11.1 表清单

| 表 | 迁移文件 | 用途 |
|---|---|---|
| `providers` | 001 | 厂商主表 |
| `provider_models` | 001 | 厂商-模型映射 + 定价 |
| `provider_api_keys` | 002 | 厂商 API Key 池(加密存) |
| `usage_records` | 003 | 用量明细 |
| `model_aliases` | 004 | 模型别名 |
| `routing_configs` | 004 | 路由配置 |
| `gateway_keys` | 005 | 客户端 API Key |
| `access_logs` | (AutoMigrate) | 接入日志(P67) |

### 11.2 关键关系

```
providers (Name)
  ├── provider_models (ProviderName FK)
  ├── provider_api_keys (ProviderName FK)
  └── usage_records (ProviderName FK)
gateway_keys (ID)
  └── usage_records (GatewayKeyID FK)
model_aliases (ProviderName FK)
routing_configs (无 FK)
access_logs (ProviderKeyID / GatewayKeyID 都是字符串,无 FK)
```

### 11.3 ID 格式

- 所有内部 ID 都是 `uint` 自增
- `ProviderAPIKey.ID` 用**数字字符串**进入 `keypool.Key.ID`(不是 `<provider>-key-<N>`)
- `parseKeyIDUint` 函数同时兼容两种格式(向前兼容)

---

## 12. 迁移

### 12.1 数据库迁移

- `cmd/gateway/main.go` 启动时调 `database.Migrate(db)`
- 唯一 schema 权威是 GORM AutoMigrate(database.Migrate 遍历 9 个模型 struct);
  已删后端 dormant 的 migrations/*.sql(此前与 struct 漂移且无代码执行)
- 新增字段用 GORM 的 `AutoMigrate`(改 struct + `gorm:"column:..."` tag)
- 大改直接改 struct,由 AutoMigrate 增加/改列(不维护独立 SQL 迁移)

### 12.2 SQLite → PostgreSQL 迁移工具

`backend/scripts/sqlite2pg` — 独立工具,详见 `backend/scripts/sqlite2pg/README.md`(若有)。

```
go run ./backend/scripts/sqlite2pg \
  -src /tmp/gateway-data/gateway.db \
  -dst "postgres://gateway:password@localhost:5432/gateway"
```

校验工具会逐表列集合 → COUNT → 逐行逐字段对比(2µs 容差)→ setval 抽查。

### 12.3 key-state.json 快照

优雅关停时写,启动时恢复。**SQLite 在 DSN 目录,PG 在 cwd**(系统服务下即仓库根)。

路径解析(`server.go:529` `keyStateSnapshotPath`):

```go
if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
    return filepath.Join(".", "key-state.json")
}
return filepath.Join(filepath.Dir(dsn), "key-state.json")
```

---

## 13. 热重载

### 13.1 哪些字段改了会触发 Reload

| 字段 | 触发 |
|---|---|
| `providers.*.endpoint` | `manager` 重新加载 |
| `providers.*.protocol` | 同上 |
| `providers.*.models` | `usage` 重新计算 pricing |
| `providers.*.cost_*` | 同上 |
| `routing.aliases` | `router` 重新加载 |
| `routing.catch_all` | 同上 |
| `keypool.*` | `pool` 重新构建(部分) |
| `auth.keys` | `auth` 重新加载 |

### 13.2 哪些字段改了**不**生效

- `server.port` — 改完需要重启
- `database.driver` / `database.dsn` — 同上
- `server.static_dir` — 同上

### 13.3 触发机制

`config.Watch(ctx, cfgPath, fn(srv.Reload))` 用 fsnotify 监听,文件变更调 `srv.Reload(newCfg)`。
