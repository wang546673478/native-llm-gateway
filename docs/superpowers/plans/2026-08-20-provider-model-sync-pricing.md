# 上游模型同步 + 手工计费定价 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把「厂商有哪些模型」从硬编码 `DefaultModels` + config `models[]` 迁到 DB `provider_models`(上游 `/v1/models` 拉取),把「模型定价」改为每百万 token 三档手工填写存 DB,并新增独立「模型管理」页;现有 Providers 页不动。

**Architecture:** 激活现有死表 `provider_models` 为唯一权威。按厂商(vendor)粒度存模型 id + 三档每百万价。新增 `Provider.ListModels(ctx)` 窄接口(openai base 真查、anthropic base 返回 NotSupported、google base 单独适配)。`manager` 改为从 DB 读 models/pricing/defaultModels。新增 admin 端点 + 前端页。

**Tech Stack:** Go 1.x + GORM(PostgreSQL)、gin、Vue 3 + naive-ui + vue-router + axios。

## Global Constraints

> 每一条 task 的要求都隐式包含本节。执行者必须逐字遵守。

- **低耦合高内聚是第一原则**(CLAUDE.md):pool 不得直接 import circuit/quotacheck;跨包改动必须用接口注入或回调槽;一个函数干 ≤2 件事。
- **计费单位 = 每百万 token**。DB 字段名必须用 `cost_per_million_*`,**禁止**再出现 `cost_per_1k_*` 字样(避免"名字叫 1k 存的是 1M"的坑)。
- **计费维度只三档**:`CostPerMillionInput`(未缓存输入)、`CostPerMillionCacheRead`(缓存命中输入)、`CostPerMillionOutput`(输出)。**删除** `CacheCreation`(缓存写入)与 `LongContext`(长上下文悬崖)两个概念 —— spec §2/§3 明确砍掉。
- **价格不迁移**:config 旧 `cost_per_1k_*` 一律作废,不做任何"旧价自动带入"逻辑。
- **未定价模型可用**:价格=0 或空的模型照样进 `Models()` 参与路由,不阻塞。
- **DB 粒度 = 厂商(vendor)**,不是注册面。`provider_models` 主键/唯一键 = `(vendor, model_id)`。读时经 `Manager.VendorFor(name)` 把厂商模型归位到每个注册面。
- **迁移顺序严格** (§4.4.1):先上游读进 DB → 再 manager 改读 DB → 最后删 config `models` 段。**不允许乱序**,否则 `Models()` 空 → 路由 503 / 计费归零。
- **alias 不动**(方案 B):`routing.aliases` / `model_aliases` / `ModelAlias` 表一律不碰。
- 改完必跑 `make test` / `make vet` / `make build`(repo 根,内部 `cd backend`)与 `cd frontend && npx vue-tsc --noEmit`。
- 新增 DB 字段必须有 GORM tag + (如引入新表结构变更)确认 AutoMigrate 覆盖。
- 新 provider 面/接口方法改完,编译期必须让所有现有实现(6 vendor base + 各协议面)通过 —— 接口加方法时同步落实所有实现,否则失败。

---

## 任务总览(依赖顺序)

```
Task 1: DB 表改造 — provider_models 重定义(每百万、按 vendor)
Task 2: ModelCost 收敛 + ComputeCost 改写(三档、每百万、去缓存写/长上下文)
Task 3: Provider.ListModels 窄接口 + openai/anthropic/google base 三实现 + 厂商透传
Task 4: provider_models Store(database 包,CRUD)
Task 5: sync service — 调 ListModels → upsert 到 Store
Task 6: manager 改从 DB 读 models/pricing/defaultModels
Task 7: 删 config models 段 + 清理 DefaultModels 常量 + main/server 投影
Task 8: admin 端点(list / sync / save pricing)
Task 9: 前端模型管理页 + 路由 + 菜单 + client.ts
```

Task 6 与 Task 7 是贯穿全盘的「换数据源 + 砍来源」两步,必须在 Task 1→5 的 DB 能力就绪后、且 Task 8/9 之前完成,保证切换那一刻 DB 已有真实清单。Task 8/9 依赖 Task 6 的 manager 读 DB + Task 5 的 sync。

---

### Task 1: DB 表改造 — `provider_models` 重定义

**Files:**
- Modify: `backend/internal/database/models.go:26-40`(`ProviderModel` struct + TableName)
- Test: `backend/internal/database/database_test.go`(追加)

**Interfaces:**
- Consumes: 现有 `gorm` model 约定(`models.go` 里其他 struct 的 tag 风格)。
- Produces: `database.ProviderModel` 类型,后续 Task 4 的 Store 和 Task 6 的 manager 读取依赖它的字段名。

- [ ] **Step 1: 改写 `ProviderModel` struct**

把 `database.ProviderModel` 改为下面形状(每百万、按 vendor):

```go
// ProviderModel 厂商在售模型 + 手工定价(每百万 token)。
// 粒度 = vendor(厂商),不是注册面(openai/anthropic 面共享同一批模型)。
// 模型清单来自上游 /v1/models 同步,价格由用户在模型管理页手工填写。
type ProviderModel struct {
	ID     uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Vendor string `gorm:"column:vendor;uniqueIndex:idx_vendor_model;not null" json:"vendor"`
	ModelID string `gorm:"column:model_id;uniqueIndex:idx_vendor_model;not null" json:"model_id"`
	// 每百万 token 价;0 = 未填(未定价模型仍可用)
	CostPerMillionInput     float64 `gorm:"column:cost_per_million_input;not null;default:0" json:"cost_per_million_input"`
	CostPerMillionCacheRead float64 `gorm:"column:cost_per_million_cache_read;not null;default:0" json:"cost_per_million_cache_read"`
	CostPerMillionOutput    float64 `gorm:"column:cost_per_million_output;not null;default:0" json:"cost_per_million_output"`
	// 同步元数据
	SyncedAt *time.Time `gorm:"column:synced_at" json:"synced_at"`
	Source   string     `gorm:"column:source;not null;default:'manual'" json:"source"` // "upstream" | "manual"
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (ProviderModel) TableName() string { return "provider_models" }
```

说明:删除旧字段 `provider_name` / `cost_per_1k_input` / `cost_per_1k_output` / `cost_per_1k_cache_read` / `cost_per_1k_cache_creation`;没有新引入"长上下文"字段(本设计砍掉)。`Provider` 主表里对 `ProviderModel` 的 `foreignKey:ProviderName;references:Name` 关联也会失效,一并调整(见 Step 2)。

- [ ] **Step 2: 调整 `Provider.Models` 关联**

在 `backend/internal/database/models.go:20` 的 `Provider` struct 里,把:

```go
Models []ProviderModel `gorm:"foreignKey:ProviderName;references:Name" json:"models,omitempty"`
```

改为:

```go
Models []ProviderModel `gorm:"foreignKey:Vendor;references:Name" json:"models,omitempty"`
```

(厂商名 `vendor` 现在对应 `providers.name`;该关联仅用于预加载,实际读写走 Task 4 的 Store。)

- [ ] **Step 3: 写测试**

在 `database_test.go` 追加一个测试:`ProviderModel` 表名 = `provider_models`,且 GORM tag 里 `vendor` + `model_id` 是复合唯一 `idx_vendor_model`,字段含 `cost_per_million_input`(用反射或对一个实例覆写后查询验证唯一约束生效)。

- [ ] **Step 4: 跑测试**

Run: `cd /home/hhhh/llm-gateway && make test`
Expected: PASS;新测试覆盖 ProviderModel 字段/表名/唯一键。

- [ ] **Step 5: Commit**

```bash
git add backend/internal/database/models.go backend/internal/database/database_test.go
git commit -m "refactor(database): ProviderModel 改每百万 token + 按 vendor 粒度"
```

---

### Task 2: `ModelCost` 收敛 + `ComputeCost` 改写

**Files:**
- Modify: `backend/internal/provider/provider.go:59-66`(`ModelCost`)与 `:147-173`(`ComputeCost`)、`:125-145`(`Usage` 注释可选)
- Modify: `backend/internal/provider/manager.go:59-66` 的 `ModelCost` 重复定义 —— **注意** `provider.go` 里 `ModelCost` 定义在 `manager.go`(两处?见下方 Notes)
- Test: `backend/internal/provider/*_test.go`(追加 ComputeCost 用例)

**Interfaces:**
- Produces: 收敛后的 `provider.ModelCost`(3 字段)与 `ComputeCost` 新公式,后续 Task 6/8 用。

- [ ] **Step 1: 定位 `ModelCost` 唯一定义**

实际 `ModelCost` 定义在 `backend/internal/provider/manager.go:59-66`(provider.go 只在 `ComputeCost` 里引用)。把 `manager.go` 的 `ModelCost` 改为:

```go
type ModelCost struct {
	CostPerMillionInput     float64 // 输入(未缓存)每百万 token
	CostPerMillionCacheRead float64 // 缓存命中输入每百万 token;无此概念 = 0
	CostPerMillionOutput    float64 // 输出每百万 token
}
```

删除 `CostPer1kInput` / `CostPer1kOutput` / `CostPer1kCacheRead` / `CostPer1kCacheCreation` / `LongContextInputThreshold` / `LongContextMultiplier`。

- [ ] **Step 2: 改写 `ComputeCost`**

把 `provider.go` 的 `ComputeCost` 改为(底数从 `/1000` 改 `/1_000_000`,去掉 cache creation fallback 与长上下文分支):

```go
func ComputeCost(c ModelCost, u *Usage) float64 {
	hasAnyCost := c.CostPerMillionInput > 0 || c.CostPerMillionCacheRead > 0 || c.CostPerMillionOutput > 0
	if !hasAnyCost {
		return 0
	}
	return (float64(u.PromptTokens)/1_000_000.0)*c.CostPerMillionInput +
		(float64(u.CacheReadTokens)/1_000_000.0)*c.CostPerMillionCacheRead +
		(float64(u.CompletionTokens)/1_000_000.0)*c.CostPerMillionOutput
}
```

注意:`u.PromptTokens` 语义含「不计 cache 的输入」;cache 命中用 `u.CacheReadTokens`。`Usage.CacheCreationTokens` 保留在 struct 里(上游响应仍可能带),但不再参与计费。

- [ ] **Step 3: 清理所有对旧 ModelCost 字段的引用**

`grep -rn "CostPer1k\|LongContextInput\|LongContextMultiplier\|CostPer1kCacheCreation"` 全仓库,把 `manager.go` 的 `LoadFromConfig` / `ReloadPricing`、`cmd/gateway/main.go` 的 `toManagerConfig`、`server.go` 的 `toManagerConfigForReload` 里构造 `provider.ModelCost{...}` 的字段全部改为三字段(暂填 `CostPerMillionInput/CacheRead/Output`,值仍从 config 旧字段取下)。

> ⚠️ **单位换算注意**:config 旧字段是 `cost_per_1k_*`(每千 token),而新 `ModelCost` 字段语义是**每百万 token**。本 task 是**纯结构收敛**(改字段名/删除字段),**不做 ×1000 换算**——因为 Task 7 会彻底删 config 模型价格、价格改为在新页面手工填每百万值,这里临时保留"旧千价数值"只是过渡期编译通过的手段,最终会被 Task 7 丢弃。**不要在本 task 引入换算逻辑**(否则与 Task 7"价格不迁移"矛盾)。

- [ ] **Step 4: 重写 TestComputeCost + 跑**

现有 `provider_test.go:40-106` 的 `TestComputeCost` 用了 `CostPer1k*` / `LongContext*` / `CacheCreationTokens`,这些字段已被删,会编译失败。**整体重写**为三档每百万:

```go
func TestComputeCost(t *testing.T) {
	m3 := ModelCost{
		CostPerMillionInput:     2.1, // 每百万 token
		CostPerMillionCacheRead: 0.42,
		CostPerMillionOutput:    8.4,
	}
	t.Run("三档每百万", func(t *testing.T) {
		u := &Usage{PromptTokens: 1_500_000, CacheReadTokens: 1_000_000, CompletionTokens: 500_000}
		got := ComputeCost(m3, u)
		want := 1.5*2.1 + 1.0*0.42 + 0.5*8.4
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})
	t.Run("无定价 → 0", func(t *testing.T) {
		if got := ComputeCost(ModelCost{}, &Usage{PromptTokens: 1_000_000}); got != 0 {
			t.Errorf("cost = %v, want 0", got)
		}
	})
	t.Run("CacheCreationTokens 不参与计费", func(t *testing.T) {
		u := &Usage{PromptTokens: 1_000_000, CacheCreationTokens: 999_000_000, CompletionTokens: 1_000_000}
		got := ComputeCost(m3, u)
		want := 1.0*2.1 + 1.0*8.4 // 不含 cache creation
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})
}
```

(删除所有 `LongContext*`、`CacheCreationTokens` 计费相关的用例,保留并改写三档 + 无定价两个基本语义。)

Run: `cd /home/hhhh/llm-gateway && make test`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git commit -am "refactor(provider): ModelCost 收敛三档每百万定价,去掉缓存写入/长上下文"
```

> 注意:本 task 只改结构/公式,暂不删 config 字段(那步在 Task 7)。这里新的 `CostPerMillion*` 字段里的值在过渡期仍是"千价数值"(未换算),但 Task 7 会彻底删 config 模型价格,届时这个过渡不再有意义。

---

### Task 3: `Provider.ListModels` 窄接口 + 三协议面实现 + 厂商透传

**Files:**
- Modify: `backend/internal/provider/provider.go:179-203`(Provider interface 加方法)
- Modify: `backend/internal/provider/openai_compatible/openai_compatible.go`(加 `ListModels`)
- Modify: `backend/internal/provider/anthropic_compatible/anthropic_compatible.go`(加 `ListModels` 返回 NotSupported)
- Modify: `backend/internal/provider/google/google.go`(加 `ListModels`)
- Modify: 6 个厂商包的 openai/anthropic 面 wrapper:`deepseek/deepseek.go` + `deepseek/anthropic.go`、`glm/glm.go` + `glm/anthropic.go`、`mimo/mimo.go` + `mimo/anthropic.go`、`minimax/minimax.go` + `minimax/openai.go`、`qwen/qwen.go`、`gemini/gemini.go`(所有 `*Provider` / `*AnthropicProvider` 加 `ListModels` 透传 `base.ListModels`)
- Test: `backend/internal/provider/openai_compatible/*_test.go`(追加)

**Interfaces:**
- Consumes: `provider.Provider` 接口;`openai_compatible.Base` / `anthropic_compatible.Base` / `google.Base`。
- Produces: `ListModels(ctx context.Context) ([]string, error)`,Task 5 的 sync service 用它。

- [ ] **Step 1: 定义 sentinel error + 接口方法**

在 `provider.go` 加:

```go
// ErrListModelsNotSupported 该协议面不提供模型列表能力。
// 同步层收到它时,回退到同 vendor 的 OpenAI 面去查。
var ErrListModelsNotSupported = errors.New("provider: list models not supported")
```

Provider interface 加一行(放在 `HealthCheck` 后):

```go
	// ListModels 返回上游当前在售模型 id 列表。
	// 不支持(如 anthropic 面)返回 ErrListModelsNotSupported。
	ListModels(ctx context.Context) ([]string, error)
```

- [ ] **Step 2: openai base 实现**

在 `openai_compatible.go` 加(复用 HealthCheck 的 `/v1/models` + Bearer 形状):

```go
// ListModels 调 GET {endpoint}/v1/models 拉上游模型 id 列表。
func (b *Base) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if b.cfg.Pool != nil {
		if k, err := b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolOpenAI)); err == nil {
			req.Header.Set("Authorization", "Bearer "+k.Key)
			defer b.cfg.Pool.ReportSuccess(k)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
```

- [ ] **Step 3: anthropic base 实现**

```go
// ListModels anthropic 协议面无模型列表端点 → NotSupported。
func (b *Base) ListModels(ctx context.Context) ([]string, error) {
	return nil, provider.ErrListModelsNotSupported
}
```

- [ ] **Step 4: google base 实现**

`google.go` 的 `/models` 用 `x-goog-api-key` 头 + 响应含 `models[].name`(形如 `models/gemini-2.5-pro`),返回时剥 `models/` 前缀。实现:

```go
// ListModels GET {endpoint}/models,返回 models/* 去前缀后的模型 id。
func (b *Base) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if b.cfg.Pool != nil {
		if k, err := b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolGoogle)); err == nil {
			req.Header.Set("x-goog-api-key", k.Key)
			defer b.cfg.Pool.ReportSuccess(k)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
```

- [ ] **Step 5: 厂商 wrapper 透传**

给每个 `*Provider` / `*AnthropicProvider` 加一行方法(以 deepseek openai 面为例):

```go
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return p.base.ListModels(ctx)
}
```

覆盖:deepseek(`Provider` + `AnthropicProvider`)、glm、mimo、minimax(`Provider`=anthropic 面 + `OpenAIProvider`=openai 面)、qwen、gemini。anthropic 面的 wrapper 也照写透传 `base.ListModels`(base 会返回 NotSupported,同步层据此回退)。

- [ ] **Step 6: 写测试 & 跑**

openai base 的 `ListModels` 用 `httptest.Server` 返回 `{"data":[{"id":"a"},{"id":"b"}]}` 断言返回 `["a","b"]`;anthropic base 断言返回 `ErrListModelsNotSupported`。

Run: `cd /home/hhhh/llm-gateway && make test && make vet`
Expected: 全绿;接口加方法后所有实现编译通过。

- [ ] **Step 7: Commit**

```bash
git commit -am "feat(provider): ListModels 窄接口 + 三协议面实现(anthropic 面 NotSupported)"
```

---

### Task 4: `provider_models` Store(database 包)

**Files:**
- Create: `backend/internal/database/provider_model_store.go`
- Test: `backend/internal/database/provider_model_store_test.go`

**Interfaces:**
- Produces: `database.ProviderModelStore` 接口,Task 5(sync)与 Task 6(manager 读)与 Task 8(admin)依赖。仿 `RouteOrderStore` 的 GORM store 模式。

- [ ] **Step 1: 写 Store 接口 + 实现**

```go
package database

import (
	"context"
	"gorm.io/gorm"
)

// ProviderModelStore 访问 provider_models(厂商在售模型 + 定价)。
type ProviderModelStore interface {
	// ListByVendor 取某厂商的全部模型(按 model_id 排序,确定性)。
	ListByVendor(ctx context.Context, vendor string) ([]ProviderModel, error)
	// UpsertModels 用上游同步结果整体替换某厂商的模型清单:
	// 保留已有手工价格(按 model_id 匹配),新增模型价格 0、source=upstream;
	// 不再存在于上游列表的旧模型保留(不删除,避免误删手工价格)。
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
	// SavePricing 手工更新某厂商某模型的三档每百万价格。
	SavePricing(ctx context.Context, vendor, modelID string, input, cacheRead, output float64) error
	// All 列出全部分组后的厂商模型(供模型管理页一次性渲染)。
	All(ctx context.Context) ([]ProviderModel, error)
}

type gormProviderModelStore struct{ db *gorm.DB }

func NewProviderModelStore(db *gorm.DB) ProviderModelStore {
	return &gormProviderModelStore{db: db}
}

func (s *gormProviderModelStore) ListByVendor(ctx context.Context, vendor string) ([]ProviderModel, error) {
	var out []ProviderModel
	if err := s.db.WithContext(ctx).Where("vendor = ?", vendor).
		Order("model_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormProviderModelStore) UpsertModels(ctx context.Context, vendor string, modelIDs []string) error {
	// 读现有(带手工价),映射 model_id → 价格;再逐条 upsert。
	existing, err := s.ListByVendor(ctx, vendor)
	if err != nil {
		return err
	}
	keep := make(map[string]ProviderModel, len(existing))
	for _, m := range existing {
		keep[m.ModelID] = m
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, id := range modelIDs {
			if id == "" {
				continue
			}
			var row ProviderModel
			if prev, ok := keep[id]; ok {
				row = prev // 保留手工价
			}
			row.Vendor = vendor
			row.ModelID = id
			row.Source = "upstream"
			now := time.Now()
			row.SyncedAt = &now
			if err := tx.Where("vendor = ? AND model_id = ?", vendor, id).
				Assign(row).FirstOrCreate(&ProviderModel{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *gormProviderModelStore) SavePricing(ctx context.Context, vendor, modelID string, input, cacheRead, output float64) error {
	return s.db.WithContext(ctx).
		Where("vendor = ? AND model_id = ?", vendor, modelID).
		Updates(map[string]interface{}{
			"cost_per_million_input":      input,
			"cost_per_million_cache_read": cacheRead,
			"cost_per_million_output":     output,
			"source":                      "manual",
		}).Error
}

func (s *gormProviderModelStore) All(ctx context.Context) ([]ProviderModel, error) {
	var out []ProviderModel
	if err := s.db.WithContext(ctx).Order("vendor ASC, model_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
```

注意 `Assign(row)` 的 `row` 含 `ID` 字段,`FirstOrCreate` 时若已存在会按 `where` 命中并用 `Assign` 覆盖;若不存在则 `Create` 该 row(含 `ID`=0,自增)。确保 upsert 语义正确(保留价格、更新 synced_at/source)。

- [ ] **Step 2: 写测试**

用内存 GORM(参考 `database_test.go` 现有 harness)测:`UpsertModels` 后 `ListByVendor` 返回模型且保留手工价;`SavePricing` 更新三档;`All` 返回排序确定。

- [ ] **Step 3: 跑 & Commit**

Run: `cd /home/hhhh/llm-gateway && make test`
Commit:
```bash
git add backend/internal/database/provider_model_store.go backend/internal/database/provider_model_store_test.go
git commit -m "feat(database): ProviderModelStore(UpsertModels/SavePricing/ListByVendor/All)"
```

---

### Task 5: sync service — 调 `ListModels` → upsert 到 Store

**Files:**
- Create: `backend/internal/provider/modelsync.go`(在 provider 包内,避免跨包 import database 造成 provider→database 反向依赖 —— 见 Notes)
- Test: `backend/internal/provider/modelsync_test.go`

**Interfaces:**
- Consumes: `provider.Provider.ListModels`;`manager` 的 `Get`/`GetByProtocol`/`VendorFor`;`database.ProviderModelStore`。
- Produces: `SyncVendorModels(ctx, vendor)` 之类函数,Task 8 admin 端点调用。

**关键设计(低耦合)**:sync 逻辑不能放 `database` 包(否则 database→provider 双向)。把它放 `provider` 包,但 `ProviderModelStore` 接口**反向注入**——由 server 层传一个 `func`/接口进来(与 `FingerprintSanitizer`/`inflight` 同构的回调注入)。见 Step 1。

- [ ] **Step 1: 定义 sync 依赖接口 + 函数**

```go
package provider

// ModelSyncStore 同步落库所需的最小 DB 接口(由 database 实现注入),
// 放 provider 包定接口 = 依赖倒置,provider 不 import database。
type ModelSyncStore interface {
	UpsertModels(ctx context.Context, vendor string, modelIDs []string) error
}

// SyncVendorModels 拉某厂商上游在售模型并落库。
// 优先用 vendor 的 OpenAI 面;若无 openai 面则跳过(返回 nil)。
// 单个面 ListModels 返回 ErrListModelsNotSupported 时,换同 vendor 的 openai 面。
func SyncVendorModels(ctx context.Context, m *Manager, vendor string, store ModelSyncStore) ([]string, error) {
	names := m.Names()
	var openaiFace string
	for _, n := range names {
		if m.VendorFor(n) == vendor {
			if p, ok := m.Get(n); ok && p.Protocol() == ProtocolOpenAI {
				openaiFace = n
				break
			}
		}
	}
	if openaiFace == "" {
		// 没有 openai 面 → 无法同步(当前 6 vendor 都有,防御性返回)
		return nil, fmt.Errorf("vendor %q has no openai-compatible face to sync models", vendor)
	}
	p, _ := m.Get(openaiFace)
	ids, err := p.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	if err := store.UpsertModels(ctx, vendor, ids); err != nil {
		return nil, err
	}
	return ids, nil
}
```

- [ ] **Step 2: 写测试**

fake provider(`ListModels` 返回固定 `[]string`) + fake `ModelSyncStore`(记录收到的 vendor/modelIDs)。测:找到 openai 面、调用 ListModels、把结果喂给 store;无 openai 面时返回 error。

- [ ] **Step 3: 跑 & Commit**

Run: `cd /home/hhhh/llm-gateway && make test`
Commit:
```bash
git commit -am "feat(provider): SyncVendorModels 按 vendor 走 openai 面拉列表 → store"
```

---

### Task 6: manager 改从 DB 读 models / pricing / defaultModels

**Files:**
- Modify: `backend/internal/provider/manager.go`(LoadFromConfig / ReloadPricing / 新增读 DB 入口)
- Modify: `backend/internal/provider/lookup.go`(如接口需要)
- Test: `backend/internal/provider/manager_test.go`

**Interfaces:**
- Consumes: `database.ProviderModelStore`(注入)。
- Produces: manager 内存 `models` / `pricing` / `defaultModels` 改为 DB 来源。

**关键点**:引入 DB 后,`LoadFromConfig` 不再自行填 `pricing`/`defaultModels`/`Models`。改为 Manager 持有一个 `modelStore ProviderModelStore`(注入),新增 `LoadModelsFromStore()` 方法,在 server 装配时读 DB 填充。保留 `LoadFromConfig` 只负责 provider 实例构建(它不再碰 pricing/defaultModels/models)。

- [ ] **Step 1: Manager 注入 store + 新方法**

在 `Manager` struct 加字段:

```go
	modelStore ModelSyncStore // 扩展为完整读接口(见下面 ModelStore 定义)
```

定义完整读接口(既用于 sync 写入、也用于 manager 读取):

```go
type ModelStore interface {
	ListByVendor(ctx context.Context, vendor string) ([]providerModelLike, error)
	All(ctx context.Context) ([]providerModelLike, error)
}
```

为免 provider 包 import database 的 `ProviderModel` 具体类型,在 provider 包定义一个**契约结构体**(若嫌麻烦,可让 store 返回 `[]struct{Vendor,ModelID string,CostPerMillionInput,CacheRead,Output float64}` 的投影)。本计划用投影结构体 `DBModelRow`:

```go
type DBModelRow struct {
	Vendor string
	ModelID string
	CostPerMillionInput float64
	CostPerMillionCacheRead float64
	CostPerMillionOutput float64
}
```

`ModelStore` 接口:
```go
type ModelStore interface {
	ListByVendor(ctx context.Context, vendor string) ([]DBModelRow, error)
	All(ctx context.Context) ([]DBModelRow, error)
}
```

> Task 4 的 `database.ProviderModelStore` 返回 `[]database.ProviderModel`。这里需要一个**适配器**在 server 装配层把 `database.ProviderModelStore` 适配成 `provider.ModelStore`(返回 `[]DBModelRow`)。见 Task 6 Step 4 / server 装配。

- [ ] **Step 2: 新增 `LoadModelsFromStore`**

```go
func (m *Manager) LoadModelsFromStore(ctx context.Context, store ModelStore) error {
	rows, err := store.All(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pricing = make(map[string]ModelCost)
	m.defaultModels = make(map[string]string)
	// vendor → 首个 model_id(排序确定)作默认模型
	first := make(map[string]string)
	byVendor := make(map[string][]string) // vendor → model ids 用于 modelSet
	for _, r := range rows {
		c := ModelCost{
			CostPerMillionInput:     r.CostPerMillionInput,
			CostPerMillionCacheRead: r.CostPerMillionCacheRead,
			CostPerMillionOutput:    r.CostPerMillionOutput,
		}
		// pricing 键统一改为 "<vendor>:<model_id>"(不是注册面名)。
		// 代价:CostFor(Step 3) 与 ModelsFor(Task 7) 都必须先用 VendorFor 归位再查。
		m.pricing[pricingKey(r.Vendor, r.ModelID)] = c
		if _, ok := first[r.Vendor]; !ok {
			first[r.Vendor] = r.ModelID
		}
	}
	// 给每个启用 provider 的 defaultModel + 内存 model 集按 VendorFor 归位
	for name := range m.providers {
		v := m.VendorFor(name)
		if dm, ok := first[v]; ok {
			m.defaultModels[name] = dm
		}
	}
	m.logger.Info("models loaded from store", zap.Int("rows", len(rows)))
	return nil
}
```

- [ ] **Step 3: 调整 `CostFor` / `DefaultModelFor` 用 vendor 归位**

`CostFor(provider, model)` 改为先 `v := m.VendorFor(provider)`,再查 `m.pricing[pricingKey(v, model)]`(因为 Step 2 的 pricing 键是 vendor):

```go
func (m *Manager) CostFor(provider, model string) ModelCost {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.VendorFor(provider)
	if c, ok := m.pricing[pricingKey(v, model)]; ok {
		return c
	}
	return ModelCost{}
}
```

其余调用方(`proxy.go:875`)传的是 `result.ProviderName`(注册面),`VendorFor` 会把它归到厂商,正确。

> ⚠️ **键一致性硬约束(三处必须同时改,否则静默错误)**:
> 1. `LoadModelsFromStore` 用 `pricingKey(r.Vendor, r.ModelID)` 存(Step 2);
> 2. `CostFor` 用 `VendorFor(provider)` 后 `pricingKey(vendor, model)` 查(本 Step);
> 3. `LoadFromConfig` 里原来的 `m.pricing[pricingKey(name, modelID)]`(name=注册面)整段删除,否则与 DB 源不一致残留;
> 4. `ModelsFor`(Task 7)按 `pricingKey(vendor:model)` 前缀查。
> 若任一漏改:`CostFor` 归位缺失 → 返回零价 → **计费全 0**;`ModelsFor` 前缀错 → **路由无候选 → 503**。此为"编译通过但运行错"的最高风险点,须写一条 `CostFor`(传注册面名→命中 vendor 价)的断言测试。

- [ ] **Step 4: server 装配层加 store 适配 + 注入**

在 `server.go` New 里,构造 `database.NewProviderModelStore(db)`,用一个适配器函数注入 manager:

```go
type providerModelStoreAdapter struct{ s database.ProviderModelStore }
func (a providerModelStoreAdapter) All(ctx context.Context) ([]provider.DBModelRow, error) {
	rows, err := a.s.All(ctx)
	if err != nil { return nil, err }
	out := make([]provider.DBModelRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, provider.DBModelRow{
			Vendor: r.Vendor, ModelID: r.ModelID,
			CostPerMillionInput: r.CostPerMillionInput,
			CostPerMillionCacheRead: r.CostPerMillionCacheRead,
			CostPerMillionOutput: r.CostPerMillionOutput,
		})
	}
	return out, nil
}
func (a providerModelStoreAdapter) ListByVendor(ctx context.Context, v string) ([]provider.DBModelRow, error) { /* 同 All 过滤 */ }
```

然后在 `manager.LoadFromConfig` 之后调用 `manager.LoadModelsFromStore(ctx, adapter)`(首次启动)。注意:Task 6 落地时 DB 可能还没同步(Task 5 才提供同步),此时 DB 若空,`LoadModelsFromStore` 只是让 memory 空 —— 这正是 §4.4.1 顺序的**风险点**,必须与 Task 7 的"删 config"严格解耦:即 **Task 6 实现后,若 DB 空,应在启动时返回一个明确 warning 而非静默零**。见 Step 5。

- [ ] **Step 5: 空 DB 兜底 + 测试**

在 `LoadModelsFromStore` 里若 `len(rows)==0`,log Warn("provider_models empty — route may have no candidates until first sync")。测试:`LoadModelsFromStore` 喂一个 fake store(含 vendor=minimax, model=MiniMax-M3, 价 0.28/0/1.10)后,`CostFor(VendorFor 归位后的注册面, MiniMax-M3)` 返回对应 ModelCost;`DefaultModelFor` 返回 `MiniMax-M3`。

- [ ] **Step 6: `ReloadPricing` 语义拆分(热重载路径,关键)**

`ReloadPricing`(manager.go:293-318)目前同时刷新 **pricing + defaultModels + billingSources + responsesAPI**。其中:
- `billingSources` / `responsesAPI` 来自 config 的 `billing_source` / `responses_api` 字段——**本计划不删这两字段**,所以这两段刷新逻辑**必须保留**;
- `pricing` / `defaultModels` 来自 config 的 `models` 段——**本计划删**,这两段改成由 `LoadModelsFromStore` 负责。

因此 `ReloadPricing` 要做**拆分**(不做就热重载静默失效):
1. 保留 `billingSources` + `responsesAPI` 的刷新(仍读 config 的 `billing_source`/`responses_api`);
2. 删除 `pricing` + `defaultModels` 的刷新(不再读 `pcfg.ModelCosts`/`pcfg.Models`);
3. `server.go:1029` 的调用从 `s.manager.ReloadPricing(toManagerConfigForReload(...))` 改为:
   ```go
   s.manager.ReloadPricing(toManagerConfigForReload(newCfg, s.pools)) // 保留:刷 billingSource/responsesAPI
   _ = s.manager.LoadModelsFromStore(context.Background(), s.modelStoreAdapter) // 新增:刷 pricing/defaultModels(DB)
   ```
   (`s.modelStoreAdapter` 是 Task 6 Step 4 构造的适配器;需把它存为 Server 字段。)

> 这一步若漏掉,`Reload` 热重载会拿到空的 pricing(因为 `toManagerConfigForReload` 删掉 models 后,`ReloadPricing` 里的 `pcfg.ModelCosts` 也删了)→ 计费归零。**高风险,必须做。**

Run: `cd /home/hhhh/llm-gateway && make test`
Commit:
```bash
git commit -am "feat(provider): manager 改从 DB provider_models 读 models/pricing/defaultModels"
```

---

### Task 7: 删 config `models` 段 + 清理 `DefaultModels` 常量

**Files:**
- Modify: `backend/internal/config/config.go`(删 `Provider.Models` 字段 + `config.ProviderModel` struct)
- Modify: `backend/cmd/gateway/main.go`(`toManagerConfig`,删 Models/ModelCosts 投影)
- Modify: `backend/internal/server/server.go`(`toManagerConfigForReload`,删 Models/ModelCosts 投影)
- Modify: `backend/internal/provider/lookup.go`(`ProviderLookup` 接口加 `ModelsFor`)
- Modify: `backend/internal/provider/manager.go`(删 `ManagerProviderConfig.Models`/`ModelCosts` 字段、`LoadFromConfig`/`ReloadPricing` 里对它们的消费、加 `ModelsFor`)
- Modify: `backend/internal/provider/provider.go`(删 `Provider` 接口的 `Models()` 方法)
- Modify: 6 厂商包(`deepseek`/`glm`/`mimo`/`minimax`/`qwen`/`gemini` 的 `var DefaultModels` + `Models()` 实现)
- Modify: `backend/internal/router/router.go`(3 处 `p.Models()` → `manager.ModelsFor`)
- Modify: `backend/internal/api/http/handler/admin.go`(3 处 `p.Models()` → `manager.ModelsFor`)
- Modify: `config.yaml` / `config.example.yaml` / `config.docker.example.yaml`(删每个 `providers.<name>.models` 段)
- Test: 相关 test 同步删(见 Step 6)

**Interfaces:**
- Consumes: Task 6 的 DB 读路径 + `ModelsFor`(本 task Step 4)。
- Produces: config 不再有任何模型/价格字段;`Provider` 接口删 `Models()`;`ProviderLookup` 接口新增 `ModelsFor`;`ManagerProviderConfig` 删 `Models`/`ModelCosts`。

- [ ] **Step 1: 删 config struct 的 Models + ProviderModel**

`config.Provider` 里的 `Models []ProviderModel` 字段删除;`config.ProviderModel` struct 整个删除(它只被 `Provider.Models` 用,长上下文/cache/aliases/cost 全删)。搜 `grep -rn "config.ProviderModel\|\.Models\b" internal/ cmd/` 确认删除后无残留引用。

- [ ] **Step 2: 删 main.go / server.go 的投影**

`toManagerConfig`/`toManagerConfigForReload` 里 `models := ...; modelCosts := ...; Models: models; ModelCosts: modelCosts` 全部删除,并对 `ManagerProviderConfig` struct(manager.go)删掉 `Models []string` 与 `ModelCosts map[string]ModelCost` 两字段(它们不再有来源)。

- [ ] **Step 3: 删厂商包 `DefaultModels` 常量 + `Models()` 实现**

6 个厂商包里 `var DefaultModels = []string{...}` 常量整段删除,并删除该包 openai/anthropic 面的 `Models()` 方法实现。删除清单(逐文件):

| 文件 | `DefaultModels` | `Models()` 方法 |
|---|---|---|
| `deepseek/deepseek.go` | `:49` | `:88`(`*Provider`) |
| `deepseek/anthropic.go` | —(复用 deepseek.go 的) | `:69`(`*AnthropicProvider`) |
| `glm/glm.go` | `:46` | `:83`(`*Provider`) |
| `glm/anthropic.go` | — | `:62`(`*AnthropicProvider`) |
| `mimo/mimo.go` | `:63` | `:110`(`*Provider`) |
| `mimo/anthropic.go` | — | `:83`(`*AnthropicProvider`) |
| `minimax/minimax.go` | `:44` | `:80`(`*Provider`) |
| `minimax/openai.go` | — | `:74`(`*OpenAIProvider`) |
| `qwen/qwen.go` | `:25` | `:65`(`*Provider`) |
| `gemini/gemini.go` | `:33` | `:66`(`*Provider`) |

- [ ] **Step 4: 删 `Provider.Models()` 接口方法 + 迁移全部 7 个消费点**

删除 `provider.Provider` 接口里的 `Models() []string` 方法(`provider.go:182`),并在 `manager.go` 加 `ModelsFor(name)`(见下)。

**先把 `ModelsFor` 加进 `ProviderLookup` 接口**(`lookup.go`),否则 router 的 `r.manager`(类型 `provider.ProviderLookup`)和 proxy 的 `e.router.Manager()` 都无法调用它:

```go
// lookup.go 的 ProviderLookup 接口追加一行:
	// ModelsFor 返回某注册面按 vendor 归位后的模型 id 列表(见 Manager.ModelsFor)。
	ModelsFor(name string) []string
```

> ⚠️ 这个接口方法必须加,否则 `router.go` 三处的 `r.manager.ModelsFor(...)` 编译不过。proxy 侧没用 `ModelsFor`,但 `proxy_test` 里若 mock 了 `*Manager`,需确认 `Manager` 已实现该方法(它会实现,因为要满足 `ProviderLookup`)。

`ModelsFor` 实现(语义 = 按 vendor 读 DB 清单,各协议面共享同一份):

```go
// ModelsFor 返回某注册面的可用模型 id。
// 语义(方案 A):vendor 是模型归属的唯一维度 — 同一 vendor 下所有协议面
// (openai/anthropic/token-plan...)共享同一份模型清单,天然等价于"每个面自己的清单"
// (因为 DB 按 vendor 存、各面不单独声明)。经 VendorFor 归位后返回该 vendor 的清单。
// 未同步/无数据 → 空切片。排序确定(按 model 名字典序)。
func (m *Manager) ModelsFor(name string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.VendorFor(name)
	out := make([]string, 0)
	seen := map[string]bool{}
	for k := range m.pricing {
		if strings.HasPrefix(k, v+":") { // pricingKey = vendor:model
			model := strings.TrimPrefix(k, v+":")
			if !seen[model] {
				seen[model] = true
				out = append(out, model)
			}
		}
	}
	sort.Strings(out)
	return out
}
```

**关键约束(方案 A 的前提,必须写进 spec)**:同一 vendor 下所有协议面共享同一份模型清单。当前 config 里 minimax/mimo/deepseek/glm 各面本就声明同一批——迁移到 DB 按 vendor 存后,这一前提成立。若未来某厂商需要「openai 面和 anthropic 面支持不同模型」,需回到 per-注册面存储。**加一条断言测试**:构造 vendor=minimax 的 DB 行,断言 `ModelsFor("minimax")`、`ModelsFor("minimax-openai")` 返回同一清单。

**逐条替换全部 7 个消费点(一个不漏):**

| # | 文件:行 | 现状 | 改为 |
|---|---|---|---|
| 1 | `router.go:180`(热路径 `routeCatchAllAuto`) | `pickAllowedModel(p.Models(), o.AllowedModels)` | `pickAllowedModel(r.manager.ModelsFor(name), o.AllowedModels)` |
| 2 | `router.go:349`(`routeDirectModelWithOpts`) | `for _, m := range p.Models()` | `for _, m := range r.manager.ModelsFor(name)` |
| 3 | `router.go:407`(`filterCandidates`) | `pickAllowedModel(pv.Models(), o.AllowedModels)` | `pickAllowedModel(r.manager.ModelsFor(pv.Name()), o.AllowedModels)` |
| 4 | `admin.go:191`(`listRegisteredProviders`) | `models = p.Models()` | `models = a.Manager.ModelsFor(name)` |
| 5 | `admin.go:231`(`listProviders`) | `entry.Models = append(entry.Models, p.Models()...)` | `entry.Models = append(entry.Models, a.Manager.ModelsFor(name)...)` |
| 6 | `admin.go:283`(`getProvider`) | `"models": p.Models()` | `"models": a.Manager.ModelsFor(name)` |
| 7 | `manager.go:183`(`LoadFromConfig` 日志) | `zap.Int("models", len(p.Models()))` | 直接删除该日志字段(此处在 `m.mu` 锁内,`ModelsFor` 再取 RLock 会死锁)。改为 `zap.String("provider", name)` 不带 models 计数,或整行删除。 |

> ⚠️ **死锁警告**:`LoadFromConfig` 全程持有 `m.mu.Lock()`。第 7 处若直接调用 `m.ModelsFor(name)`(它取 `m.mu.RLock()`)会造成**自己锁自己 → 死锁**。正确做法:第 7 处**直接删除该日志字段**,或改为不取锁的本地变量(如 `len(pcfg.Models)` 在 config 删除前已无意义 → 直接删掉整行日志,或改为 `zap.String("provider", name)` 不带 models 计数)。

- [ ] **Step 5: 删三个 config 模板的 models 段**

`config.yaml`、`config.example.yaml`、`config.docker.example.yaml` 里每个 `providers.<name>.models:` 块(含 id/aliases/cost/long_context)整段删除。`config.yaml` 具体路径在 repo 根。

- [ ] **Step 6: 跑全量 + 修 test**

Run: `cd /home/hhhh/llm-gateway && make test && make vet && make build`
Expected: 全绿。删接口方法/字段导致编译错误的 test 范围，审计结果如下（逐个修）:

**必须改的 test(删 `Models()` 方法 / 删 `Models`/`ModelCosts` 字段 / 补 `ListModels`):**

> 注意:Task 3(加 `ListModels`)就会让下面**所有 5 个 fakeProvider 因缺 `ListModels` 方法而编译失败**(需补方法);Task 7(删 `Models()`)再进一步删它们的方法。两个 task 都要处理 fake。

**5 个各自独立的 `fakeProvider`(每包一份,补 `ListModels` + 删 `Models()`):**
- `internal/proxy/proxy_test.go:30-124` — `fakeProvider`(`Models()` @ :55);`Models: p.models` @ :139/:502,`Models: [...]` @ :1126-1127/:1284-1285
- `internal/router/router_test.go:16-33` — `fakeProvider`(`Models()` @ :24);`Models: p.Models()` @ :55/:91,`Models: [...]` @ :833-834
- `internal/server/server_test.go:28-42` — `fakeProvider`(`Models()` @ :33)
- `internal/api/http/handler/admin_test.go:21-38` — `fakeProvider`(`Models()` @ :29)
- `internal/provider/manager_test.go:15-32` — `fakeProviderForEndpoint`(`Models()` @ :23);`Models: []string{"m1"}` @ :48

**其它受影响 test:**
- `internal/provider/provider_test.go:40-106` — `TestComputeCost` 用 `CostPer1k*`/`LongContext*` 字段,Task 2 整个重写(见 Task 2 Step 4)。
- `cmd/gateway/integration_test.go:290` — `Models: []string{"deepseek-chat"}`(构造 `provider.ProviderConfig`,Task 7 删 `ProviderConfig.Models` 字段);`New` 工厂返回的 Provider 需补 `ListModels`。

**不受影响(不要误改):**
- `provider_test.go` 里 `ModelCost`/`ComputeCost` 已由 Task 2 重写,本 task 不动。
- `admin_test.go` 的 `ds.Models`(:141-142)是 dashboard 结构体字段,与本次无关。
- `quotacheck` 的 mock `fakeProviderLookup`(只实现 `EndpointFor`,不碰 `Models`/`ListModels`)——**不受影响**。

**注意:`ProviderLookup` 接口加了 `ModelsFor`,任何在 test 里手写 mock 实现 `ProviderLookup` 的地方都要补 `ModelsFor` 方法,否则编译失败。** `grep -rn "ProviderLookup\|var _.*=.*Manager\|ModelsFor" --include="*_test.go"` 定位。

- [ ] **Step 7: Commit**

```bash
git commit -am "refactor: 删 config models 段 + DefaultModels 常量,模型集改由 manager.ModelsFor 从 DB 读"
```

---

### Task 8: admin 端点 — list / sync / save pricing

**Files:**
- Modify: `backend/internal/api/http/handler/admin.go`(新 handler + NewAdmin 参数)
- Modify: `backend/internal/server/server.go`(装配注入)
- Test: `backend/internal/api/http/handler/admin_test.go`

**Interfaces:**
- Consumes: `provider.SyncVendorModels`(Task 5)、`database.ProviderModelStore`(Task 4)、`provider.Manager`。
- Produces: 三个 HTTP 端点供前端页(Task 9)。

- [ ] **Step 1: Admin 结构加依赖**

`Admin` struct 加:
```go
	ModelStore database.ProviderModelStore // 可 nil(模型管理不可用)
	ModelSync  func(ctx context.Context, vendor string) ([]string, error) // 由 server 注入封装好 store+manager
	ModelReload func() error // 同步/定价变更后热刷 manager 内存(可选 nil)
```

- [ ] **Step 2: 三个 handler**

```go
// GET /providers/models — 列出所有厂商模型。
func (a *Admin) listProviderModels(c *gin.Context) {
	if a.ModelStore == nil { c.JSON(503, gin.H{"error":"model_store_unavailable"}); return }
	rows, err := a.ModelStore.All(c.Request.Context())
	if err != nil { c.JSON(500, gin.H{"error":err.Error()}); return }
	// 按 vendor 分组返回
	group := map[string][]dbpkg.ProviderModel{}
	for _, r := range rows { group[r.Vendor] = append(group[r.Vendor], r) }
	c.JSON(200, gin.H{"vendors": group, "count": len(group)})
}

// POST /providers/sync-models {vendor}
func (a *Admin) syncProviderModels(c *gin.Context) {
	var body struct{ Vendor string `json:"vendor"` }
	if err := c.ShouldBindJSON(&body); err != nil || body.Vendor == "" {
		c.JSON(400, gin.H{"error":"vendor required"}); return
	}
	if a.ModelSync == nil { c.JSON(503, gin.H{"error":"sync_unavailable"}); return }
	ids, err := a.ModelSync(c.Request.Context(), body.Vendor)
	if err != nil { c.JSON(500, gin.H{"error":err.Error()}); return }
	if a.ModelReload != nil { _ = a.ModelReload() }
	c.JSON(200, gin.H{"vendor": body.Vendor, "synced_models": len(ids)})
}

// PUT /providers/models {vendor, model_id, cost_per_million_*}
func (a *Admin) saveProviderModelPricing(c *gin.Context) {
	var body struct {
		Vendor string `json:"vendor"`
		ModelID string `json:"model_id"`
		CostPerMillionInput float64 `json:"cost_per_million_input"`
		CostPerMillionCacheRead float64 `json:"cost_per_million_cache_read"`
		CostPerMillionOutput float64 `json:"cost_per_million_output"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Vendor == "" || body.ModelID == "" {
		c.JSON(400, gin.H{"error":"vendor and model_id required"}); return
	}
	if err := a.ModelStore.SavePricing(c.Request.Context(), body.Vendor, body.ModelID,
		body.CostPerMillionInput, body.CostPerMillionCacheRead, body.CostPerMillionOutput); err != nil {
		c.JSON(500, gin.H{"error":err.Error()}); return
	}
	if a.ModelReload != nil { _ = a.ModelReload() }
	c.JSON(200, gin.H{"ok": true})
}
```

在 `Register` 里加路由(`/providers/models` 必须在 `/providers/:name` **之前**注册,否则被 :name 吞):
```go
r.GET("/providers/models", a.listProviderModels)
r.POST("/providers/sync-models", a.syncProviderModels)
r.PUT("/providers/models", a.saveProviderModelPricing)
```

- [ ] **Step 3: NewAdmin 加参数 + server 装配**

`NewAdmin` 增加参数 `modelStore database.ProviderModelStore` 与 `modelSync func(...)` 与 `modelReload func()`,写入 Admin。server.go 里构造:
- `store := database.NewProviderModelStore(db)`
- `adapter := providerModelStoreAdapter{store}`(Task 6 定义)
- `modelSync := func(ctx, vendor) ([]string, error) { return provider.SyncVendorModels(ctx, s.manager, vendor, adapter) }`
- `modelReload := func() error { return s.manager.LoadModelsFromStore(ctx, adapter) }`

- [ ] **Step 4: 写测试 & 跑**

admin_test.go 加:`ModelStore=nil` → 503;list 返回分组;sync 走闭包返回 model 数;save 调 store。用 fake ModelStore 实现接口。

Run: `cd /home/hhhh/llm-gateway && make test && make vet && make build`
Commit:
```bash
git commit -am "feat(admin): provider 模型 list/sync/save-pricing 端点 + 装配"
```

---

### Task 9: 前端模型管理页 + 路由 + 菜单 + client.ts

**Files:**
- Create: `frontend/src/views/ModelManager.vue`
- Modify: `frontend/src/router/index.ts`
- Modify: `frontend/src/App.vue`
- Modify: `frontend/src/api/client.ts`

**Interfaces:**
- Consumes: `api.providers`(取 vendor 列表)、新加的 `api.models` 端点(Task 8)。
- Produces: `/models` 页面。

- [ ] **Step 1: client.ts 加类型 + API**

```ts
export interface ProviderModelRow {
  vendor: string
  model_id: string
  cost_per_million_input: number
  cost_per_million_cache_read: number
  cost_per_million_output: number
  synced_at: string | null
  source: string
}
export interface ProviderModelsResp {
  vendors: Record<string, ProviderModelRow[]>
  count: number
}
```

API 方法:
```ts
models: {
  list: () => client.get<ProviderModelsResp>('/providers/models').then(r => r.data),
  sync: (vendor: string) => client.post<{vendor:string; synced_models:number}>('/providers/sync-models', { vendor }).then(r => r.data),
  save: (body: {vendor:string; model_id:string; cost_per_million_input:number; cost_per_million_cache_read:number; cost_per_million_output:number}) => client.put('/providers/models', body).then(r => r.data),
},
```

- [ ] **Step 2: ModelManager.vue**

用 naive-ui:每个 vendor 一张 `n-card`,内含 `n-data-table`(列:模型 id / 输入价 / 缓存命中价 / 输出价 / 同步时间 / 未定价标记)+ 一个「同步」`n-button`。价格单元格用 `n-input-number` 可编辑,blur 时调 `api.models.save`。未定价(三档全 0)→ id 旁显示「未定价」tag,但行仍保留(表示可用)。顶部用 `api.providers` 或直接 `api.models.list` 驱动 vendor 列表。

- [ ] **Step 3: router + menu**

`router/index.ts` 加 `{ path: '/models', component: () => import('../views/ModelManager.vue') }`。`App.vue` 的 `menuOptions` 加 `{ key: '/models', label: renderMenuLabel('/models', '🧩 模型管理') }`,并给 `activeKey` 映射加 `/models`。

- [ ] **Step 4: type check + build**

Run: `cd /home/hhhh/llm-gateway/frontend && npx vue-tsc --noEmit && npm run build`
Expected: 类型通过;产物里出现 ModelManager chunk。

- [ ] **Step 5: Commit**

```bash
git add frontend/
git commit -m "feat(frontend): 模型管理页(上游同步 + 手工定价),Providers 页不动"
```

---

## 自检(写计划后)

**1. Spec 覆盖:**
- §4.1 DB 表 → Task 1 ✅
- §4.2 ModelCost/ComputeCost → Task 2 ✅
- §4.3 ListModels + 三协议面 → Task 3 ✅
- §4.4 路由从 DB 读 → Task 6 ✅
- §4.4.1 迁移顺序 → Task 6(读 DB)→ Task 7(删 config)顺序强制 ✅
- §4.5 admin 端点 → Task 8 ✅
- §4.6 前端页 → Task 9 ✅
- 删 config models + DefaultModels 常量 → Task 7 ✅

**2. Placeholder 扫描:** 无 TODO/TBD;每个 code 步骤有完整代码。

**3. 类型一致性:** `DBModelRow` / `ModelStore` / `ModelSyncStore` 在 Task 4/5/6/8 间复用一致;`ModelsFor` 在 Task 6/7 一致;`ProviderModelStore` 在 Task 4/8 一致。

**4. 迁移顺序硬约束:** Task 6 明确"空 DB → Warn 不崩";Task 7 依赖 Task 6 就绪。执行者不得把 Task 7 提前到 Task 6 之前。

**5. 潜在风险:** 删 `Provider.Models()` 接口方法会牵连 `admin.go:191/231/283`(已暴露的 listProviders 用 `p.Models()`)。这些也需在 Task 7 Step 4 一并替换为 `manager.ModelsFor(name)`。执行者务必 `grep -rn "\.Models()"` 全项目改干净,否则编译失败(编译期强制发现,符合第一原则)。
