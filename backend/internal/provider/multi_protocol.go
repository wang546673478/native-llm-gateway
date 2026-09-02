// Package provider - Multi-Protocol Provider Adapter
// 支持单个 provider 同时处理多种协议的请求

package provider

import (
	"context"
	"fmt"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// multiProtocolAdapter 是支持多协议的 Provider 适配器
// 根据请求路径动态选择底层实现(OpenAI 或 Anthropic 协议)
type multiProtocolAdapter struct {
	name            string
	primaryProtocol Protocol // 主协议(用于 Protocol() 返回值和兼容性)
	implementations map[Protocol]Provider
}

// MultiProtocolConfig 多协议 Provider 的配置
type MultiProtocolConfig struct {
	Name            string
	PrimaryProtocol Protocol              // 主协议
	Implementations map[Protocol]Provider // 各协议的实现
}

// NewMultiProtocolAdapter 创建多协议 Provider 适配器
func NewMultiProtocolAdapter(cfg MultiProtocolConfig) (*multiProtocolAdapter, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("provider name is required")
	}
	if cfg.PrimaryProtocol == "" {
		return nil, fmt.Errorf("primary protocol is required")
	}
	if len(cfg.Implementations) == 0 {
		return nil, fmt.Errorf("at least one protocol implementation is required")
	}

	return &multiProtocolAdapter{
		name:            cfg.Name,
		primaryProtocol: cfg.PrimaryProtocol,
		implementations: cfg.Implementations,
	}, nil
}

// Name 返回 provider 名称
func (p *multiProtocolAdapter) Name() string {
	return p.name
}

// Protocol 返回主协议(用于向后兼容)
func (p *multiProtocolAdapter) Protocol() Protocol {
	return p.primaryProtocol
}

// SupportedProtocols 返回所有支持的协议
func (p *multiProtocolAdapter) SupportedProtocols() []Protocol {
	protocols := make([]Protocol, 0, len(p.implementations))
	for proto := range p.implementations {
		protocols = append(protocols, proto)
	}
	return protocols
}

// SupportsProtocol 检查是否支持指定协议
func (p *multiProtocolAdapter) SupportsProtocol(proto Protocol) bool {
	_, ok := p.implementations[proto]
	return ok
}

// selectImplementation 根据请求路径选择协议实现
func (p *multiProtocolAdapter) selectImplementation(req *Request) (Provider, error) {
	if req == nil {
		return nil, NewError(p.name, 0, ErrorTypeInvalidRequest, "request is nil")
	}
	// 从请求路径推断协议；未知路径不得静默回退到 primary face。
	reqProto, ok := ProtocolForPath(req.Path)
	if !ok {
		return nil, NewError(p.name, 0, ErrorTypeInvalidRequest, "request path does not identify a supported protocol")
	}

	// 优先使用推断的协议
	if impl, ok := p.implementations[reqProto]; ok && reqProto != "" {
		return impl, nil
	}

	return nil, NewError(p.name, 0, ErrorTypeInvalidRequest,
		fmt.Sprintf("request protocol %q is not supported", reqProto))
}

// detectProtocolFromPath 从请求路径推断协议(与 router 的 detectProtocol 逻辑一致)
func detectProtocolFromPath(path string) Protocol {
	proto, _ := ProtocolForPath(path)
	return proto
}

// SendRequest 根据请求协议选择实现并发送请求
func (p *multiProtocolAdapter) SendRequest(ctx context.Context, req *Request) (*Response, error) {
	impl, err := p.selectImplementation(req)
	if err != nil {
		return nil, err
	}
	return impl.SendRequest(ctx, req)
}

// SendStreamRequest 根据请求协议选择实现并发送流式请求
func (p *multiProtocolAdapter) SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error) {
	impl, err := p.selectImplementation(req)
	if err != nil {
		return nil, nil, err
	}
	return impl.SendStreamRequest(ctx, req)
}

// DiagnoseKey selects exactly one registered protocol face and delegates its
// read-only diagnostic capability. Unlike request routing, an unsupported or
// mismatched path is never silently sent through the primary protocol.
func (p *multiProtocolAdapter) DiagnoseKey(ctx context.Context, key *keypool.Key, d KeyDiagnosticRequest) (*KeyDiagnosticResult, error) {
	proto := d.Protocol
	if proto == "" {
		if inferred, ok := DiagnosticProtocolForPath(d.Path); ok {
			proto = inferred
		} else {
			proto = p.primaryProtocol
		}
	}
	impl, ok := p.implementations[proto]
	if !ok {
		return nil, NewDiagnosticUnavailable(proto, d.Path, "multi-protocol provider does not support requested protocol")
	}
	diagnoser, ok := impl.(KeyDiagnoser)
	if !ok {
		return nil, NewDiagnosticUnavailable(proto, d.Path, "selected protocol has no key diagnostic implementation")
	}
	d.Protocol = proto
	return diagnoser.DiagnoseKey(ctx, key, d)
}

// HealthCheck 对所有协议实现执行健康检查
func (p *multiProtocolAdapter) HealthCheck(ctx context.Context) error {
	// 只检查主协议实现
	if impl, ok := p.implementations[p.primaryProtocol]; ok {
		return impl.HealthCheck(ctx)
	}
	// 回退到任意实现
	for _, impl := range p.implementations {
		return impl.HealthCheck(ctx)
	}
	return fmt.Errorf("no protocol implementation available for health check")
}

// ListModels 列出所有支持的模型(合并所有协议的模型列表)
func (p *multiProtocolAdapter) ListModels(ctx context.Context) ([]string, error) {
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
		return nil, fmt.Errorf("no models available from any protocol implementation")
	}

	return allModels, nil
}

// SetPool 为所有协议实现注入 KeyPool
func (p *multiProtocolAdapter) SetPool(pool *keypool.Pool) {
	for _, impl := range p.implementations {
		impl.SetPool(pool)
	}
}

// Close 关闭所有协议实现
func (p *multiProtocolAdapter) Close() error {
	var firstErr error
	for proto, impl := range p.implementations {
		if err := impl.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s implementation: %w", proto, err)
		}
	}
	return firstErr
}
