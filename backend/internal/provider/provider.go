// Package provider 定义所有 LLM Provider 必须实现的接口与共享类型
// 对应规格书 5.1 Provider 接口
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// Protocol Provider 协议类型
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGoogle    Protocol = "google"
)

// MiniMax base_resp 错误识别(P-quota-minimax)
// MiniMax 的错误经常藏在 HTTP 200 的 body 里:{"base_resp":{"status_code":1008,...}},
// 只看 HTTP 状态码会把它当成功响应透传。两个协议基座(openai/anthropic)的
// SendRequest / SendStreamRequest 都在返回成功前调 ParseMiniMaxBaseResp;
// 非 MiniMax 响应没有 base_resp 字段,检查为 no-op。

// MiniMax 配额耗尽错误码:1008 = 余额不足,2056 = 超出 Token Plan 限制
func IsMiniMaxQuotaCode(code int) bool {
	return code == 1008 || code == 2056
}

// ParseMiniMaxStreamBaseResp 流式场景解析 base_resp 错误(peek 到的前几行):
// MiniMax 可能直接发 JSON 错误体(非 SSE),也可能用 SSE 帧包装
// (event:/data: 前缀行)。两种都解析;无 base_resp → (0,"")
func ParseMiniMaxStreamBaseResp(peeked []byte) (int, string) {
	if code, msg := ParseMiniMaxBaseResp(peeked); code != 0 {
		return code, msg
	}
	for _, line := range bytes.Split(peeked, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if code, msg := ParseMiniMaxBaseResp(payload); code != 0 {
			return code, msg
		}
	}
	return 0, ""
}

// ParseMiniMaxBaseResp 解析 body 里的 base_resp 错误。
// 返回 (status_code, status_msg);body 无 base_resp 或 status_code==0 返回 (0,"")
func ParseMiniMaxBaseResp(body []byte) (int, string) {
	var parsed struct {
		BaseResp *struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, ""
	}
	if parsed.BaseResp == nil || parsed.BaseResp.StatusCode == 0 {
		return 0, ""
	}
	return parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg
}

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
	// Key P-key-mismatch: 路由层(proxy)已 acquire 的 key。
	// Provider.SendRequest/SendStreamRequest 必须用它发请求,不能再内部
	// acquire — 否则双 acquire 可能拿到不同 key,429 时 reportKeyError 用
	// proxy 的 key 上报,冷却标到没发过请求的 healthy key 上
	// (2026-08-06 实测:weige 429 把 key-1 误标 COOLING,两把 key 同时
	// 冷却,全链掉 deepseek)。nil = 让 Provider 自己 acquire(兼容
	// HealthCheck 等无路由上下文的调用)
	Key *keypool.Key
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
//
// P65: 新增 Model 字段 — 上游响应里的真实 model 名
//   - OpenAI 协议: 响应顶层 "model" 字段
//   - Anthropic 协议: 响应顶层 "model" 字段
//   - Google 协议: 响应顶层 "modelVersion" 字段(Gemini 命名不一样)
//     proxy 写入 UsageRecord.ModelID 时,优先用此字段覆盖客户端请求的 model
type Usage struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	CacheCreationTokens int
	CacheReadTokens     int
	RawUsage            map[string]interface{}
}

// ComputeCost 按三档每百万定价表算单次请求费用(元,CNY)
//   - cost = prompt*input + cache_read*cache_read + completion*output,单位按 /1M tokens
//   - u.PromptTokens 语义含「不计 cache 的输入」;cache 命中用 u.CacheReadTokens
//   - Usage.CacheCreationTokens 保留在 struct 里,但不再参与计费
//   - 无任何定价时返回 0(不收费的模型/未配置)
func ComputeCost(c ModelCost, u *Usage) float64 {
	hasAnyCost := c.CostPerMillionInput > 0 || c.CostPerMillionCacheRead > 0 || c.CostPerMillionOutput > 0
	if !hasAnyCost {
		return 0
	}
	return (float64(u.PromptTokens)/1_000_000.0)*c.CostPerMillionInput +
		(float64(u.CacheReadTokens)/1_000_000.0)*c.CostPerMillionCacheRead +
		(float64(u.CompletionTokens)/1_000_000.0)*c.CostPerMillionOutput
}

// Provider 所有 LLM Provider 必须实现的接口
//
// 设计原则(规格书 1.2 原则 1):Provider 只负责协议细节,
// Gateway 核心不感知也不修改 body / response 格式
type Provider interface {
	Name() string
	Protocol() Protocol
	Models() []string

	// SetPool 注入 KeyPool(server 启动时 / ReloadProviderPool 热重载时调用)。
	// 列进接口而非可选 type-assert:任何 Provider 实现都必须可注入 pool,
	// 新 provider 漏实现会在编译期报错,而不是运行时静默 nil pool("keypool not
	// configured")。所有现有实现(6 vendor base + 各协议面)均已实现,故加进接口
	// 不破坏编译。
	SetPool(*keypool.Pool)

	// SendRequest 发送非流式请求
	SendRequest(ctx context.Context, req *Request) (*Response, error)

	// SendStreamRequest 发送流式请求
	// 返回 channel 逐步推送 SSE chunk,流结束关闭 channel
	SendStreamRequest(ctx context.Context, req *Request) (<-chan *StreamChunk, *Response, error)

	// HealthCheck 健康检查
	HealthCheck(ctx context.Context) error

	// ListModels 返回上游当前在售模型 id 列表。
	// 不支持(如 anthropic 面)返回 ErrListModelsNotSupported。
	ListModels(ctx context.Context) ([]string, error)

	// Close 清理资源
	Close() error
}

// ErrListModelsNotSupported 该协议面不提供模型列表能力。
// 同步层收到它时,回退到同 vendor 的 OpenAI 面去查。
var ErrListModelsNotSupported = errors.New("provider: list models not supported")

// ErrorType 错误分类
type ErrorType string

const (
	ErrorTypeRateLimit          ErrorType = "rate_limit"
	ErrorTypeAuth               ErrorType = "auth"
	ErrorTypeInvalidRequest     ErrorType = "invalid_request"
	ErrorTypeServerError        ErrorType = "server_error"
	ErrorTypeTimeout            ErrorType = "timeout"
	ErrorTypeConnection         ErrorType = "connection"
	ErrorTypeModelNotFound      ErrorType = "model_not_found"
	ErrorTypeClientDisconnected ErrorType = "client_disconnected" // P: stream 客户端断开,非 retryable
	// P49: 配额/额度耗尽错误(MiniMax token plan 5h 用完等场景)
	// 与 auth 不同:quota 用完应该 failover 到下一个 provider(api 计费),
	// 而 auth 错误说明 key 本身有问题,不该 failover
	ErrorTypeQuotaExceeded ErrorType = "quota_exceeded"
	// P-catch-all: 白名单拒绝 — key 不允许该(路由后的)模型。
	// gateway 自己返的 403,用于 access log 标注,区分上游 auth 失败
	ErrorTypeModelNotAllowed ErrorType = "model_not_allowed"
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
// P49 调整:auth 错误**可重试**
//
//	理由:chain failover 场景下,provider A 的 key 错误 → failover 到 provider B 的 key
//	pool 已经把 A 的坏 key 标记为 DISABLED(ReportError),
//	下次 iter.Next() 会跳过 A 选 B,B 有独立 key 可以成功
//	invalid_request 和 model_not_found 仍然不重试(请求/模型本身有问题,换 provider 也没用)
func (e *ProviderError) IsRetryable() bool {
	switch e.ErrorType {
	case ErrorTypeInvalidRequest, ErrorTypeModelNotFound, ErrorTypeClientDisconnected:
		return false
	default:
		return true
	}
}

// ClassifyError 根据 HTTP 状态码 + body 分类错误
// P49: 识别 quota exceeded 错误(从 body 关键字判断)
//   - 402 Payment Required → 明确是 quota/账单问题
//   - 429 Too Many Requests → 优先按 rate limit,但 body 含 quota 关键字时升级为 quota_exceeded
//   - 403 Forbidden + body 含 quota/usage_limit/insufficient/balance 关键字 → quota_exceeded(不是 auth)
//   - 其他 403 → auth(说明 key 本身有问题)
func ClassifyError(statusCode int) ErrorType {
	return ClassifyErrorWithBody(statusCode, nil)
}

// ClassifyErrorWithBody P49: 带 body 的错误分类(检测 quota 关键字)
// body 是上游响应的原始字节,可能为 nil(未知)
func ClassifyErrorWithBody(statusCode int, body []byte) ErrorType {
	isQuotaBody := LooksLikeQuotaError(body)

	switch {
	case statusCode == http.StatusPaymentRequired: // 402
		return ErrorTypeQuotaExceeded
	case statusCode == http.StatusTooManyRequests:
		// 429 大多数是 rate limit,但也可能是 quota
		// 如果 body 含 quota 关键字,升级为 quota_exceeded
		if isQuotaBody {
			return ErrorTypeQuotaExceeded
		}
		return ErrorTypeRateLimit
	case statusCode == http.StatusForbidden:
		// 403 可能是 auth,也可能 quota
		if isQuotaBody {
			return ErrorTypeQuotaExceeded
		}
		return ErrorTypeAuth
	case statusCode == http.StatusUnauthorized:
		return ErrorTypeAuth
	case statusCode == http.StatusBadRequest:
		// D: GLM(1113 余额不足)/qwen(Throttling.RateQuota/QuotaExhausted)等把
		// 配额错误落在 HTTP 400。body 含 quota 关键字 → 升级为 quota_exceeded
		// (触发 failover 到下一 provider + key 标 QE),而非 invalid_request。
		// 纯 400 无 quota 关键字仍是 invalid_request(请求内容问题)。
		if isQuotaBody {
			return ErrorTypeQuotaExceeded
		}
		return ErrorTypeInvalidRequest
	case statusCode == http.StatusNotFound:
		return ErrorTypeModelNotFound
	case statusCode >= 500:
		return ErrorTypeServerError
	default:
		return ErrorTypeServerError
	}
}

// LooksLikeQuotaError 检测 body 是否含 quota/usage limit 相关关键字。
// 单一来源:prober.go 的 quotaKeywords 两套关键字表与这里本来就该一致(其注释
// "保持一致"),直接合并无冗余,prober 也走这同一个函数。
// 兼容各 provider 的英文/中文错误信息
//
// P-quota-minimax-429-fix: 只保留「强配额标记」,去掉 rate limit / exceeded
// 这类通用限流词 — 否则 MiniMax 纯限流 429(如 "rate limit exceeded")
// 会被升级成 QUOTA_EXCEEDED,把 healthy key 误杀到 poll 恢复(≤60s),
// 期间 token_plan 桶空 → 整链掉到 api 层。
// MiniMax 真套餐耗尽 429 的 message 是 "已达到 Token Plan 用量上限 (2056)",
// 靠 "token plan" / "用量上限" 仍能命中,不依赖通用词。
func LooksLikeQuotaError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 转小写匹配,避免大小写差异
	lower := toLowerASCII(body)
	// 含 prober.go quotaKeywords 的关键词(insufficient_* / quota_exceeded / billing /
	// plan required 等),与 provider 的 MiniMax 中文关键词并集。
	keywords := []string{
		"quota",
		"usage limit",
		"insufficient",
		"余额", "额度", "配额", "余额不足",
		"balance",
		"out of quota",
		"quota exceeded",
		"exceeded_current_quota",
		"quota_exceeded",
		"rate_limit_reached",
		"billing_not_active",
		"payment_required",
		"plan required",
		"plan_required",
		// P-quota-minimax-429: MiniMax anthropic 面把套餐耗尽报成
		// HTTP 429 + {"error":{"type":"rate_limit_error","message":"已达到
		// Token Plan 用量上限:请升级 Token Plan 套餐或购买积分补充用量。(2056)"}}
		// — 必须识别为 quota,否则 key 被误标 COOLING 而非 QUOTA_EXCEEDED
		// (实测 2026-08-05:双 key 长期冷却,整链掉到 api 层)
		"token plan", "用量上限", "超套餐",
	}
	for _, kw := range keywords {
		if contains(lower, kw) {
			return true
		}
	}
	return false
}

// 简单的 ASCII 小写转换(避免引入 strings.ToLower 的 unicode 复杂度)
func toLowerASCII(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return out
}

func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// AsProviderError 尝试把 error 转成 *ProviderError
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
