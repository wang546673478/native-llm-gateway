// Package relay 实现通用中转站 Provider
// 中转站 = 纯透传代理,无需编写代码,只需配置 URL + 协议
package relay

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

// Config 通用中转站配置
type Config struct {
	Name               string
	BaseURL            string
	ProtocolMode       string // "single" | "multi"
	PrimaryProtocol    provider.Protocol
	SupportedProtocols []provider.Protocol
	Timeout            int // seconds
	ProtocolConfigs    map[provider.Protocol]interface{}
}

// GenericRelayProvider 通用中转站 Provider
// 根据协议动态路由到对应的协议实现(透传)
type GenericRelayProvider struct {
	name               string
	protocolMode       string // "single" | "multi"
	primaryProtocol    provider.Protocol
	implementations    map[provider.Protocol]provider.Provider
	pool               *keypool.Pool
}

// NewGenericRelayProvider 创建通用中转站 Provider
func NewGenericRelayProvider(cfg Config) (*GenericRelayProvider, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("relay station name is required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("relay station base_url is required")
	}
	if cfg.PrimaryProtocol == "" {
		return nil, fmt.Errorf("relay station primary_protocol is required")
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	implementations := make(map[provider.Protocol]provider.Provider)

	// 为每个支持的协议创建实现
	protocols := cfg.SupportedProtocols
	if len(protocols) == 0 {
		protocols = []provider.Protocol{cfg.PrimaryProtocol}
	}

	for _, proto := range protocols {
		var impl provider.Provider
		var err error

		switch proto {
		case provider.ProtocolOpenAI:
			base := openai_compatible.NewBase(openai_compatible.Config{
				Name:        cfg.Name,
				Endpoint:    cfg.BaseURL,
				Timeout:     timeout,
				StreamUsage: true,
			})
			impl = &RelayOpenAIProvider{Base: base, name: cfg.Name}
		case provider.ProtocolAnthropic:
			base := anthropic_compatible.NewBase(anthropic_compatible.Config{
				Name:     cfg.Name,
				Endpoint: cfg.BaseURL,
				Timeout:  timeout,
			})
			impl = &RelayAnthropicProvider{Base: base, name: cfg.Name}
		case provider.ProtocolGoogle:
			return nil, fmt.Errorf("google protocol not yet supported for relay stations")
		default:
			return nil, fmt.Errorf("unsupported protocol: %s", proto)
		}

		if err != nil {
			return nil, fmt.Errorf("create %s implementation: %w", proto, err)
		}

		implementations[proto] = impl
	}

	return &GenericRelayProvider{
		name:            cfg.Name,
		protocolMode:    cfg.ProtocolMode,
		primaryProtocol: cfg.PrimaryProtocol,
		implementations: implementations,
	}, nil
}

// MarshalJSON 自定义 JSON 序列化(用于调试/日志)
func (p *GenericRelayProvider) MarshalJSON() ([]byte, error) {
	protocols := make([]string, 0, len(p.implementations))
	for proto := range p.implementations {
		protocols = append(protocols, string(proto))
	}
	return json.Marshal(map[string]interface{}{
		"name":             p.name,
		"type":             "relay",
		"primary_protocol": p.primaryProtocol,
		"protocols":        protocols,
	})
}
