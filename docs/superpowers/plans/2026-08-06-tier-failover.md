# tier 降档语义改造实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 failover 语义从「错误一律推进候选」改为「额度驱动分层」:token_plan 层额度未耗尽,请求绝不落 api 付费层;网络类错误在层内解决(换 key → 同层换 provider),额度类错误才降档。

**Architecture:** 4 个基础能力(CheckQuota / EndpointFor / AcquireFromTierExcluding / RouteResult.Tier)+ 1 个核心改造(proxy 候选循环错误分类路由)+ config/文档。TDD:每个 task 先写失败测试。

**Tech Stack:** Go(proxy / router / keypool / quotacheck / provider 五个包)+ config.yaml + 文档。

## Global Constraints

- **不变式(优先级最高)**:token_plan 层额度未耗尽 → 请求绝不落到 api 付费层;网络类错误层内穷尽 = 请求失败返回,不降档
- 额度判定三源:轮询(IsPolledAndExhausted)+ 错误码(quota_exceeded / 429 / base_resp 1008·2056)+ 主动查询(CheckQuota)
- 主动查询失败 → 按未耗尽处理(继续层内换有额度的 key)
- 主动查询 = 统一基础方法(quotacheck.CheckQuota,查 RegisterBalancer 注册表);厂商包不各自写调用
- 网络类 = connection / timeout / server_error;额度类 = quota_exceeded / rate_limit(429);auth 换 key 不降档;invalid_request / model_not_found / client_disconnected 不可重试直接失败
- 不新增 key 状态、不改熔断器、不动 quotacheck 轮询;每 key 一次机会
- 换 key 重试是路由层显式重新 acquire(踩坑 #15:冷却标到真正发请求的 key)
- 现在 token_plan 层只有 minimax(同层换 provider 路径暂走不到),但语义与测试按完整层写
- api/free 层同规则;free 层无 provider 不特殊处理

---

### Task 1:quotacheck.CheckQuota 统一方法

**Files:**
- Modify: `backend/internal/quotacheck/prober.go`
- Test: `backend/internal/quotacheck/prober_test.go`(新建)

**Interfaces:**
- Consumes: 现有 `LookupBalancer(providerName) Balancer`、`Balancer.FetchBalance(ctx, baseURL, k) (Balance, error)`、`Balance.HasQuota bool`(prober.go:42-66)
- Produces: `CheckQuota(ctx context.Context, providerName, baseURL string, k *keypool.Key) (hasQuota bool, err error)` — Task 5 消费

- [ ] **Step 1:写失败测试**

在 `backend/internal/quotacheck/prober_test.go`:

```go
package quotacheck

import (
	"context"
	"errors"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

type fakeBalancer struct {
	hasQuota bool
	err      error
}

func (f *fakeBalancer) FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (Balance, error) {
	if f.err != nil {
		return Balance{}, f.err
	}
	return Balance{Raw: 100, HasQuota: f.hasQuota, Kind: "percent"}, nil
}

func TestCheckQuota(t *testing.T) {
	k := &keypool.Key{ID: "7", ProviderName: "minimax", Name: "key-1"}

	t.Run("已注册且有余量 → true", func(t *testing.T) {
		RegisterBalancer("test-quota-ok", &fakeBalancer{hasQuota: true})
		got, err := CheckQuota(context.Background(), "test-quota-ok", "https://x.example", k)
		if err != nil || !got {
			t.Fatalf("want (true, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("已注册且耗尽 → false", func(t *testing.T) {
		RegisterBalancer("test-quota-out", &fakeBalancer{hasQuota: false})
		got, err := CheckQuota(context.Background(), "test-quota-out", "https://x.example", k)
		if err != nil || got {
			t.Fatalf("want (false, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("未注册 → (true, nil) 未知按未耗尽", func(t *testing.T) {
		got, err := CheckQuota(context.Background(), "no-such-provider", "https://x.example", k)
		if err != nil || !got {
			t.Fatalf("want (true, nil), got (%v, %v)", got, err)
		}
	})

	t.Run("FetchBalance 出错 → 返回错误", func(t *testing.T) {
		RegisterBalancer("test-quota-err", &fakeBalancer{err: errors.New("boom")})
		_, err := CheckQuota(context.Background(), "test-quota-err", "https://x.example", k)
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}
```

- [ ] **Step 2:跑测试确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/quotacheck/ -run TestCheckQuota -v`
Expected: 编译失败(undefined: CheckQuota)

- [ ] **Step 3:实现**

在 `backend/internal/quotacheck/prober.go`(LookupBalancer 定义之后)加:

```go
// CheckQuota 请求路径主动查额度 — 网络类错误后由 proxy 触发,区分
// 「网络故障但额度充足(→ 留在层内重试)」vs「额度耗尽(→ 可降档)」。
// 未注册 balancer 的 provider → 返回 (true, nil):额度未知按未耗尽,
// 与「拿不到耗尽证据就不降档」的不变式一致。
func CheckQuota(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error) {
	bal := LookupBalancer(providerName)
	if bal == nil {
		return true, nil
	}
	balResult, err := bal.FetchBalance(ctx, baseURL, k)
	if err != nil {
		return false, err
	}
	return balResult.HasQuota, nil
}
```

- [ ] **Step 4:跑测试确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/quotacheck/ -run TestCheckQuota -v`
Expected: 4 个 subtest 全 PASS

- [ ] **Step 5:Commit**

```bash
git add backend/internal/quotacheck/prober.go backend/internal/quotacheck/prober_test.go
git commit -m "feat(quotacheck): CheckQuota 统一主动查额度 — 复用 balancer 注册表

- 未注册 → (true, nil) 未知按未耗尽(不降档原则)
- FetchBalance 错误透传,由调用方决定(查询失败 → 按未耗尽)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2:provider.Manager.EndpointFor

**Files:**
- Modify: `backend/internal/provider/manager.go`
- Test: `backend/internal/provider/manager_test.go`(现有文件,加测试)

**Interfaces:**
- Consumes: `ManagerConfig`(含 `Endpoints` 或 per-provider Endpoint 字段——以 LoadFromConfig 现有解析为准,读代码确认字段名)
- Produces: `func (m *Manager) EndpointFor(name string) string` — Task 5 里 proxy 拿 baseURL 给 CheckQuota

- [ ] **Step 1:读现状确认字段名**

Run: `grep -n "Endpoint" /home/hhhh/llm-gateway/backend/internal/provider/manager.go | head -20`
确认 ManagerConfig 里 provider endpoint 的存储字段(如 `Endpoints map[string]string` 或 config 结构内嵌),用它实现。

- [ ] **Step 2:写失败测试**

在 `backend/internal/provider/manager_test.go`(先看现有测试怎么构造 ManagerConfig,照抄结构):

```go
func TestManager_EndpointFor(t *testing.T) {
	// cfg: 注册一个 provider 名为 "fake",Endpoint 为 "https://fake.example/v1"
	// m, err := New(...); m.LoadFromConfig(ctx, cfg)
	// got := m.EndpointFor("fake")   → "https://fake.example/v1"
	// got := m.EndpointFor("nope")   → ""
}
```

(测试里构造 ManagerConfig 的方式以现有 manager_test.go 的 LoadFromConfig 测试为准,保持一致。)

- [ ] **Step 3:跑测试确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/provider/ -run TestManager_EndpointFor -v`
Expected: 编译失败(undefined: EndpointFor)

- [ ] **Step 4:实现**

在 manager.go 加:

```go
// EndpointFor 查 provider 的 endpoint(给 quotacheck.CheckQuota 提供 baseURL)
func (m *Manager) EndpointFor(name string) string {
	// 从 LoadFromConfig 存下的 endpoint 映射查;未注册返回 ""
}
```

- [ ] **Step 5:跑测试确认通过**

Run: 同上
Expected: PASS

- [ ] **Step 6:Commit**

```bash
git add backend/internal/provider/manager.go backend/internal/provider/manager_test.go
git commit -m "feat(provider): Manager.EndpointFor — 请求路径查 provider baseURL

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3:keypool.AcquireFromTierExcluding

**Files:**
- Modify: `backend/internal/keypool/pool.go`
- Test: `backend/internal/keypool/pool_test.go`(现有文件,加测试)

**Interfaces:**
- Consumes: 现有 `AcquireFromTier(tier string, allowedIDSet map[uint]struct{}, proto string) (*Key, error)`(pool.go:189)——内部已有 ID 集合过滤逻辑可复用
- Produces: `func (p *Pool) AcquireFromTierExcluding(tier, excludeID string, proto string) (*Key, error)` — Task 5 换 key 重试用

- [ ] **Step 1:写失败测试**

在 `backend/internal/keypool/pool_test.go`(构造 Pool 的方式以现有测试为准):

```go
func TestPool_AcquireFromTierExcluding(t *testing.T) {
	// pool: 2 个 key — "1"(billing_source=token_plan)、"2"(billing_source=token_plan)
	// k, err := pool.AcquireFromTierExcluding("token_plan", "1", "")
	//  → 必须返回 key "2",不能返回 "1"
	// k, err := pool.AcquireFromTierExcluding("token_plan", "1", "")
	//  → 连续第二次调用仍返回 "2"(排除逻辑不依赖轮询位置)
}
```

- [ ] **Step 3:跑测试确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/keypool/ -run TestPool_AcquireFromTierExcluding -v`
Expected: 编译失败

- [ ] **Step 4:实现**

在 pool.go 加(复用 AcquireFromTier 的过滤逻辑,在 allowedIDSet 过滤后追加 exclude 检查):

```go
// AcquireFromTierExcluding 从指定 tier 桶挑 key,排除指定 ID(换 key 重试用)
// excludeID 为空 = 与 AcquireFromTier 等价
func (p *Pool) AcquireFromTierExcluding(tier, excludeID string, proto string) (*Key, error) {
	// 实现:调内部公共逻辑(把 AcquireFromTier 的 allowedIDSet 过滤扩展为
	// 同时排除 excludeID;注意 key.ID 是 DB 数字 ID 字符串,见 parseKeyIDUint)
}
```

(实现时把 AcquireFromTier 主体抽成内部函数 `acquireFromTierLocked(tier, allowedIDSet, excludeID, proto)`,两个公开方法都调它,避免复制逻辑。)

- [ ] **Step 5:跑测试确认通过**

Run: 同上
Expected: PASS

- [ ] **Step 6:Commit**

```bash
git add backend/internal/keypool/pool.go backend/internal/keypool/pool_test.go
git commit -m "feat(keypool): AcquireFromTierExcluding — 换 key 重试排除已失败 key

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4:RouteResult 带 Tier + 迭代器暴露

**Files:**
- Modify: `backend/internal/router/router.go`
- Test: `backend/internal/router/router_test.go`(现有文件,加测试)

**Interfaces:**
- Consumes: `KeyCandidate` 已有 `Tier string` 字段(router.go:339-347)
- Produces: `RouteResult.Tier string` 字段(Next() 赋值)— Task 5 判定层切换;`RouteResult.KeyID()` 或直接读 `Key.ID`

- [ ] **Step 1:读现状**

Run: `grep -n "type RouteResult" -A 12 /home/hhhh/llm-gateway/backend/internal/router/router.go`
确认 RouteResult 字段,加 `Tier string`。

- [ ] **Step 2:改代码(先改后测,纯加字段)**

在 RouteResult 加 `Tier string`;`Next()` 的两个 return 处赋 `Tier: c.Tier`。

- [ ] **Step 3:写测试**

在 `backend/internal/router/router_test.go`(用现有 buildEngine/router 构造方式):

```go
func TestRouteIterator_TierTagged(t *testing.T) {
	// 两个 provider:minimax(token_plan)、deepseek(api),mock pools
	// iter := ...; 依次 Next():
	//  1st → ProviderName=minimax, Tier="token_plan"
	//  2nd → ProviderName=deepseek, Tier="api"
}
```

- [ ] **Step 4:跑测试确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/router/ -run TestRouteIterator_TierTagged -v`
Expected: PASS

- [ ] **Step 5:全量测试(防字段改动破坏现有断言)**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/router/`
Expected: 全 PASS

- [ ] **Step 6:Commit**

```bash
git add backend/internal/router/router.go backend/internal/router/router_test.go
git commit -m "feat(router): RouteResult 带 Tier — 层切换判定基础

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5:proxy 候选循环错误分类路由(核心)

**Files:**
- Modify: `backend/internal/proxy/proxy.go`(主循环 361-433 行 + runWithFirstResult 918-950 行)
- Test: `backend/internal/proxy/proxy_test.go`(加集成测试)

**Interfaces:**
- Consumes: Task 1 `CheckQuota`、Task 2 `EndpointFor`、Task 3 `AcquireFromTierExcluding`、Task 4 `RouteResult.Tier`
- Produces: 行为矩阵 6 行全部生效;`handleAllFailed` 复用为层内穷尽失败出口

- [ ] **Step 1:写失败测试(行为矩阵 6 行)**

在 `backend/internal/proxy/proxy_test.go` 加(扩展 buildEngine 支持:多 provider、每 provider 多 key、可控错误序列——先看 buildEngine 现状,加 `buildEngineMulti(t, providers []*fakeProvider, pools map[string]*keypool.Pool)` 或等价 helper):

```go
// 1) 网络类层内穷尽 → 失败返回,不降档(不变式)
//    provider minimax(token_plan) 2 把 key 都返回 connection 错误;
//    provider deepseek(api) healthy → 最终必须 502/超时,请求不能到 deepseek
func TestProxy_NetworkExhaustedInTier_FailsWithoutDowngrade(t *testing.T)

// 2) 额度类全层穷尽 → 降档 api 层成功
//    minimax(token_plan) 返回 quota_exceeded;deepseek(api) healthy → 200,provider=deepseek
func TestProxy_QuotaExhausted_DowngradesToApi(t *testing.T)

// 3) 换 key 重试:key-1 connection 失败 → 同 provider key-2 成功(不走 failover)
//    minimax 2 把 key:key-1 connection 错误,key-2 healthy → 200,provider=minimax
func TestProxy_RetryWithSecondKey(t *testing.T)

// 4) 主动查询失败 → 按未耗尽:继续层内尝试(不降档)
//    minimax(token_plan, 1 把 key connection 失败,CheckQuota 返回 error)
//    deepseek(api) healthy → 请求失败返回,不进 deepseek
func TestProxy_CheckQuotaError_StaysInTier(t *testing.T)

// 5) 同层换 provider:minimax 全网络失败 → 同层 kimi(token_plan) 成功
//    两个 token_plan provider + 一个 api provider → 200,provider=kimi
func TestProxy_SameTierNextProvider(t *testing.T)

// 6) 不可重试错误直接失败:invalid_request 不重试不降档(现有语义回归)
func TestProxy_InvalidRequest_NoRetry(t *testing.T)
```

(测试 4 需要 CheckQuota 可注入——proxy 侧通过一个可替换的函数变量 `var quotaCheckFn = quotacheck.CheckQuota` 实现,测试里替换;或者 fakeBalancer 注册进注册表,返回 err——优先后者,不动 proxy 结构。测试 1/5 需要多个 provider,检查 buildEngine 是否支持多 provider,不支持则扩展。)

- [ ] **Step 2:跑测试确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/proxy/ -run "TestProxy_NetworkExhausted|TestProxy_QuotaExhausted|TestProxy_RetryWithSecondKey|TestProxy_CheckQuotaError|TestProxy_SameTier|TestProxy_InvalidRequest" -v`
Expected: 新测试失败(现在网络类会降档 / 不换 key)

- [ ] **Step 3:实现核心逻辑**

主循环(handle,约 361-433 行)改造,骨架:

```go
currentTier := ""
quotaEvidenceInTier := false // 当前层是否出现过额度类证据

for {
	if attempts >= e.maxRetry {
		break
	}
	attempts++

	result, err := iter.Next()
	if err != nil {
		break // 没更多候选
	}

	// 层切换判定:候选 tier 变了 = 上一层穷尽
	if result.Tier != currentTier {
		if currentTier != "" && !quotaEvidenceInTier {
			// 网络类穷尽 → 不变式:失败返回,不降档
			e.handleAllFailed(c, req, lastErr, traceID)
			return
		}
		// 有额度证据 → 降档,接受新 tier 候选
		quotaEvidenceInTier = false
		currentTier = result.Tier
	}
	// 首次进入:currentTier = result.Tier

	// 尝试候选(现有逻辑:req.Key = result.Key、rewriteModel、doRequest/doStream)
	// 失败分支:
	//   网络类(perr.ErrorType ∈ {connection, timeout, server_error}):
	//     - 同 provider 换 key 重试一次:pool.AcquireFromTierExcluding(result.Tier, result.Key.ID, proto)
	//       拿到新 key → 换 req.Key → 重发 → 成功 return
	//     - 换不到 key 或仍失败(provider 全 key 网络失败)→ 主动查询:
	//       has, qerr := CheckQuota(ctx, result.ProviderName,
	//           e.router.Manager().EndpointFor(result.ProviderName), result.Key)
	//       - has == true 或 qerr != nil(查询失败=未知)→ 按未耗尽,继续循环(下一候选)
	//       - has == false(确认耗尽)→ quotaEvidenceInTier = true,继续循环
	//   额度类(quota_exceeded / rate_limit):
	//     - quotaEvidenceInTier = true,继续循环
	//   不可重试:break
}
```

runWithFirstResult 做同样的改造(把 tryOneCandidate 的循环体改为共享的 `tryCandidate` helper,两个循环都调,避免双份逻辑漂移——先看 runWithFirstResult/tryOneCandidate 现状再抽,保持行为一致)。

- [ ] **Step 4:跑测试确认通过**

Run: Task 5 Step 2 的命令
Expected: 6 个新测试全 PASS

- [ ] **Step 5:回归全量测试**

Run: `cd /home/hhhh/llm-gateway/backend && go build ./... && go test ./...`
Expected: 全 PASS(现有 failover/熔断测试不能破)

- [ ] **Step 6:Commit**

```bash
git add backend/internal/proxy/proxy.go backend/internal/proxy/proxy_test.go
git commit -m "feat(proxy): 错误分类分层 failover — 网络类层内解决,额度类才降档

- 不变式:token_plan 层额度未耗尽绝不落 api 层,网络类穷尽失败返回
- 网络类:同 provider 换 key 重试(AcquireFromTierExcluding)→ 同层下个 provider
- 额度类:标记后层内换候选,全层额度穷尽才降档
- 层切换由 RouteResult.Tier 判定;主循环与 runWithFirstResult 共享 tryCandidate

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6:config.yaml + 文档同步

**Files:**
- Modify: `config.yaml`(minimax timeout 90s → 30s,第 151 行附近)
- Modify: `.claude/skills/provider-vendor/SKILL.md`(概念速记补一句)
- Modify: `docs/provider厂商定制包指南.md`(Step 4 余额查询处补一句)
- Modify: `docs/踩坑与排错.md`(新增坑 #18)

**Interfaces:**
- Consumes: 无(纯配置/文档)
- Produces: 运行期 timeout 生效;skill 文档与新机制一致

- [ ] **Step 1:改 config.yaml**

`config.yaml` 第 151 行附近 `timeout: 90s`(minimax anthropic 面)→ `timeout: 30s`;同文件 minimax-openai 块(228 行附近)同样 90s → 30s。

- [ ] **Step 2:改 skill 与指南**

SKILL.md 概念速记最后加一行:
`- 请求路径主动查额度走统一入口 quotacheck.CheckQuota(复用 RegisterBalancer 注册表),厂商包不需要写自己的查询调用`

指南文档 Step 4(余额查询)「有官方余额端点的厂商一律写」句后补一句:
`- balancer 会被请求路径的主动查额度复用(quotacheck.CheckQuota):网络类错误后由网关统一调用,厂商包无需另写查询入口`

- [ ] **Step 3:踩坑文档新增坑 #18**

在 `docs/踩坑与排错.md` 末尾(调试三板斧之前)加:

```markdown
## 18. 网络类错误不降档(额度未耗尽绝不落 api 层)

token_plan 层额度未耗尽时,connection/timeout/5xx 类错误在层内解决
(换 key → 换同层 provider),全层穷尽 = 请求失败返回,**不降档到 api 付费层**;
只有额度类证据(quota_exceeded / 429 / 主动查询确认耗尽)才降档。
误判信号:看到 failover 日志里 token_plan provider 网络失败后直接出现
api provider——先查是不是新代码没生效,再查该层是否真有额度证据。
```

- [ ] **Step 4:验证**

Run: `grep -n "timeout: 30s" config.yaml | head -3 && grep -c "CheckQuota" .claude/skills/provider-vendor/SKILL.md docs/provider厂商定制包指南.md docs/踩坑与排错.md`
Expected: timeout 两处命中(或更多);CheckQuota 三处文档各 ≥1

- [ ] **Step 5:Commit**

```bash
git add config.yaml .claude/skills/provider-vendor/SKILL.md docs/provider厂商定制包指南.md docs/踩坑与排错.md
git commit -m "feat(config): minimax timeout 90s→30s + 文档同步 tier 降档语义

- timeout 30s:换 key 重试最坏 60s 内完成,不会分钟级累积
- skill/指南:balancer 会被请求路径 CheckQuota 复用
- 踩坑 #18:网络类错误不降档

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7:全量验证 + 干跑

**Files:**
- 无文件改动(只读验证)

**Interfaces:**
- Consumes: Task 1-6 全部交付物
- Produces: 验证结论

- [ ] **Step 1:全量构建 + 测试**

Run: `cd /home/hhhh/llm-gateway/backend && go build ./... && go test ./...`
Expected: 全 PASS,0 失败

- [ ] **Step 2:行为矩阵逐行核对(对照 spec §4)**

对照 spec 行为矩阵 6 行,确认 Task 5 的 6 个集成测试各覆盖一行;矩阵第 3 行(同层换 provider)由 TestProxy_SameTierNextProvider 覆盖。逐行列核对表。

- [ ] **Step 3:汇报**

对话中汇报:测试统计、矩阵覆盖、config 变更;不 commit。
