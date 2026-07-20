// Package openai_compatible 实现 OpenAI Chat Completions 兼容协议的共享逻辑
// 适用 Provider: DeepSeek / GLM / Qwen / Kimi / 任意 OpenAI 兼容 API
//
// 对应规格书 8.2
package openai_compatible

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
	Endpoint string // e.g. https://api.deepseek.com
	Timeout  time.Duration
	Pool     *keypool.Pool
}

// Base 是 OpenAI 兼容 Provider 的共享实现
// DeepSeek / GLM / Qwen / Kimi 等只需要薄薄一层 wrapper 即可
type Base struct {
	cfg    Config
	client *http.Client
}

// NewBase 构造 Base
func NewBase(cfg Config) *Base {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Base{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Name / Protocol / Models 由 wrapper 提供
// 这里把方法放在 wrapper 中,Base 只提供 HTTP 调用

// SendRequest 发送非流式请求
//   1. 从 Pool 取 Key
//   2. POST 到 {endpoint}/v1/chat/completions
//   3. Authorization: Bearer {key}
//   4. body 原样透传
//   5. 解析 OpenAI 格式响应,提取 Usage
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      "keypool not configured",
		}
	}
	key, err := b.cfg.Pool.Acquire()
	if err != nil {
		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      fmt.Sprintf("no available key: %v", err),
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/chat/completions",
		bytes.NewReader(req.Body))
	if err != nil {
		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      err.Error(),
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+key.Key)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.TraceID != "" {
		httpReq.Header.Set("X-Request-Id", req.TraceID)
	}
	// 透传客户端的部分 header(hop-by-hop 已在 Server 层删除)
	for _, h := range []string{"Accept", "Accept-Language"} {
		if v := req.Headers.Get(h); v != "" {
			httpReq.Header.Set(h, v)
		}
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		errType := provider.ErrorTypeConnection
		if ctx.Err() == context.DeadlineExceeded {
			errType = provider.ErrorTypeTimeout
		}
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name, ErrorType: errType,
			Message: err.Error(),
		}
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		b.cfg.Pool.ReportError(key, "io_error")
		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name, ErrorType: provider.ErrorTypeConnection,
			Message: err.Error(),
		}
	}

	if httpResp.StatusCode >= 400 {
		retryAfter := parseRetryAfter(httpResp.Header.Get("Retry-After"))
		errType := provider.ClassifyError(httpResp.StatusCode)

		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, retryAfter)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}

		return nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			StatusCode:   httpResp.StatusCode,
			ErrorType:    errType,
			Message:      fmt.Sprintf("upstream returned %d", httpResp.StatusCode),
			RetryAfter:   retryAfter,
			RawError:     body,
		}
	}

	// 成功
	b.cfg.Pool.ReportSuccess(key)

	// 解析 Usage(OpenAI 格式)
	usage := parseOpenAIUsage(body)

	return &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Usage:      usage,
	}, nil
}

// SendStreamRequest 发送流式请求,返回 chunk channel
// 流式响应是 SSE 格式:
//   data: {json}\n\n
//   data: [DONE]\n\n
func (b *Base) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      "keypool not configured",
		}
	}
	key, err := b.cfg.Pool.Acquire()
	if err != nil {
		return nil, nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      fmt.Sprintf("no available key: %v", err),
		}
	}

	// 流式超时比非流式长
	streamTimeout := b.cfg.Timeout
	if streamTimeout < 120*time.Second {
		streamTimeout = 120 * time.Second
	}
	client := &http.Client{Timeout: streamTimeout}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/chat/completions",
		bytes.NewReader(req.Body))
	if err != nil {
		return nil, nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      err.Error(),
		}
	}
	httpReq.Header.Set("Authorization", "Bearer "+key.Key)
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
		return nil, nil, &provider.ProviderError{
			ProviderName: b.cfg.Name, ErrorType: errType,
			Message: err.Error(),
		}
	}

	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		errType := provider.ClassifyError(httpResp.StatusCode)
		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, 0)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		return nil, nil, &provider.ProviderError{
			ProviderName: b.cfg.Name,
			StatusCode:   httpResp.StatusCode,
			ErrorType:    errType,
			Message:      fmt.Sprintf("upstream returned %d", httpResp.StatusCode),
			RawError:     body,
		}
	}

	// 流式响应开始 — 上报 Key 成功
	b.cfg.Pool.ReportSuccess(key)

	ch := make(chan *provider.StreamChunk, 16)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()
		reader := bufio.NewReader(httpResp.Body)

		// SSE 格式:每行 "data: {...}" 直到 "data: [DONE]"
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					ch <- &provider.StreamChunk{Err: io.EOF}
				} else {
					ch <- &provider.StreamChunk{Err: err}
				}
				return
			}
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				continue
			}
			// 只转发 data: 行(包含原始 SSE 格式)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[5:])
			if bytes.Equal(payload, []byte("[DONE]")) {
				ch <- &provider.StreamChunk{Data: line}
				ch <- &provider.StreamChunk{Err: io.EOF}
				return
			}
			// 把 "data: {...}\n\n" 还原成 SSE 事件
			ch <- &provider.StreamChunk{Data: append(line, '\n', '\n')}
		}
	}()

	return ch, &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
	}, nil
}

// HealthCheck 简单 GET 请求(检查 endpoint 可达)
func (b *Base) HealthCheck(ctx context.Context) error {
	hcTimeout := 5 * time.Second
	hctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return err
	}
	if b.cfg.Pool != nil {
		if k, err := b.cfg.Pool.Acquire(); err == nil {
			req.Header.Set("Authorization", "Bearer "+k.Key)
			defer b.cfg.Pool.ReportSuccess(k)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
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

// parseOpenAIUsage 从 OpenAI Chat Completions 响应中抽取 usage
// 格式: {"usage": {"prompt_tokens": N, "completion_tokens": M, "total_tokens": T}}
func parseOpenAIUsage(body []byte) *provider.Usage {
	var resp struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	raw := map[string]interface{}{
		"prompt_tokens":     resp.Usage.PromptTokens,
		"completion_tokens": resp.Usage.CompletionTokens,
		"total_tokens":      resp.Usage.TotalTokens,
	}
	return &provider.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		RawUsage:         raw,
	}
}

// parseRetryAfter 解析 Retry-After header(秒数或 HTTP 日期)
// 简化:只支持秒数
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
