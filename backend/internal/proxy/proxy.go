// Package proxy — Engine 主入口
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// Engine 是 Gateway 的代理引擎
type Engine struct {
	logger   *zap.Logger
	router   *router.Router
	usage    UsageRecorder
	metrics  MetricsRecorder
	breaker  CircuitReporter
	maxRetry int
}

// Config 构造 Engine 的配置
type Config struct {
	Router   *router.Router
	Logger   *zap.Logger
	Usage    UsageRecorder
	Metrics  MetricsRecorder
	Breaker  CircuitReporter
	MaxRetry int // 最大 failover 次数,默认 3
}

// NewEngine 构造 Proxy Engine
func NewEngine(cfg Config) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.Usage == nil {
		cfg.Usage = NoopUsageRecorder{}
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NoopMetricsRecorder{}
	}
	if cfg.Breaker == nil {
		cfg.Breaker = NoopCircuitReporter{}
	}
	if cfg.MaxRetry <= 0 {
		cfg.MaxRetry = 3
	}
	return &Engine{
		logger:   cfg.Logger,
		router:   cfg.Router,
		usage:    cfg.Usage,
		metrics:  cfg.Metrics,
		breaker:  cfg.Breaker,
		maxRetry: cfg.MaxRetry,
	}
}

// HandleRequest 处理非流式代理请求
func (e *Engine) HandleRequest(c *gin.Context) {
	e.handle(c, false)
}

// HandleStreamRequest 处理流式代理请求
func (e *Engine) HandleStreamRequest(c *gin.Context) {
	e.handle(c, true)
}

// handle 是 HandleRequest / HandleStreamRequest 的共同主体
func (e *Engine) handle(c *gin.Context, isStream bool) {
	ctx := c.Request.Context()
	traceID := extractOrGenTraceID(c)

	// 1. 读取 body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		e.logger.Error("read body", zap.Error(err), zap.String("trace_id", traceID))
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}

	// 2. 提取 model + stream
	model, _, err := extractModelAndStream(body)
	if err != nil || model == "" {
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "request body must include non-empty 'model' field")
		return
	}

	// 3. 构造 Provider.Request(Body 透传)
	req := &provider.Request{
		Method:       c.Request.Method,
		Path:         c.Request.URL.Path,
		Headers:      c.Request.Header.Clone(),
		Body:         body,
		Model:        model,
		IsStream:     isStream,
		GatewayKeyID: c.GetString("gateway_key_id"),
		TraceID:      traceID,
	}

	// 4. 路由(failover iterator)
	iter, err := e.router.Route(ctx, req)
	if err != nil {
		e.logger.Warn("no route",
			zap.String("model", model),
			zap.String("trace_id", traceID),
			zap.Error(err))
		writeJSONError(c, http.StatusServiceUnavailable, "no_route",
			fmt.Sprintf("no available provider for model %q", model))
		return
	}

	// 5. 依次尝试,failover
	var lastErr *provider.ProviderError
	attempts := 0
	for {
		if attempts >= e.maxRetry {
			break
		}
		attempts++

		result, err := iter.Next()
		if err != nil {
			// 没更多候选
			break
		}

		// 用候选的 provider + key 调 Provider
		req.Headers.Set("X-Request-Id", traceID)
		if result.Key != nil {
			req.Headers.Set("Authorization", "Bearer "+result.Key.Key)
		}

		// 选 Provider 实例
		pv, ok := e.router.Manager().Get(result.ProviderName)
		if !ok {
			continue
		}

		start := time.Now()
		if isStream {
			ok, perr := e.doStream(ctx, c, pv, req, result)
			e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), true, perr)
			if ok {
				e.recordUsage(req, result, time.Since(start), http.StatusOK, "", isStream)
				return
			}
			lastErr = perr
			if perr != nil && !errorIsRetryable(perr) {
				break
			}
		} else {
			resp, perr := e.doRequest(ctx, pv, req, result)
			e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), false, perr)
			if perr == nil && resp != nil {
				e.writeNonStreamResponse(c, req, resp, result, time.Since(start))
				return
			}
			lastErr = perr
			if perr != nil && !errorIsRetryable(perr) {
				break
			}
		}
	}

	// 所有尝试都失败
	e.handleAllFailed(c, req, lastErr, traceID)
}

// doRequest 调一次 Provider.SendRequest,处理 KeyPool 报告和 Circuit 上报
func (e *Engine) doRequest(
	ctx context.Context,
	pv provider.Provider,
	req *provider.Request,
	result *router.RouteResult,
) (*provider.Response, *provider.ProviderError) {
	resp, err := pv.SendRequest(ctx, req)
	if err == nil {
		e.breaker.RecordSuccess(result.ProviderName)
		e.reportKeySuccess(result)
		return resp, nil
	}

	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		e.reportKeyError(result, pe)
		switch pe.ErrorType {
		case provider.ErrorTypeServerError, provider.ErrorTypeTimeout, provider.ErrorTypeConnection:
			e.breaker.RecordFailure(result.ProviderName, string(pe.ErrorType))
		case provider.ErrorTypeRateLimit:
			// Key Pool 会自动冷却这个 Key,无需 breaker 上报
		}
		return nil, pe
	}

	// 非 ProviderError 的错误(例如网络层未到 Provider)
	e.breaker.RecordFailure(result.ProviderName, "unknown")
	return nil, &provider.ProviderError{
		ProviderName: result.ProviderName,
		ErrorType:    provider.ErrorTypeConnection,
		Message:      err.Error(),
	}
}

// doStream 调一次 Provider.SendStreamRequest
// 返回 (success, lastErr)
// success=true 表示流已经成功传完
func (e *Engine) doStream(
	ctx context.Context,
	c *gin.Context,
	pv provider.Provider,
	req *provider.Request,
	result *router.RouteResult,
) (bool, *provider.ProviderError) {
	chunkCh, headerResp, err := pv.SendStreamRequest(ctx, req)
	if err != nil {
		var pe *provider.ProviderError
		if errors.As(err, &pe) {
			e.reportKeyError(result, pe)
			return false, pe
		}
		return false, &provider.ProviderError{
			ProviderName: result.ProviderName,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      err.Error(),
		}
	}

	// 流式响应开始 — 此后不可 failover
	e.breaker.RecordSuccess(result.ProviderName)
	e.reportKeySuccess(result)

	// 设置 SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	if req.TraceID != "" {
		c.Writer.Header().Set("X-Request-Id", req.TraceID)
	}
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	flusher, _ := c.Writer.(http.Flusher)
	canFlush := flusher != nil

	for chunk := range chunkCh {
		if chunk.Err != nil {
			if errors.Is(chunk.Err, io.EOF) {
				break
			}
			// 流中途错误:写一个 error event 给客户端,然后退出
			e.logger.Warn("stream mid-error",
				zap.String("provider", result.ProviderName),
				zap.Error(chunk.Err))
			fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":{\"type\":\"stream_error\",\"message\":%q}}\n\n",
				chunk.Err.Error())
			if canFlush {
				flusher.Flush()
			}
			break
		}
		if len(chunk.Data) == 0 {
			continue
		}
		// chunk.Data 已经是 SSE data 行的内容(Provider 负责格式化)
		if _, err := c.Writer.Write(chunk.Data); err != nil {
			e.logger.Warn("write stream chunk", zap.Error(err))
			break
		}
		if canFlush {
			flusher.Flush()
		}
	}

	// headerResp 包含头信息和(可能的) Usage
	_ = headerResp
	return true, nil
}

// writeNonStreamResponse 把 Provider Response 原样写回客户端
func (e *Engine) writeNonStreamResponse(
	c *gin.Context,
	req *provider.Request,
	resp *provider.Response,
	result *router.RouteResult,
	latency time.Duration,
) {
	copyResponseHeaders(c, resp.Headers)
	c.Writer.WriteHeader(resp.StatusCode)
	if _, err := c.Writer.Write(resp.Body); err != nil {
		e.logger.Warn("write response", zap.Error(err))
	}
	e.recordUsage(req, result, latency, resp.StatusCode, "", req.IsStream)
}

// handleAllFailed 所有 failover 都失败
func (e *Engine) handleAllFailed(
	c *gin.Context,
	req *provider.Request,
	lastErr *provider.ProviderError,
	traceID string,
) {
	if lastErr == nil {
		writeJSONError(c, http.StatusBadGateway, "gateway_error", "all providers failed")
		return
	}

	// invalid_request / auth 等不应 failover 的错误:直接返回 Provider 原始错误
	if !errorIsRetryable(lastErr) {
		// 尽量按 Provider 状态码透传
		c.Writer.Header().Set("X-Request-Id", traceID)
		c.Writer.WriteHeader(lastErr.StatusCode)
		if len(lastErr.RawError) > 0 {
			c.Writer.Write(lastErr.RawError)
		} else {
			writeJSONError(c, lastErr.StatusCode, string(lastErr.ErrorType), lastErr.Message)
		}
		return
	}

	writeJSONError(c, http.StatusBadGateway, "gateway_error",
		fmt.Sprintf("all providers failed: %s", lastErr.Message))
}

// recordUsage 异步上报用量
func (e *Engine) recordUsage(
	req *provider.Request,
	result *router.RouteResult,
	latency time.Duration,
	statusCode int,
	errorType string,
	isStream bool,
) {
	e.usage.Record(&UsageRecord{
		TraceID:      req.TraceID,
		GatewayKeyID: req.GatewayKeyID,
		ProviderName: result.ProviderName,
		ModelID:      result.ModelID,
		Protocol:     string(result.Protocol),
		LatencyMs:    latency.Milliseconds(),
		StatusCode:   statusCode,
		ErrorType:    errorType,
		IsStream:     isStream,
	})
}

func (e *Engine) recordMetrics(providerName string, statusCode int, latency time.Duration, isStream bool, perr *provider.ProviderError) {
	errType := ""
	if perr != nil {
		errType = string(perr.ErrorType)
	}
	e.metrics.RecordRequest(providerName, statusCode, latency, isStream, errType)
}

// reportKeySuccess / reportKeyError 把 Key Pool 反馈一并处理
// 注意:RouteResult.Key 在 RouteIterator.Next() 里 acquire 出来,
// 这里直接调用 Pool.ReportXxx。但我们没有直接持有 Pool 引用,
// 所以走 router 提供的 helper(后续 P6/P9 完善)。
func (e *Engine) reportKeySuccess(result *router.RouteResult) {
	if pool := e.router.Pool(result.ProviderName); pool != nil && result.Key != nil {
		pool.ReportSuccess(result.Key)
	}
}

func (e *Engine) reportKeyError(result *router.RouteResult, pe *provider.ProviderError) {
	if pool := e.router.Pool(result.ProviderName); pool != nil && result.Key != nil {
		switch pe.ErrorType {
		case provider.ErrorTypeRateLimit:
			pool.ReportRateLimit(result.Key, pe.RetryAfter)
		default:
			pool.ReportError(result.Key, string(pe.ErrorType))
		}
	}
}

// statusFromErr 从 error 提取状态码,失败返回 0
func statusFromErr(pe *provider.ProviderError) int {
	if pe == nil {
		return http.StatusOK
	}
	return pe.StatusCode
}

// extractOrGenTraceID 提取 X-Request-Id,没有则生成
// 总是回写到响应 header,方便客户端链路追踪
func extractOrGenTraceID(c *gin.Context) string {
	id := c.GetHeader("X-Request-Id")
	if id == "" {
		id = uuid.NewString()
	}
	c.Writer.Header().Set("X-Request-Id", id)
	return id
}
