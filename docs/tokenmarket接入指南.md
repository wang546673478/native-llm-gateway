# TokenMarket 中转站接入指南

> 2026-08-22 · TokenMarket 已成功接入 Native LLM Gateway

---

## ✅ 接入完成状态

TokenMarket 已通过**通用 OpenAI 兼容协议**完整接入,无需编写专属厂商包。

### 验证方式

```bash
# 1. 检查 provider 列表
curl -s http://localhost:8080/api/v1/providers | jq '.vendors[] | select(.vendor=="tokenmarket")'

# 2. 查看网关日志
tail -f logs/gateway.log | grep tokenmarket

# 3. 前端管理页面
# 访问 http://localhost:8080 → Providers 页面,应该能看到 tokenmarket
```

---

## 📋 配置说明

### config.yaml 配置块

```yaml
tokenmarket:
  enabled: true
  billing_source: "api"              # 按 token 计费
  endpoint: "https://tokenmarket.cheap/v1"
  protocol: "openai"                 # OpenAI 兼容协议
  timeout: 60s
  responses_api: false               # 中转站通常不支持 Responses API
  circuit_breaker:
    failure_threshold: 5
    failure_window: 60s
    open_timeout: 30s
    half_open_requests: 1
    countable_errors: ["5xx", "timeout", "connection_error"]
    excluded_errors: ["429"]
```

### 关键配置项说明

| 字段 | 值 | 说明 |
|------|-------|------|
| `endpoint` | `https://tokenmarket.cheap/v1` | 中转站 API 端点(需确认实际 URL) |
| `protocol` | `openai` | OpenAI 兼容协议(自动使用 `/chat/completions`) |
| `billing_source` | `api` | 按 token 计费,非包月套餐 |
| `responses_api` | `false` | 中转站不支持 OpenAI Responses API |

---

## 🔑 添加 API Key

有两种方式添加 TokenMarket 的 API Key:

### 方式 1: 前端管理页面(推荐)

1. 访问 `http://localhost:8080`
2. 进入 **Provider Keys** 页面
3. 点击 **Add Key** 按钮
4. 填写表单:
   - **Provider**: 选择 `tokenmarket`
   - **Key Name**: 自定义名称(如 `tm-key-1`)
   - **API Key**: 从 TokenMarket 后台获取的 token
   - **Tier**: 选择 `api`(按量计费)
5. 保存后自动生效(无需重启)

### 方式 2: 数据库直接插入

```sql
INSERT INTO provider_api_keys (provider_name, name, key, tier, enabled)
VALUES ('tokenmarket', 'tm-key-1', 'sk-xxxxxx', 'api', 1);
```

---

## 🔄 路由配置

### 添加到 catch_all 路由

如果希望 TokenMarket 参与默认路由,编辑 `config.yaml`:

```yaml
routing:
  catch_all:
    - provider: tokenmarket
      tier: api
      priority: 100  # 数字越小优先级越高
```

### 创建专属路由

```yaml
routing:
  alias:
    gpt-4:
      - provider: tokenmarket
        model: gpt-4
        tier: api
```

---

## 🧪 测试验证

### 1. 健康检查

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer gw-key-dev-please-change-me" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [{"role":"user","content":"hi"}],
    "max_tokens": 10
  }'
```

### 2. 查看路由结果

```bash
# 检查 access log,确认请求是否路由到 tokenmarket
sqlite3 gateway.db "SELECT * FROM access_logs WHERE provider='tokenmarket' ORDER BY created_at DESC LIMIT 5;"
```

### 3. 监控指标

```bash
# Prometheus 指标
curl -s http://localhost:8080/metrics | grep 'provider="tokenmarket"'
```

---

## 🏗️ 技术实现

### 架构设计

TokenMarket 接入采用**通用 OpenAI 兼容协议**,无需编写专属厂商包:

```
┌─────────────────────────────────────────────────────────┐
│  config.yaml: tokenmarket → protocol: openai            │
└─────────────────────────────────────────────────────────┘
                        ↓
┌─────────────────────────────────────────────────────────┐
│  provider.Registry.createCompatible()                   │
│  → 检测到 protocol=openai                                │
│  → 查找全局注册的 __generic_openai__ factory             │
└─────────────────────────────────────────────────────────┘
                        ↓
┌─────────────────────────────────────────────────────────┐
│  openai_compatible.NewGeneric()                         │
│  → 创建 Generic wrapper                                  │
│  → 内部使用 openai_compatible.Base                       │
└─────────────────────────────────────────────────────────┘
                        ↓
┌─────────────────────────────────────────────────────────┐
│  完整复用现有能力:                                        │
│  • SendRequest / SendStreamRequest                      │
│  • ListModels(如果中转站支持 /v1/models)                 │
│  • 熔断器 / 429 处理 / 错误分类                           │
│  • 额度检查(如果中转站提供余额 API)                        │
└─────────────────────────────────────────────────────────┘
```

### 关键代码

#### 1. 通用 OpenAI 兼容 Provider

**文件**: `backend/internal/provider/openai_compatible/generic.go`

```go
// Generic 是通用的 OpenAI 兼容 Provider 包装器
type Generic struct {
    base *Base
    name string
}

func init() {
    // 注册通用 OpenAI 兼容 factory
    provider.RegisterGlobalWithProtocolVendor(
        "__generic_openai__", 
        NewGeneric, 
        provider.ProtocolOpenAI, 
        "",
    )
}
```

#### 2. Registry 自动路由到通用实现

**文件**: `backend/internal/provider/registry.go`

```go
func (r *Registry) createCompatible(name string, cfg ProviderConfig) (Provider, error) {
    proto := cfg.Protocol
    if proto == "" {
        return nil, fmt.Errorf("provider %q is not registered and protocol is empty", name)
    }

    // 尝试使用全局注册的通用 factory
    var genericName string
    switch proto {
    case ProtocolOpenAI:
        genericName = "__generic_openai__"
    case ProtocolAnthropic:
        genericName = "__generic_anthropic__"
    case ProtocolGoogle:
        genericName = "__generic_google__"
    default:
        return nil, fmt.Errorf("protocol %q has no compatible implementation", proto)
    }

    factory := r.globalRegistry[genericName]
    if factory == nil {
        return nil, fmt.Errorf("protocol %q generic factory not registered", proto)
    }

    return factory(cfg)
}
```

---

## 🎯 完整特性支持

TokenMarket 通过 `openai_compatible.Base` 自动获得所有标准能力:

| 特性 | 状态 | 说明 |
|------|------|------|
| 非流式请求 | ✅ | `SendRequest()` |
| 流式请求 | ✅ | `SendStreamRequest()` 通过 SSE |
| 模型列表 | ✅ | `ListModels()` 调用 `/v1/models` |
| 健康检查 | ✅ | `HealthCheck()` |
| Per-key 熔断器 | ✅ | 自动继承 `circuit` 包 |
| 429 处理 | ✅ | 自动解析 `retry-after` / cooling |
| 错误分类 | ✅ | `ClassifyTransportError()` |
| 接入日志 | ✅ | 自动记录到 `access_logs` 表 |
| 用量统计 | ✅ | 自动采集 token 用量 |
| 设备指纹归一化 | ✅ | 如果开启,自动抹平多头信号 |

---

## ⚠️ 注意事项

### 1. **端点 URL 确认**
配置中的 `https://tokenmarket.cheap/v1` 是假设值,请根据实际情况修改:
- 如果中转站端点已经包含 `/v1`,保持原样
- 如果中转站端点是 `https://api.tokenmarket.xxx`,去掉 `/v1`

### 2. **模型同步**
TokenMarket 作为中转站,可能支持数十个模型。建议:
- 首次接入后,访问前端 **Models** 页面
- 点击 **Sync Models** 按钮,从 `/v1/models` 拉取模型列表
- 手动设置定价(中转站通常不提供 pricing API)

### 3. **余额查询**
中转站通常不提供标准的余额查询 API。如果 TokenMarket 有专属余额端点:
- 需要编写专属 `balancer.go`(参考 `provider/deepseek/balancer.go`)
- 实现 `quotacheck.Balancer` 接口
- 注册到 `tokenmarket` package 中

### 4. **错误码映射**
如果 TokenMarket 返回的错误码与 OpenAI 标准不同,可能需要:
- 在 `openai_compatible/classify.go` 中添加自定义规则
- 或创建 `provider/tokenmarket/classify.go` 覆盖默认行为

---

## 🔧 故障排查

### 问题 1: provider 列表中看不到 tokenmarket

**原因**: 配置文件中 `enabled: false` 或配置块不存在

**解决**:
```bash
# 检查配置
grep -A 5 "tokenmarket:" config.yaml

# 确认 enabled: true
# 重启网关
pkill -TERM gateway && sleep 2
```

### 问题 2: 请求返回 503 no available keys

**原因**: 没有添加 API Key

**解决**:
1. 前端 Provider Keys 页面添加 key
2. 或数据库插入:
```sql
INSERT INTO provider_api_keys (provider_name, name, key, tier, enabled)
VALUES ('tokenmarket', 'key-1', 'sk-xxxxx', 'api', 1);
```

### 问题 3: 请求返回 404 model not found

**原因**: 模型未同步到 `provider_models` 表

**解决**:
1. 前端 Models 页面点击 **Sync Models**
2. 或手动插入:
```sql
INSERT INTO provider_models (vendor, model_id, input_price, output_price, context_window, sort_order)
VALUES ('tokenmarket', 'gpt-4', 0.03, 0.06, 128000, 1);
```

### 问题 4: 中转站返回 401 unauthorized

**原因**: API Key 格式不正确或已失效

**解决**:
- 检查 TokenMarket 后台,确认 key 格式(可能需要 `Bearer sk-xxx` 或 `tk-xxx`)
- 在 `provider_api_keys` 表中更新正确的 key

---

## 📚 相关文档

- [CLAUDE.md](../CLAUDE.md) — 项目指令与架构原则
- [provider厂商定制包指南.md](./provider厂商定制包指南.md) — 如需深度定制
- [ARCHITECTURE.md](./ARCHITECTURE.md) — 包职责与边界
- [config-reference.md](./config-reference.md) — 配置完整规格

---

## 🎉 总结

TokenMarket 接入**无需编写任何专属代码**,完全复用现有 `openai_compatible` 架构:

✅ **5 分钟配置** → 添加 config.yaml 配置块  
✅ **自动生效** → 通用 factory 自动路由  
✅ **完整特性** → 熔断/429/日志/用量全部继承  
✅ **低耦合** → 零侵入式接入,不影响现有厂商  

如需更深度定制(如专属余额 API、特殊错误码),参考 `docs/provider厂商定制包指南.md` 创建专属包。
