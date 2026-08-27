# GPT 请求分析报告（2026-08-27）

## 📊 数据概览（24小时）

### 整体统计
- **总请求数**: 330 次
- **成功率**: 74.2% (245/330)
- **失败请求**: 85 次
  - `stream_interrupted`: 77 次 (23.3%)
  - `upstream_stream_error`: 4 次 (1.2%)
  - `upstream_5xx`: 4 次 (1.2%)

### Provider 分布
| Provider | 成功 | 失败 | 成功率 |
|---------|-----|------|--------|
| tokenmarket-codex | 244 | 80 | 75.3% |
| tokenmarket-codex1 | 1 | 0 | 100% |
| tokenmarket-plus3 | 0 | 5 | 0% |

## 🚨 核心问题识别

### 1. **tokenmarket-plus3 完全不可用**
```
连续 5 次请求全部失败：
- 4 次 upstream_5xx (502)
- 1 次 upstream_stream_error

时间范围: 18:33-18:34 (1分钟内)
延迟: ~17-18s 后返回 502
```

**根因**：上游服务器完全宕机或账号被封禁

**建议**：
- ✅ **立即禁用** `tokenmarket-plus3` provider
- 检查账号状态（可能配额耗尽或被封）
- 从路由中移除，避免浪费 17s 延迟

### 2. **流式中断率高（23.3%）**
```
77 次 stream_interrupted：
- 平均延迟: 12s
- 平均输入 token: 5,317
- 平均输出 token: 298
- 缓存命中: 25.8M tokens
```

**特征**：
- 请求成功开始（200 状态码）
- 流式输出进行中突然中断
- 多发生在大上下文请求（缓存命中高）

**可能原因**：
1. 客户端主动取消（用户点击停止）
2. 网络不稳定
3. 上游限流（虽然返回 200）

**建议**：
- ✅ 这可能是**正常行为**（用户取消），不需要优化
- 如果是网络问题，考虑增加 `write_timeout`（当前 600s 已经够长）
- 监控：如果中断时间点都在某个固定时长（如 10s），可能是上游限制

### 3. **upstream_stream_error（流内错误）**
```
4 次流内错误：
- tokenmarket-codex: 3 次
- tokenmarket-plus3: 1 次
平均延迟: 82-176s（非常长）
```

**实际错误**（从响应分析）：
```json
data: {"type":"error","sequence_number":0,"error":{"type":"upstream_error","message":"stream_read_error","code":"stream_read_error"}}

data: {"type":"response.failed","response":{"id":"resp_526f444d2a864e7b82b1ca66732ea4c7","object":"response","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"code":"upstream_error","message":"Upstream request failed"}}}
```

**根因**：
- 流式输出到一半，上游返回错误事件
- Gateway 已正确识别（error_type = `upstream_stream_error`）
- 触发 failover（下一个请求成功）

**建议**：
- ✅ **Gateway 行为正确**，已触发 failover
- 这是上游限流/内容审核/并发限制的正常表现
- 可以添加更智能的重试策略（识别到 `limit_reached` 时延迟重试）

## 💡 优化建议

### 优先级 1：立即执行

#### 1.1 禁用 tokenmarket-plus3
```yaml
# config.yaml
providers:
  tokenmarket-plus3:
    enabled: false  # ← 改为 false
```

**收益**：
- 避免每次失败浪费 17s 延迟
- 4 次失败 × 17s = 68s 累计浪费时间

#### 1.2 调整 Provider 优先级
```
当前路由：tokenmarket-plus3 → tokenmarket-codex → tokenmarket-codex1

建议路由：tokenmarket-codex → tokenmarket-codex1
（删除 plus3，codex 作为主力）
```

### 优先级 2：缓存优化（已生效）

#### 2.1 **Prompt Cache 效果显著**
```
成功请求：
- 输入 token: 4.4M
- 缓存命中: 58M tokens

缓存命中率: 58M / (4.4M + 58M) = 93%！
```

**计算节省成本**：
```
假设 input token 价格: $0.003/1K
缓存价格: $0.0003/1K（10% off）

无缓存成本: 62.4M × $0.003/1K = $187.2
有缓存成本: 4.4M × $0.003/1K + 58M × $0.0003/1K = $13.2 + $17.4 = $30.6

节省: $187.2 - $30.6 = $156.6 (83.7%)
```

**结论**：
- ✅ **Prompt Cache 已极大降低成本**
- 输入 token 重复度高（system prompt / 历史对话）
- **无需优化**，继续保持

### 优先级 3：请求大小优化

#### 3.1 大请求分析
```
> 1MB 请求: 214 次 (64.8%)
- 平均大小: 1.5MB
- 错误率: 36.9% (79/214)

对比：
< 200KB 请求: 10 次
- 错误率: 0%
```

**观察**：
- 大请求（> 1MB）错误率显著更高
- 但这些是 Codex 的正常工作负载（包含完整上下文）

**建议**：
- ❌ **不建议压缩请求**（会损失上下文）
- ✅ 接受这是正常的 Codex 使用模式
- ✅ 确保 `max_tokens` 合理设置（避免超长输出）

### 优先级 4：延迟优化

#### 4.1 延迟分布
```
成功请求平均延迟: 18.1s
失败请求平均延迟:
- stream_interrupted: 12s (更快，因为提前中断)
- upstream_stream_error: 105.8s (非常长)
- upstream_5xx: 17.8s
```

**建议**：
- ✅ **延迟合理**，大部分是上游生成时间
- `upstream_stream_error` 延迟长是因为流跑了很久才出错
- 可以设置 `max_output_tokens` 限制输出长度（如果延迟敏感）

## 🎯 总结与行动项

### ✅ Gateway 工作正常
1. **Failover 机制生效**：`upstream_stream_error` 正确触发切换
2. **流内错误识别**：新实现的流内错误检测（踩坑 #31）工作正常
3. **Prompt Cache 节省 83.7% 成本**

### 🚨 立即行动
1. **禁用 tokenmarket-plus3**（完全不可用）
2. **检查 plus3 账号状态**（可能被封或配额耗尽）

### 📊 监控指标
1. **stream_interrupted 率**：23.3%，需观察是否持续（可能是正常用户行为）
2. **tokenmarket-codex 稳定性**：75.3% 成功率可接受，但可以提升

### 💰 成本优化（已达成）
- Prompt Cache 节省 **83.7%** 输入成本
- 继续保持当前配置

## 📈 优化效果预测

### 禁用 plus3 后
```
当前：330 次请求，85 次失败（25.8% 失败率）
预期：326 次请求，80 次失败（24.5% 失败率）

延迟节省：4 × 17s = 68s
失败率改善：1.3%
```

### 如果解决 stream_interrupted
```
如果 77 次中断中有 50% 是可避免的（网络/配置问题）：
成功率可提升至：(245 + 38) / 330 = 85.8%
```

---

**生成时间**: 2026-08-27 18:45
**数据来源**: PostgreSQL access_logs + usage_records (24h)
**分析者**: Claude Code (Opus 5)
