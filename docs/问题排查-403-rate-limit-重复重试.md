# 问题交接文档：403 rate_limit 重复重试

**日期**：2026-08-30  
**问题编号**：403-rate-limit-retry-issue  
**状态**：部分修复完成，核心问题待解决

---

## 问题描述

Access logs 中出现 `tokenmarket-kiro特惠` 中转站遇到 403 rate_limit 错误后，反复用同一把 key（ID=85）重试 11 次，才切换到另一把 key（ID=92），最终切换到另一个 provider 才成功。

**预期行为**：遇到 403 rate_limit 应该立即切换到下一把 key，而不是用同一把 key 重试多次。

**实际行为**：同一把 key 被重试 11 次，浪费时间和请求配额。

---

## 已完成的修复 ✅

### 1. 路径拼接鲁棒性修复
**文件**：
- `backend/internal/provider/openai_compatible/openai_compatible.go`
- `backend/internal/provider/anthropic_compatible/anthropic_compatible.go`

**问题**：base_url 配置带 `/v1` 时会导致路径重复（如 `/v1/v1/responses`）

**修复**：
- OpenAI：`upstreamPath()` 智能去重 `/v1` 前缀
- Anthropic：`buildMessagesURL()` 自动适配 endpoint 是否含 `/v1`

**Commit**: `70c481b` （已推送到 GitHub main 分支）

### 2. vendor 级 Provider 绑定匹配修复
**文件**：`backend/internal/router/router.go`

**问题**：Gateway Key 绑定厂商名（如 `providers: ["minimax"]`）时，只能匹配同名注册面，无法匹配该厂商的其他协议面（`minimax-openai`），导致 OpenAI 协议请求返回 `no_route`

**修复**：路由过滤时同时检查注册面名和 vendor 名
```go
if len(o.AllowedProviders) > 0 {
    vendor := r.manager.VendorFor(name)
    if !sliceContains(o.AllowedProviders, name) && !sliceContains(o.AllowedProviders, vendor) {
        continue
    }
}
```

**Commit**: `70c481b` （已推送到 GitHub main 分支）

---

## 未解决的核心问题：403 rate_limit 重复重试

### 问题现象

**Trace ID**: `06167f9a-3ba0-43f5-809c-70fc2e126f23`

**执行序列**：
| 序号 | Provider | Key ID | 状态码 | 错误类型 |
|------|----------|--------|--------|----------|
| 1-11 | tokenmarket-kiro特惠 | 85 | 403 | rate_limit |
| 12 | tokenmarket-kiro特惠 | 92 | 403 | rate_limit |
| 13 | tokenmarket-kiro正价特惠组 | 86 | 200 | ok |

**配置**：
- `tokenmarket-kiro特惠` 有 2 把 key (ID: 85, 92)
- `config.yaml`: `retry.max_attempts: 0` （动态模式，不封顶）

---

## 根因确认 ✅

### 完整执行流程

1. **上游返回 403**：body 包含 quota 关键字（如 MiniMax 的 base_resp.status_code=2056）

2. **错误分类** (`anthropic_compatible.go:124-143`):
   ```go
   errType := provider.ClassifyErrorWithBody(status, body)  // 返回 quota_exceeded
   if errType == provider.ErrorTypeQuotaExceeded && b.balanceGuardHealthy(key) {
       errType = provider.ErrorTypeRateLimit  // 降级为 rate_limit
   }
   ```
   - `ClassifyErrorWithBody(403, body)` → 检测到 quota 关键字 → `quota_exceeded`
   - `balanceGuardHealthy(key)` 检查余额守卫：如果 key 的 remaining > threshold → 返回 true
   - 降级为 `rate_limit`（认为是瞬时误报）

3. **Base 内部重试** (`anthropic_compatible.go:202-213`):
   ```go
   if errType == provider.ErrorTypeRateLimit {
       b.cfg.Pool.ReportRateLimit(key, retryAfter)
       if !retried {
           retried = true
           time.Sleep(1s)
           continue  // 用同一把 key 重试
       }
   }
   ```
   - 第 1 次请求 → 403 → 等 1s → 第 2 次请求 → 403 → 返回错误

4. **Proxy 层检测到 rate_limit** (`proxy.go:1431-1444`):
   ```go
   if pe.ErrorType == provider.ErrorTypeRateLimit {
       if e.retrySameKeyRateLimit(...) {  // 最多 10 次
           return outcomeOK
       }
       // 10 次全失败后才换 key
       if e.swapToOtherKey(...) { ... }
   }
   ```

5. **`retrySameKeyRateLimit` 循环** (`proxy.go:1623-1656`):
   ```go
   func (e *Engine) retrySameKeyRateLimit(...) bool {
       attempts := 0
       for {
           if e.attemptOne(...) { return true }  // 每次调用 Base.SendRequest
           
           pe := *lastErr
           if pe.ErrorType != provider.ErrorTypeRateLimit { return false }
           
           attempts++
           if attempts >= rateLimitSameKeyRetries {  // 10 次
               pool.ReportRateLimit(result.Key, pe.RetryAfter)
               return false
           }
           // 继续下一轮...
       }
   }
   ```
   - 每次 `attemptOne` 都会调用 Base，Base 内部又重试 1 次
   - 理论最大 = 10 × 2 = 20 次请求
   - 实际 11 次可能是因为某次中途退出

6. **为什么是 11 次而不是 20 次？**
   - 可能 Base 内部的 `retried` flag 在某些情况下没有重置
   - 或者 `retrySameKeyRateLimit` 只执行了 5 轮（5 × 2 + 1 = 11）

### 数据验证

**SQL 查询结果**：
```sql
-- 403 错误的分类统计（7 天内）
status_code | error_type | count 
------------|------------|-------
403         | rate_limit | 36      ← 被 balanceGuardHealthy 降级
403         | auth       | 6       ← 没有 quota 关键字
```

**关键常量** (`proxy.go:1159-1161`):
```go
rateLimitSameKeyRetries = 10  // 同一把 key 最多重试 10 次
```

### 调用链路

```
proxy.tryCandidate (proxy.go:1323)
  ↓
bindKey (proxy.go:1659): 设置 req.Key = result.Key (来自 router)
  ↓
attemptOne
  ↓
doRequest (proxy.go:536): pv.SendRequest(ctx, req)
  ↓
anthropic_compatible.Base.SendRequest (anthropic_compatible.go:161-219)
  ↓
检查 req.Key != nil → 直接使用这把 key (line 161-168)
  ↓
遇到 rate_limit → 等 1s → 用同一把 key 重试 1 次 (line 202-213)
  ↓
返回错误到 proxy.tryCandidate
  ↓
检测到 ErrorTypeRateLimit (proxy.go:1431)
  ↓
retrySameKeyRateLimit (proxy.go:1432): 用同一把 key 重试最多 10 次
  ↓
所有重试失败后 → swapToOtherKey (proxy.go:1444): 换 key
```

### 关键代码位置

#### 1. Base 不换 key 的原因
**文件**: `backend/internal/provider/anthropic_compatible/anthropic_compatible.go:161-168`

```go
key := req.Key
var err error
if key == nil {
    key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic))
    if err != nil {
        return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
    }
}
```

**问题**：
- 如果 `req.Key != nil`（router 已分配），就**直接使用**，不从 Pool 获取其他 key
- Base 内部的 rate_limit 重试只会用同一把 key 重试 1 次（line 202-213）

#### 2. Proxy 的 rate_limit 重试逻辑
**文件**: `backend/internal/proxy/proxy.go:1431-1444`

```go
if pe.ErrorType == provider.ErrorTypeRateLimit {
    if e.retrySameKeyRateLimit(c, ctx, req, result, outProviderName, lastErr, entry) {
        return outcomeOK, false, true
    }
    // 10 次全 429 → 已 ReportRateLimit(COOLING),推进到下一把 key
    pe = *lastErr
    if pe == nil {
        return outcomeContinue, false, true
    }
    if !errorIsRetryable(pe) {
        return outcomeFatal, false, true
    }
    // 429 之后换 key 重试一次(同网络类语义)
    if e.swapToOtherKey(c, req, result) {
        if e.attemptOne(c, ctx, req, result, outProviderName, lastErr, entry) {
            return outcomeOK, false, true
        }
        ...
    }
}
```

**问题**：
- `retrySameKeyRateLimit` 会用同一把 key 重试很多次
- 只有在所有重试失败后，才会调用 `swapToOtherKey` 换 key

#### 3. swapToOtherKey 换 key 逻辑
**文件**: `backend/internal/proxy/proxy.go:1671-1693`

```go
func (e *Engine) swapToOtherKey(c *gin.Context, req *provider.Request, result *router.RouteResult) bool {
    if result.Key == nil {
        return false
    }
    pool := e.router.Pool(result.ProviderName)
    if pool == nil {
        return false
    }
    var idSet map[uint]struct{}
    if gk := e.gkCtx.Get(c); gk != nil && len(gk.ProviderKeyIDs) > 0 {
        idSet = make(map[uint]struct{}, len(gk.ProviderKeyIDs))
        for _, id := range gk.ProviderKeyIDs {
            idSet[id] = struct{}{}
        }
    }
    newKey, err := pool.AcquireFromTierExcludingIDs(result.Tier, result.Key.ID, idSet, string(result.Protocol))
    if err != nil {
        return false
    }
    result.Key = newKey
    req.Key = newKey
    req.Headers.Set("Authorization", "Bearer "+newKey.Key)
    return true
}
```

**机制**：
- 从同一个 provider 的同一个 tier 获取另一把 key
- 排除刚失败的 key
- 更新 `result.Key` 和 `req.Key`

---

## 需要检查的点

### 1. `retrySameKeyRateLimit` 方法的实现
**位置**: `backend/internal/proxy/proxy.go`（需要搜索方法定义）

**问题**：
- 这个方法具体实现是什么？
- 为什么会重试这么多次？
- 重试次数是硬编码的 10 次，还是可配置？

**待确认**：
```bash
grep -n "func.*retrySameKeyRateLimit" backend/internal/proxy/proxy.go
```

### 2. 403 vs 429 的处理差异
**位置**: `backend/internal/provider/provider.go:442-466`

**问题**：
- 代码注释多处提到 "429"，但实际错误是 403
- `ClassifyErrorWithBody` 是否将 403 错误正确分类为 `rate_limit`？
- 403 和 429 是否应该用不同的重试策略？

**语义差异**：
- **429 Too Many Requests**：瞬时限流，等待后可能恢复 → 适合同 key 重试
- **403 Forbidden**：权限/配额问题，通常是 key 级别的限制 → 应该立即换 key

**待确认**：
```bash
grep -A 20 "ClassifyErrorWithBody" backend/internal/provider/provider.go | grep -A 5 "403"
```

### 3. Relay provider 的 Pool 注入
**位置**: 
- `backend/internal/provider/relay/methods.go:129-134`
- `backend/internal/provider/relay/implementations.go`

**已验证**：
- `GenericRelayProvider.SetPool` 会调用 `impl.SetPool(pool)`
- 通过 Go 方法提升，`RelayOpenAIProvider` 和 `RelayAnthropicProvider` 会继承 `Base.SetPool`

**待确认**：
- Pool 是否真的被注入到了 Base？
- 可以通过日志验证：在 `Base.SendRequest` 开头打印 `b.cfg.Pool != nil`

### 4. 为什么是 11 次而不是 12 次（10 + 1 + 1）？
**理论计算**：
- Base 内部重试 1 次 = 2 次请求
- `retrySameKeyRateLimit` 最多 10 轮
- 理论最多 = 10 × 2 = 20 次

**实际只有 11 次**：
- 可能 `retrySameKeyRateLimit` 中途退出
- 或者某次请求被其他机制拦截

---

## 修复方案

### ✅ 方案 A：在 ProviderError 中传递 HTTP 状态码（推荐并实施）

**核心思路**：
- 在 `ProviderError` 结构中已有 `StatusCode` 字段
- 在 `proxy.go:1431` 检查 `pe.StatusCode == 403` 时，立即换 key，不进入 `retrySameKeyRateLimit`
- 429 仍然允许同 key 重试（应对瞬时限流）

**修改位置**: `backend/internal/proxy/proxy.go:1431-1444`

**修改前**：
```go
if pe.ErrorType == provider.ErrorTypeRateLimit {
    if e.retrySameKeyRateLimit(c, ctx, req, result, outProviderName, lastErr, entry) {
        return outcomeOK, false, true
    }
    // 10 次全 429 → 已 ReportRateLimit(COOLING),推进到下一把 key
    pe = *lastErr
    if pe == nil {
        return outcomeContinue, false, true
    }
    if !errorIsRetryable(pe) {
        return outcomeFatal, false, true
    }
    // 429 之后换 key 重试一次(同网络类语义)
    if e.swapToOtherKey(c, req, result) {
        ...
    }
}
```

**修改后**：
```go
if pe.ErrorType == provider.ErrorTypeRateLimit {
    // 403 rate_limit: 配额/权限问题,立即换 key,不同 key 重试
    // 背景: 403 quota 被 balanceGuardHealthy 降级为 rate_limit 后,不应反复用同一把 key
    // (2026-08-30: tokenmarket-kiro 403 重试 11 次才换 key 的根因)
    if pe.StatusCode == http.StatusForbidden {
        if pool := e.router.Pool(result.ProviderName); pool != nil {
            pool.ReportRateLimit(result.Key, pe.RetryAfter)
        }
        if e.swapToOtherKey(c, req, result) {
            if e.attemptOne(c, ctx, req, result, outProviderName, lastErr, entry) {
                return outcomeOK, false, true
            }
        }
        // 换不到其他 key / 换 key 仍失败 → 继续 failover
        return outcomeContinue, false, true
    }
    
    // 429 rate_limit: 瞬时限流,允许同 key 重试(但最多 10 次)
    if e.retrySameKeyRateLimit(c, ctx, req, result, outProviderName, lastErr, entry) {
        return outcomeOK, false, true
    }
    // 10 次全 429 → 已 ReportRateLimit(COOLING),推进到下一把 key
    pe = *lastErr
    if pe == nil {
        return outcomeContinue, false, true
    }
    if !errorIsRetryable(pe) {
        return outcomeFatal, false, true
    }
    // 429 之后换 key 重试一次(同网络类语义)
    if e.swapToOtherKey(c, req, result) {
        if e.attemptOne(c, ctx, req, result, outProviderName, lastErr, entry) {
            return outcomeOK, false, true
        }
        ...
    }
}
```

**优点**：
- ✅ 符合 HTTP 语义（403 = 权限/配额问题，429 = 瞬时限流）
- ✅ 减少无效重试（403 不再重试同一把 key）
- ✅ 提高 failover 速度
- ✅ 不影响 429 的现有重试逻辑

**影响范围**：
- 只影响 `ErrorTypeRateLimit` 且 `StatusCode == 403` 的场景
- 不影响 429、其他错误类型、以及没有 StatusCode 的历史错误

**测试验证**：
- 模拟 403 rate_limit，验证立即切换到另一把 key
- 模拟 429 rate_limit，验证仍然同 key 重试
- 验证所有 key 都失败后，会 failover 到下一个 provider

---

### 方案 B：减少 `retrySameKeyRateLimit` 的重试次数

**修改位置**: `backend/internal/proxy/proxy.go:1159-1161`

```go
// 从 10 次降到 2-3 次
rateLimitSameKeyRetries = 3
```

**优点**：
- 简单快速
- 保留了同 key 重试的机制（应对瞬时限流）

**缺点**：
- ❌ 没有解决 403 应该立即换 key 的问题
- ❌ 429 的重试次数也被减少（可能影响瞬时限流的恢复）

**不推荐**：治标不治本

---

### 方案 C：Base 层支持换 key（长期重构）

**思路**：
- 让 Base 在遇到 rate_limit 时，主动从 Pool 获取下一把 key
- 不再依赖 Proxy 层的换 key 逻辑

**修改位置**: `backend/internal/provider/anthropic_compatible/anthropic_compatible.go:202-213`

**优点**：
- 更快的 failover（不需要返回到 Proxy 层）
- 适配更多场景

**缺点**：
- ❌ 改动较大
- ❌ 需要协调 Base 和 Proxy 的职责边界
- ❌ Base 需要知道 GatewayKey 的 ProviderKeyIDs 白名单（打破分层）

**不推荐**：过度设计，方案 A 已足够

---

## 测试验证

### SQL 查询：查看问题 trace 的详细记录

```sql
SELECT 
  ROW_NUMBER() OVER (ORDER BY created_at) as seq,
  to_char(created_at, 'HH24:MI:SS.MS') as time,
  provider_name,
  provider_key_id,
  status_code,
  error_type
FROM access_logs
WHERE trace_id = '06167f9a-3ba0-43f5-809c-70fc2e126f23'
ORDER BY created_at;
```

### 手动复现步骤

1. 找一个有多把 key 的中转站（如 `tokenmarket-kiro特惠`）
2. 手动禁用其中一把 key（让它始终返回 403）
3. 发送请求，观察 access logs 中的重试行为
4. 验证修复后是否立即切换到另一把 key

### 回归测试

- 验证 429 错误仍然可以同 key 重试（保留瞬时限流场景）
- 验证 403 错误会立即换 key
- 验证所有 key 都失败后，会 failover 到下一个 provider

---

## 相关文件清单

```
backend/internal/proxy/proxy.go                                # 主要逻辑，包含 tryCandidate, swapToOtherKey, retrySameKeyRateLimit
backend/internal/provider/anthropic_compatible/anthropic_compatible.go  # Base 实现
backend/internal/provider/openai_compatible/openai_compatible.go        # Base 实现
backend/internal/provider/relay/relay.go                       # 中转站 provider
backend/internal/provider/relay/methods.go                     # SetPool 实现
backend/internal/provider/provider.go                          # 错误分类 ClassifyErrorWithBody
backend/internal/keypool/pool.go                              # Key pool 管理
config.yaml                                                    # retry.max_attempts 配置
```

---

## 联系人

- **问题发现者**: 用户
- **初步排查**: Claude Code
- **待跟进**: 下一位开发者

---

## 附录：相关代码片段

### A. Base 的 req.Key 检查逻辑
```go
// anthropic_compatible.go:161-168
key := req.Key
var err error
if key == nil {
    key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic))
    if err != nil {
        return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
    }
}
```

### B. Base 的 rate_limit 重试逻辑
```go
// anthropic_compatible.go:202-213
if errType == provider.ErrorTypeRateLimit {
    retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
    b.cfg.Pool.ReportRateLimit(key, retryAfter)
    if !retried {
        retried = true
        select {
        case <-time.After(rateLimitRetryDelay(retryAfter)):
        case <-ctx.Done():
            return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeClientDisconnected, "client disconnected during rate-limit retry")
        }
        continue
    }
}
```

### C. ClassifyErrorWithBody 的 403 处理
```go
// provider.go:461-466
case statusCode == http.StatusForbidden:
    // 403 可能是 auth,也可能 quota
    if isQuotaBody {
        return ErrorTypeQuotaExceeded
    }
    return ErrorTypeAuth
```

**注意**：如果 body 不含 quota 关键字，403 会被分类为 `ErrorTypeAuth`，而不是 `ErrorTypeRateLimit`。需要确认实际的 403 响应体内容。

---

## 下一步行动

1. ✅ **确认 `retrySameKeyRateLimit` 的实现** — 找到方法定义 (proxy.go:1623)
2. ✅ **确认 403 错误的实际分类** — 确认为 `rate_limit` (被 balanceGuardHealthy 降级)
3. ✅ **根因确认** — 403 quota 被降级为 rate_limit 后，进入 10 次同 key 重试循环
4. ⬜ **实施修复** — 在 ProviderError 中传递 HTTP 状态码，区分 403 和 429
5. ⬜ **编写测试用例** — 验证 403 立即换 key，429 同 key 重试
6. ⬜ **部署验证** — 观察线上 access logs 的改善情况
