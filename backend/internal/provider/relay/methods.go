package relay

import (
	"context"
	"strings"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// Name 返回中转站名称
func (p *GenericRelayProvider) Name() string {
	return p.name
}

// Protocol 返回主协议(用于向后兼容)
func (p *GenericRelayProvider) Protocol() provider.Protocol {
	return p.primaryProtocol
}

// selectImplementation 根据请求路径选择协议实现
func (p *GenericRelayProvider) selectImplementation(req *provider.Request) (provider.Provider, error) {
	// 单协议模式:直接返回主协议实现
	if p.protocolMode == "single" {
		if impl, ok := p.implementations[p.primaryProtocol]; ok {
			return impl, nil
		}
		return nil, provider.NewError(p.name, 500, provider.ErrorTypeServerError, "primary protocol implementation not found")
	}

	// 多协议模式:从请求路径推断协议
	reqProto := detectProtocolFromPath(req.Path)

	// 优先使用推断的协议
	if reqProto != "" {
		if impl, ok := p.implementations[reqProto]; ok {
			return impl, nil
		}
	}

	// 回退到主协议
	if impl, ok := p.implementations[p.primaryProtocol]; ok {
		return impl, nil
	}

	// 最后使用任意可用的实现
	for _, impl := range p.implementations {
		return impl, nil
	}

	return nil, provider.NewError(p.name, 500, provider.ErrorTypeServerError, "no suitable protocol implementation found")
}

// detectProtocolFromPath 从请求路径推断协议(与 router 的 detectProtocol 逻辑一致)
func detectProtocolFromPath(path string) provider.Protocol {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/v1/messages"):
		return provider.ProtocolAnthropic
	case strings.Contains(p, "/chat/completions"):
		return provider.ProtocolOpenAI
	case strings.Contains(p, "/responses"):
		return provider.ProtocolOpenAI
	case strings.Contains(p, ":generatecontent") || strings.Contains(p, "/v1beta/models"):
		return provider.ProtocolGoogle
	default:
		return ""
	}
}

// SendRequest 根据请求协议选择实现并发送请求
func (p *GenericRelayProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	impl, err := p.selectImplementation(req)
	if err != nil {
		return nil, err
	}
	return impl.SendRequest(ctx, req)
}

// SendStreamRequest 根据请求协议选择实现并发送流式请求
func (p *GenericRelayProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	impl, err := p.selectImplementation(req)
	if err != nil {
		return nil, nil, err
	}
	return impl.SendStreamRequest(ctx, req)
}

// HealthCheck 对主协议实现执行健康检查
func (p *GenericRelayProvider) HealthCheck(ctx context.Context) error {
	// 只检查主协议实现
	if impl, ok := p.implementations[p.primaryProtocol]; ok {
		return impl.HealthCheck(ctx)
	}
	// 回退到任意实现
	for _, impl := range p.implementations {
		return impl.HealthCheck(ctx)
	}
	return provider.NewError(p.name, 500, provider.ErrorTypeServerError, "no implementation available for health check")
}

// ListModels 列出所有支持的模型(合并所有协议的模型列表)
func (p *GenericRelayProvider) ListModels(ctx context.Context) ([]string, error) {
	seen := make(map[string]bool)
	var allModels []string

	// 合并所有协议实现的模型列表
	for _, impl := range p.implementations {
		models, err := impl.ListModels(ctx)
		if err != nil {
			continue // 忽略单个实现的错误
		}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				allModels = append(allModels, m)
			}
		}
	}

	if len(allModels) == 0 {
		return nil, provider.NewError(p.name, 500, provider.ErrorTypeServerError, "no models available from any protocol implementation")
	}

	return allModels, nil
}

// SetPool 为所有协议实现注入 KeyPool
func (p *GenericRelayProvider) SetPool(pool *keypool.Pool) {
	p.pool = pool
	for _, impl := range p.implementations {
		impl.SetPool(pool)
	}
}

// Close 关闭所有协议实现
func (p *GenericRelayProvider) Close() error {
	var firstErr error
	for proto, impl := range p.implementations {
		if err := impl.Close(); err != nil && firstErr == nil {
			firstErr = provider.NewError(p.name, 500, provider.ErrorTypeServerError, "close "+string(proto)+" implementation: "+err.Error())
		}
	}
	return firstErr
}
