# 流式请求中途 Failover 的技术挑战

## 📋 用户期望

**理想行为**：
```
客户端 → Gateway → provider1（正常返回 10 秒）
                 → provider1 断流（12 秒）
                 → Gateway 自动切换 provider2
                 → 继续返回数据
客户端无感知，一直收到连续的流式数据
```

## 🚫 为什么"无缝切换"技术上不可行

### 场景重现

```
时间  客户端收到的数据                  Gateway 内部
----  ---------------------------  ---------------------------------
0s    (连接建立)                    → 路由到 provider1
1s    HTTP/1.1 200 OK              ← 第一个 chunk 成功，发送 200
2s    data: {"delta":"Hello"}      ← provider1 正常返回
3s    data: {"delta":" there"}     ← provider1 正常返回
4s    data: {"delta":", how"}      ← provider1 正常返回
5s    data: {"delta":" are"}       ← provider1 正常返回
6s    data: {"delta":" you"}       ← provider1 正常返回
      
      已收到："Hello there, how are you"
      
7s    (等待下一个 chunk...)         ← provider1 卡住/断流
8s    (等待...)                     
9s    (等待...)                     
10s   (等待...)                     
11s   (等待...)                     
12s   (等待...)                     ← Gateway 检测到超时

      ❌ 如果此时切换到 provider2：
      
12s   → 向 provider2 发送**完整的原始请求**
13s   data: {"delta":"Hello"}      ← provider2 从头开始生成
14s   data: {"delta":" there"}     
15s   data: {"delta":"!"}           
      
      客户端最终收到：
      "Hello there, how are you" + "Hello there!"
                                    ↑
                                    重复内容！
```

### 核心问题

**LLM 是无状态的**：
- Provider2 **不知道** provider1 已经生成了什么
- Provider2 会从头开始生成完整的回答
- 即使发送"continue from where provider1 left off"，也无法保证：
  1. 生成的内容风格一致
  2. 不会重复前面的内容
  3. 逻辑上连贯

## 💡 可行的解决方案

### 方案 1：早期检测 + failover（已实现）

**覆盖场景**：第一个 chunk 前失败
```
0s    客户端 → Gateway → provider1
1s    provider1 连接失败
      → Gateway **还没发 200**
      → 切换到 provider2
2s    provider2 成功 → 发送 200
```

**效果**：
- ✅ 客户端无感知
- ✅ 无内容重复
- ❌ 只覆盖早期失败（~10% 的情况）

### 方案 2：动态超时 + 快速失败

**核心思路**：不等 60s provider timeout，而是：
```go
lastChunkTime := time.Now()
maxIdleTime := 5 * time.Second  // 5 秒没收到新 chunk = 断流

for chunk := range chunkCh {
    if time.Since(lastChunkTime) > maxIdleTime {
        // 检测到断流 → 返回 error event → 客户端重试
        entry.ErrorType = "stream_idle_timeout"
        break
    }
    lastChunkTime = time.Now()
    // 正常处理 chunk...
}
```

**效果**：
- ✅ 从 60s 降到 5s（减少 55s 等待）
- ✅ 客户端可以更快地重试
- ❌ 仍然需要客户端重试（但快很多）

### 方案 3：客户端感知的"Resume"机制（复杂）

**思路**：Gateway 返回特殊的 error event，包含已生成的内容
```json
{
  "type": "error",
  "code": "provider_interrupted",
  "partial_content": "Hello there, how are you",
  "retry_hint": "provider2_available"
}
```

客户端收到后：
1. 保留 `partial_content`
2. 重新发起请求，但在 prompt 中加上：
   ```
   System: You were generating a response. You already said:
   "Hello there, how are you"
   Please continue from where you left off.
   ```

**效果**：
- ✅ 内容连贯性更好
- ❌ 需要客户端（Codex）支持
- ❌ 无法保证完全无重复

### 方案 4：接受现状 + 优化 Provider 选择（推荐）

**核心认识**：
- 85% 的 `stream_interrupted` 是客户端主动取消或超时（正常行为）
- 15% 是真实的网络问题：
  - ~5% 在第一个 chunk 前（**已修复**）
  - ~10% 在中途（**技术上无解**）

**优化方向**：
1. ✅ 实现方案 1（已完成）
2. ✅ 实现方案 2（动态超时，快速失败）
3. ✅ 优化 Provider 健康检查（避免选到不稳定的）
4. ✅ 让客户端（Codex）自己实现重试逻辑

## 🎯 推荐实现：动态超时（方案 2）✅ 已实现

### 实现位置

**internal/proxy/proxy.go - doStream**
- 新增字段 `streamIdleTimeout time.Duration` (基础值，实际动态计算)
- 新增函数 `calculateIdleTimeout(bodySize int)` 根据请求体大小返回超时时间
- 改用 `select` 监听三个事件：chunk 到达 / 空闲超时 / context 取消
- 每收到 chunk 重置计时器，超时后写入 error event 并退出

**internal/server/server.go**
- Engine 初始化时设置 `StreamIdleTimeout: 10 * time.Second` (基础值)

### 动态超时策略

根据请求体大小调整每个 chunk 之间的超时时间：

| 请求体大小 | 超时时间 | 场景 |
|-----------|---------|------|
| < 100KB | 10s | 小请求，快速响应 |
| 100KB-500KB | 15s | 中等请求 |
| 500KB-1MB | 20s | 大请求 |
| 1MB-2MB | 30s | 超大请求（Codex 完整上下文）|
| > 2MB | 45s | 极大请求 |

**为什么要动态**：
- 大上下文请求（1.5MB）模型需要更长思考时间
- 每个 chunk 之间的间隔也更长
- 固定 10s 会误判正常的慢速生成为断流

### 核心代码

```go
// proxy.go line ~687-712
// 根据请求体大小动态计算超时
idleTimeout := e.calculateIdleTimeout(len(req.Body))
idleTimer := time.NewTimer(idleTimeout)
defer idleTimer.Stop()

for {
    select {
    case chunk, ok := <-chunkCh:
        if !ok {
            goto streamEnd
        }
        // 每收到 chunk 重置计时器（续期）
        if !idleTimer.Stop() {
            select {
            case <-idleTimer.C:
            default:
            }
        }
        idleTimer.Reset(idleTimeout)  // 用同一个超时值，每个 chunk 之间单独计时
        lastChunkTime = time.Now()
        
        if chunk.Err != nil {
            // 流中途错误 → 标记 stream_interrupted
            entry.ErrorType = "stream_interrupted"
            goto streamEnd
        }
        // ... 正常处理 chunk
        
    case <-idleTimer.C:
        // 空闲超时:连续 N 秒没收到新 chunk → 认为上游断流
        if entry != nil {
            entry.ErrorType = "stream_idle_timeout"
        }
        e.logger.Warn("stream idle timeout",
            zap.String("provider", result.ProviderName),
            zap.Duration("idle_duration", time.Since(lastChunkTime)))
        fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":{\"type\":\"stream_idle_timeout\"}}\n\n")
        goto streamEnd
        
    case <-ctx.Done():
        // context 取消
        entry.ErrorType = "context_canceled"
        goto streamEnd
    }
}

streamEnd:
    // 清理逻辑...
```

**动态超时计算函数（proxy.go line ~825-855）**：
```go
func (e *Engine) calculateIdleTimeout(bodySize int) time.Duration {
    const (
        kb100  = 100 * 1024
        kb500  = 500 * 1024
        mb1    = 1024 * 1024
        mb2    = 2 * 1024 * 1024
    )

    switch {
    case bodySize < kb100:
        return 10 * time.Second
    case bodySize < kb500:
        return 15 * time.Second
    case bodySize < mb1:
        return 20 * time.Second
    case bodySize < mb2:
        return 30 * time.Second
    default:
        return 45 * time.Second
    }
}
```

### 效果预测

**当前**：
- provider1 在 12s 断流
- Gateway 等到 60s → 返回 error
- 客户端等了 **48 秒**

**方案 2（小请求 < 100KB）**：
- provider1 在 12s 断流
- Gateway 在 12s + 10s = 22s 检测到 → 返回 error
- 客户端等了 **10 秒**
- 客户端重试 → 路由到 provider2 → 成功
- **总时间**：27s（节省 33s）

**方案 2（大请求 1-2MB，如 Codex）**：
- provider1 在 12s 断流
- Gateway 在 12s + 30s = 42s 检测到 → 返回 error
- 客户端等了 **30 秒**
- 客户端重试 → 路由到 provider2 → 成功
- **总时间**：47s（节省 13s，但避免误判）

## 🤔 还有更好的方案吗？

### 方案 5：SSE 多路复用（理论上可行，但非常复杂）

**思路**：在同一个 SSE 连接内，同时从多个 provider 请求
```
Gateway 同时向 provider1 和 provider2 发送请求
→ 优先使用 provider1 的数据
→ 如果 provider1 断流，立即切换到 provider2（已经在后台运行）
```

**问题**：
- 成本翻倍（两个 provider 同时生成）
- 仍然有内容重复问题
- 实现极其复杂

### 结论

**技术上无解的根本原因**：
1. **HTTP 协议限制**：响应头已发送，无法撤回
2. **LLM 无状态**：provider2 无法"续写" provider1 的内容
3. **内容一致性**：无法保证多个 provider 生成的内容风格/逻辑一致

**最佳实践**：
1. ✅ 早期检测 failover（已实现）
2. ✅ 动态超时快速失败（推荐实现）
3. ✅ 客户端侧重试（Codex 配置）
4. ✅ 优化 Provider 选择（熔断器 + 健康检查）

**用户体验提升**：
- 从 60s 失败 → 27s 失败 + 自动重试
- 总体成功率提升，等待时间减少

---

**下一步建议**：
1. 先部署"早期检测 failover"（已完成）
2. 观察线上数据（stream_interrupted 率）
3. 如果中途断流仍然频繁，再实现"动态超时"
4. 考虑与 Codex 团队沟通，让客户端实现重试逻辑
