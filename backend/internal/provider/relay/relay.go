// Package relay 实现通用中转站 Provider
// 中转站 = 纯透传代理,无需编写代码,只需配置 URL + 协议
package relay

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

// DefaultTimeout 中转站单次请求超时的兜底值(DB 里 timeout_seconds 没填时用)。
//
// 这是**非流式**路径的实际上限:流式两个 Base 都按 StreamTimeoutFloor(600s)取下限,
// 不受这里影响。原值 60s 太小 —— 大 body(40k+ token)的非流式推理本身就要 60s 以上,
// 每个候选都在 60s 整点被切断,failover 试完所有候选仍然全败(踩坑:kiro2 全 502)。
//
// 与 server.write_timeout(600s)的关系:write_timeout 是整个响应的绝对写上限,
// 单次超时除进去 = 还能试几个候选。400s 意味着 600s 预算内只够 1 次多一点,
// 即"宁可等单个上游,不追求多次 failover"——这是权衡后选定的取值,不是笔误。
// 改这里要连带看 RelayStations.vue 的 :max(跟 write_timeout 对齐)。
const DefaultTimeout = 400 * time.Second

// Config 通用中转站配置
type Config struct {
	Name    string
	BaseURL string
	// BillingSource is the station default for dynamically loaded route faces.
	// Individual key records may still override this value.
	BillingSource      string
	ProtocolMode       string // "single" | "multi"
	PrimaryProtocol    provider.Protocol
	SupportedProtocols []provider.Protocol
	Timeout            int // seconds
	ProtocolConfigs    map[provider.Protocol]interface{}
}

// GenericRelayProvider 通用中转站 Provider
// 根据协议动态路由到对应的协议实现(透传)
type GenericRelayProvider struct {
	name            string
	baseURL         string
	billingSource   string
	protocolMode    string // "single" | "multi"
	primaryProtocol provider.Protocol
	implementations map[provider.Protocol]provider.Provider
	pool            *keypool.Pool
	poolMu          sync.Mutex
	// closeStates is shared by GenericRelayProvider and its face views. A
	// multi-protocol station is registered under one name per face, so Manager
	// may call Close once for each view during reload.
	closeStates map[provider.Protocol]*faceCloseState
	// closeMu protects lazy initialization of closeStates for providers built
	// directly in tests or by legacy callers. Constructor-created providers
	// already initialize the map, but face Close calls may still race with a
	// legacy GenericRelayProvider.Close call during reload.
	closeMu sync.Mutex
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
	if cfg.BillingSource == "" {
		cfg.BillingSource = "api"
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = DefaultTimeout
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
				Passthrough: true,
			})
			impl = &RelayOpenAIProvider{Base: base, name: cfg.Name}
		case provider.ProtocolAnthropic:
			base := anthropic_compatible.NewBase(anthropic_compatible.Config{
				Name: cfg.Name, Endpoint: cfg.BaseURL, Timeout: timeout, Passthrough: true,
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

	closeStates := make(map[provider.Protocol]*faceCloseState, len(implementations))
	for proto := range implementations {
		closeStates[proto] = &faceCloseState{}
	}

	return &GenericRelayProvider{
		name:            cfg.Name,
		baseURL:         cfg.BaseURL,
		billingSource:   cfg.BillingSource,
		protocolMode:    cfg.ProtocolMode,
		primaryProtocol: cfg.PrimaryProtocol,
		implementations: implementations,
		closeStates:     closeStates,
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
