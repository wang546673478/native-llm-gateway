// Package google 实现 Google Generative AI 兼容协议(Gemini)
// 对应规格书 8.4
//
// 特点:
//   - Auth 通过 URL query ?key={api_key}
//   - 端点:/v1beta/models/{model}:generateContent 或 :streamGenerateContent
//   - Body 格式:{contents: [{parts: [{text: "..."}], role: "user"}]}
//   - Usage 字段名不同:promptTokenCount / candidatesTokenCount / totalTokenCount
package google

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// Config 构造 Provider 所需的最小配置
type Config struct {
	Name     string
	Endpoint string // e.g. https://generativelanguage.googleapis.com/v1beta
	Timeout  time.Duration
	Pool     *keypool.Pool
}

// Base Google 协议基类
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

// SendRequest POST {endpoint}/models/{model}:generateContent?key={apiKey}
// body 原样透传(Gateway 已经抽出了 model,这里直接从 body 找 model)
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, "keypool not configured")
	}
	key, err := b.cfg.Pool.Acquire()
	if err != nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
	}

	// Google 需要把 model 拼到 URL path 里
	endpoint := b.buildEndpoint(req.Model, false)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, b.newError(0, provider.ErrorTypeConnection, err.Error())
	}
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
		errType := provider.ClassifyError(httpResp.StatusCode)
		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, 0)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		return nil, b.newError(httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
	}

	b.cfg.Pool.ReportSuccess(key)
	usage := parseGoogleUsage(body)

	return &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Usage:      usage,
	}, nil
}

// SendStreamRequest 流式 Google 请求
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

	endpoint := b.buildEndpoint(req.Model, true)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, nil, b.newError(0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
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
		errType := provider.ClassifyError(httpResp.StatusCode)
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
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()
		reader := bufio.NewReader(httpResp.Body)
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
			// Google 流式响应是 JSON 数组,用 [,{...},{...}] 形式
			// 每行一个对象,加 data: 前缀适配 SSE 客户端
			var lineBuf bytes.Buffer
			lineBuf.WriteString("data: ")
			lineBuf.Write(line)
			lineBuf.WriteString("\n\n")
			ch <- &provider.StreamChunk{Data: lineBuf.Bytes()}
		}
	}()

	return ch, &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
	}, nil
}

// HealthCheck GET {endpoint}/models?key={apiKey}
func (b *Base) HealthCheck(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(b.cfg.Endpoint, "/") + "/models"
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if b.cfg.Pool != nil {
		if k, err := b.cfg.Pool.Acquire(); err == nil {
			q := req.URL.Query()
			q.Set("key", k.Key)
			req.URL.RawQuery = q.Encode()
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

// Close
func (b *Base) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// buildEndpoint 拼接 URL: {endpoint}/models/{model}:generateContent?key={apiKey}
// 注意:stream=true 时用 :streamGenerateContent
// 这里简化为:调用方自己选择 stream vs 非 stream,通过 stream 参数
// (当前 SendRequest 调非流式,SendStreamRequest 调流式)
func (b *Base) buildEndpoint(model string, stream bool) string {
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	return strings.TrimRight(b.cfg.Endpoint, "/") + "/models/" + url.PathEscape(model) + ":" + action
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

// parseGoogleUsage 从 Google 响应抽 usage
// 格式: {"usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": M, "totalTokenCount": T}}
func parseGoogleUsage(body []byte) *provider.Usage {
	var resp struct {
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.UsageMetadata == nil {
		return nil
	}
	return &provider.Usage{
		PromptTokens:     resp.UsageMetadata.PromptTokenCount,
		CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		RawUsage: map[string]interface{}{
			"promptTokenCount":     resp.UsageMetadata.PromptTokenCount,
			"candidatesTokenCount": resp.UsageMetadata.CandidatesTokenCount,
			"totalTokenCount":      resp.UsageMetadata.TotalTokenCount,
		},
	}
}
