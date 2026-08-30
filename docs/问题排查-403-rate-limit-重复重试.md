# 问题交接文档：403 rate_limit 重复重试

**日期**：2026-08-30  
**问题编号**：403-rate-limit-retry-issue  
**状态**：代码修复完成，待线上观察验证

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

2. **错误分类** (`anthropic_compatible.go:128-143`):
   ```go
   errType := provider.ClassifyErrorWithBody(status, body)  // 返回 quota_exceeded
   if errType == provider.ErrorTypeQuotaExceeded && b.balanceGuardHealthy(key) {
       errType = provider.ErrorTypeRateLimit  // 降级为 rate_limit
   }
   ```
   - `ClassifyErrorWithBody(403, body)` → 检测到 quota 关键字 → `quota_exceeded`
   - `balanceGuardHealthy(key)` 检查余额守卫：如果 key 的 remaining > threshold → 返回 true
   - 降级为 `rate_limit`（认为是瞬时误报）

3. **Base 内部重试** (`anthropic_compatible.go:213-224`):
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

4. **Proxy 层检测到 rate_limit** (`proxy.go:1428-1485`):
   ```go
   if pe.ErrorType == provider.ErrorTypeRateLimit {
       if e.retrySameKeyRateLimit(...) {  // 最多 10 次
           return outcomeOK
       }
       // 10 次全失败后才换 key
       if e.swapToOtherKey(...) { ... }
   }
   ```

5. **修复前的 `retrySameKeyRateLimit` 循环** (`proxy.go:1651-1682`):
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
   - 每次 `attemptOne` 都会调用 Base，旧逻辑下 Base 内部还会重试 1 次
   - access log 记录的是 Proxy 的 `attemptOne`，所以 11 条记录正好是：首次 1 次 + 同 key 重试 10 次
   - 在旧 Anthropic Base 逻辑下，每个 Proxy attempt 还可能产生 2 次上游 HTTP 请求，实际最多约 22 次

6. **为什么是 11 次而不是 20 次？**
   - 11 次是 access log 中的 Proxy 尝试数，不是上游 HTTP 请求数
   - `retrySameKeyRateLimit` 的 `attempts` 统计的是首次失败之后的 10 次重试，因此总记录数为 1 + 10 = 11
   - 修复后 403 在 Provider 和 Proxy 两层都不会进入同 key 重试，首把 key 只发 1 次后换 key

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
bindKey (proxy.go:1684): 设置 req.Key = result.Key (来自 router)
  ↓
attemptOne
  ↓
doRequest (proxy.go:536): pv.SendRequest(ctx, req)
  ↓
anthropic_compatible.Base.SendRequest (anthropic_compatible.go:155-237)
  ↓
检查 req.Key != nil → 直接使用这把 key (line 161-168)
  ↓
遇到 rate_limit → 等 1s → 用同一把 key 重试 1 次 (line 202-213)
  ↓
返回错误到 proxy.tryCandidate
  ↓
检测到 ErrorTypeRateLimit (proxy.go:1428)
  ↓
retrySameKeyRateLimit (proxy.go:1651): 用同一把 key 重试最多 10 次
  ↓
所有重试失败后 → swapToOtherKey (proxy.go:1699): 换 key
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
**文件**: `backend/internal/proxy/proxy.go:1428-1485`

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
**文件**: `backend/internal/proxy/proxy.go:1699-1721`

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

## 已确认的检查点

### 1. `retrySameKeyRateLimit` 方法的实现
**位置**: `backend/internal/proxy/proxy.go`（需要搜索方法定义）

**结论**：方法位于 `proxy.go:1651`，`rateLimitSameKeyRetries` 为硬编码常量 10；当前仅 HTTP 429 会进入该循环。

### 2. 403 vs 429 的处理差异
**位置**: `backend/internal/provider/provider.go:442-466`

**结论**：普通 403 会被分类为 `auth`，含 quota 关键词且余额守卫健康时才会降级成 `rate_limit`；403 与 429 使用不同重试策略。

**语义差异**：
- **429 Too Many Requests**：瞬时限流，等待后可能恢复 → 适合同 key 重试
- **403 Forbidden**：权限/配额问题，通常是 key 级别的限制 → 应该立即换 key


### 3. Relay provider 的 Pool 注入
**位置**: 
- `backend/internal/provider/relay/methods.go:129-134`
- `backend/internal/provider/relay/implementations.go`

**已验证**：
- `GenericRelayProvider.SetPool` 会调用 `impl.SetPool(pool)`
- 通过 Go 方法提升，`RelayOpenAIProvider` 和 `RelayAnthropicProvider` 会继承 `Base.SetPool`

**结论**：Pool 已通过 `GenericRelayProvider.SetPool` 注入每个协议实现，Relay Anthropic 实现继承 Base 的 `SetPool`；新增 Provider 测试也验证了请求可使用注入的 Pool。

### 4. 为什么日志是 11 次而不是 20 次？
**理论计算**：
- `retrySameKeyRateLimit` 的 10 次是首次失败之后的重试次数
- access log 的记录单位是 Proxy 的 `attemptOne`，所以是首次 1 次 + 重试 10 次 = 11 条
- 旧 Anthropic Base 每次还会内部重试 1 次，上游 HTTP 请求数与 access log 条数不同

**修复后的行为**：
- 403 在 Provider 层直接返回，在 Proxy 层直接换 key，不再对首把 key 做同 key 重试
- 429 仍按原策略允许同 key 重试

---

## 修复方案

### ✅ 方案 A：按 HTTP 状态码区分重试策略（已实施）

**核心思路**：
- `ProviderError` 已有 `StatusCode` 字段，Proxy 直接使用该字段区分策略
- Proxy 仅对 HTTP 429 进入 `retrySameKeyRateLimit`；403 及其他非 429 `rate_limit` 立即换 key
- Anthropic compatible Base 的非流式、流式路径仅对 429 做内部同 key 重试，403 直接返回给 Proxy
- Anthropic compatible Base 上报 KeyPool 后在 `ProviderError.KeyPoolReported` 留下标记，Proxy 只补报尚未上报的错误
- 去掉 Proxy 403 分支和同 key 重试结束处的额外 `ReportRateLimit`，确保一次 403 只累计一次冷却计数

**修改位置**: `backend/internal/proxy/proxy.go:1434-1485`；`backend/internal/provider/anthropic_compatible/anthropic_compatible.go:106-115,213-224`

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
    // 只有 HTTP 429 允许同 key 重试；403/其他非 429 直接换 key。
    if pe.StatusCode != http.StatusTooManyRequests {
        if e.swapToOtherKey(c, req, result) {
            if e.attemptOne(c, ctx, req, result, outProviderName, lastErr, entry) {
                return outcomeOK, false, true
            }
        }
        // 换不到其他 key / 换 key 仍失败 → 继续 failover
        return outcomeContinue, false, true
    }

    // 429 rate_limit: 瞬时限流,允许同 key 重试(最多 10 次)
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

**实现文件**：
- `backend/internal/proxy/proxy.go`
- `backend/internal/provider/anthropic_compatible/anthropic_compatible.go`
- `backend/internal/proxy/proxy_test.go`
- `backend/internal/provider/anthropic_compatible/anthropic_compatible_test.go`

**影响范围**：
- `ErrorTypeRateLimit` 只有 HTTP 429 继续同 key 重试
- HTTP 403 及其他非 429 `rate_limit` 直接换 key；其他错误类型不变
- Anthropic Base 保留 HTTP 429 和 HTTP 200 内嵌错误的一次重试

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

### B. Base 的 rate_limit 重试逻辑（当前）
```go
// anthropic_compatible.go:202-213
if errType == provider.ErrorTypeRateLimit {
    retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
    b.cfg.Pool.ReportRateLimit(key, retryAfter)
    if shouldRetryRateLimitStatus(httpResp.StatusCode) && !retried {
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

其中 `shouldRetryRateLimitStatus` 只允许 HTTP 429 和 HTTP 200（MiniMax
内嵌错误）进入 Provider 内部重试；HTTP 403 会立即返回给 Proxy。

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

1. ✅ **确认 `retrySameKeyRateLimit` 的实现** — 找到方法定义 (proxy.go:1651)
2. ✅ **确认 403 错误的实际分类** — 确认为 `rate_limit` (被 balanceGuardHealthy 降级)
3. ✅ **根因确认** — 403 quota 被降级为 rate_limit 后，进入 10 次同 key 重试循环
4. ✅ **实施修复** — Proxy 和 Anthropic Base 两层区分 403/429，403 立即换 key
5. ✅ **去重冷却上报** — Anthropic compatible Base 用 `KeyPoolReported` 标记已上报错误，Proxy 不再重复累计；403 分支和同 key 重试收尾也不再额外上报
6. ✅ **编写测试用例** — 覆盖非流式/流式 403、429 保持重试、200 内嵌错误换 key，以及 Provider 已上报时 Proxy 不重复累计
7. ✅ **提交代码** — 初版 Commit `a21994c` 已推送；本次后续修复随当前提交推送
8. ⬜ **线上验证** — 观察后续 access logs，确认 403 不再重复同 key 重试
