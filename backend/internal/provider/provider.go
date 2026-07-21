// Package provider 定义所有 LLM Provider 必须实现的接口与共享类型
// 对应规格书 5.1 Provider 接口
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Protocol Provider 协议类型
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGoogle    Protocol = "google"
)

// ParseProtocol 解析协议字符串,失败返回 error
func ParseProtocol(s string) (Protocol, error) {
	switch s {
	case "openai":
		return ProtocolOpenAI, nil
	case "anthropic":
		return ProtocolAnthropic, nil
	case "google":
		return ProtocolGoogle, nil
	default:
		return "", fmt.Errorf("unknown protocol %q (want openai|anthropic|google)", s)
	}
}

// Request 是 Gateway 收到的原始请求的包装
// 重要:Body 是原始字节,Gateway 不做解析或转换
type Request struct {
	Method       string
	Path         string
	Headers      http.Header
	Body         []byte
	Model        string // 解析后的目标模型 ID(已从别名解析)
	IsStream     bool
	GatewayKeyID string
	TraceID      string
}

// Response Provider 返回的原始响应包装
// Body 是原始字节,Gateway 不做修改
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Usage      *Usage
}

// StreamChunk 流式响应的单条数据
type StreamChunk struct {
	Data []byte // SSE data 行的原始内容
	Err  error  // io.EOF 表示流结束
}

// Usage 从 Provider 响应中提取的用量
// P40: 新增 cache 字段 — 支持 prefix caching 精细计费
//   - PromptTokens:        不计 cache 的输入 token(DeepSeek 的 prompt_cache_miss_tokens)
//   - CacheCreationTokens: 创建新 cache 单元的 token(Anthropic 才有,DeepSeek = 0)
//   - CacheReadTokens:     命中已有 cache 的 token(DeepSeek prompt_cache_hit_tokens,Anthropic cache_read_input_tokens)
//   - CompletionTokens:    输出 token
type Usage struct {
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	CacheCreationTokens int
	CacheReadTokens     int
	RawUsage            map[string]interface{}
}

// Provider 所有 LLM Provider 必须实现的接口
//
// 设计原则(规格书 1.2 原则 1):Provider 只负责协议细节,
// Gateway 核心不感知也不修改 body / response 格式
type Provider interface {
	Name() string
	Protocol() Protocol
	Models() []string

	// SendRequest 发送非流式请求
	SendRequest(ctx context.Context, req *Request) (*Response, error)

	// SendStreamRequest 发送流式请求
	// 返回 channel 逐步推送 SSE chunk,流结束关闭 channel
	SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error)

	// HealthCheck 健康检查
	HealthCheck(ctx context.Context) error

	// Close 清理资源
	Close() error
}

// ErrorType 错误分类
type ErrorType string

const (
	ErrorTypeRateLimit      ErrorType = "rate_limit"
	ErrorTypeAuth           ErrorType = "auth"
	ErrorTypeInvalidRequest ErrorType = "invalid_request"
	ErrorTypeServerError    ErrorType = "server_error"
	ErrorTypeTimeout        ErrorType = "timeout"
	ErrorTypeConnection     ErrorType = "connection"
	ErrorTypeModelNotFound  ErrorType = "model_not_found"
)

// ProviderError 是 Provider 返回的结构化错误
type ProviderError struct {
	ProviderName string
	StatusCode   int
	ErrorType    ErrorType
	Message      string
	RetryAfter   time.Duration
	RawError     []byte
}

func (e *ProviderError) Error() string {
	if e.ProviderName != "" {
		return fmt.Sprintf("[%s] %s: %s (status=%d)", e.ProviderName, e.ErrorType, e.Message, e.StatusCode)
	}
	return fmt.Sprintf("%s: %s (status=%d)", e.ErrorType, e.Message, e.StatusCode)
}

// IsRetryable 判断错误是否触发 failover
// 规格书:invalid_request / auth 不重试
func (e *ProviderError) IsRetryable() bool {
	switch e.ErrorType {
	case ErrorTypeInvalidRequest, ErrorTypeAuth, ErrorTypeModelNotFound:
		return false
	default:
		return true
	}
}

// ClassifyError 根据 HTTP 状态码分类错误
func ClassifyError(statusCode int) ErrorType {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return ErrorTypeRateLimit
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return ErrorTypeAuth
	case statusCode == http.StatusBadRequest:
		return ErrorTypeInvalidRequest
	case statusCode == http.StatusNotFound:
		return ErrorTypeModelNotFound
	case statusCode >= 500:
		return ErrorTypeServerError
	default:
		return ErrorTypeServerError
	}
}

// AsProviderError 尝试把 error 转成 *ProviderError
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
