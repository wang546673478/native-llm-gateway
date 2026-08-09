// Package database 负责数据库连接初始化和 GORM 模型定义
// 对应规格书第七部分(migration 001-005)的表结构
package database

import "time"

// Provider Provider 主表
type Provider struct {
	ID       uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Name     string `gorm:"column:name;uniqueIndex;not null" json:"name"`
	Protocol string `gorm:"column:protocol;not null" json:"protocol"`
	Endpoint string `gorm:"column:endpoint;not null" json:"endpoint"`
	Enabled  bool   `gorm:"column:enabled;not null;default:true" json:"enabled"`
	Timeout  int    `gorm:"column:timeout_seconds;not null;default:60" json:"timeout_seconds"`
	// P47: 计费来源 — token_plan / api / free
	BillingSource string    `gorm:"column:billing_source;not null;default:'api'" json:"billing_source"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Models []ProviderModel `gorm:"foreignKey:ProviderName;references:Name" json:"models,omitempty"`
}

// TableName 显式指定表名
func (Provider) TableName() string { return "providers" }

// ProviderModel Provider 的模型声明
type ProviderModel struct {
	ID              uint    `gorm:"primaryKey;autoIncrement" json:"id"`
	ProviderName    string  `gorm:"column:provider_name;uniqueIndex:idx_provider_model;not null" json:"provider_name"`
	ModelID         string  `gorm:"column:model_id;uniqueIndex:idx_provider_model;not null" json:"model_id"`
	CostPer1kInput  float64 `gorm:"column:cost_per_1k_input;not null;default:0" json:"cost_per_1k_input"`
	CostPer1kOutput float64 `gorm:"column:cost_per_1k_output;not null;default:0" json:"cost_per_1k_output"`
	// P40: cache pricing 字段 — GORM AutoMigrate 会自动加列
	CostPer1kCacheRead     float64   `gorm:"column:cost_per_1k_cache_read;not null;default:0" json:"cost_per_1k_cache_read"`
	CostPer1kCacheCreation float64   `gorm:"column:cost_per_1k_cache_creation;not null;default:0" json:"cost_per_1k_cache_creation"`
	CreatedAt              time.Time `json:"created_at"`
}

// TableName
func (ProviderModel) TableName() string { return "provider_models" }

// ModelAlias 别名路由(可由多个 Provider 的多个 model 映射到同一个别名)
type ModelAlias struct {
	ID           uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Alias        string    `gorm:"column:alias;uniqueIndex:idx_alias_target;not null" json:"alias"`
	ProviderName string    `gorm:"column:provider_name;uniqueIndex:idx_alias_target;not null" json:"provider_name"`
	ModelID      string    `gorm:"column:model_id;uniqueIndex:idx_alias_target;not null" json:"model_id"`
	Priority     int       `gorm:"column:priority;not null;default:0" json:"priority"`
	Weight       int       `gorm:"column:weight;not null;default:0" json:"weight"`
	CreatedAt    time.Time `json:"created_at"`
}

// TableName
func (ModelAlias) TableName() string { return "model_aliases" }

// ProviderAPIKey(P30)每个 Provider 的上游 LLM API key
// 替代之前 config.yaml 里的 providers.x.keys[] 段
// Gateway 调上游时由 Authenticator 从这里构建 KeyPool 取 key
// P48: 每把 key 独立标注 billing_source — 支持同 provider 同时有 token_plan + api key
type ProviderAPIKey struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`
	// ProviderName+Name 复合唯一 — 消除重复 key 可被插入的 DB 不变量缺失
	// (migration 002 曾声明 UNIQUE(provider_name,name),AutoMigrate 此前漏了)。
	// 同 provider 内 key 名唯一,避免重复行导致调度歧义。
	ProviderName string `gorm:"column:provider_name;not null;uniqueIndex:idx_provider_key_name" json:"provider_name"`
	Name         string `gorm:"column:name;not null;uniqueIndex:idx_provider_key_name" json:"name"`
	// KeyHash 存明文(P30 暂不上加密,跟 GatewayKey 一样,生产可加)
	KeyHash string `gorm:"column:key_hash;not null" json:"-"`
	Enabled bool   `gorm:"column:enabled;not null;default:true" json:"enabled"`
	// P48: 单 key 的计费来源(token_plan / api / free)
	// 默认 api(向后兼容);创建时如果不指定,可用 provider 的 billing_source 作默认值
	BillingSource string `gorm:"column:billing_source;default:'api'" json:"billing_source"`
	// P-provider-vendor: key 可用的协议列表,逗号分隔("openai,anthropic");空 = 全部协议
	// 同一把 key 物理上两个协议端点都能用,protocols 只是用户限制语义
	Protocols string    `gorm:"column:protocols;default:''" json:"protocols"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName
func (ProviderAPIKey) TableName() string { return "provider_api_keys" }

// UsageRecord 单次请求的用量记录(P8 阶段真正写入)
type UsageRecord struct {
	ID           uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	TraceID      string `gorm:"column:trace_id;index;not null" json:"trace_id"`
	GatewayKeyID string `gorm:"column:gateway_key_id;index" json:"gateway_key_id"`
	ProviderName string `gorm:"column:provider_name;index;not null" json:"provider_name"`
	ModelID      string `gorm:"column:model_id;index;not null" json:"model_id"`
	Protocol     string `gorm:"column:protocol;not null" json:"protocol"`
	// P47: 冗余存 billing_source — 方便按 token_plan / api / free 聚合统计
	// 取自请求时刻该 provider 的 billing_source,改 config 不会影响历史记录
	BillingSource string    `gorm:"column:billing_source;index;default:'api'" json:"billing_source"`
	InputTokens   int       `gorm:"column:input_tokens;not null;default:0" json:"input_tokens"`
	OutputTokens  int       `gorm:"column:output_tokens;not null;default:0" json:"output_tokens"`
	TotalTokens   int       `gorm:"column:total_tokens;not null;default:0" json:"total_tokens"`
	Cost          float64   `gorm:"column:cost;not null;default:0" json:"cost"`
	LatencyMs     int       `gorm:"column:latency_ms;not null;default:0" json:"latency_ms"`
	IsStream      bool      `gorm:"column:is_stream;not null;default:false" json:"is_stream"`
	StatusCode    int       `gorm:"column:status_code" json:"status_code"`
	ErrorType     string    `gorm:"column:error_type" json:"error_type"`
	CreatedAt     time.Time `gorm:"index;column:created_at" json:"created_at"`
}

// TableName
func (UsageRecord) TableName() string { return "usage_records" }

// RoutingConfig 路由配置(JSON 存储,P4 阶段使用)
type RoutingConfig struct {
	ID         uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Alias      string    `gorm:"column:alias;uniqueIndex;not null" json:"alias"`
	Strategy   string    `gorm:"column:strategy;not null;default:'priority'" json:"strategy"`
	ConfigJSON string    `gorm:"column:config_json;not null" json:"config_json"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TableName
func (RoutingConfig) TableName() string { return "routing_configs" }

// GatewayKey 客户端使用的 Gateway API Key(P7 阶段真正生效)
type GatewayKey struct {
	ID      uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Name    string `gorm:"column:name;uniqueIndex;not null" json:"name"`
	KeyHash string `gorm:"column:key_hash;uniqueIndex;not null" json:"-"`
	// Providers 绑定:JSON 数组,空 = 不限制(可用于任意 Provider)
	// 非空 = 只能用路由解析到这些 Provider 之一的请求
	// 例:"[\"deepseek\",\"deepseek-anthropic\"]" 表示 deepseek 的 OpenAI 和
	// Anthropic 兼容端点都能用(用同一个 API key)
	Providers string `gorm:"column:providers;default:'[]'" json:"providers"`
	// P34: ProviderKeyIDs 绑定:JSON 数组存 ProviderAPIKey.ID(uint)
	// 空 = 不限制(用该 provider 的所有 key 池)
	// 非空 = 只能用这些 ID 对应的 provider key 调上游
	// 例:"[5,7]" 表示只能从 minimax provider_api_keys 表 ID=5 和 ID=7 的 key 池里挑
	ProviderKeyIDs string `gorm:"column:provider_key_ids;default:'[]'" json:"provider_key_ids"`
	AllowedModels  string `gorm:"column:allowed_models;not null;default:'[\"*\"]'" json:"allowed_models"`
	// DefaultModel: 客户端发 Gateway 没见过的 model 名(claude-sonnet-4-5 / gpt-4o 等
	// Claude Code / CodeX 的探测名)时,fallback 到这个 model。
	// 空字符串 = 不 fallback,严格返回 503。
	// 也必须经过 AllowedModels 白名单 — 防止用 fallback 绕过白名单。
	DefaultModel string    `gorm:"column:default_model;default:''" json:"default_model"`
	RPM          int       `gorm:"column:rpm;not null;default:100" json:"rpm"`
	TPM          int       `gorm:"column:tpm;not null;default:500000" json:"tpm"`
	Enabled      bool      `gorm:"column:enabled;not null;default:true" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName
func (GatewayKey) TableName() string { return "gateway_keys" }

// AccessLog 每次客户端请求的接入日志(P67: 新增 — 给管理员调试用)
//
// 设计要点:
//   - 只存 metadata;body 落地文件(.jsonl 滚动),DB 不存以防 SQLite 单行过大
//   - trace_id 与 X-Request-Id 一致,跨 usage_records / access_logs 可 join
//   - gateway_key_name 是冗余字段,UI 不用 join 也能展示;auth 关掉时为空
//   - body_path 是相对 body_dir 的路径,不存绝对路径(避免重启后指向失效位置)
type AccessLog struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraceID   string    `gorm:"column:trace_id;index;not null" json:"trace_id"`
	CreatedAt time.Time `gorm:"index;column:created_at" json:"created_at"`
	// GatewayKeyID 唯一身份(落库)。GatewayKeyName 不落库 — 查询时按 ID 现查
	// gateway_keys 当前名字(改名即时生效,不存快照;与 ProviderKeyID 同策略)
	GatewayKeyID   string `gorm:"column:gateway_key_id;index" json:"gateway_key_id"`
	Method         string `gorm:"column:method" json:"method"`
	Path           string `gorm:"column:path" json:"path"`
	ClientIP       string `gorm:"column:client_ip" json:"client_ip"`
	UserAgent      string `gorm:"column:user_agent" json:"user_agent"`
	RequestedModel string `gorm:"column:requested_model;index" json:"requested_model"`
	FinalModel     string `gorm:"column:final_model;index" json:"final_model"`
	ProviderName   string `gorm:"column:provider_name;index" json:"provider_name"`
	ProviderKeyID  string `gorm:"column:provider_key_id" json:"provider_key_id"`
	// ProviderKeyName 不落库 — 查询时按 provider_key_id 现查 provider_api_keys
	// 当前名字(改名即时生效,不存快照)
	Protocol     string `gorm:"column:protocol" json:"protocol"`
	IsStream     bool   `gorm:"column:is_stream" json:"is_stream"`
	StatusCode   int    `gorm:"column:status_code;index" json:"status_code"`
	ErrorType    string `gorm:"column:error_type;index" json:"error_type"`
	LatencyMs    int    `gorm:"column:latency_ms" json:"latency_ms"`
	ReqBodyPath  string `gorm:"column:req_body_path" json:"req_body_path"`
	ReqBodySize  int    `gorm:"column:req_body_size" json:"req_body_size"`
	RespBodyPath string `gorm:"column:resp_body_path" json:"resp_body_path"`
	RespBodySize int    `gorm:"column:resp_body_size" json:"resp_body_size"`
	// 注意:truncated 信息放在文件后缀 `.truncated.json`,不存 DB 列(spec §1.1 锁定 21 字段;
	// 2026-08-07 +ProviderKeyID 第 22 列 — 排查「请求实际用哪把 key」;名字不落库,
	// 查询时按 ID 现查 provider_api_keys(改名即时生效,不存快照)
}

// TableName
func (AccessLog) TableName() string { return "access_logs" }

// MimoQuotaCookie P-mimo-quota: MIMO 控制台登录 cookie 持久化(单行表,ID 恒为 1)。
// 敏感凭据 — 不在 API 返回明文,只在注入/更新时写入。
type MimoQuotaCookie struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Cookie    string    `gorm:"column:cookie;size:8192" json:"-"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName
func (MimoQuotaCookie) TableName() string { return "mimo_quota_cookie" }

// RouteOrder 层级排序改写(2026-08-10,Level 2/3)。
// 只存「用户改写」的排序;默认排序(未改写)由 created_at 派生,route_order 无行 = 零代价。
// Scope=provider → Provider 是 provider 名,Name 空,Seq 是层内 provider 位次;
// Scope=key       → Provider 是所属 provider 名,Name 是 key 名,Seq 是 provider 内 key 位次。
// BillingSource 可选:按层隔离顺序(token_plan/api 各自一段)。
type RouteOrder struct {
	ID            uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Scope         string    `gorm:"column:scope;not null" json:"scope"` // "provider" | "key"
	Provider      string    `gorm:"column:provider;not null" json:"provider"`
	Name          string    `gorm:"column:name;not null;default:''" json:"name"`
	BillingSource string    `gorm:"column:billing_source;not null;default:''" json:"billing_source"`
	Seq           int       `gorm:"column:seq;not null;default:0" json:"seq"`
	UpdatedAt     time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName
func (RouteOrder) TableName() string { return "route_order" }
