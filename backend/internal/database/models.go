// Package database 负责数据库连接初始化和 GORM 模型定义
// 对应规格书第七部分(migration 001-005)的表结构
package database

import "time"

// Provider Provider 主表
type Provider struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Name      string    `gorm:"column:name;uniqueIndex;not null" json:"name"`
	Protocol  string    `gorm:"column:protocol;not null" json:"protocol"`
	Endpoint  string    `gorm:"column:endpoint;not null" json:"endpoint"`
	Enabled   bool      `gorm:"column:enabled;not null;default:true" json:"enabled"`
	Timeout   int       `gorm:"column:timeout_seconds;not null;default:60" json:"timeout_seconds"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Models []ProviderModel `gorm:"foreignKey:ProviderName;references:Name" json:"models,omitempty"`
}

// TableName 显式指定表名
func (Provider) TableName() string { return "providers" }

// ProviderModel Provider 的模型声明
type ProviderModel struct {
	ID                     uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	ProviderName           string    `gorm:"column:provider_name;uniqueIndex:idx_provider_model;not null" json:"provider_name"`
	ModelID                string    `gorm:"column:model_id;uniqueIndex:idx_provider_model;not null" json:"model_id"`
	CostPer1kInput         float64   `gorm:"column:cost_per_1k_input;not null;default:0" json:"cost_per_1k_input"`
	CostPer1kOutput        float64   `gorm:"column:cost_per_1k_output;not null;default:0" json:"cost_per_1k_output"`
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
type ProviderAPIKey struct {
	ID            uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	ProviderName  string     `gorm:"column:provider_name;index;not null" json:"provider_name"`
	Name          string     `gorm:"column:name;not null" json:"name"`
	// KeyHash 存明文(P30 暂不上加密,跟 GatewayKey 一样,生产可加)
	KeyHash       string     `gorm:"column:key_hash;not null" json:"-"`
	Enabled       bool       `gorm:"column:enabled;not null;default:true" json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// TableName
func (ProviderAPIKey) TableName() string { return "provider_api_keys" }

// UsageRecord 单次请求的用量记录(P8 阶段真正写入)
type UsageRecord struct {
	ID            uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraceID       string    `gorm:"column:trace_id;index;not null" json:"trace_id"`
	GatewayKeyID  string    `gorm:"column:gateway_key_id;index" json:"gateway_key_id"`
	ProviderName  string    `gorm:"column:provider_name;index;not null" json:"provider_name"`
	ModelID       string    `gorm:"column:model_id;index;not null" json:"model_id"`
	Protocol      string    `gorm:"column:protocol;not null" json:"protocol"`
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
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Alias     string    `gorm:"column:alias;uniqueIndex;not null" json:"alias"`
	Strategy  string    `gorm:"column:strategy;not null;default:'priority'" json:"strategy"`
	ConfigJSON string   `gorm:"column:config_json;not null" json:"config_json"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName
func (RoutingConfig) TableName() string { return "routing_configs" }

// GatewayKey 客户端使用的 Gateway API Key(P7 阶段真正生效)
type GatewayKey struct {
	ID            uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Name          string    `gorm:"column:name;uniqueIndex;not null" json:"name"`
	KeyHash       string    `gorm:"column:key_hash;uniqueIndex;not null" json:"-"`
	// Providers 绑定:JSON 数组,空 = 不限制(可用于任意 Provider)
	// 非空 = 只能用路由解析到这些 Provider 之一的请求
	// 例:"[\"deepseek\",\"deepseek-anthropic\"]" 表示 deepseek 的 OpenAI 和
	// Anthropic 兼容端点都能用(用同一个 API key)
	Providers     string    `gorm:"column:providers;default:'[]'" json:"providers"`
	// P34: ProviderKeyIDs 绑定:JSON 数组存 ProviderAPIKey.ID(uint)
	// 空 = 不限制(用该 provider 的所有 key 池)
	// 非空 = 只能用这些 ID 对应的 provider key 调上游
	// 例:"[5,7]" 表示只能从 minimax provider_api_keys 表 ID=5 和 ID=7 的 key 池里挑
	ProviderKeyIDs string    `gorm:"column:provider_key_ids;default:'[]'" json:"provider_key_ids"`
	AllowedModels string    `gorm:"column:allowed_models;not null;default:'[\"*\"]'" json:"allowed_models"`
	RPM           int       `gorm:"column:rpm;not null;default:100" json:"rpm"`
	TPM           int       `gorm:"column:tpm;not null;default:500000" json:"tpm"`
	Enabled       bool      `gorm:"column:enabled;not null;default:true" json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TableName
func (GatewayKey) TableName() string { return "gateway_keys" }
