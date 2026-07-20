// Package config 负责加载和验证 Gateway 配置
// 对应规格书 4.1 config.yaml 完整规格
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 是 Gateway 的根配置
type Config struct {
	Server    ServerConfig         `mapstructure:"server"`
	Database  DatabaseConfig       `mapstructure:"database"`
	Redis     RedisConfig          `mapstructure:"redis"`
	Auth      AuthConfig           `mapstructure:"auth"`
	Providers map[string]Provider  `mapstructure:"providers"`
	Routing   RoutingConfig        `mapstructure:"routing"`
	KeyPool   KeyPoolConfig        `mapstructure:"keypool"`
	Timeouts  TimeoutsConfig       `mapstructure:"timeouts"`
	Retry     RetryConfig          `mapstructure:"retry"`
	Logging   LoggingConfig        `mapstructure:"logging"`
	Metrics   MetricsConfig        `mapstructure:"metrics"`
	Usage     UsageConfig          `mapstructure:"usage"`
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

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
	Enabled bool         `mapstructure:"enabled"`
	Keys    []AuthKey    `mapstructure:"keys"`
}

// AuthKey 单个 Gateway 客户端 Key
type AuthKey struct {
	Name          string         `mapstructure:"name"`
	Key           string         `mapstructure:"key"`
	AllowedModels []string       `mapstructure:"allowed_models"`
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
	Models         []ProviderModel   `mapstructure:"models"`
	Keys           []ProviderKey     `mapstructure:"keys"`
	CircuitBreaker CircuitBreakerCfg `mapstructure:"circuit_breaker"`
}

// ProviderModel Provider 模型声明
type ProviderModel struct {
	ID               string   `mapstructure:"id"`
	Aliases          []string `mapstructure:"aliases"`
	CostPer1kInput   float64  `mapstructure:"cost_per_1k_input"`
	CostPer1kOutput  float64  `mapstructure:"cost_per_1k_output"`
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
	Aliases         map[string]AliasRule `mapstructure:"aliases"`
	DefaultStrategy string               `mapstructure:"default_strategy"`
}

// AliasRoute 单条路由目标
type AliasRoute struct {
	Name     string `mapstructure:"name"`
	Model    string `mapstructure:"model"`
	Priority int    `mapstructure:"priority"`
	Weight   int    `mapstructure:"weight"`
}

// AliasRule 别名路由规则
type AliasRule struct {
	Strategy  string       `mapstructure:"strategy"`
	Providers []AliasRoute `mapstructure:"providers"`
}

// KeyPoolConfig Key 池配置
type KeyPoolConfig struct {
	CoolingDuration    time.Duration `mapstructure:"cooling_duration"`
	MaxCoolingCount    int           `mapstructure:"max_cooling_count"`
	HealthCheckInterval time.Duration `mapstructure:"health_check_interval"`
	KeyRotation        string        `mapstructure:"key_rotation"`
}

// TimeoutsConfig 超时配置
type TimeoutsConfig struct {
	ServerRead      time.Duration `mapstructure:"server_read"`
	ServerWrite     time.Duration `mapstructure:"server_write"`
	ServerIdle      time.Duration `mapstructure:"server_idle"`
	ProviderDefault time.Duration `mapstructure:"provider_default"`
	RequestTotal    time.Duration `mapstructure:"request_total"`
}

// RetryConfig 重试配置
type RetryConfig struct {
	Enabled       bool     `mapstructure:"enabled"`
	MaxAttempts   int      `mapstructure:"max_attempts"`
	NoFailoverOn  []string `mapstructure:"no_failover_on"`
	FailoverOn    []string `mapstructure:"failover_on"`
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

// Load 从指定路径加载配置文件
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
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
		if len(p.Keys) == 0 {
			return fmt.Errorf("provider %s: at least one key is required", name)
		}
	}
	return nil
}
