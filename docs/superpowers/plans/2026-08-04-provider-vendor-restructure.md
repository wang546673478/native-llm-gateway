# P-provider-vendor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provider 按厂商建模 — 目录按厂商合并(定制包)、UI 按厂商显示、key 厂商级一份(DB protocols 列 + 迁移 + 共享 pool + 协议过滤)、删除 glm/kimi、落地文档调研发现的两个真实缺口(OpenAI 标准 cached_tokens 解析、MiniMax 缓存价)。

**Architecture:** 注册名(`deepseek` / `deepseek-anthropic` / `minimax` / `minimax-openai`)与 config.yaml 结构不变,只在:① registry 加 vendor 元数据(UI 聚合用)② DB `provider_api_keys` 加 protocols 列并把协议变体行并入厂商名 ③ 同厂商两个注册名共享同一 `*keypool.Pool`,取 key 时按请求协议过滤 ④ quotacheck 按唯一 pool 轮询 ⑤ 目录:每家一个包,协议是包内文件 ⑥ 前端两页按 vendor 消费。

**Tech Stack:** Go 1.22+ / GORM(SQLite via glebarez) / Vue3 + naive-ui / gin

## Global Constraints

- 注册名不变:`"deepseek"` / `"deepseek-anthropic"` / `"minimax"` / `"minimax-openai"`(spec §0.2-2)
- config.yaml provider 条目结构不变;唯一改动是删 glm/glm-anthropic/kimi/kimi-anthropic 四个条目 + minimax 模型补缓存价(spec §4.8, §4.7-B2)
- 协议字符串值:`"openai"` / `"anthropic"` / `"google"`(provider.go:17-19)
- `protocols` 列语义:逗号分隔,空 = 全部协议;非空 = 仅列出的协议(限制性)
- DB 迁移幂等:重复执行无副作用;只迁移变体注册名(deepseek-anthropic / minimax-openai),主条目不标协议(空 = 全部 — 用户 2026-08-04 裁决,spec §4.2)
- vendor 默认值 = 注册名本身(未声明 vendor 的注册)
- 路由对外行为不变;`Poll.Acquire()` 语义不变(无协议参数 = 不过滤)
- 命令统一在 `backend/` 目录执行:`go build ./...`、`go test ./...`
- 工作树在 main 分支,直接 commit 到 main

---

### Task 1: Registry vendor 元数据

**Files:**
- Modify: `backend/internal/provider/registry.go`
- Test: Create `backend/internal/provider/registry_test.go`

**Interfaces:**
- Consumes: 无(独立任务)
- Produces(后续任务依赖):
  - `provider.RegisterGlobalWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string)`
  - `(*Registry).RegisterWithProtocolVendor(name, factory, proto, vendor)` — vendor 为空时默认 = name
  - `(*Registry).VendorFor(name string) string` — 未注册/未声明时返回 name 本身
  - `(*Registry).ListRegisteredInfo() map[string]RegisteredInfo` — `RegisteredInfo{Protocol Protocol; Vendor string}`
  - `RegisterGlobalWithProtocol`(旧签名)行为不变,内部委托新方法

- [ ] **Step 1: 写失败测试**

创建 `backend/internal/provider/registry_test.go`:

```go
package provider

import "testing"

func TestRegisterWithProtocolVendor(t *testing.T) {
	r := NewRegistry()
	r.RegisterWithProtocolVendor("deepseek", func(ProviderConfig) (Provider, error) { return nil, nil }, ProtocolOpenAI, "deepseek")
	r.RegisterWithProtocolVendor("deepseek-anthropic", func(ProviderConfig) (Provider, error) { return nil, nil }, ProtocolAnthropic, "deepseek")

	infos := r.ListRegisteredInfo()
	if infos["deepseek"].Vendor != "deepseek" {
		t.Fatalf("deepseek vendor = %q, want deepseek", infos["deepseek"].Vendor)
	}
	if infos["deepseek-anthropic"].Vendor != "deepseek" {
		t.Fatalf("deepseek-anthropic vendor = %q, want deepseek", infos["deepseek-anthropic"].Vendor)
	}
	if infos["deepseek"].Protocol != ProtocolOpenAI {
		t.Fatalf("deepseek protocol = %q, want openai", infos["deepseek"].Protocol)
	}
	if got := r.VendorFor("deepseek-anthropic"); got != "deepseek" {
		t.Fatalf("VendorFor = %q, want deepseek", got)
	}
}

func TestVendorForDefault(t *testing.T) {
	r := NewRegistry()
	r.Register("solo", func(ProviderConfig) (Provider, error) { return nil, nil })
	if got := r.VendorFor("solo"); got != "solo" {
		t.Fatalf("VendorFor default = %q, want solo", got)
	}
	if got := r.VendorFor("unknown"); got != "unknown" {
		t.Fatalf("VendorFor unknown = %q, want unknown", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider/ -run TestRegisterWithProtocolVendor -v`
Expected: FAIL — `undefined: RegisterWithProtocolVendor`

- [ ] **Step 3: 实现**

`backend/internal/provider/registry.go` 修改:

```go
// RegisteredInfo 单个注册名的注册元数据(vendor 用于前端按厂商聚合)
type RegisteredInfo struct {
	Protocol Protocol
	Vendor   string
}
```

`Registry` struct 加字段(第 36 行 `protocols` 旁边):

```go
	protocols map[string]Protocol // 用于前端显示绑定选项,即使 provider 未启用
	vendors   map[string]string   // P-provider-vendor: name → vendor(默认 = name)
```

`NewRegistry`(第 40 行)初始化:

```go
	return &Registry{
		factories: make(map[string]Factory),
		protocols: make(map[string]Protocol),
		vendors:   make(map[string]string),
	}
```

新增注册函数(放在 `RegisterGlobalWithProtocol` 旁边):

```go
// RegisterGlobalWithProtocolVendor 注册时同时记录 protocol 和 vendor 元数据
func RegisterGlobalWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string) {
	defaultRegistry.RegisterWithProtocolVendor(name, factory, proto, vendor)
}
```

`RegisterWithProtocol` 改为委托(第 78 行):

```go
func (r *Registry) RegisterWithProtocol(name string, factory Factory, proto Protocol) {
	r.RegisterWithProtocolVendor(name, factory, proto, name)
}

// RegisterWithProtocolVendor 同 RegisterWithProtocol,但额外记录 vendor 元数据
// vendor 为空时默认 = name(单协议厂商)
func (r *Registry) RegisterWithProtocolVendor(name string, factory Factory, proto Protocol, vendor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("provider factory %q already registered", name))
	}
	r.factories[name] = factory
	r.protocols[name] = proto
	if vendor == "" {
		vendor = name
	}
	r.vendors[name] = vendor
}

// VendorFor 查询注册名的 vendor;未注册或未声明时返回 name 本身
func (r *Registry) VendorFor(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if v, ok := r.vendors[name]; ok {
		return v
	}
	return name
}

// ListRegisteredInfo 返回所有已注册 name 的注册元数据
func (r *Registry) ListRegisteredInfo() map[string]RegisteredInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]RegisteredInfo, len(r.factories))
	for n := range r.factories {
		out[n] = RegisteredInfo{
			Protocol: r.protocols[n],
			Vendor:   r.vendors[n],
		}
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/ -run 'TestRegisterWithProtocolVendor|TestVendorForDefault' -v`
Expected: PASS(两个测试)

- [ ] **Step 5: 全量回归 + 提交**

Run: `go build ./... && go test ./...`
Expected: 通过(既有测试不受影响;`TestAuthenticator_InvalidFormat` 为已知既有失败,与本任务无关)

```bash
git add internal/provider/registry.go internal/provider/registry_test.go
git commit -m "feat(provider): registry vendor 元数据 — RegisterWithProtocolVendor + VendorFor + ListRegisteredInfo"
```

---

### Task 2: DB protocols 列 + 启动迁移 + Key.Protocols

**Files:**
- Modify: `backend/internal/database/models.go`(ProviderAPIKey 加 Protocols)
- Modify: `backend/internal/database/database.go`(Migrate 里调迁移函数)
- Modify: `backend/internal/keypool/key.go`(Key 加 Protocols)
- Test: Create `backend/internal/database/database_test.go`

**Interfaces:**
- Consumes: 无
- Produces(后续任务依赖):
  - `dbpkg.ProviderAPIKey.Protocols string` — column `protocols`,默认空 = 全部协议
  - `keypool.Key.Protocols string` — 运行时字段(从 DB 行读入)
  - `database.Migrate` 内部自动执行 `migrateProviderVendorKeys(db)`(幂等)

- [ ] **Step 1: 写失败测试**

创建 `backend/internal/database/database_test.go`:

```go
package database

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMigrateProviderVendorKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ProviderAPIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	// 迁移前状态:四个注册名各一行(模拟现状)
	rows := []ProviderAPIKey{
		{ProviderName: "deepseek", Name: "a", KeyHash: "k1", BillingSource: "api"},
		{ProviderName: "deepseek-anthropic", Name: "b", KeyHash: "k2", BillingSource: "api"},
		{ProviderName: "minimax", Name: "c", KeyHash: "k3", BillingSource: "token_plan"},
		{ProviderName: "minimax-openai", Name: "d", KeyHash: "k4", BillingSource: "token_plan"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrateProviderVendorKeys(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 第一次迁移后
	var all []ProviderAPIKey
	if err := db.Order("id").Find(&all).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	want := []struct{ provider, protocols string }{
		{"deepseek", ""}, // 主条目不标 = 全部协议(用户 2026-08-04 裁决)
		{"deepseek", "anthropic"},
		{"minimax", ""},
		{"minimax", "openai"},
	}
	if len(all) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(all), len(want), all)
	}
	for i, w := range want {
		if all[i].ProviderName != w.provider || all[i].Protocols != w.protocols {
			t.Errorf("row %d = (%s, %q), want (%s, %q)",
				i, all[i].ProviderName, all[i].Protocols, w.provider, w.protocols)
		}
	}

	// 幂等:再跑一遍,结果不变
	if err := migrateProviderVendorKeys(db); err != nil {
		t.Fatalf("migrate second run: %v", err)
	}
	var all2 []ProviderAPIKey
	if err := db.Order("id").Find(&all2).Error; err != nil {
		t.Fatalf("query2: %v", err)
	}
	for i, w := range want {
		if all2[i].ProviderName != w.provider || all2[i].Protocols != w.protocols {
			t.Errorf("idempotent row %d = (%s, %q), want (%s, %q)",
				i, all2[i].ProviderName, all2[i].Protocols, w.provider, w.protocols)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/database/ -run TestMigrateProviderVendorKeys -v`
Expected: FAIL — `undefined: migrateProviderVendorKeys`

- [ ] **Step 3: 实现**

`backend/internal/database/models.go` — ProviderAPIKey struct 加字段(第 71 行 `BillingSource` 之后):

```go
	// P-provider-vendor: key 可用的协议列表,逗号分隔("openai,anthropic");空 = 全部协议
	// 同一把 key 物理上两个协议端点都能用,protocols 只是用户限制语义
	Protocols string `gorm:"column:protocols;default:''" json:"protocols"`
```

`backend/internal/database/database.go` — `Migrate` 函数里 `migrateProviderToProviders` 调用之后加:

```go
	// P-provider-vendor: 协议变体并入厂商名,并标协议
	if err := migrateProviderVendorKeys(db); err != nil {
		return fmt.Errorf("vendor key migrate: %w", err)
	}
```

`backend/internal/database/database.go` 文件末尾加迁移函数(放在 `migrateProviderToProviders` 之后):

```go
// migrateProviderVendorKeys P-provider-vendor: 把协议变体注册名下的 key 并入厂商名,并标协议
//   - deepseek-anthropic 行 → provider_name='deepseek', protocols='anthropic'
//   - minimax-openai 行     → provider_name='minimax',  protocols='openai'
// 主条目(deepseek/minimax)的 key 不标协议 — 保持空 = 全部协议(物理上同一 key 两端点通用,
// 且避免每次重启把用户新加的全协议 key 覆盖成单协议 — 用户 2026-08-04 裁决)。
// 幂等:变体注册名迁移后不再存在,重复执行影响 0 行,无副作用。
func migrateProviderVendorKeys(db *gorm.DB) error {
	stmts := []string{
		`UPDATE provider_api_keys SET provider_name='deepseek', protocols='anthropic' WHERE provider_name='deepseek-anthropic'`,
		`UPDATE provider_api_keys SET provider_name='minimax', protocols='openai' WHERE provider_name='minimax-openai'`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return err
		}
	}
	return nil
}
```

`backend/internal/keypool/key.go` — Key struct 加字段(`QuotaKind` 之后):

```go
	// P-provider-vendor: 该 key 可用的协议列表(逗号分隔,空 = 全部);从 DB ProviderAPIKey.Protocols 读入
	Protocols string
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/database/ -run TestMigrateProviderVendorKeys -v`
Expected: PASS

- [ ] **Step 5: 全量回归 + 提交**

Run: `go build ./... && go test ./...`
Expected: 通过(既有测试不受影响)

```bash
git add internal/database/models.go internal/database/database.go internal/database/database_test.go internal/keypool/key.go
git commit -m "feat(db): provider_api_keys.protocols 列 + 启动迁移(协议变体并入厂商名)+ Key.Protocols"
```

---

### Task 3: 目录合并(deepseek/minimax)+ 删除 glm/kimi + config 清理

**Files:**
- Create: `backend/internal/provider/deepseek/anthropic.go`(从 `deepseek_anthropic/` 搬,改名 `NewAnthropic`)
- Create: `backend/internal/provider/minimax/openai.go`(从 `minimax_openai/` 搬,改名 `NewOpenAI`)
- Modify: `backend/internal/provider/deepseek/deepseek.go`(init 注册两个名字 + vendor)
- Modify: `backend/internal/provider/minimax/minimax.go`(init 注册两个名字 + vendor)
- Delete: `backend/internal/provider/deepseek_anthropic/`、`minimax_openai/`、`glm/`、`glm_anthropic/`、`kimi/`、`kimi_anthropic/`
- Modify: `config.yaml`(删 glm / glm-anthropic / kimi / kimi-anthropic 四个条目)

**Interfaces:**
- Consumes: Task 1 的 `RegisterGlobalWithProtocolVendor`
- Produces:
  - `deepseek.New(cfg)`(openai,不变)+ `deepseek.NewAnthropic(cfg)`(anthropic)
  - `minimax.New(cfg)`(anthropic,不变)+ `minimax.NewOpenAI(cfg)`(openai)
  - 删除后不再有 `deepseek_anthropic` / `minimax_openai` / `glm*` / `kimi*` 包

**同包常量冲突处理(必须照做,否则编译失败):**
- `deepseek/anthropic.go`:原 `name` → `anthropicName`;原 `DefaultEndpoint` → `anthropicDefaultEndpoint`;删除原 `DefaultModels`(与 deepseek.go 同名变量冲突,共用 deepseek.DefaultModels)
- `minimax/openai.go`:原 `name` → `openaiName`;原 `DefaultEndpoint` → `openaiDefaultEndpoint`;原 `ChatPath` → `openaiChatPath`;删除原 `DefaultModels`(与 minimax.go 同名冲突,内容相同,共用)
- 两文件删除各自的 `toPool` 函数(同包已有)

- [ ] **Step 1: 创建 `deepseek/anthropic.go`**

从 `backend/internal/provider/deepseek_anthropic/deepseek_anthropic.go` 复制,改为:

```go
// Package deepseek 实现 DeepSeek Provider(OpenAI + Anthropic 两种协议)
//
// P-provider-vendor: 本包同时承载两个注册名:
//   - "deepseek"           → New(OpenAI 协议,本文件下方 deepseek.go)
//   - "deepseek-anthropic" → NewAnthropic(Anthropic 兼容,本文件)
//
// Anthropic 兼容端点(官方文档 https://api-docs.deepseek.com/zh-cn/guides/anthropic_api):
//   - base URL: https://api.deepseek.com/anthropic
//   - 鉴权:     x-api-key: {DEEPSEEK_API_KEY}(anthropic-version / anthropic-beta 头被忽略)
//   - 端点:     /v1/messages(相对 base URL 拼接)
//
// 官方文档特性(2026-08-04 全量调研):
//   - 模型映射:claude-opus* → deepseek-v4-pro;claude-haiku*/claude-sonnet* → deepseek-v4-flash;
//     任何不支持的模型名自动映射到 deepseek-v4-flash(不报错 — 网关需显式校验模型名)
//   - 支持:max_tokens / stop_sequences / stream / system / temperature(0.0~2.0)/ top_p / thinking
//   - 忽略:budget_tokens / container / mcp_servers / service_tier / top_k / cache_control
//   - 消息块:支持 text / thinking / tool_use / tool_result / server_tool_use / web_search_tool_result;
//     不支持 image / document / search_result / redacted_thinking / code_execution_tool_result
//   - 注意:Anthropic 模式的 usage 响应字段官方文档未公开,网关按 Anthropic 标准字段解析(实测确认)
package deepseek

import (
	"context"
	"fmt"
	"github.com/wang546673478/native-llm-gateway/internal/keypool"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
)

const (
	anthropicName             = "deepseek-anthropic"
	anthropicDefaultEndpoint  = "https://api.deepseek.com/anthropic"
)

// Provider Anthropic 兼容 Provider(协议面是 anthropic,账号与 openai 面同一组 key)
type Provider struct {
	base *anthropic_compatible.Base
	cfg  provider.ProviderConfig
}

// NewAnthropic Anthropic 协议工厂函数
func NewAnthropic(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolAnthropic {
		return nil, fmt.Errorf("deepseek-anthropic requires protocol=anthropic, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("deepseek-anthropic endpoint is required")
	}
	return &Provider{
		base: anthropic_compatible.NewBase(anthropic_compatible.Config{
			Name:     anthropicName,
			Endpoint: cfg.Endpoint,
			Timeout:  cfg.Timeout,
			Pool:     toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return anthropicName }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (p *Provider) Models() []string {
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models
	}
	return DefaultModels // 与 openai 面共用(同包变量)
}

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}

func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}

func (p *Provider) HealthCheck(ctx context.Context) error {
	return p.base.HealthCheck(ctx)
}

func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }
```

注意:import 块里的 `"github.com/wang546673478/native-llm-gateway/internal/keypool"` 与 deepseek.go 的 import 分开写在各自文件里(Go 按文件 import,无冲突)。

- [ ] **Step 2: 创建 `minimax/openai.go`**

从 `backend/internal/provider/minimax_openai/minimax_openai.go` 复制,改为:

```go
// Package minimax 实现 MiniMax Provider(Anthropic + OpenAI 两种协议)
//
// P-provider-vendor: 本包同时承载两个注册名:
//   - "minimax"        → New(Anthropic 协议,minimax.go)
//   - "minimax-openai" → NewOpenAI(OpenAI 兼容,本文件)
//
// OpenAI 兼容端点(官方文档 https://platform.minimaxi.com/docs/api-reference/text-openai-api):
//   - base URL: https://api.minimaxi.com/v1(国内站;国际站 api.minimax.io)
//   - 端点:POST /chat/completions(OpenAI 标准)
//   - 鉴权:Authorization: Bearer <API_KEY>
//
// 官方文档特性(2026-08-04 全量调研,与 Anthropic 面不同处):
//   - thinking:默认开启 adaptive(M3);省略 = adaptive;M2.x 无法关闭
//   - reasoning_split(extra_body):true → thinking 拆到 message.reasoning_content +
//     reasoning_details[];false/缺省 → thinking 以 <think>...</think> 标签内嵌在 content 里,
//     多轮必须原样回传完整 content,否则思维链断裂
//   - service_tier: "standard"(默认) | "priority"(1.5x 价);OpenAI SDK 需 extra_body
//   - max_tokens 已废弃,用 max_completion_tokens;n 仅支持 1;
//     presence_penalty / frequency_penalty / logit_bias 忽略
//   - usage:prompt_tokens_details.cached_tokens(自动缓存命中,按缓存价计费);
//     流式 usage 默认 null,需 stream_options.include_usage=true(网关已默认注入)
//   - 缓存:自动 Prompt 缓存(≥512 tokens,服务端自动,支持 M3/M2.7/M2.5/M2.1);
//     M3 输入 >512k tokens 时按长上下文价计费(含缓存命中 tokens)
package minimax

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

const (
	openaiName            = "minimax-openai"
	openaiDefaultEndpoint = "https://api.minimaxi.com/v1"
	openaiChatPath        = "/chat/completions" // base 已含 /v1,不要再拼
)

// Provider OpenAI 兼容 Provider
type Provider struct {
	base *openai_compatible.Base
	cfg  provider.ProviderConfig
}

// NewOpenAI OpenAI 协议工厂函数
func NewOpenAI(cfg provider.ProviderConfig) (provider.Provider, error) {
	if cfg.Protocol != provider.ProtocolOpenAI {
		return nil, fmt.Errorf("minimax-openai requires protocol=openai, got %q", cfg.Protocol)
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("minimax-openai endpoint is required")
	}
	return &Provider{
		base: openai_compatible.NewBase(openai_compatible.Config{
			Name:        openaiName,
			Endpoint:    cfg.Endpoint,
			Timeout:     cfg.Timeout,
			ChatPath:    openaiChatPath,
			StreamUsage: true, // MiniMax 支持 stream_options.include_usage
			Pool:        toPool(cfg.Pool),
		}),
		cfg: cfg,
	}, nil
}

func (p *Provider) Name() string                { return openaiName }
func (p *Provider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }
func (p *Provider) Models() []string {
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models
	}
	return DefaultModels // 与 anthropic 面共用(同包变量,内容相同)
}

func (p *Provider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return p.base.SendRequest(ctx, req)
}
func (p *Provider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return p.base.SendStreamRequest(ctx, req)
}
func (p *Provider) HealthCheck(ctx context.Context) error { return p.base.HealthCheck(ctx) }

func (p *Provider) SetPool(pool *keypool.Pool) {
	p.base.SetPool(pool)
}

func (p *Provider) Close() error { return p.base.Close() }
```

- [ ] **Step 3: 改 `deepseek.go` 的 init(注册两个名字 + vendor)**

`backend/internal/provider/deepseek/deepseek.go` 末尾的 init 改为:

```go
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolOpenAI, name)
	provider.RegisterGlobalWithProtocolVendor(anthropicName, NewAnthropic, provider.ProtocolAnthropic, name)
}
```

同文件包注释第一行改为:

```go
// Package deepseek 实现 DeepSeek Provider(OpenAI + Anthropic 两种协议)
```

并把原注释里"实现策略:继承 openai_compatible.Base…"下方追加一行:

```go
// P-provider-vendor: anthropic 协议实现在 anthropic.go(注册名 "deepseek-anthropic")。
// 官方文档特性(2026-08-04 全量调研,OpenAI 面):
//   - thinking:默认 enabled;reasoning_effort: low|high|max(medium/xhigh 映射 high)
//   - 响应 choices[].message.reasoning_content;流式在 delta.reasoning_content;
//     usage.completion_tokens_details.reasoning_tokens 计思维链 token
//   - 思考模式下 temperature/top_p 不生效;带 tools 时必须逐轮回传 reasoning_content,否则 400
//   - KV cache:自动开启无参数;usage.prompt_cache_hit_tokens / prompt_cache_miss_tokens
//     (prompt_tokens = hit + miss;缓存价仅为未命中价 2%~0.8%)
//   - JSON output:response_format={"type":"json_object"};prompt 须含 "json" 字样
//   - FIM / Chat Prefix / Responses API:需要 /beta base_url 或独立端点,本包未实现(YAGNI)
//   - 峰谷定价预告:高峰(北京 9-12 / 14-18 点)2 倍价,生效后需调整定价配置
```

- [ ] **Step 4: 改 `minimax.go` 的 init**

`backend/internal/provider/minimax/minimax.go` 末尾的 init 改为:

```go
func init() {
	provider.RegisterGlobalWithProtocolVendor(name, New, provider.ProtocolAnthropic, name)
	provider.RegisterGlobalWithProtocolVendor(openaiName, NewOpenAI, provider.ProtocolOpenAI, name)
}
```

包注释第一行改为 `// Package minimax 实现 MiniMax(MiniMax 稀宇科技)Provider(Anthropic + OpenAI 两种协议)`,并在文件末尾追加:

```go
// P-provider-vendor: openai 协议实现在 openai.go(注册名 "minimax-openai")。
// 官方文档特性(2026-08-04 全量调研,Anthropic 面):
//   - thinking:仅 disabled / adaptive(MiniMax 扩展);M3 默认关闭(省略 = disabled);
//     M2.x 系列 thinking 恒开不可关
//   - 响应 content 块含 thinking(文本 + signature);多轮必须原样回带 thinking 块(含 signature)
//   - service_tier:standard | priority(1.5x 价,优先准入);standard Anthropic SDK 可能不识别
//   - 缓存:主动缓存(cache_control 断点,5min TTL,最多 4 断点/20 块回溯)支持 M2.x 但不支持 M3;
//     usage.cache_creation_input_tokens / cache_read_input_tokens(message_start 即返回)
//   - tool_choice 仅 auto / none;top_k / stop_sequences / mcp_servers 静默忽略
//   - 无官方余额查询 API — 本包 balancer 用未文档化端点 www.minimaxi.com/v1/token_plan/remains
//   - 错误:HTTP 状态码 + body {type:"error", error:{type,message}};余额不足 1008 / 超套餐 2056 走 base_resp
```

- [ ] **Step 5: 删除 6 个旧包 + 验证构建**

Run:

```bash
git rm -r internal/provider/deepseek_anthropic internal/provider/minimax_openai internal/provider/glm internal/provider/glm_anthropic internal/provider/kimi internal/provider/kimi_anthropic
```

Run: `go build ./...`
Expected: 通过(若报错,检查是否有其他文件 import 了被删的包路径 — 本任务前已确认只有注释引用,无代码引用)

Run: `go test ./...`
Expected: 通过

- [ ] **Step 6: 清理 config.yaml**

`config.yaml` 删除四个条目,每个条目连同它上方的注释块一起删:
- `glm:` 条目(从 `# --- 智谱 GLM (OpenAI 兼容) ---` 注释到 `glm` 的 circuit_breaker 结束)
- `glm-anthropic:` 条目(从 `# 端点:POST https://open.bigmodel.cn/api/anthropic/v1/messages` 注释块到其 circuit_breaker 结束)
- `kimi-anthropic:` 条目(从 `# --- 月之暗面 Kimi (Anthropic 兼容…) ---` 注释到其 circuit_breaker 结束)
- `kimi:` 条目(从 `# --- Kimi (Moonshot,OpenAI 兼容) ---` 注释到其 circuit_breaker 结束)

注意:**qwen、deepseek、minimax、minimax-openai 条目必须保留**。删除后用 `python3 -c "import yaml,sys; yaml.safe_load(open('config.yaml')); print('yaml ok')"` 验证 YAML 语法(或 `ruby -e "require 'yaml'; YAML.load_file('config.yaml'); puts 'ok'"`)。

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "refactor(provider): 目录按厂商合并(deepseek/minimax 双协议同包)+ 删除 glm/kimi + config 清理"
```

---

### Task 4: key pool 共享(厂商级)+ quotacheck 轮询去重

**Files:**
- Modify: `backend/internal/server/server.go`(`buildKeyPools` 按 vendor 去重;`buildOnePool` 读 Protocols)
- Modify: `backend/internal/quotacheck/manager.go`(`pollAllBalancers` 按 pool 指针去重)
- Test: Modify `backend/internal/quotacheck/manager_test.go`(加共享 pool 去重断言)

**Interfaces:**
- Consumes: Task 1 `provider.Default().VendorFor(name)`;Task 2 `Key.Protocols` / `ProviderAPIKey.Protocols`
- Produces:
  - `pools map[string]*keypool.Pool` 中 `deepseek` 与 `deepseek-anthropic` 指向**同一个** pool(pool 名 = vendor)
  - `keypool.Key.Protocols` 从 DB 行读入

- [ ] **Step 1: 写失败测试**

`backend/internal/quotacheck/manager_test.go` 末尾追加(复用该文件现有模式:stubBalancer 自带 calls 计数、newTestPool、balancerRegistry 手动注册 + t.Cleanup 恢复):

```go
func TestPollAllBalancers_DedupSharedPool(t *testing.T) {
	// P-provider-vendor: deepseek / deepseek-anthropic 共享同一 pool →
	// 轮询按 pool 指针去重,balance 查询每轮只发生一次
	pool := newTestPool(t) // 1 个 api tier key
	m := &Manager{
		logger: zap.NewNop(),
		cfg:    DefaultManagerConfig(),
		pools: NewPoolsRef(map[string]*keypool.Pool{
			"deepseek":           pool,
			"deepseek-anthropic": pool,
		}),
		prov: &fakeProviderLookup{endpoints: map[string]string{
			"deepseek":           "http://x",
			"deepseek-anthropic": "http://y",
		}},
		sched: NewScheduler(),
		now:   time.Now,
	}

	b := &stubBalancer{balance: Balance{HasQuota: true, Raw: 50, Kind: "currency"}}
	originalReg := balancerRegistry["deepseek"]
	RegisterBalancer("deepseek", b)
	RegisterBalancer("deepseek-anthropic", b)
	t.Cleanup(func() {
		if originalReg != nil {
			balancerRegistry["deepseek"] = originalReg
		} else {
			delete(balancerRegistry, "deepseek")
		}
		delete(balancerRegistry, "deepseek-anthropic")
	})

	m.pollAllBalancers(context.Background())

	if got := atomic.LoadInt32(&b.calls); got != 1 {
		t.Fatalf("balancer calls = %d, want 1 (shared pool dedup)", got)
	}
}
```

(该测试在 Step 5 实现前会失败:calls = 2, want 1。用 `TestPollAllBalancers_TierBlocked`(第 323 行)的注册/清理模式照抄。) 

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/quotacheck/ -run TestPollAllBalancers_DedupSharedPool -v`
Expected: FAIL — calls = 2, want 1

- [ ] **Step 3: 实现 server.go 的 pool 共享**

`backend/internal/server/server.go` — `buildKeyPools`(第 215 行)改为:

```go
func buildKeyPools(cfg *config.Config, db *gorm.DB, logger *zap.Logger) map[string]*keypool.Pool {
	out := make(map[string]*keypool.Pool)
	sched := keypool.NewScheduler(cfg.KeyPool.KeyRotation)
	poolCfg := keypool.Config{
		CoolingDuration: cfg.KeyPool.CoolingDuration,
		MaxCoolingCount: cfg.KeyPool.MaxCoolingCount,
	}
	store := auth.NewProviderKeyStore(db)
	// P-provider-vendor: 同一 vendor 的多个注册名(如 deepseek / deepseek-anthropic)
	// 共享同一个 pool — key 厂商级一份,协议由 key 的 Protocols 标记过滤
	vendorPools := make(map[string]*keypool.Pool)
	for name, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		vendor := provider.Default().VendorFor(name)
		pool, ok := vendorPools[vendor]
		if !ok {
			pool = buildOnePool(context.Background(), vendor, sched, poolCfg, store, logger)
			vendorPools[vendor] = pool
		}
		out[name] = pool
	}
	return out
}
```

(把原 `buildKeyPools` 循环里的 `buildOnePool(context.Background(), name, ...)` 换成按 vendor 复用;`provider` 包已在 server.go 的 import 里,无需新增。)

- [ ] **Step 4: buildOnePool 读 Protocols**

`backend/internal/server/server.go` — `buildOnePool` 的 Key 构造(第 246 行附近)加字段:

```go
		keys = append(keys, &keypool.Key{
			ID:            fmt.Sprintf("%d", row.ID),
			ProviderName:  name,
			Name:          row.Name,
			Key:           row.KeyHash,
			Status:        keypool.KeyStatusActive,
			BillingSource: bs, // P48: 单 key 计费 tier,Pool.Acquire 按此排序
			// P-provider-vendor: key 可用协议列表(空 = 全部),取 key 时按请求协议过滤
			Protocols:     row.Protocols,
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		})
```

- [ ] **Step 5: pollAllBalancers 去重**

`backend/internal/quotacheck/manager.go` — `pollAllBalancers` 的 for 循环前加:

```go
	// P-provider-vendor: 同一 vendor 共享 pool 后,两个注册名指向同一 *Pool,
	// 按 pool 指针去重,避免每轮重复 poll 同一批 key 的余额
	seen := make(map[*keypool.Pool]bool)
	for providerName, pool := range m.pools.Get() {
		if seen[pool] {
			continue
		}
		seen[pool] = true
		balancer := LookupBalancer(providerName)
```

(其余循环体不变。)

- [ ] **Step 6: 跑测试**

Run: `go test ./internal/quotacheck/ -run TestPollAllBalancers -v`
Expected: 新测试 PASS,既有 poll 测试不受影响(不同 pool 仍各 poll 一次)

Run: `go build ./... && go test ./...`
Expected: 通过

- [ ] **Step 7: 提交**

```bash
git add internal/server/server.go internal/quotacheck/manager.go internal/quotacheck/manager_test.go
git commit -m "feat(keypool): 同厂商共享 pool(按 vendor 建池)+ pollAllBalancers 按 pool 去重"
```

---

### Task 5: 取 key 协议过滤

**Files:**
- Modify: `backend/internal/keypool/pool.go`(`AcquireForProtocol` 新方法;`AcquireFromTier` 加 proto 参数)
- Modify: `backend/internal/router/router.go`(`Next()` 两处传协议)
- Modify: `backend/internal/provider/openai_compatible/openai_compatible.go`(3 处 `Acquire()` → `AcquireForProtocol("openai")`)
- Modify: `backend/internal/provider/anthropic_compatible/anthropic_compatible.go`(3 处 → `AcquireForProtocol("anthropic")`)
- Modify: `backend/internal/provider/google/google.go`(3 处 → `AcquireForProtocol("google")`)
- Test: Modify `backend/internal/keypool/keypool_test.go`(加协议过滤测试)

**Interfaces:**
- Consumes: Task 2 `Key.Protocols`
- Produces:
  - `func (p *Pool) AcquireForProtocol(proto string) (*Key, error)` — proto 空 = 不过滤
  - `func (p *Pool) AcquireFromTier(tier string, allowedIDSet map[uint]struct{}, proto string) (*Key, error)`
  - 过滤规则:`k.Protocols == ""` 匹配任何协议;非空时用逗号分割,包含 `proto` 才匹配

- [ ] **Step 1: 写失败测试**

`backend/internal/keypool/keypool_test.go` 末尾追加(先读该文件了解现有 Pool 构造方式,复用):

```go
func TestAcquireForProtocol(t *testing.T) {
	now := time.Now()
	keys := []*Key{
		{ID: "1", Name: "openai-only", Key: "k1", Status: KeyStatusActive, BillingSource: "api", Protocols: "openai", CreatedAt: now, UpdatedAt: now},
		{ID: "2", Name: "all", Key: "k2", Status: KeyStatusActive, BillingSource: "api", CreatedAt: now, UpdatedAt: now},
		{ID: "3", Name: "anthropic-only", Key: "k3", Status: KeyStatusActive, BillingSource: "api", Protocols: "anthropic", CreatedAt: now, UpdatedAt: now},
	}
	pool := NewPool("p", keys, nil, Config{})

	// anthropic 请求:只能拿到 "all"(Protocols="" 匹配任何)
	k, err := pool.AcquireForProtocol("anthropic")
	if err != nil {
		t.Fatalf("acquire anthropic: %v", err)
	}
	if k.Name != "all" {
		t.Fatalf("anthropic request got key %q, want all", k.Name)
	}

	// openai 请求:可从 openai-only 或 all 中取
	k2, err := pool.AcquireForProtocol("openai")
	if err != nil {
		t.Fatalf("acquire openai: %v", err)
	}
	if k2.Name != "openai-only" && k2.Name != "all" {
		t.Fatalf("openai request got key %q, want openai-only or all", k2.Name)
	}

	// 空 proto = 不过滤,三个都能取
	k3, err := pool.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_ = k3
}
```

(注意:RoundRobin scheduler 会轮转;断言"能取到且名字在合法集合内"即可,不要断言具体名字。)

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/keypool/ -run TestAcquireForProtocol -v`
Expected: FAIL — `undefined: AcquireForProtocol`

- [ ] **Step 3: 实现 pool.go**

`backend/internal/keypool/pool.go`:

```go
// Acquire P64: 这里保留 tier 降级是作为"无 tier 信息的旧 caller"的兼容入口
// 新调用方应明确用 AcquireFromTier
func (p *Pool) Acquire() (*Key, error) {
	return p.AcquireForProtocol("")
}

// AcquireForProtocol P-provider-vendor: 按请求协议取 key(带 tier 降级)
// proto 为空 = 不过滤;非空时只取 Protocols 为空或包含该协议的 key
func (p *Pool) AcquireForProtocol(proto string) (*Key, error) {
	for _, tier := range []string{"token_plan", "api", "free"} {
		k, err := p.AcquireFromTier(tier, nil, proto)
		if err == nil {
			return k, nil
		}
	}
	return nil, ErrNoAvailableKey
}
```

`AcquireFromIDs` 内部(第 91 行)的 `p.Acquire()` 调用改为 `p.AcquireForProtocol("")`,其 tier 循环改为:

```go
	for _, tier := range []string{"token_plan", "api", "free"} {
		k, err := p.AcquireFromTier(tier, set, "")
		if err == nil {
			return k, nil
		}
	}
```

`AcquireFromTier` 签名(第 113 行)改为:

```go
func (p *Pool) AcquireFromTier(tier string, allowedIDSet map[uint]struct{}, proto string) (*Key, error) {
```

在 `usable` 收集循环之后(第 150 行 `if len(usable) == 0` 之前)插入协议过滤:

```go
	// P-provider-vendor: 协议过滤 — Key.Protocols 为空 = 所有协议可用;非空 = 仅列出的协议
	if proto != "" {
		filtered := usable[:0]
		for _, k := range usable {
			if k.Protocols == "" || containsProtocol(k.Protocols, proto) {
				filtered = append(filtered, k)
			}
		}
		usable = filtered
	}
	if len(usable) == 0 {
		return nil, ErrNoAvailableKey
	}
```

文件末尾(或 parseKeyIDUint 附近)加 helper:

```go
// containsProtocol 判断逗号分隔的协议列表是否包含指定协议
func containsProtocol(list, proto string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == proto {
			return true
		}
	}
	return false
}
```

(pool.go 需要 import `strings`,检查现有 import,缺则加。)

- [ ] **Step 4: 改 router.Next()**

`backend/internal/router/router.go` — `Next()` 里两处 `AcquireFromTier` 调用(第 287、293 行)改为:

```go
			if len(it.providerKeyIDs) > 0 {
				// P34 + P64: 限定 ProviderKey ID 子集,同时指定 tier
				idSet := make(map[uint]struct{}, len(it.providerKeyIDs))
				for _, id := range it.providerKeyIDs {
					idSet[id] = struct{}{}
				}
				// P-provider-vendor: 按请求协议过滤 key(Protocols 为空 = 不过滤)
				k, err = pool.AcquireFromTier(c.Tier, idSet, string(pv.Protocol()))
			} else {
				k, err = pool.AcquireFromTier(c.Tier, nil, string(pv.Protocol()))
			}
```

- [ ] **Step 5: 改 3 个 base 包的取 key 调用**

`openai_compatible/openai_compatible.go` 三处(第 81、183、327 行):

```go
	key, err := b.cfg.Pool.AcquireForProtocol("openai") // P-provider-vendor: 按本包协议过滤
```

`anthropic_compatible/anthropic_compatible.go` 三处(第 62、138、356 行):

```go
	key, err := b.cfg.Pool.AcquireForProtocol("anthropic")
```

`google/google.go` 三处(第 60、124、218 行):

```go
	key, err := b.cfg.Pool.AcquireForProtocol("google")
```

(每处调用上下文不同,有的用 `if k, err := ...` 短变量形式 — 保持原有变量名与错误处理不变,只换方法名和加参数。)

- [ ] **Step 6: 更新既有测试的签名**

`keypool_test.go` / `router_test.go` 里直接调用 `AcquireFromTier` 的地方,补第三个参数 `""`。用 `grep -rn "AcquireFromTier" internal/` 找出全部调用点逐一更新。

Run: `go build ./... && go test ./...`
Expected: 通过(既有 router 测试的 key 无 Protocols 字段 = 空,不过滤,行为不变)

- [ ] **Step 7: 提交**

```bash
git add internal/keypool/pool.go internal/keypool/keypool_test.go internal/router/router.go internal/provider/openai_compatible/openai_compatible.go internal/provider/anthropic_compatible/anthropic_compatible.go internal/provider/google/google.go
git commit -m "feat(keypool): 取 key 按协议过滤 — AcquireForProtocol + AcquireFromTier(proto)"
```

---

### Task 6: admin API 按 vendor 聚合 + handler Protocols 透传

**Files:**
- Modify: `backend/internal/api/http/handler/admin.go`(`listProviders` 重写 — 注意是 GET /api/v1/providers,不是 registered)
- Modify: `backend/internal/auth/provider_keys_handler.go`(create 接受 protocols;View 加字段)
- Test: Modify `backend/internal/auth/provider_keys_handler_test.go`

**Interfaces:**
- Consumes: Task 1 `VendorFor`;Task 2 `ProviderAPIKey.Protocols` / `Key.Protocols`
- Produces:
  - `GET /api/v1/providers` 返回 `{"vendors":[{"vendor","names":[{"name","protocol"}],"models","key_pool","circuit_breaker"}]}`(v 从 `a.Registry.VendorFor(name)` 聚合;`listRegisteredProviders` 保持原样,AccessLogs.vue 还在用)
  - `ProviderKeyView.Protocols string \`json:"protocols"\``
  - create 请求体接受 `protocols` 字段

- [ ] **Step 1: 重写 listProviders**

`backend/internal/api/http/handler/admin.go` — `listProviders`(第 137-164 行)整体替换为:

```go
// listProviders GET /api/v1/providers
// 列出所有 Provider + 状态(KeyPool + Circuit Breaker)
// P-provider-vendor: 按 vendor 聚合输出 — 同一厂商的多个注册名(deepseek / deepseek-anthropic)
// 归到同一 vendor 条目下,前端按厂商展示。key_pool / circuit_breaker 取该 vendor 第一个注册名的
// (共享 pool 时状态相同)。
func (a *Admin) listProviders(c *gin.Context) {
	type vendorEntry struct {
		Vendor         string
		Names          []gin.H
		Models         []string
		KeyPool        *keypool.PoolStatus
		CircuitBreaker gin.H
	}
	byVendor := make(map[string]*vendorEntry)
	order := make([]string, 0)
	for name, p := range a.Manager.GetAll() {
		v := a.Registry.VendorFor(name)
		entry, ok := byVendor[v]
		if !ok {
			entry = &vendorEntry{Vendor: v}
			byVendor[v] = entry
			order = append(order, v)
		}
		entry.Names = append(entry.Names, gin.H{"name": name, "protocol": string(p.Protocol())})
		entry.Models = append(entry.Models, p.Models()...)
		if pool, ok := a.Pools[name]; ok && entry.KeyPool == nil {
			st := pool.Status()
			entry.KeyPool = &st
		}
		if a.Breakers != nil && entry.CircuitBreaker == nil {
			for _, s := range a.Breakers.AllStats() {
				if s.Name == name {
					entry.CircuitBreaker = s
					break
				}
			}
		}
	}

	out := make([]gin.H, 0, len(order))
	for _, vendor := range order {
		v := byVendor[vendor]
		// models 并集去重
		seen := make(map[string]bool, len(v.Models))
		models := make([]string, 0, len(v.Models))
		for _, m := range v.Models {
			if !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
		entry := gin.H{
			"vendor": v.Vendor,
			"names":  v.Names,
			"models": models,
		}
		if v.KeyPool != nil {
			entry["key_pool"] = v.KeyPool
		}
		if v.CircuitBreaker != nil {
			entry["circuit_breaker"] = v.CircuitBreaker
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"vendors": out, "count": len(out)})
}
```

(检查 admin.go 是否已 import `keypool` 包;没有则补。)

**注意:不要改 `listRegisteredProviders`(GET /api/v1/providers/registered)** — AccessLogs.vue 仍按平铺 `providers` 结构消费。

- [ ] **Step 2: handler 的 Protocols 透传**

`backend/internal/auth/provider_keys_handler.go`:

(a) `ProviderKeyView`(第 84 行附近)加字段:

```go
	Protocols string `json:"protocols"` // P-provider-vendor: key 可用协议列表(空 = 全部)
```

(b) `createProviderKeyReq`(grep 定位)加字段:

```go
	// P-provider-vendor: 协议限制(逗号分隔,空 = 全部);默认空
	Protocols string `json:"protocols"`
```

(c) `create` 里构造 `k`(第 283 行)加:

```go
	k := &dbpkg.ProviderAPIKey{
		ProviderName:  providerName,
		Name:          name,
		KeyHash:       req.Key,
		Enabled:       enabled,
		BillingSource: billingSource,
		Protocols:     strings.TrimSpace(req.Protocols),
	}
```

(d) `toProviderKeyView`(第 102 行)加:

```go
		Protocols:      k.Protocols,
```

(e) `toProviderKeyViewFromPool`(第 122 行)加:

```go
	if live != nil {
		v.Remaining = live.Remaining
		v.QuotaKind = live.QuotaKind
		v.Protocols = live.Protocols // P-provider-vendor
		// ...(保持原有代码)
	}
```

- [ ] **Step 3: 写 handler 测试**

`backend/internal/auth/provider_keys_handler_test.go` 末尾追加(沿用该文件现有的纯函数断言风格,无 httptest/DB):

```go
func TestProviderKeyView_IncludesProtocols(t *testing.T) {
	past := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	view := ProviderKeyView{
		ID: 1, ProviderName: "deepseek", Name: "k",
		KeyMasked: "sk-te...est", Enabled: true,
		Status: "ACTIVE", BillingSource: "api",
		CreatedAt: past, UpdatedAt: past,
		Remaining: 7.0, LastPolledAt: &past, QuotaKind: "currency",
		Protocols: "openai,anthropic",
	}
	out, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !contains(string(out), `"protocols":"openai,anthropic"`) {
		t.Errorf("missing protocols field in JSON output: %s", string(out))
	}
}

func TestProviderKeyViewFromPool_IncludesProtocols(t *testing.T) {
	now := time.Now()
	live := &keypool.Key{Remaining: 43, QuotaKind: "percent", LastPolledAt: now, Protocols: "anthropic"}
	v := toProviderKeyViewFromPool(dbpkg.ProviderAPIKey{
		ProviderName: "deepseek", Name: "k", KeyHash: "sk-1234567890", Enabled: true,
		BillingSource: "api", Protocols: "openai,anthropic", CreatedAt: now, UpdatedAt: now,
	}, "ACTIVE", live)

	if v.Protocols != "anthropic" {
		t.Errorf("Protocols = %q, want anthropic (live key pass-through)", v.Protocols)
	}
}
```

(requires: `ProviderKeyView` 加 `Protocols` 字段、`toProviderKeyViewFromPool` 透传 — Step 2 实现。现有 `contains` helper 可复用。)

- [ ] **Step 4: 跑测试**

Run: `go build ./... && go test ./internal/auth/ ./internal/api/...`
Expected: 通过

- [ ] **Step 5: 提交**

```bash
git add internal/api/http/handler/admin.go internal/auth/provider_keys_handler.go internal/auth/provider_keys_handler_test.go
git commit -m "feat(api): /providers/registered 按 vendor 聚合 + provider key 支持 protocols 字段"
```

---

### Task 7: 前端 — client.ts / Providers.vue / ProviderKeys.vue

**Files:**
- Modify: `frontend/src/api/client.ts`
- Modify: `frontend/src/views/Providers.vue`
- Modify: `frontend/src/views/ProviderKeys.vue`

**Interfaces:**
- Consumes: Task 6 的 API 结构(`vendors` 数组;`ProviderKeyView.protocols`)

- [ ] **Step 1: client.ts 类型**

`frontend/src/api/client.ts` — 找 `ProviderInfo` 定义(当前含 name/protocol/models/key_pool/circuit_breaker),改为:

```ts
// P-provider-vendor: /providers 按 vendor 聚合 — 一个厂商多个注册名(协议面)
export interface ProviderNameInfo {
  name: string
  protocol: string
}

export interface VendorInfo {
  vendor: string
  names: ProviderNameInfo[]
  models: string[]
  key_pool?: KeyPoolStatus | null
  circuit_breaker?: CircuitBreakerInfo | null
}

export interface ProvidersResponse {
  vendors: VendorInfo[]
  count: number
}
```

(保留/移动原有 `KeyPoolStatus` / `CircuitBreakerInfo` 类型定义;`ProviderInfo` 若无其他消费方则删除,若有(AccessLogs 用 `RegisteredProvider`)保留。)把 `api.providers()` 的返回类型改成 `ProvidersResponse`(响应键从 `providers` 变 `vendors`)。`ProviderKeyView`(本地 interface,第 78 行)加 `protocols: string`。

- [ ] **Step 2: Providers.vue 按厂商渲染**

`frontend/src/views/Providers.vue` — 表格数据源从 `providers`(平铺)改为按 vendor 行:

```vue
// 列定义:
//   Name 列 → row.vendor
//   Protocol 列 → row.names.map(n => h(NTag, { type: 'info', size: 'small', style: { marginRight: '4px' } }, () => n.protocol))
//   Models 列 → row.models.join(', ')
//   Key Pool / Circuit Breaker 列 → row.key_pool / row.circuit_breaker(渲染逻辑同现状)
```

数据源:`providers.value = resp.vendors`;`ProviderInfo` 引用改 `VendorInfo`(import 自 client.ts)。

- [ ] **Step 3: ProviderKeys.vue 两级选择 + 单条提交**

`frontend/src/views/ProviderKeys.vue` — 添加表单的 `Provider` 下拉(第 23-27 行)改为两级:

(a) 表单 state 加字段:

```ts
const form = ref({
  provider_name: '',      // 提交目标:选中 vendor 的第一个注册名
  vendor: '',             // 新增:厂商
  protocols: [] as string[], // 新增:勾选的协议(空数组 = 全部)
  name: '',
  key: '',
  enabled: true,
  billing_source: 'api',
})
```

(b) 下拉 options(替换现有 `providerOptions`,第 123 行):

```ts
const vendorOptions = computed(() =>
  providers.value.map(v => ({ label: v.vendor, value: v.vendor }))
)
// 选中的 vendor 下的协议选项:
const protocolOptions = computed(() => {
  const v = providers.value.find(p => p.vendor === form.value.vendor)
  if (!v) return []
  return v.names.map(n => ({ label: n.protocol, value: n.protocol }))
})
// 提交目标 provider_name = vendor 的第一个注册名(协议面任意,pool 共享):
const targetProviderName = computed(() => {
  const v = providers.value.find(p => p.vendor === form.value.vendor)
  return v?.names[0]?.name ?? ''
})
```

(c) 表单把 "Provider" 下拉换成两级 `n-form-item`(厂商下拉 + 协议多选):

```vue
<n-form-item label="厂商" path="vendor">
  <n-select
    v-model:value="form.vendor"
    :options="vendorOptions"
    placeholder="选择厂商"
    :disabled="editing"
    @update:value="() => { form.protocols = protocolOptions.value.map(o => o.value) }"
  />
</n-form-item>
<n-form-item label="协议(不选 = 全部)" path="protocols">
  <n-select v-model:value="form.protocols" multiple :options="protocolOptions" placeholder="默认全勾" />
</n-form-item>
```

(厂商切换时把 `form.protocols` 重置为该厂商全部协议 — 默认全勾,用户可取消。)

(d) `save()`(第 307 行)改为:

```ts
async function save() {
  if (!form.value.vendor) {
    message.error('选择厂商')
    return
  }
  if (!form.value.key) {
    message.error('Key 必填')
    return
  }
  const target = targetProviderName.value
  if (!target) {
    message.error('厂商无可用注册名')
    return
  }
  saving.value = true
  try {
    // P-provider-vendor: 一把 key 存一条(厂商级一份),protocols 标勾选的协议;
    // 全勾 → 空(全部);勾选子集 → 逗号分隔。pool 共享,另一协议面的请求同样能取到
    const allProtocols = protocolOptions.value.map(o => o.value)
    const isAll =
      form.value.protocols.length === allProtocols.length &&
      allProtocols.every(p => form.value.protocols.includes(p))
    await axios.post(
      `/api/v1/providers/${encodeURIComponent(target)}/api-keys`,
      {
        name: form.value.name,
        key: form.value.key,
        enabled: form.value.enabled,
        billing_source: form.value.billing_source,
        protocols: isAll ? '' : form.value.protocols.join(','),
      },
    )
    message.success('已添加')
    modalVisible.value = false
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    saving.value = false
  }
}
```

(e) `load()`(第 258 行)数据源改为 vendor.names 展开(循环拉每个注册名的 api-keys,与现状循环 provider 等价):

```ts
const provResp = await api.providers()
providers.value = provResp.vendors
const allNames = (provResp.vendors || []).flatMap(v => v.names.map(n => n.name))
const allKeys = await Promise.all(
  allNames.map(async name => {
    try {
      const r = await axios.get<{ keys: ProviderKeyView[] }>(`/api/v1/providers/${encodeURIComponent(name)}/api-keys`)
      return r.data.keys || []
    } catch (e) {
      return []
    }
  })
)
keys.value = allKeys.flat()
```

(f) `openCreate()`(第 295 行)默认值改为第一个 vendor:

```ts
form.value = {
  provider_name: targetProviderName.value,
  vendor: providers.value[0]?.vendor ?? '',
  protocols: providers.value[0]?.names.map(n => n.protocol) ?? [],
  name: '',
  key: '',
  enabled: true,
  billing_source: 'api',
}
```

(列表列保持按 `provider_name` 显示即可,同 vendor 的 key 因共享池而 provider_name 相同,天然相邻。)

- [ ] **Step 4: 构建验证**

Run: `cd frontend && npm run build`(或项目现有的 build 脚本,先 `ls package.json` 确认)
Expected: 构建通过(TypeScript 无类型错误)

- [ ] **Step 5: 提交**

```bash
git add frontend/src/api/client.ts frontend/src/views/Providers.vue frontend/src/views/ProviderKeys.vue
git commit -m "feat(frontend): Provider 按厂商显示 + ProviderKeys 选厂商勾协议(单条创建)"
```

---

### Task 8: 特性落地 — cached_tokens 解析 + MiniMax 缓存价 + 包注释

**Files:**
- Modify: `backend/internal/provider/openai_compatible/openai_compatible.go`(parseOpenAIUsage)
- Test: Modify `backend/internal/provider/openai_compatible/openai_compatible_test.go`
- Modify: `config.yaml`(minimax 模型补缓存价)
- Modify: `backend/internal/provider/deepseek/balancer.go`(注释头部补官方特性要点)
- Modify: `backend/internal/provider/minimax/balancer.go`(注释头部补官方特性要点)

**Interfaces:**
- Consumes: 无(独立)
- Produces:
  - `parseOpenAIUsage` 同时解析 DeepSeek 风格 `prompt_cache_hit_tokens` 与 OpenAI 标准 `prompt_tokens_details.cached_tokens`,`CacheReadTokens = 两者之和`
  - MiniMax 各模型配置 `cost_per_1k_cache_read` / `cost_per_1k_cache_creation`

- [ ] **Step 1: 写失败测试**

`backend/internal/provider/openai_compatible/openai_compatible_test.go` 追加(沿用现有测试对该函数的断言方式):

```go
func TestParseOpenAIUsage_MiniMaxCachedTokens(t *testing.T) {
	// P-provider-vendor: OpenAI 标准 prompt_tokens_details.cached_tokens(MiniMax 等)
	body := []byte(`{
		"model": "MiniMax-M3",
		"usage": {
			"prompt_tokens": 1200,
			"completion_tokens": 300,
			"total_tokens": 1500,
			"prompt_tokens_details": {"cached_tokens": 800}
		}
	}`)
	u := parseOpenAIUsage(body)
	if u == nil {
		t.Fatal("parseOpenAIUsage returned nil")
	}
	if u.CacheReadTokens != 800 {
		t.Fatalf("CacheReadTokens = %d, want 800", u.CacheReadTokens)
	}
	if u.PromptTokens != 1200 {
		t.Fatalf("PromptTokens = %d, want 1200", u.PromptTokens)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider/openai_compatible/ -run TestParseOpenAIUsage_MiniMaxCachedTokens -v`
Expected: FAIL — CacheReadTokens = 0, want 800

- [ ] **Step 3: 实现 parseOpenAIUsage**

`backend/internal/provider/openai_compatible/openai_compatible.go` — `parseOpenAIUsage`(第 366 行)的匿名 struct 加字段:

```go
		Usage *struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			TotalTokens             int `json:"total_tokens"`
			PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens   int `json:"prompt_cache_miss_tokens"`
			// P-provider-vendor: OpenAI 标准缓存字段(MiniMax / OpenAI 官方 / qwen 等)
			// 与 DeepSeek 风格 prompt_cache_hit_tokens 并存,二者都算 CacheReadTokens
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
```

解析逻辑(现有 `raw := map[string]interface{}{...}` 处)改为:

```go
	cachedTokens := 0
	if resp.Usage.PromptTokensDetails != nil {
		cachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
	}

	raw := map[string]interface{}{
		"prompt_tokens":            resp.Usage.PromptTokens,
		"completion_tokens":        resp.Usage.CompletionTokens,
		"total_tokens":             resp.Usage.TotalTokens,
		"prompt_cache_hit_tokens":  resp.Usage.PromptCacheHitTokens,
		"prompt_cache_miss_tokens": resp.Usage.PromptCacheMissTokens,
		"cached_tokens":            cachedTokens,
		"reasoning_tokens":         reasoningTokens,
	}
```

`CacheReadTokens`(现有 `CacheReadTokens: resp.Usage.PromptCacheHitTokens` 处)改为:

```go
		// P40: DeepSeek 的 cache 模型 — prompt_cache_hit_tokens 视为 cache read,
		// prompt_cache_miss_tokens 已经包含在 PromptTokens 里(完整输入)
		// P-provider-vendor: OpenAI 标准 cached_tokens(MiniMax 等)同样按缓存价计费,
		// 与 DeepSeek 风格并存相加
		CacheReadTokens:     resp.Usage.PromptCacheHitTokens + cachedTokens,
		CacheCreationTokens: 0,
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/openai_compatible/ -v`
Expected: 新旧测试全部 PASS(既有 `TestParseOpenAIUsage` 相关测试断言 `CacheReadTokens` 的地方,若用 `prompt_cache_hit_tokens` 构造的响应,`cached_tokens` 缺省为 0,行为不变)

- [ ] **Step 5: config.yaml 补 MiniMax 缓存价**

`config.yaml` — `minimax:` 与 `minimax-openai:` 两个条目里每个模型加缓存价字段(文档 2026-08-04:按量页):

| 模型 | cost_per_1k_cache_read | cost_per_1k_cache_creation |
|---|---|---|
| MiniMax-M3 | 0.00042 | 不加 |
| MiniMax-M2.7 / M2.7-highspeed | 0.00042 | 0.002625 |
| MiniMax-M2.5 / M2.5-highspeed | 0.00021 | 0.002625 |
| MiniMax-M2.1 / M2.1-highspeed | 0.00021 | 0.002625 |
| MiniMax-M2 | 0.00021 | 0.002625 |

示例(M3,价格按官方按量页 2026-08-05 更新 — 永久五折后 2.1/8.4/0.42 元/M):

```yaml
      - id: "MiniMax-M3"              # 1M tokens,旗舰
        aliases: ["MiniMax"]
        cost_per_1k_input: 0.0021
        cost_per_1k_output: 0.0084
        # P-provider-vendor: 缓存价(文档 2026-08-04)— M3 仅自动缓存(读 0.42 元/M),无主动缓存
        cost_per_1k_cache_read: 0.00042
        # P-quota-512k: 官方悬崖 — 输入含缓存 > 512k 时输入/输出/缓存全项 ×2(永久五折后价)
        long_context_input_threshold: 512000
        long_context_multiplier: 2
```

(M2.7 再补 `cost_per_1k_cache_creation: 0.002625`;M2.5/M2.1 的 read 用 0.00021。`minimax-openai` 条目同样补,模型列表相同。)

- [ ] **Step 6: balancer 包注释补官方特性要点**

`backend/internal/provider/deepseek/balancer.go` 头部注释(现有 "P68 + P-quota-balance" 块之后)追加:

```go
// 官方余额 API(2026-08-04 文档):
//   GET https://api.deepseek.com/user/balance
//   - 响应 balance_infos[]:每项 {currency: "CNY"|"USD", total_balance/granted_balance/topped_up_balance}
//   - 注意:金额字段是 STRING 类型,不是数字;balance_infos 是对象数组
//   - 鉴权失败 401;余额不足(调用时)402;限流 429(并发维度:pro 500 / flash 2500,账号粒度)
//   - 峰谷定价预告:高峰(北京 9-12 / 14-18 点)2 倍价,未生效
```

`backend/internal/provider/minimax/balancer.go` 头部注释追加:

```go
// 官方文档(2026-08-04 调研):无公开余额/套餐额度查询 REST API —
// 余额与 Token Plan 用量只能在 Web 控制台查看;API 侧只能靠错误码被动感知:
//   base_resp.status_code = 1008(余额不足)/ 2056(超出 Token Plan 限制)
// 本 balancer 使用的 https://www.minimaxi.com/v1/token_plan/remains 为未文档化端点,
// 实测有效(2026-08-04),官方不保证稳定性;如失效,可降级为错误码驱动。
```

- [ ] **Step 7: 全量验证 + 提交**

Run: `go build ./... && go test ./...`
Expected: 通过

```bash
git add internal/provider/openai_compatible/openai_compatible.go internal/provider/openai_compatible/openai_compatible_test.go config.yaml internal/provider/deepseek/balancer.go internal/provider/minimax/balancer.go
git commit -m "feat(quota): 解析 OpenAI 标准 cached_tokens 计费 + MiniMax 缓存价配置 + balancer 注释补官方特性"
```

---

## 收尾:部署验证(执行者必须做)

1. 启动 DB 迁移验证:`go build -o ../bin/gateway ./cmd/gateway && ../bin/gateway`(config.yaml port 8080)
2. 验证启动日志无 provider 创建失败;`/api/v1/providers/registered` 返回 `vendors` 结构,deepseek 一个 vendor 两个 names
3. `GET /api/v1/providers/deepseek/api-keys` 返回的 key 带 `protocols` 字段
4. 用 deepseek 的 openai 和 anthropic 两个协议各发一次真实请求,确认都能路由到 key(同一 pool 共享生效)
5. 观察日志 `pollAllBalancers` 每轮对 deepseek 只 poll 一次(去重生效)
6. ProviderKeys 页:添加 key 选厂商→勾协议;列表按 vendor 分组
