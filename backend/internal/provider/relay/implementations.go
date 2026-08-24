package relay

import (
	"fmt"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/provider/anthropic_compatible"
	"github.com/wang546673478/native-llm-gateway/internal/provider/openai_compatible"
)

// RelayAnthropicProvider 为中转站包装 anthropic_compatible.Base
type RelayAnthropicProvider struct {
	*anthropic_compatible.Base
	name string
}

func (p *RelayAnthropicProvider) Name() string {
	return p.name
}

func (p *RelayAnthropicProvider) Protocol() provider.Protocol {
	return provider.ProtocolAnthropic
}

// RelayOpenAIProvider 为中转站包装 openai_compatible.Base
type RelayOpenAIProvider struct {
	*openai_compatible.Base
	name string
}

func (p *RelayOpenAIProvider) Name() string {
	return p.name
}

func (p *RelayOpenAIProvider) Protocol() provider.Protocol {
	return provider.ProtocolOpenAI
}

// createAnthropicImplementation 创建 Anthropic 协议实现
func createAnthropicImplementation(name, baseURL string, timeout int) (provider.Provider, error) {
	if timeout <= 0 {
		timeout = 60
	}
	base := anthropic_compatible.NewBase(anthropic_compatible.Config{
		Name:     name,
		Endpoint: baseURL,
		Timeout:  time.Duration(timeout) * time.Second,
	})
	return &RelayAnthropicProvider{Base: base, name: name}, nil
}

// createOpenAIImplementation 创建 OpenAI 协议实现
func createOpenAIImplementation(name, baseURL string, timeout int) (provider.Provider, error) {
	if timeout <= 0 {
		timeout = 60
	}
	base := openai_compatible.NewBase(openai_compatible.Config{
		Name:        name,
		Endpoint:    baseURL,
		Timeout:     time.Duration(timeout) * time.Second,
		StreamUsage: true,
	})
	return &RelayOpenAIProvider{Base: base, name: name}, nil
}

// createGoogleImplementation 创建 Google 协议实现
func createGoogleImplementation(name, baseURL string, timeout int) (provider.Provider, error) {
	// Google 协议实现暂不支持
	// 因为当前没有 google_compatible 包,而且 gemini 已下线
	return nil, fmt.Errorf("google protocol not yet supported for relay stations")
}
