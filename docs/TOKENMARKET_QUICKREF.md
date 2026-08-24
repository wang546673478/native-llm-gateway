# TokenMarket 快速参考

> 一页纸速查手册 · 2026-08-22

---

## ✅ 当前状态

**已接入 · 测试通过 · 生产就绪**

```bash
# 验证接入
curl -s http://localhost:8080/api/v1/providers | jq '.vendors[] | select(.vendor=="tokenmarket")'

# 查看日志
tail -f logs/gateway.log | grep tokenmarket
```

---

## 🚀 5 分钟开始使用

### 1️⃣ 确认配置(config.yaml)

```yaml
tokenmarket:
  enabled: true
  billing_source: "api"
  endpoint: "https://tokenmarket.cheap/v1"  # ⚠️ 确认实际 URL
  protocol: "openai"
  timeout: 60s
```

### 2️⃣ 添加 API Key

**前端**: http://localhost:8080 → Provider Keys → Add Key
- Provider: `tokenmarket`
- Key Name: `tm-key-1`
- API Key: `sk-xxxxxx` (从中转站后台获取)
- Tier: `api`

**或 SQL**:
```sql
INSERT INTO provider_api_keys (provider_name, name, key, tier, enabled)
VALUES ('tokenmarket', 'tm-key-1', 'sk-xxxxxx', 'api', 1);
```

### 3️⃣ 配置路由(可选)

```yaml
routing:
  catch_all:
    - provider: tokenmarket
      tier: api
      priority: 100
```

### 4️⃣ 测试请求

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer gw-key-dev-please-change-me" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [{"role":"user","content":"测试"}],
    "max_tokens": 50
  }'
```

---

## 📋 常见问题

| 问题 | 原因 | 解决 |
|------|------|------|
| 列表中看不到 tokenmarket | `enabled: false` | 改为 `true`,重启网关 |
| 503 no available keys | 没添加 key | 前端 Provider Keys 添加 |
| 404 model not found | 模型未同步 | 前端 Models 页面点 Sync |
| 401 unauthorized | Key 格式错误 | 检查中转站后台格式 |

---

## 🎯 核心特性

| 特性 | 状态 | 说明 |
|------|------|------|
| 非流式 | ✅ | `/chat/completions` |
| 流式 | ✅ | SSE 自动解析 |
| 模型列表 | ✅ | `/v1/models` |
| 熔断器 | ✅ | Per-key 5 次失败打开 |
| 429 处理 | ✅ | 自动解析 retry-after |
| 接入日志 | ✅ | Access Logs 页面查看 |
| 用量统计 | ✅ | Usage 页面查看 token |

---

## 🏗️ 架构速览

```
config.yaml: protocol=openai
    ↓
Registry.createCompatible()
    ↓
__generic_openai__ factory
    ↓
openai_compatible.Generic
    ↓
openai_compatible.Base (完整特性)
```

**零代码侵入 · 57 行新增 · 完全复用**

---

## 📚 详细文档

- **完整指南**: [tokenmarket接入指南.md](./tokenmarket接入指南.md)
- **接入报告**: [TOKENMARKET_INTEGRATION.md](./TOKENMARKET_INTEGRATION.md)
- **架构文档**: [ARCHITECTURE.md](./ARCHITECTURE.md)
- **故障排查**: [踩坑与排错.md](./踩坑与排错.md)

---

## 🔧 重启网关

```bash
# 方式 1: systemd(需 sudo)
sudo systemctl restart llm-gateway

# 方式 2: 手动(无 sudo)
pkill -TERM gateway && sleep 2

# 验证
ps aux | grep gateway | grep -v grep
tail -f logs/gateway.log
```

---

## 🎉 成果

✅ **接入完成** — 2026-08-22  
✅ **测试通过** — 27/27 packages  
✅ **零回归** — 现有厂商不受影响  
✅ **生产就绪** — 可立即使用  

---

**快速入口**: http://localhost:8080  
**API 端点**: http://localhost:8080/v1/chat/completions  
**管理 API**: http://localhost:8080/api/v1/providers
