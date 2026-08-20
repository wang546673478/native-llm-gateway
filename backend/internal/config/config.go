// Package config 负责加载和验证 Gateway 配置
// 对应规格书 4.1 config.yaml 完整规格
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 是 Gateway 的根配置
type Config struct {
	Server    ServerConfig        `mapstructure:"server"`
	Database  DatabaseConfig      `mapstructure:"database"`
	Redis     RedisConfig         `mapstructure:"redis"`
	Auth      AuthConfig          `mapstructure:"auth"`
	Providers map[string]Provider `mapstructure:"providers"`
	Routing   RoutingConfig       `mapstructure:"routing"`
	KeyPool   KeyPoolConfig       `mapstructure:"keypool"`
	Timeouts  TimeoutsConfig      `mapstructure:"timeouts"`
	Retry     RetryConfig         `mapstructure:"retry"`
	Logging   LoggingConfig       `mapstructure:"logging"`
	Metrics   MetricsConfig       `mapstructure:"metrics"`
	Usage     UsageConfig         `mapstructure:"usage"`
	// Fingerprint 设备指纹归一化配置(见 docs/fingerprint-sanitize-plan.md)。
	Fingerprint FingerprintConfig `mapstructure:"fingerprint"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// StaticDir 前端构建产物目录(方案 B:Go 进程直接托管静态文件,无 nginx)。
	// 为空 = 不启用(保持纯 API 网关行为);相对路径按进程 cwd 解析。
	// 未命中文件时 SPA fallback 到 index.html(vue-router history 模式)。
	StaticDir string `mapstructure:"static_dir"`
	// AccessLog 接入日志模块配置(§3.4 spec)
	AccessLog AccessLogConfig `mapstructure:"access_log"`
}

// AccessLogConfig 接入日志模块配置(§3.4 spec)
type AccessLogConfig struct {
	Enabled       bool          `mapstructure:"enabled"`
	BodyDir       string        `mapstructure:"body_dir"`
	BufferSize    int           `mapstructure:"buffer_size"`
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	Retention     time.Duration `mapstructure:"retention"`
}

// AccessLog 默认值(零值兜底)
// 公开常量,方便 Server.New 直接引用
const (
	DefaultAccessLogBodyDir       = "./data/access"
	DefaultAccessLogBufferSize    = 10000
	DefaultAccessLogBatchSize     = 100
	DefaultAccessLogFlushInterval = time.Second
	DefaultAccessLogRetention     = 24 * time.Hour

	// Usage 异步批写入默认值 — 消除 collector 里硬编码 100/10s 与 config 模板
	// 的"双份真相"(operator 改 config 只局部生效的孤岛隐患)。
	DefaultUsageBatchSize     = 100
	DefaultUsageFlushInterval = 10 * time.Second
)

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Driver          string        `mapstructure:"driver"`
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

// RedisConfig Redis 配置(可选)
type RedisConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

// AuthConfig 客户端认证配置
type AuthConfig struct {
	Enabled bool      `mapstructure:"enabled"`
	Keys    []AuthKey `mapstructure:"keys"`
}

// AuthKey 单个 Gateway 客户端 Key
type AuthKey struct {
	Name          string          `mapstructure:"name"`
	Key           string          `mapstructure:"key"`
	AllowedModels []string        `mapstructure:"allowed_models"`
	RateLimit     RateLimitConfig `mapstructure:"rate_limit"`
}

// RateLimitConfig 速率限制
type RateLimitConfig struct {
	RPM int `mapstructure:"rpm"`
	TPM int `mapstructure:"tpm"`
}

// Provider 单个 Provider 配置
type Provider struct {
	Enabled        bool              `mapstructure:"enabled"`
	Endpoint       string            `mapstructure:"endpoint"`
	Protocol       string            `mapstructure:"protocol"`
	Timeout        time.Duration     `mapstructure:"timeout"`
	Keys           []ProviderKey     `mapstructure:"keys"`
	CircuitBreaker CircuitBreakerCfg `mapstructure:"circuit_breaker"`
	// DefaultModel P-catch-all: catch_all 自动模式(catch_all: {})下,
	// 该 provider 用哪个模型承接未知模型名的请求。
	// 空 = 取 models 列表第一个声明。其他路由路径不涉及此字段
	DefaultModel string `mapstructure:"default_model"`
	// ResponsesAPI P-responses: 原生支持 OpenAI Responses API(/v1/responses)。
	// true 的 provider 才会收到 Codex 等客户端的 /responses 透传请求
	// (DeepSeek / MiniMax 官方支持;Qwen / Gemini 等不支持)
	ResponsesAPI bool `mapstructure:"responses_api"`
	// BillingSource 计费来源(P47)
	//   - "token_plan": 包月套餐(如 minimax token plan),优先路由,quota 用完自动 failover
	//   - "api":        按 token 计费(deepseek/openai/anthropic 等)— 默认
	//   - "free":       免费层(GLM-4-flash 等)
	// Gateway 不做 quota 跟踪,quota 由上游平台 UI 管理;这里只是标记
	// 用于 dashboard 区分"这个月 token_plan 用了多少 vs api 用了多少"
	BillingSource string `mapstructure:"billing_source"`
	// ForceThinkingDisabled P-deepseek-thinking: 上行前强制 thinking=disabled。
	// DeepSeek /anthropic 把 Claude Code 的 thinking:adaptive 当 enabled 处理,严格校验
	// 历史 thinking 块(compact 会剥离) → 400 "content[].thinking ... must be passed back"
	ForceThinkingDisabled bool `mapstructure:"force_thinking_disabled"`
	// QuotaCookie P-mimo-quota: MIMO 控制台登录 cookie(账号级,全量 Cookie header,
	// 约 1 天过期)。仅 mimo 厂商使用 — 其套餐/余额查询端点(未文档化)用 cookie
	// 鉴权而非 API key。启动时注入 balancer;空 = balancer 停用(错误码驱动)。
	// 敏感凭据:只放 gitignored 的本地 config.yaml,不放 config.example.yaml
	QuotaCookie string `mapstructure:"quota_cookie"`
}

// ProviderKey Provider 的 API Key
type ProviderKey struct {
	Name string `mapstructure:"name"`
	Key  string `mapstructure:"key"`
}

// CircuitBreakerCfg Circuit Breaker 配置
type CircuitBreakerCfg struct {
	FailureThreshold int           `mapstructure:"failure_threshold"`
	FailureWindow    time.Duration `mapstructure:"failure_window"`
	OpenTimeout      time.Duration `mapstructure:"open_timeout"`
	HalfOpenRequests int           `mapstructure:"half_open_requests"`
	CountableErrors  []string      `mapstructure:"countable_errors"`
	ExcludedErrors   []string      `mapstructure:"excluded_errors"`
}

// RoutingConfig 路由配置
type RoutingConfig struct {
	Aliases map[string]AliasRule `mapstructure:"aliases"`
	// P39: 共享 provider chain 定义。一个 chain 是一个有序的 (provider, model) 列表,
	// alias 可以用 chain_ref 引用它,这样多个 alias 共享同一条 fallback 链,
	// 加新 fallback 时只需要改 chains 里的一处。
	Chains          map[string][]AliasRoute `mapstructure:"chains"`
	DefaultStrategy string                  `mapstructure:"default_strategy"`
	// P-catch-all: 兜底路由 — 客户端发任何 alias 表外且无 provider 声明的 model 名
	// (如 gpt-5 / 任意新探测名)时,按这条规则路由。任意 agent 任意模型名都能用,
	// 仍按 tier 计费(token_plan → api → free)。结构同 alias(长格式 providers /
	// 短格式 target_model);nil = 不兜底
	CatchAll *AliasRule `mapstructure:"catch_all"`
}

// AliasRoute 单条路由目标
type AliasRoute struct {
	Name     string `mapstructure:"name"`
	Model    string `mapstructure:"model"`
	Priority int    `mapstructure:"priority"`
	Weight   int    `mapstructure:"weight"`
}

// AliasRule 别名路由规则
// P53: 加 TargetModel 字段,支持短格式 auto-discovery
// 长格式仍然支持(显式 providers 或 chain_ref)
type AliasRule struct {
	Strategy  string       `mapstructure:"strategy"`
	Providers []AliasRoute `mapstructure:"providers"`
	// P39: ChainRef 引用 routing.chains.<name> 里的 provider 列表。
	ChainRef string `mapstructure:"chain_ref"`
	// P53: TargetModel 短格式 — alias 直接指向一个目标 model id,
	// gateway 自动找所有声明该 model 的 provider(按 tier 排序)
	// 与 Providers/ChainRef 互斥,优先使用 TargetModel
	TargetModel string `mapstructure:"target_model"`
}

// KeyPoolConfig Key 池配置
type KeyPoolConfig struct {
	CoolingDuration     time.Duration `mapstructure:"cooling_duration"`
	HealthCheckInterval time.Duration `mapstructure:"health_check_interval"`
	KeyRotation         string        `mapstructure:"key_rotation"`

	// P68: quota restore 配置
	QuotaEnabled           bool          `mapstructure:"quota_enabled"`
	QuotaProbeInitialDelay time.Duration `mapstructure:"quota_probe_initial_delay"`
	QuotaProbeMaxBackoff   time.Duration `mapstructure:"quota_probe_max_backoff"`
	QuotaProbeJitterPct    int           `mapstructure:"quota_probe_jitter_pct"`
	QuotaPollInterval      time.Duration `mapstructure:"quota_poll_interval"`
	QuotaWarnThresholdPct  int           `mapstructure:"quota_warn_threshold_pct"`
	QuotaPollJitterPct     int           `mapstructure:"quota_poll_jitter_pct"`
	QuotaHTTPTimeout       time.Duration `mapstructure:"quota_http_timeout"`
	QuotaUserAgent         string        `mapstructure:"quota_user_agent"`
}

// TimeoutsConfig 超时配置
// P-配置孤岛:server_read/server_write/server_idle/request_total 四个字段曾零消费
// —— HTTP server 真实超时由 Server.ReadTimeout/WriteTimeout/IdleTimeout(mapstructure
// server.read_timeout 等)生效,这里是 falese affordance,已删。仅保留真正读取的
// ProviderDefault(provider.timeout==0 时兜底)。
type TimeoutsConfig struct {
	ProviderDefault time.Duration `mapstructure:"provider_default"`
}

// RetryConfig 重试配置
type RetryConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	MaxAttempts  int      `mapstructure:"max_attempts"`
	NoFailoverOn []string `mapstructure:"no_failover_on"`
	FailoverOn   []string `mapstructure:"failover_on"`
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Level    string `mapstructure:"level"`
	Format   string `mapstructure:"format"`
	Output   string `mapstructure:"output"`
	FilePath string `mapstructure:"file_path"`
}

// MetricsConfig 指标配置
type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
	Port    int    `mapstructure:"port"`
}

// UsageConfig 用量配置
type UsageConfig struct {
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	BatchSize     int           `mapstructure:"batch_size"`
	RetentionDays int           `mapstructure:"retention_days"`
}

// FingerprintConfig 设备指纹归一化配置。
// 把发往上游前 body 里的设备级指纹(device_id / platform / shell / os version)
// 归一成 Gateway 一套固定值,抹平多机器共用一把上游 key 的「多头」信号(封号风险)。
type FingerprintConfig struct {
	// Enabled 是否归一化。nil = 默认开(true);显式 false 关闭。
	// 用 *bool 区分「未配置」和「显式关闭」,因为 viper 解 bool 未配时给 false。
	Enabled *bool `mapstructure:"enabled"`
	// CanonicalDeviceID 统一 device_id。空则启动时随机生成一次(存内存,不落盘)。
	CanonicalDeviceID string `mapstructure:"canonical_device_id"`
}

// FingerprintEnabled 归一化的最终开关;nil 视为默认开。
func (c *FingerprintConfig) FingerprintEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Load 从指定路径加载配置文件
func Load(path string) (*Config, error) {
	// P53: 读文件 → 短格式 alias 转长格式 → 喂给 viper
	// 因为 viper 的 yaml 解码不支持自定义 UnmarshalYAML
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	normalized := normalizeShortAliasesInYAML(string(raw))

	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(normalized)); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// P-catch-all: `catch_all: {}`(空规则 = 自动模式)在 viper 里会解成 nil,
	// 但「配置了 catch_all」本身是有效语义 — 空规则 = 所有 provider 自动参与。
	// 显式补一个空 AliasRule,与「没写 catch_all」(nil = 不兜底)区分开
	if v.IsSet("routing.catch_all") && cfg.Routing.CatchAll == nil {
		cfg.Routing.CatchAll = &AliasRule{}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// normalizeShortAliasesInYAML P53: 把 "alias: model_id" 短格式转成
// "alias:\n  target_model: model_id" 长格式
//
// 只处理 aliases: 块的第一层直接子项,嵌套结构(strategy/providers)保持原样
// 注释和空行也保持原样
func normalizeShortAliasesInYAML(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) != "aliases:" {
			out = append(out, line)
			i++
			continue
		}
		aliasesIndent := len(line) - len(strings.TrimLeft(line, " "))
		out = append(out, line)
		i++
		childIndent := aliasesIndent + 2
		for i < len(lines) {
			sub := lines[i]
			trimmed := strings.TrimSpace(sub)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				out = append(out, sub)
				i++
				continue
			}
			leading := len(sub) - len(strings.TrimLeft(sub, " "))
			if leading <= aliasesIndent {
				break
			}
			if leading > childIndent {
				out = append(out, sub)
				i++
				continue
			}
			colonIdx := strings.Index(sub, ":")
			if colonIdx < 0 {
				out = append(out, sub)
				i++
				continue
			}
			rest := strings.TrimSpace(sub[colonIdx+1:])
			if rest == "" {
				out = append(out, sub)
				i++
				for i < len(lines) {
					inner := lines[i]
					innerTrimmed := strings.TrimSpace(inner)
					innerLeading := len(inner) - len(strings.TrimLeft(inner, " "))
					if innerTrimmed != "" && innerLeading <= childIndent {
						break
					}
					out = append(out, inner)
					i++
				}
				continue
			}
			firstChar := rest[0]
			if firstChar == '{' || firstChar == '[' {
				out = append(out, sub)
				i++
				continue
			}
			value := strings.Trim(rest, `"'`)
			prefixSpaces := strings.Repeat(" ", leading)
			out = append(out, prefixSpaces+sub[:])
			out = append(out, prefixSpaces+"  target_model: "+fmt.Sprintf("%q", value))
			i++
		}
	}
	return strings.Join(out, "\n")
}

// validate 校验配置完整性,失败立即报错
func (c *Config) validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be in (0, 65535], got %d", c.Server.Port)
	}
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		return fmt.Errorf("database.driver must be sqlite or postgres, got %q", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	for name, p := range c.Providers {
		if !p.Enabled {
			continue
		}
		if p.Endpoint == "" {
			return fmt.Errorf("provider %s: endpoint is required", name)
		}
		proto := strings.ToLower(p.Protocol)
		if proto != "openai" && proto != "anthropic" && proto != "google" {
			return fmt.Errorf("provider %s: protocol must be openai/anthropic/google, got %q", name, p.Protocol)
		}
		// P30: keys 段可选 — Provider Key 从 DB (provider_api_keys) 读
		// 保留 len(p.Keys) == 0 作为允许(没填 keys 段不报错)
		_ = p.Keys // 显式忽略未用
	}
	return nil
}
