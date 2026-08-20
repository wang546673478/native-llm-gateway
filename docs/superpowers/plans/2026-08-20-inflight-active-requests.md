# 实时活跃请求(In-flight)实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供一个只读、实时的「正在跑的对话」视图 —— 谁在打、什么 model、走哪家 provider、哪个 gateway key、流式/非流式、已耗时。

**Architecture:** 新增 `internal/inflight` 包维护「trace_id → 请求状态」的并发安全内存 map;proxy 在请求开始/切 provider/结束三个点旁路读写它(改现有逻辑);一个只读 `GET /api/v1/inflight` 端点读快照;前端新增独立 `/inflight` 页面 1s 轮询渲染。

**Tech Stack:** Go(golang 并发、gin、zap)、Vue 3 + TypeScript + naive-ui、vue-router。

## Global Constraints

- **低耦合高内聚是最高原则**:`inflight.Registry` 必须只暴露窄接口(Put/SetProvider/Delete/Snapshot),proxy 不 import 其内部实现;handler 不 import inflight 包(靠闭包注入);未来换 Redis 只改 inflight 包内部。
- 新字段/新组件必须遵循现有注入模式:与 `AccessLog` / `FingerprintSanitizer` 同构(通过 `proxy.Config` 传 `*inflight.Registry`,nil = 不启用)。
- config 未新增任何字段,故无需改 config.yaml / example / docker 三份模板(自检清单的「config 改了?」项此处不触发)。
- Go 测试用标准库 `testing`(现有 `*_test.go` 均为标准库,无第三方断言库)。
- 前端类型/契约必须集中在 `api/client.ts` 单一源;枚举/常量不硬编码。
- 每个 Task 结束必须 `git commit`(提交信息带 `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` 尾行)。
- 后端每 Task 必跑 `make test` + `make vet`;前端最后一 Task 必跑 `npx vue-tsc --noEmit`。

---

### Task 1: inflight 包 —— 内存快照 Registry

**Files:**
- Create: `backend/internal/inflight/inflight.go`
- Test: `backend/internal/inflight/inflight_test.go`

**Interfaces:**
- Consumes: 无(这是最底层新包)
- Produces:
  - `type Snapshot struct { TraceID string; StartedAt time.Time; Model string; ProviderName string; GatewayKeyName string; IsStream bool }`
  - `func NewRegistry() *Registry`(或 `type Registry struct` + 构造器,签名见 Step 3)
  - `func (r *Registry) Put(s *Snapshot)`
  - `func (r *Registry) SetProvider(traceID, provider string)`
  - `func (r *Registry) Delete(traceID string)`
  - `func (r *Registry) Snapshot() []*Snapshot` —— 按 `StartedAt` 升序

**说明**:`inflight` 是一个无外部依赖的小包,只依赖标准库 `sync` / `time`。`Put` 需要并发安全(同一 Registry 会被多个 goroutine 的请求同时写)。`Snapshot` 返回深拷贝的切片(拷贝 `Snapshot` 结构体本身,避免调用方读到后续被 `SetProvider` 改写的内存),但 `Snapshot` 内字段都是值类型/string,浅拷贝即足够。

- [ ] **Step 1: 编写失败测试**

```go
package inflight

import (
	"testing"
	"time"
)

func TestPutSnapshotAndDelete(t *testing.T) {
	r := NewRegistry()

	r.Put(&Snapshot{TraceID: "t1", StartedAt: time.Now().UTC(), Model: "m1", IsStream: true})

	if got := r.Snapshot(); len(got) != 1 {
		t.Fatalf("after Put, Snapshot len = %d, want 1", len(got))
	} else if got[0].TraceID != "t1" || got[0].Model != "m1" {
		t.Fatalf("Snapshot[0] = %+v, want trace_id=t1 model=m1", got[0])
	}

	r.Delete("t1")
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("after Delete, Snapshot len = %d, want 0", len(got))
	}
}

func TestSetProviderUpdatesExisting(t *testing.T) {
	r := NewRegistry()
	r.Put(&Snapshot{TraceID: "t1", StartedAt: time.Now().UTC()})

	r.SetProvider("t1", "deepseek")
	if got := r.Snapshot(); len(got) != 1 || got[0].ProviderName != "deepseek" {
		t.Fatalf("after SetProvider, got %+v, want provider=deepseek", got)
	}

	// failover 切 provider
	r.SetProvider("t1", "minimax")
	if got := r.Snapshot(); got[0].ProviderName != "minimax" {
		t.Fatalf("after failover SetProvider, provider = %q, want minimax", got[0].ProviderName)
	}
}

func TestSetProviderUnknownTraceIsNoop(t *testing.T) {
	r := NewRegistry()
	r.SetProvider("ghost", "deepseek") // 不该 panic,不该插入
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("SetProvider on unknown trace inserted an entry: %+v", got)
	}
}

func TestSnapshotSortedByStartedAt(t *testing.T) {
	r := NewRegistry()
	base := time.Now().UTC()
	r.Put(&Snapshot{TraceID: "later", StartedAt: base.Add(2 * time.Second)})
	r.Put(&Snapshot{TraceID: "earlier", StartedAt: base})

	got := r.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(got))
	}
	if got[0].TraceID != "earlier" || got[1].TraceID != "later" {
		t.Fatalf("Snapshot not sorted by StartedAt: got [%s, %s], want [earlier, later]",
			got[0].TraceID, got[1].TraceID)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/inflight/...`
Expected: FAIL —— `package inflight: no Go files`(包还不存在)

- [ ] **Step 3: 编写最小实现**

```go
// Package inflight 维护「trace_id → 请求状态」的并发安全内存快照,
// 供实时「活跃请求」视图读取。纯内存态、结束即消、不留历史。
//
// 窄接口约定:Registry 只暴露 Put / SetProvider / Delete / Snapshot 四个方法。
// 未来多实例上 Redis 时,只需替换本包内部 map 为 redis.Client,proxy 层不改一行。
package inflight

import (
	"sort"
	"sync"
	"time"
)

// Snapshot 一条活跃请求的只读快照。
// 全字段为值类型 + string,Snapsohot 返回的结构体拷贝即与后续写入隔离。
type Snapshot struct {
	TraceID        string
	StartedAt      time.Time // 请求开始,elapsed_ms 由调用方现算(now - StartedAt)
	Model          string    // alias 解析后的真实 model
	ProviderName   string    // 当前正在打的 vendor,随 failover 实时更新
	GatewayKeyName string
	IsStream       bool
}

// Registry 并发安全的内存快照表。
type Registry struct {
	mu sync.RWMutex
	m  map[string]*Snapshot
}

// NewRegistry 构造一个空的 Registry。
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]*Snapshot)}
}

// Put 记录一条最早开始的活跃请求。若同 TraceID 已存在则覆盖。
func (r *Registry) Put(s *Snapshot) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s.TraceID] = s
}

// SetProvider 更新一条活跃请求当前正在打的 provider。未知 TraceID 是 no-op。
func (r *Registry) SetProvider(traceID, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.m[traceID]; ok {
		s.ProviderName = provider
	}
}

// Delete 移除一条已结束的请求。未知 TraceID 是 no-op。
func (r *Registry) Delete(traceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, traceID)
}

// Snapshot 返回当前所有活跃请求的只读列表,按 StartedAt 升序。
func (r *Registry) Snapshot() []*Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Snapshot, 0, len(r.m))
	for _, s := range r.m {
		cp := *s // 拷贝结构体,与后续 SetProvider 写入隔离
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /home/hhhh/llm-gateway/backend && go test ./internal/inflight/...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
cd /home/hhhh/llm-gateway
git add backend/internal/inflight/inflight.go backend/internal/inflight/inflight_test.go
git commit -m "feat(inflight): 活跃请求内存快照 Registry(Put/SetProvider/Delete/Snapshot)

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: proxy 接入 —— 三个旁路插入点

**Files:**
- Modify: `backend/internal/proxy/proxy.go:41`(Engine 加 `inflight` 字段)
- Modify: `backend/internal/proxy/proxy.go:77-81`(Config 加 `Inflight` 字段)
- Modify: `backend/internal/proxy/proxy.go:103-124`(NewEngine 装配 + 默认 nil)
- Modify: `backend/internal/proxy/proxy.go:200-222`(defer 里加 Delete)
- Modify: `backend/internal/proxy/proxy.go:321`(路由前加 Put)
- Modify: `backend/internal/proxy/proxy.go:1283,1298,1301,1308,1311`(attemptOne 各处 `*outProviderName =` 后加 SetProvider)

**Interfaces:**
- Consumes: `inflight.NewRegistry()` / `*inflight.Registry`(Task 1)、`providerProviderName` 已是若干处 `*outProviderName = result.ProviderName`
- Produces(listServer 后续依赖):`Engine.inflight *inflight.Registry`,以及 `proxy.Config.Inflight` 字段(server.go → proxy 装配用)

**说明**:三个插入点全是「旁路」,不改现有逻辑分支。`Put` 放在 body/alias 解析完成、构造好 `req` 之后、`e.router.Route` 之前(proxy.go 约 321 行的 `// 4. 路由` 注释上方),此时 `model` / `isStream` / `gk`(网关 key 名)都已确定。`SetProvider` 紧贴在每一处 `*outProviderName = result.ProviderName` 之后,读同一个变量。`Delete` 放在 `handle()` 的 defer 里 `e.accessLog.RecordAsync(entry)` 同一处(但 **要独立于 `entry == nil || e.accessLog == nil` 的 early-return**——inflight 不依赖 accesslog 是否启用,见 Step 3 关键注释)。

- [ ] **Step 1: Engine 结构体加字段**

在 `proxy.go:41` 附近(`accessLog *accesslog.Recorder` 那行之后)加:

```go
	// inflight 活跃请求内存快照(nil = 不启用)。与 AccessLog 同构 —
	// server 注入,proxy 只通过窄接口(Put/SetProvider/Delete)读写,
	// 不 import inflight 包内部实现。为「实时正在跑的对话」视图供数。
	inflight *inflight.Registry
```

并在文件顶部 import 块加一行 `"github.com/wang546673478/native-llm-gateway/internal/inflight"`。

- [ ] **Step 2: Config 结构体加字段**

在 `proxy.Config`(proxy.go 约 77-81)加:

```go
	// Inflight 活跃请求内存快照(可选);nil = 不启用。
	Inflight *inflight.Registry
```

- [ ] **Step 3: NewEngine 装配**

在 `NewEngine`(proxy.go 约 103-124)的 `return &Engine{...}` 里,加 `inflight: cfg.Inflight,`(与 `accessLog: cfg.AccessLog` 相邻)。**不要加任何默认 fallback** —— 保持 nil(测试/未装配时零成本)。

- [ ] **Step 4: defer 里加 Delete**

`handle()` 的 defer(proxy.go 约 200-222)当前是:

```go
	defer func() {
		if entry == nil || e.accessLog == nil {
			return
		}
		entry.StatusCode = c.Writer.Status()
		...
		e.accessLog.RecordAsync(entry)
	}()
```

改为在 defer 一进来就处理 inflight 清理(**在 `entry == nil` 判定之前**,使 inflight 与 accesslog 开关解耦):

```go
	defer func() {
		// inflight 清理独立于 accesslog:即使 accesslog 关闭,活跃请求也要移除。
		// (Put 与 Delete 配对,成功/失败/panic 都经此 defer 收尾,防泄漏)
		if e.inflight != nil {
			e.inflight.Delete(traceID)
		}
		if entry == nil || e.accessLog == nil {
			return
		}
		entry.StatusCode = c.Writer.Status()
		...
		e.accessLog.RecordAsync(entry)
	}()
```

- [ ] **Step 5: 路由前加 Put**

在 proxy.go 约 321 行 `// 4. 路由(failover iterator)` 注释**之前**加:

```go
	// Inflight:请求已确定 model/is_stream/gateway key,即将开始路由 —
	// 写入活跃请求快照(provider 此刻未知,由 attemptOne 里的 SetProvider 补)。
	if e.inflight != nil {
		gk := e.gkCtx.Get(c)
		gkName := ""
		if gk != nil {
			gkName = gk.Name
		}
		e.inflight.Put(&inflight.Snapshot{
			TraceID:        traceID,
			StartedAt:      time.Now().UTC(),
			Model:          model,
			GatewayKeyName: gkName,
			IsStream:       isStream,
		})
	}
```

- [ ] **Step 6: attemptOne 里加 SetProvider**

`attemptOne`(proxy.go 约 1270-1315)里有 5 处 `*outProviderName = result.ProviderName`(约 1283 / 1298 / 1301 / 1308 / 1311)。在**每一处之后紧跟一行**(读同一个变量,provider 变时同步快照):

```go
		if e.inflight != nil {
			e.inflight.SetProvider(req.TraceID, result.ProviderName)
		}
```

> 注意:5 处里第 1283 行是 `*outProviderName = result.ProviderName` 后紧跟 `*lastErr = ...`(provider not found 分支)也应加。全部用同一个 snippet,保证「现在打哪家」实时跟着 result 走。

- [ ] **Step 7: 编译 + 全量测试**

Run:
```bash
cd /home/hhhh/llm-gateway/backend && make build && make vet && make test
```
Expected: 全绿。既有 proxy 测试(nil inflight)不应受影响,因为所有新增写点都包在 `if e.inflight != nil` 里。

- [ ] **Step 8: 提交**

```bash
cd /home/hhhh/llm-gateway
git add backend/internal/proxy/proxy.go
git commit -m "feat(inflight): proxy 三处旁路接入活跃请求快照(Put/SetProvider/Delete)

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 装配 —— server.go 注入 Registry + 构造 Admin

**Files:**
- Modify: `backend/internal/server/server.go:154-168`(proxy.Config 加 Inflight)
- Modify: `backend/internal/server/server.go`(Server struct 加 inflight 字段;New 时 `inflight.NewRegistry()`)
- Modify: `backend/internal/server/server.go:807-842`(`NewAdmin` 调用点注入 inflight snapshot 闭包)

**Interfaces:**
- Consumes: `*inflight.Registry`(Task 1)、`proxy.Config.Inflight`(Task 2)、`handler.NewAdmin` 现有签名
- Produces: `handler.Admin` 新增字段 `InflightSnapshot func() []*inflight.Snapshot`(Task 4 消费)

**说明**:server 是顶层编排者,把 `*inflight.Registry` 同时传给 proxy 和 admin handler(通过闭包)。这样 proxy 负责写、handler 负责读,两者都通过窄接口,不通 inflight 包内部实现。

- [ ] **Step 1: server struct 加字段 + New 里构造**

在 `internal/server/server.go` 的 `Server` struct 加 `inflight *inflight.Registry`;在 `New`/构造 server 的地方(`NewEngine` 之前)加:

```go
	inflightR := inflight.NewRegistry()
```

并加 import `"github.com/wang546673478/native-llm-gateway/internal/inflight"`。

- [ ] **Step 2: proxy.Config 加 Inflight**

在 `server.go:154` 附近的 `proxy.NewEngine(proxy.Config{...})` 里加一行:

```go
		Inflight:      inflightR,
```

(放在 `AccessLog: accessR` 旁边)

- [ ] **Step 3: NewAdmin 注入闭包**

在 `handler.NewAdmin(...)` 调用(server.go 约 807-842)的**参数末尾**(`fingerprintSet` 之后)追加一个新参数。同时需要在 `handler.NewAdmin` 的 signature 加这个参数(见 Task 4 Step 1)——这里先记录调用侧意图,闭包体:

```go
		func() []*inflight.Snapshot { return inflightR.Snapshot() },
```

> 注意:这一步依赖 Task 4 先改 `handler.NewAdmin` 签名,否则编译不过。**Task 3 与 Task 4 拆成两步只是为了结构清晰,实际执行时务必先做 Task 4 的签名改动,再回来补这个调用参数,最后统一编译。** 本计划将两者在两个 Task 里,执行时注意顺序。

- [ ] **Step 4: 编译**

Run: `cd /home/hhhh/llm-gateway/backend && make build`
Expected: PASS(与 Task 4 一起完成编译,见上注)。

- [ ] **Step 5: 提交**(与 Task 4 合并提交,见 Task 4 Step 6)

---

### Task 4: handler 端点 —— GET /api/v1/inflight

**Files:**
- Modify: `backend/internal/api/http/handler/admin.go:40-64`(Admin struct 加字段)
- Modify: `backend/internal/api/http/handler/admin.go:77-115`(NewAdmin signature + 构造体加字段)
- Modify: `backend/internal/api/http/handler/admin.go:128-155`(Register 加路由)
- Modify: `backend/internal/api/http/handler/admin.go`(新增 listInflight handler)

**Interfaces:**
- Consumes: `[]*inflight.Snapshot`(Task 1)、`InflightSnapshot func() []*inflight.Snapshot`(由 server Task 3 注入)
- Produces: `GET /api/v1/inflight` 响应体 `{"requests": [...]}`

**说明**:handler 不 import inflight 包(只 import 它的 `Snapshot` 类型做闭包签名是允许的,但更干净的做法是让 `InflightSnapshot` 返回一个 handler 本地定义的返回类型 —— 这里为求简单,允许 `InflightSnapshot func() []*inflight.Snapshot`,handler import inflight 仅用于类型签名,不 import 其内部 `Registry`/方法)。响应里 `elapsed_ms` 在 handler 内现算。字段名用 JSON snake_case,与现有 usage/access-log 风格一致。

- [ ] **Step 1: Admin struct 加字段 + NewAdmin 签名**

在 `Admin` struct(admin.go 约 40-64)加:

```go
	// P-inflight: 活跃请求内存快照的只读查询(闭包注入,handler 不依赖 inflight 包)。
	InflightSnapshot func() []*inflight.Snapshot
```

在 `NewAdmin(...)` 的参数最末尾加 `inflightSnapshot func() []*inflight.Snapshot,` 并把它赋给 `Admin.InflightSnapshot`。加 import `"github.com/wang546673478/native-llm-gateway/internal/inflight"`。

- [ ] **Step 2: Register 加路由**

在 `Register`(admin.go 约 153-154)的 fingerprint 路由之后加:

```go
	// P-inflight: 实时活跃请求列表
	r.GET("/inflight", a.listInflight)
```

- [ ] **Step 3: 写 listInflight handler**

```go
// listInflight GET /api/v1/inflight — 返回当前活跃请求的内存快照列表。
// elapsed_ms 由 now - StartedAt 现算(快照只存 StartedAt,不存耗时)。
func (a *Admin) listInflight(c *gin.Context) {
	snap := []*inflight.Snapshot{}
	if a.InflightSnapshot != nil {
		snap = a.InflightSnapshot()
	}
	type req struct {
		TraceID        string `json:"trace_id"`
		StartedAt      string `json:"started_at"`
		Model          string `json:"model"`
		ProviderName   string `json:"provider_name"`
		GatewayKeyName string `json:"gateway_key_name"`
		IsStream       bool   `json:"is_stream"`
		ElapsedMs      int64  `json:"elapsed_ms"`
	}
	now := time.Now()
	out := make([]req, 0, len(snap))
	for _, s := range snap {
		out = append(out, req{
			TraceID:        s.TraceID,
			StartedAt:      s.StartedAt.UTC().Format(time.RFC3339),
			Model:          s.Model,
			ProviderName:   s.ProviderName,
			GatewayKeyName: s.GatewayKeyName,
			IsStream:       s.IsStream,
			ElapsedMs:      now.Sub(s.StartedAt).Milliseconds(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"requests": out})
}
```

(确认 `time` 已 import;若无则加)

- [ ] **Step 4: 编译 + 全量测试**

Run:
```bash
cd /home/hhhh/llm-gateway/backend && make build && make vet && make test
```
Expected: PASS(此时 Task 3 的 server 调用点参数也对齐后一起过)

- [ ] **Step 5: 补一个 handler 单元测试(可选但推荐)**

在 `admin_test.go` 现有测试旁,补一个「InflightSnapshot 为 nil 时返回空 requests、非 nil 时返回正确 elapsed/字段」的测试(若 admin_test.go 结构允许,按其中既有 handler 测试模式写;否则可跳过,靠 make test 回归保证)。至少验证 json 结构:

```go
func TestListInflightEmpty(t *testing.T) {
	a := &Admin{} // InflightSnapshot nil
	// ... 用 gin testcontext 触发 listInflight,断言 resp 200 + {"requests":[]}
}
```

- [ ] **Step 6: 提交(Task 3 + Task 4 合并)**

```bash
cd /home/hhhh/llm-gateway
git add backend/internal/server/server.go backend/internal/api/http/handler/admin.go
git commit -m "feat(inflight): server 装配 + GET /api/v1/inflight 端点

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: 前端 —— /inflight 页面 + 路由 + 菜单

**Files:**
- Modify: `frontend/src/api/client.ts`(加 in-flight 类型 + api)
- Create: `frontend/src/views/Inflight.vue`
- Modify: `frontend/src/router/index.ts`(加路由)
- Modify: `frontend/src/App.vue`(加菜单项 + 标题映射)

**Interfaces:**
- Consumes: `GET /api/v1/inflight` 响应结构(Task 4)
- Produces: 前端 `api.inflight()` 便捷方法;`/inflight` 页面

**说明**:前端唯一数据源 `client.ts` 加类型 + 方法;`Inflight.vue` 用 `setInterval` 1s 轮询,`onUnmounted` 清理;空列表显示「当前无活跃请求」,不缓存历史、不灰显(与 spec 语义一致)。

- [ ] **Step 1: client.ts 加类型 + 方法**

在 `client.ts` 里(靠近 `AccessLog` 类型定义处)加:

```ts
// P-inflight: 一条活跃请求的内存快照(与后端 inflight.Snapshot + listInflight 对齐)
export interface InflightRequest {
  trace_id: string
  started_at: string
  model: string
  provider_name: string
  gateway_key_name: string
  is_stream: boolean
  elapsed_ms: number
}

export interface InflightResp {
  requests: InflightRequest[]
}
```

在 `api` 对象里(靠近 `fingerprint` 处)加:

```ts
  // P-inflight: 实时活跃请求列表
  inflight: () => client.get<InflightResp>('/inflight').then(r => r.data),
```

- [ ] **Step 2: 新建 Inflight.vue**

```vue
<template>
  <n-card title="活跃请求(实时)">
    <template #header-extra>
      <n-tag :type="rows.length > 0 ? 'success' : 'default'" size="small">
        {{ rows.length }} 条
      </n-tag>
    </template>
    <n-data-table :columns="columns" :data="rows" :bordered="false" :pagination="false" />
    <n-empty v-if="rows.length === 0" description="当前无活跃请求" style="margin-top: 24px" />
  </n-card>
</template>

<script setup lang="ts">
import { h, onMounted, onUnmounted, ref } from 'vue'
import { NCard, NDataTable, NEmpty, NTag } from 'naive-ui'
import { api, type InflightRequest } from '../api/client'

const rows = ref<InflightRequest[]>([])
let timer: ReturnType<typeof setInterval> | undefined

function fmtElapsed(ms: number): string {
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return `${m}m${s % 60}s`
}

async function load() {
  try {
    const r = await api.inflight()
    rows.value = r.requests
  } catch (e) {
    console.error('inflight load failed', e)
  }
}

const columns = [
  { title: 'Trace', key: 'trace_id', ellipsis: true },
  { title: 'Model', key: 'model' },
  { title: 'Provider', key: 'provider_name', render: (r: InflightRequest) => r.provider_name || '路由中…' },
  { title: 'Gateway Key', key: 'gateway_key_name', render: (r: InflightRequest) => r.gateway_key_name || '—' },
  {
    title: '流式',
    key: 'is_stream',
    render: (r: InflightRequest) => (r.is_stream ? '是' : '否'),
  },
  {
    title: '已耗时',
    key: 'elapsed_ms',
    render: (r: InflightRequest) => fmtElapsed(r.elapsed_ms),
  },
]

onMounted(() => {
  load()
  timer = setInterval(load, 1000) // 1s 轮询
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>
```

- [ ] **Step 3: 加路由**

在 `frontend/src/router/index.ts` 的 routes 里加:

```ts
    { path: '/inflight', name: 'inflight', component: () => import('../views/Inflight.vue') },
```

- [ ] **Step 4: App.vue 加菜单项 + 标题**

在 `App.vue` 的 `currentTitle` map 里加 `'/inflight': '活跃请求',`;在 `menuOptions` 里(access-logs 之后)加:

```ts
  { key: '/inflight', label: renderMenuLabel('/inflight', '⚡ 活跃请求') },
```

- [ ] **Step 5: 类型检查**

Run: `cd /home/hhhh/llm-gateway/frontend && npx vue-tsc --noEmit`
Expected: 无错误(vue-tsc 会检查 .vue + .ts;若 `timer` 类型告警,改为 `ReturnType<typeof setInterval>`——已在上方写好)。

- [ ] **Step 6: 提交**

```bash
cd /home/hhhh/llm-gateway
git add frontend/src/api/client.ts frontend/src/views/Inflight.vue frontend/src/router/index.ts frontend/src/App.vue
git commit -m "feat(inflight): 前端活跃请求页(/inflight)+ 菜单 + 1s 轮询

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## 部署与验收(全部 Task 完成后)

1. **后端**:`cd backend && make build && echo '123321' | sudo -S systemctl restart llm-gateway`
2. **前端**:`cd frontend && npm run build`(8080 托管的是 dist,必须重建,见 memory feedback-rebuild-restart-before-visible)
3. **验收**:
   - `curl http://localhost:8080/api/v1/inflight` → `{"requests":[]}`(无活跃时)
   - 触发一个流式请求(如用 Claude Code 或 curl 打一个长生成),期间立刻 `curl /api/v1/inflight` → 该请求出现在列表,`provider_name` 已填、`elapsed_ms` 递增
   - 请求结束后再 curl → 列表恢复空(结束即消)
   - 强刷 8080 的 `/inflight` 页(Ctrl+Shift+R),确认表格正常渲染、空态正常
