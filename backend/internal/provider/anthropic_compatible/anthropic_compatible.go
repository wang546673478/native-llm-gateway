// Package anthropic_compatible 实现 Anthropic Messages API 兼容协议的共享逻辑
// 对应规格书 8.3
//
// 适用 Provider: MiniMax / 任意 Anthropic 兼容 API
package anthropic_compatible

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// Config 构造 Provider 所需的最小配置
type Config struct {
	Name     string
	Endpoint string // e.g. https://api.minimax.chat
	Timeout  time.Duration
	Pool     *keypool.Pool
}

// Base Anthropic 兼容 Provider 的共享实现
type Base struct {
	cfg    Config
	client *http.Client
}

// NewBase 构造 Base
func NewBase(cfg Config) *Base {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Base{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Name 由 wrapper 提供

// SendRequest 发送非流式 Anthropic Messages 请求
//   POST {endpoint}/v1/messages
//   Headers:
//     x-api-key: {key}
//     anthropic-version: 2023-06-01
//     Content-Type: application/json
//   Body 原样透传
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, "keypool not configured")
	}
	key, err := b.cfg.Pool.Acquire()
	if err != nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/messages",
		bytes.NewReader(req.Body))
	if err != nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("x-api-key", key.Key)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Content-Type", "application/json")
	if req.TraceID != "" {
		httpReq.Header.Set("X-Request-Id", req.TraceID)
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		errType := provider.ErrorTypeConnection
		if ctx.Err() == context.DeadlineExceeded {
			errType = provider.ErrorTypeTimeout
		}
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, b.newError(0, errType, err.Error())
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		b.cfg.Pool.ReportError(key, "io_error")
		return nil, b.newError(0, provider.ErrorTypeConnection, err.Error())
	}

	if httpResp.StatusCode >= 400 {
		// P49: 带 body 检测 quota
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)
		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, parseRetryAfter(httpResp.Header.Get("Retry-After")))
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		return nil, b.newError(httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
	}

	b.cfg.Pool.ReportSuccess(key)
	usage := parseAnthropicUsage(body)

	return &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Usage:      usage,
	}, nil
}

// SendStreamRequest 发送流式 Anthropic Messages 请求
// Anthropic SSE 格式:
//   event: message_start
//   data: {"type":"message_start","message":{...}}
//
//   event: content_block_delta
//   data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}
//
//   event: message_delta
//   data: {"type":"message_delta","usage":{"output_tokens":N}}
//
//   event: message_stop
//   data: {"type":"message_stop"}
func (b *Base) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, nil, b.newError(0, provider.ErrorTypeConnection, "keypool not configured")
	}
	key, err := b.cfg.Pool.Acquire()
	if err != nil {
		return nil, nil, b.newError(0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
	}

	streamTimeout := b.cfg.Timeout
	if streamTimeout < 120*time.Second {
		streamTimeout = 120 * time.Second
	}
	client := &http.Client{Timeout: streamTimeout}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/messages",
		bytes.NewReader(req.Body))
	if err != nil {
		return nil, nil, b.newError(0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("x-api-key", key.Key)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.TraceID != "" {
		httpReq.Header.Set("X-Request-Id", req.TraceID)
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		errType := provider.ErrorTypeConnection
		if ctx.Err() == context.DeadlineExceeded {
			errType = provider.ErrorTypeTimeout
		}
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, nil, b.newError(0, errType, err.Error())
	}

	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		// P49: 带 body 检测 quota
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)
		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, 0)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		return nil, nil, b.newError(httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
	}

	b.cfg.Pool.ReportSuccess(key)

	ch := make(chan *provider.StreamChunk, 16)
	// P42: 收集流中的 usage — Anthropic 在 message_start (input+cache) 和 message_delta (output) 里发
	// P65: 也从 message_start 抽 model(message.model 字段)
	var inputTokens, outputTokens, cacheCreation, cacheRead int
	var upstreamModel string
	resp := &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
	}
	go func() {
		defer func() {
			// 在 close(ch) 前填 usage
			if inputTokens > 0 || outputTokens > 0 || cacheCreation > 0 || cacheRead > 0 {
				resp.Usage = &provider.Usage{
					Model:               upstreamModel, // P65
					PromptTokens:        inputTokens,
					CompletionTokens:    outputTokens,
					TotalTokens:         inputTokens + outputTokens + cacheCreation + cacheRead,
					CacheCreationTokens: cacheCreation,
					CacheReadTokens:     cacheRead,
					RawUsage: map[string]interface{}{
						"input_tokens":                inputTokens,
						"output_tokens":               outputTokens,
						"cache_creation_input_tokens": cacheCreation,
						"cache_read_input_tokens":     cacheRead,
					},
				}
			}
			close(ch)
		}()
		defer httpResp.Body.Close()
		reader := bufio.NewReader(httpResp.Body)

		// Anthropic SSE: 每行以 event: / data: 开头,空行分隔事件
		// 把整段当作一个 SSE 事件转发(保留原格式)
		var buf bytes.Buffer
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					if buf.Len() > 0 {
						ch <- &provider.StreamChunk{Data: append([]byte{}, buf.Bytes()...)}
					}
					ch <- &provider.StreamChunk{Err: io.EOF}
				} else {
					ch <- &provider.StreamChunk{Err: err}
				}
				return
			}
			line = bytes.TrimRight(line, "\r\n")
			// 空行 = 一个 event 结束
			if len(line) == 0 {
				if buf.Len() > 0 {
					eventData := append([]byte{}, buf.Bytes()...)
					// P42 + P65: 在转发前尝试解析 usage 和 model
					extractAnthropicStreamUsage(eventData, &inputTokens, &outputTokens, &cacheCreation, &cacheRead, &upstreamModel)
					eventData = append(eventData, '\n', '\n')
					ch <- &provider.StreamChunk{Data: eventData}
					buf.Reset()
				}
				continue
			}
			// 注释行
			if bytes.HasPrefix(line, []byte(":")) {
				continue
			}
			// 累积行
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}()

	return ch, resp, nil
}

// extractAnthropicStreamUsage 从单个 Anthropic SSE 事件中提取 usage
// 关注两种事件:
//   - message_start: data 里 {"message":{...,"usage":{input_tokens,cache_creation_input_tokens,cache_read_input_tokens}}}
//   - message_delta:  data 里 {"usage":{output_tokens}}(output 在顶层)
//
// P65: 同时抽 model(message_start.message.model 字段,作为 upstream model 名)
func extractAnthropicStreamUsage(event []byte, input, output, cacheCreate, cacheRead *int, model *string) {
	// 找 event: 类型行(决定这是哪种事件)
	var eventType string
	for _, line := range bytes.Split(event, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("event:")) {
			eventType = string(bytes.TrimSpace(line[6:]))
			break
		}
	}
	if eventType != "message_start" && eventType != "message_delta" {
		return
	}
	// 找 data: 行(JSON)
	for _, line := range bytes.Split(event, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		// 通用 usage 结构
		var u struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
			Message *struct {
				Model string `json:"model"` // P65: 上游真实 model 名
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(payload, &u); err != nil {
			return
		}
		// message_start: usage 在 message.usage;message_delta: usage 在顶层
		usageObj := u.Usage
		if usageObj == nil && u.Message != nil {
			usageObj = u.Message.Usage
		}
		// P65: 抽 model — message_start 的 message.model 是上游真实 model
		if u.Message != nil && u.Message.Model != "" && *model == "" {
			*model = u.Message.Model
		}
		if usageObj == nil {
			return
		}
		if usageObj.InputTokens > 0 {
			*input = usageObj.InputTokens
		}
		if usageObj.OutputTokens > 0 {
			*output = usageObj.OutputTokens
		}
		if usageObj.CacheCreationInputTokens > 0 {
			*cacheCreate = usageObj.CacheCreationInputTokens
		}
		if usageObj.CacheReadInputTokens > 0 {
			*cacheRead = usageObj.CacheReadInputTokens
		}
		return
	}
}

// HealthCheck 简单 GET 检查
func (b *Base) HealthCheck(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Anthropic 兼容 API 通常没有 /models 端点,直接 TCP 检查
	req, err := http.NewRequestWithContext(hctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/messages", nil)
	if err != nil {
		return err
	}
	if b.cfg.Pool != nil {
		if k, err := b.cfg.Pool.Acquire(); err == nil {
			req.Header.Set("x-api-key", k.Key)
			defer b.cfg.Pool.ReportSuccess(k)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	// 任何 2xx/4xx 都说明 endpoint 通了(401/405 都 OK)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("health check: status %d", resp.StatusCode)
	}
	return nil
}

// Close 释放 http client
func (b *Base) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// SetPool P30:让 Server 把从 DB 读出来的 Pool 注入到 Base
func (b *Base) SetPool(p *keypool.Pool) {
	b.cfg.Pool = p
}

// newError helper
func (b *Base) newError(status int, errType provider.ErrorType, msg string, rawErr ...[]byte) *provider.ProviderError {
	pe := &provider.ProviderError{
		ProviderName: b.cfg.Name,
		StatusCode:   status,
		ErrorType:    errType,
		Message:      msg,
	}
	if len(rawErr) > 0 {
		pe.RawError = rawErr[0]
	}
	return pe
}

// parseAnthropicUsage 从 Anthropic 响应抽取 usage
// 格式: {"usage": {"input_tokens": N, "output_tokens": M, "cache_creation_input_tokens": ?, "cache_read_input_tokens": ?}}
//
// P65: 同时抽取顶层 "model" 字段(上游响应的真实 model 名,例如 "MiniMax-M3")
// proxy 写入 UsageRecord.ModelID 时优先用此字段覆盖客户端请求的 model
//
// 注意:Anthropic 的 input_tokens 不含 cache 部分(cache 是另外计的)
//   - PromptTokens        = input_tokens
//   - CacheCreationTokens = cache_creation_input_tokens
//   - CacheReadTokens     = cache_read_input_tokens
func parseAnthropicUsage(body []byte) *provider.Usage {
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	u := &provider.Usage{
		Model:               resp.Model, // P65: 上游响应的真实 model 名
		PromptTokens:        resp.Usage.InputTokens,
		CompletionTokens:    resp.Usage.OutputTokens,
		TotalTokens:         resp.Usage.InputTokens + resp.Usage.OutputTokens +
			resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
		CacheCreationTokens: resp.Usage.CacheCreationInputTokens,
		CacheReadTokens:     resp.Usage.CacheReadInputTokens,
		RawUsage: map[string]interface{}{
			"input_tokens":                resp.Usage.InputTokens,
			"output_tokens":               resp.Usage.OutputTokens,
			"cache_creation_input_tokens": resp.Usage.CacheCreationInputTokens,
			"cache_read_input_tokens":     resp.Usage.CacheReadInputTokens,
		},
	}
	return u
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}
