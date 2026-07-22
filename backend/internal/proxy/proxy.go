// Package proxy — Engine 主入口
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/accesslog"
	"github.com/wang546673478/native-llm-gateway/internal/auth"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// maxConcurrentStreams 全局并发流式响应上限(spec §3.3 / F4)
// 超过此值的新流式请求只记 metadata,不缓存 response body。
const maxConcurrentStreams = 1000

// Engine 是 Gateway 的代理引擎
type Engine struct {
	logger        *zap.Logger
	router        *router.Router
	usage         UsageRecorder
	metrics       MetricsRecorder
	breaker       CircuitReporter
	tokenRecorder TokenUsageRecorder  // P13: TPM 计数回调(可选)
	authn         *auth.Authenticator // P19: Provider binding 检查
	accessLog     *accesslog.Recorder // P67: 接入日志 Recorder(可选,启用时为非 nil)
	maxRetry      int
	// streamBuf 持有当前正在累积的流式响应 buffer,key 是 traceID。
	// Task 7: 配合 streamCnt 实现 F4 全局 1000 上限。
	streamBuf sync.Map
	streamCnt int64 // atomic counter — 保护 maxConcurrentStreams 上限
}

// streamAcc 是单条流式响应的累积 buffer + truncated 标记。
//
// Mutex 保护 buf/truncated;Engine 层 streamBuf 提供 traceID 维度查找,
// streamCnt 提供全局维度计数。两个维度是不同并发问题,不能合并。
type streamAcc struct {
	sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

// Config 构造 Engine 的配置
type Config struct {
	Router        *router.Router
	Logger        *zap.Logger
	Usage         UsageRecorder
	Metrics       MetricsRecorder
	Breaker       CircuitReporter
	TokenRecorder TokenUsageRecorder  // P13: 可选
	Authenticator *auth.Authenticator // P19: 可选,绑定 Provider 检查
	AccessLog     *accesslog.Recorder // P67: 可选,nil 表示未启用
	MaxRetry      int                 // 最大 failover 次数,默认 3
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
		accessLog:     cfg.AccessLog,
		maxRetry:      cfg.MaxRetry,
	}
}

// HandleRequest 处理非流式代理请求
// tryDefaultModelFallback 尝试用 client key 的 default_model 替换 model 名
// 返回替换后的 model 名,空字符串表示不需要/无法 fallback
//
// 调用方应该:
//   1. 用返回值更新自己的 model 变量
//   2. 重写 req.Model 和 req.Body(用 rewriteModelField)
//   3. 重新调 router.Route
//
// 检查项:
//   - client key 必须有 DefaultModel 配置
//   - DefaultModel != 当前 model(避免无意义的自循环)
//   - DefaultModel 必须经过 CheckAllowed(防止 fallback 绕过白名单)
func (e *Engine) tryDefaultModelFallback(c *gin.Context, currentModel string, req *provider.Request) string {
	if gkVal, ok := c.Get("gateway_key"); ok {
		if gk, ok := gkVal.(*auth.GatewayKey); ok && gk.DefaultModel != "" && gk.DefaultModel != currentModel {
			// fallback 必须本身在白名单里 — 防止 fallback 绕过白名单
			if e.authn != nil && e.authn.CheckAllowed(gk, gk.DefaultModel) != nil {
				return ""
			}
			req.Model = gk.DefaultModel
			req.Body, _ = rewriteModelField(req.Body, gk.DefaultModel)
			return gk.DefaultModel
		}
	}
	return ""
}

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

	// P67: 接入日志 — 入口建 entry,defer 统一 RecordAsync
	var entry *accesslog.AccessEntry
	if e.accessLog != nil {
		entry = &accesslog.AccessEntry{
			TraceID:        traceID,
			CreatedAt:      time.Now().UTC(),
			Method:         c.Request.Method,
			Path:           c.Request.URL.Path, // 不含 query string(spec F1)
			ClientIP:       c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			GatewayKeyID:   c.GetString("gateway_key_id"),
			GatewayKeyName: auth.KeyNameFromCtx(c),
			IsStream:       isStream,
		}
	}
	// 持有供 defer 使用 — entry / providerName / lastErr
	var (
		lastProviderName string
		lastErr          *provider.ProviderError
	)
	defer func() {
		if entry == nil || e.accessLog == nil {
			return
		}
		entry.StatusCode = c.Writer.Status()
		entry.ErrorType = classifyError(entry.StatusCode, lastProviderName == "", lastErr)
		// 无论成功还是失败,只要命中过 provider 就记录 — 成功路径同样需要可观测性(spec §1.2 F2/F5)
		if lastProviderName != "" {
			entry.ProviderName = lastProviderName
		}
		entry.LatencyMs = int(time.Since(entry.CreatedAt) / time.Millisecond)
		if entry.FinalModel == "" {
			entry.FinalModel = entry.RequestedModel
		}
		e.accessLog.RecordAsync(entry)
	}()

	// 1. 读取 body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		e.logger.Error("read body", zap.Error(err), zap.String("trace_id", traceID))
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	// P67: 写请求 body(同步,file-per-trace,失败也继续)
	if entry != nil && e.accessLog != nil {
		if p, _ := e.accessLog.WriteBody(traceID, "req", body); p != "" {
			entry.ReqBodyPath = p
			entry.ReqBodySize = len(body)
		}
	}

	// 2. 提取 model + stream(body 里的 stream 字段是最终依据 — 客户端说了算)
	model, bodyStream, err := extractModelAndStream(body)
	if err != nil || model == "" {
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "request body must include non-empty 'model' field")
		return
	}
	isStream = bodyStream
	if entry != nil {
		entry.RequestedModel = model
	}

	// 2.4 alias 解析:把请求里的 model 名(alias,如 claude-sonnet-4-5)解析成真实 model
	// 必须在 CheckAllowed 之前完成,否则白名单要列出所有 Claude Code 探测名才能用
	// 解决后:用户配置 allowed_models 用真实 model 名(MiniMax-M3 等),
	// Claude Code 发探测名(被 alias 解析后)也能通过白名单
	if e.router != nil {
		if target, ok := e.router.ResolveAlias(model); ok && target != model {
			if newBody, ok2 := rewriteModelField(body, target); ok2 {
				body = newBody
				e.logger.Debug("alias resolved",
					zap.String("alias", model),
					zap.String("target", target),
					zap.String("trace_id", traceID),
				)
			}
			model = target
		}
	}
	if entry != nil {
		entry.FinalModel = model
	}

	// 2.5 DefaultModel fallback + 白名单检查
	// 流程:
	//   a. 先尝试路由;失败 → 用 default_model 重试路由
	//   b. 路由成功后(原 model 或 fallback 后),走白名单 CheckAllowed
	//   c. CheckAllowed 失败 → 也试 fallback(因为 alias 命中场景下 model 是 alias 名,
	//      不在白名单,但用户希望"用 default_model")
	//
	// 这样:
	//   - 客户端发 claude-sonnet-4-5 / gpt-4o 等探测名(无 alias):
	//     Route ErrNoRoute → fallback → 用 default_model 走通
	//   - 客户端发 claude-sonnet-4-5(命中 alias 表,路由成功但 alias 名不在白名单):
	//     Route OK → CheckAllowed fail → fallback → 用 default_model 走通
	//   - 客户端发 glm-4.7(真实 model 但白名单不让):
	//     Route OK → CheckAllowed fail → fallback 到 default_model(假设 default_model 在白名单)
	//     如果 default_model 不在白名单 → 403

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
	if entry != nil {
		entry.Protocol = req.Headers.Get("anthropic-version") // best-effort
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
		// 4.1: Route 失败 → 试 default_model fallback
		if fb := e.tryDefaultModelFallback(c, model, req); fb != "" {
			model = fb
			iter, err = e.router.Route(ctx, req, routeOpts...)
		}
		if err != nil {
			e.logger.Warn("no route",
				zap.String("model", model),
				zap.String("trace_id", traceID),
				zap.Error(err))
			writeJSONError(c, http.StatusServiceUnavailable, "no_route",
				fmt.Sprintf("no available provider for model %q", model))
			return
		}
	}

	// 4.5: 白名单检查 → 失败时也试 default_model fallback(alias 命中场景)
	if e.authn != nil {
		if gkVal, ok := c.Get("gateway_key"); ok {
			if gk, ok := gkVal.(*auth.GatewayKey); ok {
				if err := e.authn.CheckAllowed(gk, model); err != nil {
					if fb := e.tryDefaultModelFallback(c, model, req); fb != "" && fb != model {
						// fallback 成功:重置 iter 用新的 model 重新路由
						model = fb
						e.logger.Info("default_model fallback (whitelist miss)",
							zap.String("requested_model", req.Model),
							zap.String("fallback_to", fb),
							zap.String("trace_id", traceID))
						iter, err = e.router.Route(ctx, req, routeOpts...)
						if err != nil {
							e.logger.Warn("no route after fallback",
								zap.String("model", model),
								zap.String("trace_id", traceID),
								zap.Error(err))
							writeJSONError(c, http.StatusServiceUnavailable, "no_route",
								fmt.Sprintf("no available provider for model %q", model))
							return
						}
					} else {
						// 真没 fallback 或 fallback 失败,返回 403
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
				// 记下第一个候选的 provider name,供 defer 写 ProviderName
				if probeResult != nil {
					lastProviderName = probeResult.ProviderName
				}
				// 检查通过,把这个候选放回 iterator(不太好做,所以重置当前 idx)
				// 简单做法:迭代器不支持 reset,改为手动用 probeResult 开始循环
				e.runWithFirstResult(c, ctx, req, iter, probeResult, &lastProviderName, &lastErr, entry)
				return
			}
		}
	}

	// 5. 依次尝试,failover
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
			ok, streamUsage, perr := e.doStream(ctx, c, pv, req, result, entry)
			e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), true, perr)
			if ok {
				e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, "", isStream, streamUsage)
				lastProviderName = result.ProviderName
				return
			}
			lastProviderName = result.ProviderName
			lastErr = perr
			if perr != nil && !errorIsRetryable(perr) {
				break
			}
		} else {
			resp, perr := e.doRequest(ctx, pv, req, result)
			e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), false, perr)
			if perr == nil && resp != nil {
				e.writeNonStreamResponse(c, req, resp, result, time.Since(start), entry)
				lastProviderName = result.ProviderName
				return
			}
			lastProviderName = result.ProviderName
			lastErr = perr
			if perr != nil && !errorIsRetryable(perr) {
				break
			}
		}
	}

	// 所有尝试都失败
	e.handleAllFailed(c, req, lastErr, traceID)
}

// classifyError 把 HTTP status + 上游错误翻译成 error_type 枚举(spec §1.2)
// Pure function — 不依赖 Engine 实例,方便单元测试。
//   - statusCode 来自 c.Writer.Status()
//   - providerEmpty 表示没成功路由到任何 provider (== no_route 场景)
//   - upstreamErrType: 若最后出错有 provider.ProviderError,传它;否则传 provider.ErrorType("")
func classifyError(statusCode int, providerEmpty bool, upstreamErrType *provider.ProviderError) string {
	if statusCode == 0 {
		return "unknown"
	}
	if statusCode < 400 {
		return "ok"
	}
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth_failed"
	case http.StatusServiceUnavailable:
		if providerEmpty {
			return "no_route"
		}
		return "upstream_5xx"
	case http.StatusTooManyRequests:
		return "upstream_429"
	}
	if statusCode >= 500 {
		return "upstream_5xx"
	}
	if upstreamErrType != nil {
		switch upstreamErrType.ErrorType {
		case provider.ErrorTypeTimeout:
			return "timeout"
		case provider.ErrorTypeConnection:
			return "connection_error"
		}
	}
	return "upstream_4xx"
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
//
// Task 7 / F4: 同时累积响应 body 到 access log buffer;若全局流数 >= 1000
// 则只写 metadata 不缓存 body。entry 可为 nil(调用方未启用 accesslog)。
func (e *Engine) doStream(
	ctx context.Context,
	c *gin.Context,
	pv provider.Provider,
	req *provider.Request,
	result *router.RouteResult,
	entry *accesslog.AccessEntry,
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

	// Task 7 / F4: 全局流上限由 acquireStreamSlot 内部处理,
	// finalizeStream 用 defer 兜底,确保任意退出路径都收尾
	defer e.finalizeStream(req.TraceID, entry)

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
		// Task 7: 累积到 access log buffer(F4 上限由 appendStreamChunk 内部判断)
		e.appendStreamChunk(req.TraceID, chunk.Data)
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

// writeNonStreamResponse 把 Provider Response 原样写回客户端,并同步写
// access log 响应 body 文件(Task 7 / spec §3.3)。
//
// entry 可为 nil(调用方未启用 accesslog)。enabled body 文件写入失败只记 warn,
// 不影响响应主路径。
func (e *Engine) writeNonStreamResponse(
	c *gin.Context,
	req *provider.Request,
	resp *provider.Response,
	result *router.RouteResult,
	latency time.Duration,
	entry *accesslog.AccessEntry,
) {
	copyResponseHeaders(c, resp.Headers)
	c.Writer.WriteHeader(resp.StatusCode)
	if _, err := c.Writer.Write(resp.Body); err != nil {
		e.logger.Warn("write response", zap.Error(err))
	}
	// P67 / Task 7: 同步写响应 body 文件(失败也继续 — body 文件丢了不影响主响应)
	if entry != nil && e.accessLog != nil && !req.IsStream {
		if p, _ := e.accessLog.WriteBody(req.TraceID, "resp", resp.Body); p != "" {
			entry.RespBodyPath = p
			entry.RespBodySize = len(resp.Body)
		}
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
	// P65: 上游响应的真实 model 覆盖客户端请求的 model
	//   - 客户端发 "claude-sonnet-5"(别名)→ 路由到 minimax → 实际命中 "MiniMax-M3"
	//   - DB 用 "MiniMax-M3" 做 GROUP BY,前端按 model 归类才能显示正确
	//   - cost 计算仍用 result.ModelID(客户端请求的 model),因为计费价格表是按 client model 配的
	if u != nil && u.Model != "" {
		r.ModelID = u.Model
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
// P67: 透传 outProviderName / outLastErr 让外层 handle() 的 defer 拿得到
// Task 7: 透传 entry 让非流式分支能写 access log 响应 body 文件
func (e *Engine) runWithFirstResult(c *gin.Context, ctx context.Context, req *provider.Request, iter *router.RouteIterator, first *router.RouteResult, outProviderName *string, outLastErr **provider.ProviderError, entry *accesslog.AccessEntry) {
	attempts := 0

	// 先处理 first
	if e.tryOneCandidate(c, ctx, req, first, outProviderName, outLastErr, entry) {
		return
	}
	if *outLastErr != nil && !errorIsRetryable(*outLastErr) {
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
		if e.tryOneCandidate(c, ctx, req, result, outProviderName, outLastErr, entry) {
			return
		}
		if *outLastErr != nil && !errorIsRetryable(*outLastErr) {
			return
		}
	}
	e.handleAllFailed(c, req, *outLastErr, req.TraceID)
}

// tryOneCandidate 试一次候选。返回 true 表示成功处理(应该 return)
// lastErr / outProviderName 在错误时被更新(P67:供外层 defer 收尾)
func (e *Engine) tryOneCandidate(
	c *gin.Context,
	ctx context.Context,
	req *provider.Request,
	result *router.RouteResult,
	outProviderName *string,
	lastErr **provider.ProviderError,
	entry *accesslog.AccessEntry,
) bool {
	req.Headers.Set("X-Request-Id", req.TraceID)
	if result.Key != nil {
		req.Headers.Set("Authorization", "Bearer "+result.Key.Key)
	}
	pv, ok := e.router.Manager().Get(result.ProviderName)
	if !ok {
		*outProviderName = result.ProviderName
		*lastErr = &provider.ProviderError{
			ProviderName: result.ProviderName,
			ErrorType:    provider.ErrorTypeConnection,
			Message:      "provider instance not found",
		}
		return false
	}

	start := time.Now()
	if req.IsStream {
		ok, streamUsage, perr := e.doStream(ctx, c, pv, req, result, entry)
		e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), true, perr)
		if ok {
			e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, "", true, streamUsage)
			*outProviderName = result.ProviderName
			return true
		}
		*outProviderName = result.ProviderName
		*lastErr = perr
	} else {
		resp, perr := e.doRequest(ctx, pv, req, result)
		e.recordMetrics(result.ProviderName, statusFromErr(perr), time.Since(start), false, perr)
		if perr == nil && resp != nil {
			e.writeNonStreamResponse(c, req, resp, result, time.Since(start), entry)
			*outProviderName = result.ProviderName
			return true
		}
		*outProviderName = result.ProviderName
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

// acquireStreamSlot 试图为本 trace 申请一个流式累积 slot。
//
// F4 行为:
//   - 全局计数 < 1000 → 计数 +1,在 streamBuf 占位 traceID,返回 (acc, true)
//   - 全局计数 ≥ 1000 → 不占位,计数原样减回去,返回 (nil, false)
//     调用方应视作"超出上限" — 只记 metadata,不缓存 body。
//
// 计数与占位是两步,理论上并发下仍有微小窗口让"占位成功但计数已超"。
// 该窗口在工程上不构成问题:最坏情况是再多一两条流被累计,远小于 1000。
// 严格实现需要 CAS / mutex,这里不做。
func (e *Engine) acquireStreamSlot(traceID string) (*streamAcc, bool) {
	n := atomic.AddInt64(&e.streamCnt, 1)
	if n > maxConcurrentStreams {
		atomic.AddInt64(&e.streamCnt, -1)
		return nil, false
	}
	acc := &streamAcc{}
	actual, _ := e.streamBuf.LoadOrStore(traceID, acc)
	return actual.(*streamAcc), true
}

// appendStreamChunk 累积单个 SSE chunk 到对应 trace 的 buffer。
//
// slot 申请失败(超出 1000 上限)→ 直接返回,不留任何状态。
// 写入过程中达到 MaxBodyBytes → 标记 truncated,但不再继续追加。
// (BodyFileWriter.Write 会再次校验 data 长度并打 .truncated.json 后缀。)
func (e *Engine) appendStreamChunk(traceID string, chunk []byte) {
	acc, ok := e.acquireStreamSlot(traceID)
	if !ok {
		// 超 F4 上限 — 不累积 body,metadata 仍照写
		return
	}
	acc.Lock()
	if acc.buf.Len() < accesslog.MaxBodyBytes {
		// Write 一次写完,避免多次 mutex + 多次校验
		room := accesslog.MaxBodyBytes - acc.buf.Len()
		if len(chunk) > room {
			acc.buf.Write(chunk[:room])
			acc.truncated = true
		} else {
			acc.buf.Write(chunk)
		}
	} else {
		acc.truncated = true
	}
	acc.Unlock()
}

// finalizeStream 在流结束时调用,写入 body 文件并清理 slot。
//
// 幂等性:streamBuf.LoadAndDelete 保证只调用一次 WriteBody,
// 即使调用方多次 defer 也安全(defer 顺序 LIFO,但 LoadAndDelete 本身原子)。
//
// 调用方应负责:正常 EOF / message_stop / 客户端断开 / 错误路径都调一次。
func (e *Engine) finalizeStream(traceID string, entry *accesslog.AccessEntry) {
	accAny, ok := e.streamBuf.LoadAndDelete(traceID)
	if !ok {
		// 没累积过(可能 acquire 失败,或根本没流式响应) — 计数要减回去
		atomic.AddInt64(&e.streamCnt, -1)
		return
	}
	acc := accAny.(*streamAcc)
	if e.accessLog != nil {
		// body 文件由 BodyFileWriter.Write 根据 data 长度决定是否 .truncated.json 后缀,
		// 我们这里只需要传 raw bytes 即可。
		data := acc.buf.Bytes()
		if p, _ := e.accessLog.WriteBody(traceID, "resp", data); p != "" {
			if entry != nil {
				entry.RespBodyPath = p
				entry.RespBodySize = acc.buf.Len()
			}
		}
	}
	atomic.AddInt64(&e.streamCnt, -1)
}
