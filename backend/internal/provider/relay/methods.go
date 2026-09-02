package relay

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// Name 返回中转站名称
func (p *GenericRelayProvider) Name() string {
	return p.name
}

// Endpoint returns the station base URL for quota probes and diagnostics.
// Relay stations are loaded from the database rather than config.Providers,
// so Manager cannot recover this metadata from the static config map.
func (p *GenericRelayProvider) Endpoint() string {
	if p == nil {
		return ""
	}
	return p.baseURL
}

// Protocol 返回主协议(用于向后兼容)
func (p *GenericRelayProvider) Protocol() provider.Protocol {
	return p.primaryProtocol
}

// BillingSource returns the station-level default billing face.  Per-key
// billing_source remains authoritative when it is present in the key pool;
// this value only fills the route face metadata for dynamically loaded relay
// stations.
func (p *GenericRelayProvider) BillingSource() string {
	if p == nil || p.billingSource == "" {
		return "api"
	}
	return p.billingSource
}

// selectImplementation 根据请求路径选择协议实现
func (p *GenericRelayProvider) selectImplementation(req *provider.Request) (provider.Provider, error) {
	if req == nil {
		return nil, provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest, "relay request is nil")
	}
	reqProto, recognized := provider.ProtocolForPath(req.Path)
	if !recognized {
		return nil, provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest,
			"request path does not identify a supported relay protocol")
	}
	// 单协议模式:直接返回主协议实现
	if p.protocolMode == "single" {
		if reqProto != p.primaryProtocol {
			return nil, provider.NewError(p.name, 0, provider.ErrorTypeInvalidRequest,
				"request path protocol does not match relay protocol")
		}
		if impl, ok := p.implementations[p.primaryProtocol]; ok {
			return impl, nil
		}
		return nil, provider.NewError(p.name, 500, provider.ErrorTypeServerError, "primary protocol implementation not found")
	}

	// 多协议模式:从请求路径推断协议
	// 识别到协议后必须使用对应实现。不能因为该站缺少 secondary
	// implementation 就回退到 primary，否则会把请求发到错误的 wire
	// protocol；face wrapper 之外的旧直接调用也必须遵守同一契约。
	if reqProto != "" {
		if impl, ok := p.implementations[reqProto]; ok {
			return impl, nil
		}
		return nil, provider.NewError(p.name, http.StatusBadRequest, provider.ErrorTypeInvalidRequest,
			fmt.Sprintf("relay does not support request protocol %q", reqProto))
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

// detectProtocolFromPath 从请求路径推断协议(保留给包内旧调用方)
func detectProtocolFromPath(path string) provider.Protocol {
	proto, _ := provider.ProtocolForPath(path)
	return proto
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
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	if p.pool == pool {
		return
	}
	p.pool = pool
	for _, impl := range p.implementations {
		impl.SetPool(pool)
	}
}

// Close 关闭所有协议实现
func (p *GenericRelayProvider) Close() error {
	var firstErr error
	for proto := range p.implementations {
		if err := p.closeImplementation(proto); err != nil && firstErr == nil {
			firstErr = provider.NewError(p.name, 500, provider.ErrorTypeServerError, "close "+string(proto)+" implementation: "+err.Error())
		}
	}
	return firstErr
}

// closeImplementation closes one protocol implementation at most once.  A
// multi-protocol relay is exposed as several face providers, each of which is
// removed independently by Manager; sharing this state prevents duplicate
// CloseIdleConnections calls and makes legacy GenericRelayProvider.Close safe
// alongside face-level Close calls.
func (p *GenericRelayProvider) closeImplementation(proto provider.Protocol) error {
	if p == nil {
		return nil
	}
	impl, ok := p.implementations[proto]
	if !ok || impl == nil {
		return nil
	}
	p.closeMu.Lock()
	if p.closeStates == nil {
		p.closeStates = make(map[provider.Protocol]*faceCloseState)
	}
	state := p.closeStates[proto]
	if state == nil {
		// Providers constructed before face close state was introduced are not
		// expected in production, but lazily initialize the state so repeated
		// face/Generic Close calls remain idempotent even for struct literals.
		state = &faceCloseState{}
		p.closeStates[proto] = state
	}
	p.closeMu.Unlock()
	state.once.Do(func() { state.err = impl.Close() })
	return state.err
}
