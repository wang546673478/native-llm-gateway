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

	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// Engine 是 Gateway 的代理引擎
type Engine struct {
	logger        *zap.Logger
	router        *router.Router
	usage         UsageRecorder
	metrics       MetricsRecorder
	breaker       CircuitReporter
	tokenRecorder TokenUsageRecorder // P13: TPM 计数回调(可选)
	authn         *auth.Authenticator // P19: Provider binding 检查
	maxRetry      int
}

// Config 构造 Engine 的配置
type Config struct {
	Router        *router.Router
	Logger        *zap.Logger
	Usage         UsageRecorder
	Metrics       MetricsRecorder
	Breaker       CircuitReporter
	TokenRecorder TokenUsageRecorder // P13: 可选
	Authenticator *auth.Authenticator // P19: 可选,绑定 Provider 检查
	MaxRetry      int // 最大 failover 次数,默认 3
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
		logger:        cfg.Logger,
		router:        cfg.Router,
		usage:         cfg.Usage,
		metrics:       cfg.Metrics,
		breaker:       cfg.Breaker,
		tokenRecorder: cfg.TokenRecorder,
		authn:         cfg.Authenticator,
		maxRetry:      cfg.MaxRetry,
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

	// 2. 提取 model + stream(body 里的 stream 字段是最终依据 — 客户端说了算)
	model, bodyStream, err := extractModelAndStream(body)
	if err != nil || model == "" {
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "request body must include non-empty 'model' field")
		return
	}
	isStream = bodyStream

	// 2.5 P63: 检查 Gateway Key 的 AllowedModels 白名单
	// 在路由解析之前拒绝 → 节省 failover 开销,也避免误用配额
	if e.authn != nil {
		if gkVal, ok := c.Get("gateway_key"); ok {
			if gk, ok := gkVal.(*auth.GatewayKey); ok {
				if err := e.authn.CheckAllowed(gk, model); err != nil {
					e.logger.Warn("model not allowed for key",
						zap.String("key", gk.Name),
						zap.String("model", model),
						zap.Strings("allowed", gk.AllowedModels),
						zap.String("trace_id", traceID),
					)
					c.JSON(http.StatusForbidden, gin.H{
						"error": gin.H{
							"type":    "model_not_allowed",
							"message": fmt.Sprintf("key %q does not allow model %q (allowed: %v)", gk.Name, model, gk.AllowedModels),
						},
					})
					return
				}
			}
		}
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

	// 4. 路由(failover iterator) — P34: 把 GatewayKey 绑定的 ProviderKeyIDs 传给 Router
	var routeOpts []router.RouteOption
	if gkVal, ok := c.Get("gateway_key"); ok {
		if gk, ok := gkVal.(*auth.GatewayKey); ok && len(gk.ProviderKeyIDs) > 0 {
			routeOpts = append(routeOpts, router.WithProviderKeyIDs(gk.ProviderKeyIDs))
		}
	}
	iter, err := e.router.Route(ctx, req, routeOpts...)
	if err != nil {
		e.logger.Warn("no route",
			zap.String("model", model),
			zap.String("trace_id", traceID),
			zap.Error(err))
		writeJSONError(c, http.StatusServiceUnavailable, "no_route",
			fmt.Sprintf("no available provider for model %q", model))
		return
	}

	// 4.5 P19: 检查 Gateway Key 是否绑定了 Provider
	// 若 key.Providers 非空,则只能路由到那些 Provider 之一;若路由解析到不在列表里的,直接 403
	if gkVal, ok := c.Get("gateway_key"); ok {
		if gk, ok := gkVal.(*auth.GatewayKey); ok && len(gk.Providers) > 0 {
			// 取路由结果看 ProviderName;failover iterator 第一个就是
			probeResult, probeErr := iter.Next()
			if probeErr == nil {
				if e.authn != nil && e.authn.CheckProvider(gk, probeResult.ProviderName) != nil {
					e.logger.Warn("key provider mismatch",
						zap.String("key", gk.Name),
						zap.Strings("key_providers", gk.Providers),
						zap.String("routed_provider", probeResult.ProviderName),
						zap.String("model", model),
						zap.String("trace_id", traceID),
					)
					c.JSON(http.StatusForbidden, gin.H{
						"error": gin.H{
							"type":    "key_provider_mismatch",
							"message": fmt.Sprintf("key %q is bound to providers %v but request routes to %q",
								gk.Name, gk.Providers, probeResult.ProviderName),
						},
					})
					return
				}
				// 检查通过,把这个候选放回 iterator(不太好做,所以重置当前 idx)
				// 简单做法:迭代器不支持 reset,改为手动用 probeResult 开始循环
				e.runWithFirstResult(c, ctx, req, iter, probeResult)
				return
			}
		}
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
			e.logger.Info("P54 DEBUG: no more candidates", zap.Error(err))
			// 没更多候选
			break
		}
		e.logger.Info("P54 DEBUG: trying",
			zap.String("provider", result.ProviderName),
			zap.String("key_id", result.Key.ID),
			zap.String("key_status", string(result.Key.Status)),
			zap.String("model", result.ModelID),
			zap.Int("attempt", attempts))

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
			ok, streamUsage, perr := e.doStream(ctx, c, pv, req, result)
			e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), true, perr)
			if ok {
				e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, "", isStream, streamUsage)
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
// 返回 (success, usage, lastErr)
// success=true 表示流已经成功传完
// usage 从流最后一个 chunk 抽出(可能是 nil,如果上游没发 usage 字段)
func (e *Engine) doStream(
	ctx context.Context,
	c *gin.Context,
	pv provider.Provider,
	req *provider.Request,
	result *router.RouteResult,
) (bool, *provider.Usage, *provider.ProviderError) {
	chunkCh, headerResp, err := pv.SendStreamRequest(ctx, req)
	if err != nil {
		var pe *provider.ProviderError
		if errors.As(err, &pe) {
			e.reportKeyError(result, pe)
			return false, nil, pe
		}
		return false, nil, &provider.ProviderError{
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

	// P42: headerResp.Usage 由各 provider 的 goroutine 在 close(ch) 前填好
	// 我们 drain 完 channel 后安全读取
	var streamUsage *provider.Usage
	if headerResp != nil {
		streamUsage = headerResp.Usage
	}
	return true, streamUsage, nil
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
	e.recordUsageWithTokens(req, result, latency, resp.StatusCode, "", req.IsStream, resp.Usage)
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

// recordUsage 异步上报用量(无 token 计数)
func (e *Engine) recordUsage(
	req *provider.Request,
	result *router.RouteResult,
	latency time.Duration,
	statusCode int,
	errorType string,
	isStream bool,
) {
	e.recordUsageWithTokens(req, result, latency, statusCode, errorType, isStream, nil)
}

// recordUsageWithTokens 上报用量(含 token 数,如果有 resp.Usage)
func (e *Engine) recordUsageWithTokens(
	req *provider.Request,
	result *router.RouteResult,
	latency time.Duration,
	statusCode int,
	errorType string,
	isStream bool,
	u *provider.Usage,
) {
	r := &UsageRecord{
		TraceID:      req.TraceID,
		GatewayKeyID: req.GatewayKeyID,
		ProviderName: result.ProviderName,
		ModelID:      result.ModelID,
		Protocol:     string(result.Protocol),
		LatencyMs:    latency.Milliseconds(),
		StatusCode:   statusCode,
		ErrorType:    errorType,
		IsStream:     isStream,
	}
	// P48: 计费来源 — 优先用这把 key 自己的 billing_source(provider 上的是默认值)
	if result.Key != nil && result.Key.BillingSource != "" {
		r.BillingSource = result.Key.BillingSource
	} else if e.router != nil {
		if mgr := e.router.Manager(); mgr != nil {
			r.BillingSource = mgr.BillingSourceFor(result.ProviderName)
		}
	} else {
		r.BillingSource = "api"
	}
	if u != nil {
		r.InputTokens = u.PromptTokens
		r.OutputTokens = u.CompletionTokens
		r.TotalTokens = u.TotalTokens
		// P37 + P40: 算 cost(支持 cache pricing,单位 CNY ¥)
		//   cost = prompt * input_cost
		//        + cache_creation * cache_create_cost(0 则 fallback 到 input_cost)
		//        + cache_read * cache_read_cost(0 则跳过)
		//        + completion * output_cost
		if mgr := e.router.Manager(); mgr != nil {
			c := mgr.CostFor(result.ProviderName, result.ModelID)
			hasAnyCost := c.CostPer1kInput > 0 || c.CostPer1kOutput > 0 ||
				c.CostPer1kCacheRead > 0 || c.CostPer1kCacheCreation > 0
			if hasAnyCost {
				cacheCreateCost := c.CostPer1kCacheCreation
				if cacheCreateCost == 0 {
					cacheCreateCost = c.CostPer1kInput // fallback
				}
				r.Cost = (float64(u.PromptTokens)/1000.0)*c.CostPer1kInput +
					(float64(u.CacheCreationTokens)/1000.0)*cacheCreateCost +
					(float64(u.CacheReadTokens)/1000.0)*c.CostPer1kCacheRead +
					(float64(u.CompletionTokens)/1000.0)*c.CostPer1kOutput
			}
		}
		// 同时记入 metrics
		if mr, ok := e.metrics.(interface {
			RecordTokens(string, int, int)
		}); ok {
			mr.RecordTokens(result.ProviderName, u.PromptTokens, u.CompletionTokens)
		}
		// TPM 计数:回填到 Authenticator
		if e.tokenRecorder != nil && req.GatewayKeyID != "" && r.TotalTokens > 0 {
			e.tokenRecorder.RecordUsage(req.GatewayKeyID, int64(r.TotalTokens))
		}
	}
	e.usage.Record(r)
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

// runWithFirstResult 用已经 Next 出来的第一个 result 开始循环
// (P19:把 provider-binding 检查 pass 的第一个候选"放回"循环)
func (e *Engine) runWithFirstResult(c *gin.Context, ctx context.Context, req *provider.Request, iter *router.RouteIterator, first *router.RouteResult) {
	var lastErr *provider.ProviderError
	attempts := 0

	// 先处理 first
	if e.tryOneCandidate(c, ctx, req, first, &lastErr) {
		return
	}
	if lastErr != nil && !errorIsRetryable(lastErr) {
		return
	}

	// 再继续 Next 剩下的
	for {
		if attempts >= e.maxRetry-1 {
			break
		}
		attempts++
		result, err := iter.Next()
		if err != nil {
			break
		}
		if e.tryOneCandidate(c, ctx, req, result, &lastErr) {
			return
		}
		if lastErr != nil && !errorIsRetryable(lastErr) {
			return
		}
	}
	e.handleAllFailed(c, req, lastErr, req.TraceID)
}

// tryOneCandidate 试一次候选。返回 true 表示成功处理(应该 return)
// lastErr 在错误时被更新
func (e *Engine) tryOneCandidate(
	c *gin.Context,
	ctx context.Context,
	req *provider.Request,
	result *router.RouteResult,
	lastErr **provider.ProviderError,
) bool {
	req.Headers.Set("X-Request-Id", req.TraceID)
	if result.Key != nil {
		req.Headers.Set("Authorization", "Bearer "+result.Key.Key)
	}
	pv, ok := e.router.Manager().Get(result.ProviderName)
	if !ok {
		*lastErr = &provider.ProviderError{
			ProviderName: result.ProviderName,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      "provider instance not found",
		}
		return false
	}

	start := time.Now()
	if req.IsStream {
		ok, streamUsage, perr := e.doStream(ctx, c, pv, req, result)
		e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), true, perr)
		if ok {
			e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, "", true, streamUsage)
			return true
		}
		*lastErr = perr
	} else {
		resp, perr := e.doRequest(ctx, pv, req, result)
		e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), false, perr)
		if perr == nil && resp != nil {
			e.writeNonStreamResponse(c, req, resp, result, time.Since(start))
			return true
		}
		*lastErr = perr
	}
	return false
}
// 总是回写到响应 header,方便客户端链路追踪
func extractOrGenTraceID(c *gin.Context) string {
	id := c.GetHeader("X-Request-Id")
	if id == "" {
		id = uuid.NewString()
	}
	c.Writer.Header().Set("X-Request-Id", id)
	return id
}
