// Package google 实现 Google Generative AI 兼容协议基座。
// 当前没有内置厂商或动态 Relay 使用该包。
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
	"sync/atomic"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// Config 构造 Provider 所需的最小配置
type Config struct {
	Name     string
	Endpoint string // e.g. https://generativelanguage.googleapis.com/v1beta
	Timeout  time.Duration
	// StreamTimeoutFloor 流式请求超时下限(默认 120s,见 NewBase)。
	// Google 长生成(thinking/长上下文)可覆盖此值调高;<=0 用协议默认。
	StreamTimeoutFloor time.Duration
	Pool               *keypool.Pool
}

// Base Google 协议基类
type Base struct {
	cfg    Config
	client *http.Client
	pool   atomic.Pointer[keypool.Pool]
}

// newReportedError marks an error after this provider has applied the
// corresponding result to its key pool.  The proxy uses the marker to avoid
// counting one upstream response twice.
func newReportedError(providerName string, status int, errType provider.ErrorType, msg string, rawErr ...[]byte) *provider.ProviderError {
	pe := provider.NewError(providerName, status, errType, msg, rawErr...)
	pe.KeyPoolReported = true
	return pe
}

// NewBase 构造 Base
func NewBase(cfg Config) *Base {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if cfg.StreamTimeoutFloor <= 0 {
		cfg.StreamTimeoutFloor = 120 * time.Second // google 协议默认 2 分钟
	}
	b := &Base{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
	b.pool.Store(cfg.Pool)
	return b
}

func (b *Base) keyPool() *keypool.Pool { return b.pool.Load() }

// SendRequest POST {endpoint}/models/{model}:generateContent
// 鉴权: x-goog-api-key header(官方推荐;不用 ?key= query 是为了不让 key 进 URL 日志)
// body 原样透传(Gateway 已经抽出了 model,这里直接从 body 找 model)
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	pool := b.keyPool()
	if pool == nil {
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 优先用路由层已 acquire 的 key(双 acquire 会标错冷却 key)
	key := req.Key
	var err error
	if key == nil {
		key, err = pool.AcquireForProtocol(string(provider.ProtocolGoogle))
		if err != nil {
			return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	// Google 需要把 model 拼到 URL path 里
	endpoint := b.buildEndpoint(req.Model, false)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 官方推荐用 header 而不是 ?key= query
	httpReq.Header.Set("x-goog-api-key", key.Key)
	if req.TraceID != "" {
		httpReq.Header.Set("X-Request-Id", req.TraceID)
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		errType := provider.ClassifyTransportError(ctx, err)
		if provider.ShouldReportKeyPool(ctx, errType) {
			pool.ReportError(key, string(errType))
		}
		return nil, newReportedError(b.cfg.Name, 0, errType, err.Error())
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		errType := provider.ClassifyTransportError(ctx, err)
		if provider.ShouldReportKeyPool(ctx, errType) {
			pool.ReportError(key, string(errType))
		}
		return nil, newReportedError(b.cfg.Name, 0, errType, err.Error())
	}

	if httpResp.StatusCode >= 400 {
		// P49: 带 body 检测 quota
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)
		if errType == provider.ErrorTypeRateLimit {
			// P-429: 与 openai/anthropic 一致,解析 Retry-After 头用冷却时长
			// (核心源 provider.ParseRetryAfter;google 之前丢弃头部用 0)。
			// 注:google 不做 in-provider 重试(与 openai 同),只用于冷却上报。
			retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
			if provider.ShouldReportKeyPool(ctx, errType) {
				pool.ReportRateLimit(key, retryAfter)
			}
		} else {
			if provider.ShouldReportKeyPool(ctx, errType) {
				pool.ReportError(key, string(errType))
			}
		}
		return nil, newReportedError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
	}

	keyPoolReported := false
	if provider.ShouldReportKeyPool(ctx, provider.ErrorType("")) {
		pool.ReportSuccess(key)
		keyPoolReported = true
	}
	usage := parseGoogleUsage(body)

	return &provider.Response{
		StatusCode:      httpResp.StatusCode,
		Headers:         httpResp.Header,
		Body:            body,
		Usage:           usage,
		KeyPoolReported: keyPoolReported,
	}, nil
}

// SendStreamRequest 流式 Google 请求
func (b *Base) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	pool := b.keyPool()
	if pool == nil {
		return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 同 SendRequest — 优先用路由层已 acquire 的 key
	key := req.Key
	var err error
	if key == nil {
		key, err = pool.AcquireForProtocol(string(provider.ProtocolGoogle))
		if err != nil {
			return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	streamTimeout := b.cfg.Timeout
	if streamTimeout < b.cfg.StreamTimeoutFloor {
		streamTimeout = b.cfg.StreamTimeoutFloor
	}
	client := &http.Client{Timeout: streamTimeout}

	endpoint := b.buildEndpoint(req.Model, true)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", key.Key)
	if req.TraceID != "" {
		httpReq.Header.Set("X-Request-Id", req.TraceID)
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		errType := provider.ClassifyTransportError(ctx, err)
		if provider.ShouldReportKeyPool(ctx, errType) {
			pool.ReportError(key, string(errType))
		}
		return nil, nil, newReportedError(b.cfg.Name, 0, errType, err.Error())
	}

	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		// P49: 带 body 检测 quota
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)
		if errType == provider.ErrorTypeRateLimit {
			retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
			if provider.ShouldReportKeyPool(ctx, errType) {
				pool.ReportRateLimit(key, retryAfter)
			}
		} else {
			if provider.ShouldReportKeyPool(ctx, errType) {
				pool.ReportError(key, string(errType))
			}
		}
		return nil, nil, newReportedError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
	}

	keyPoolReported := false
	if provider.ShouldReportKeyPool(ctx, provider.ErrorType("")) {
		pool.ReportSuccess(key)
		keyPoolReported = true
	}

	ch := make(chan *provider.StreamChunk, 16)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()
		reader := bufio.NewReader(httpResp.Body)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Err: io.EOF}) {
						return
					}
				} else {
					if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Err: err}) {
						return
					}
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
			if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Data: lineBuf.Bytes()}) {
				return
			}
		}
	}()

	return ch, &provider.Response{
		StatusCode:      httpResp.StatusCode,
		Headers:         httpResp.Header,
		KeyPoolReported: keyPoolReported,
	}, nil
}

// HealthCheck GET {endpoint}/models
// 鉴权用 x-goog-api-key header(避免 URL 日志泄露)
func (b *Base) HealthCheck(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(b.cfg.Endpoint, "/") + "/models"
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	// HealthCheck is endpoint liveness only. Do not acquire a key or report a
	// synthetic success: a 401/405 proves the endpoint is reachable but says
	// nothing about any configured key, and touching the pool here can rotate or
	// mutate key state before a real request is made.
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

// ListModels GET {endpoint}/models,返回 models/* 去前缀后的模型 id。
func (b *Base) ListModels(ctx context.Context) ([]string, error) {
	pool := b.keyPool()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if pool != nil {
		if k, err := pool.AcquireForProtocol(string(provider.ProtocolGoogle)); err == nil {
			req.Header.Set("x-goog-api-key", k.Key)
			defer pool.ReportSuccess(k)
		}
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Close
func (b *Base) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// SetPool P30:让 Server 把从 DB 读出来的 Pool 注入到 Base
func (b *Base) SetPool(p *keypool.Pool) {
	b.pool.Store(p)
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

// parseGoogleUsage 从 Google 响应抽 usage
// 格式: {"usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": M, "totalTokenCount": T}}
//
// P65: 同时抽取顶层 "modelVersion" 字段(Gemini 用 modelVersion 而非 model)
// proxy 写入 UsageRecord.ModelID 时优先用此字段覆盖客户端请求的 model
func parseGoogleUsage(body []byte) *provider.Usage {
	var resp struct {
		ModelVersion  string `json:"modelVersion"`
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
		Model:            resp.ModelVersion, // P65: 上游响应的真实 model 名
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
