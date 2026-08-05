// Package proxy — Engine 主入口
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	// writeTimeout 流式写 deadline 的续期预算(取 server.write_timeout)。
	// http.Server.WriteTimeout 是「响应整体绝对上限」,流式场景下长生成会被
	// 120s 掐断(Claude Code 报 Connection closed mid-response);doStream 每个
	// chunk 后用 SetWriteDeadline 续期,把绝对上限变成「空闲超时」。
	writeTimeout time.Duration
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
	// WriteTimeout 流式写 deadline 续期预算(取 server.write_timeout)。
	// 非流式响应仍是绝对上限;流式场景按 chunk 续期成空闲超时。<=0 时默认 2min。
	WriteTimeout time.Duration
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
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 2 * time.Minute
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
		writeTimeout:  cfg.WriteTimeout,
	}
}

// HandleRequest 处理非流式代理请求
// tryDefaultModelFallback 尝试用 client key 的 default_model 替换 model 名
// 返回替换后的 model 名,空字符串表示不需要/无法 fallback
//
// 调用方应该:
//  1. 用返回值更新自己的 model 变量
//  2. 重写 req.Model 和 req.Body(用 rewriteModelField)
//  3. 重新调 router.Route
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
	// 持有供 defer 使用 — entry / providerName / lastErr / gatewayValidation
	var (
		lastProviderName  string
		lastErr           *provider.ProviderError
		gatewayValidation bool // status=400 是 gateway 自己设的(model 缺失 / 字段类型错),不是 upstream 返的
	)
	defer func() {
		if entry == nil || e.accessLog == nil {
			return
		}
		entry.StatusCode = c.Writer.Status()
		// doStream 可能已预设 error_type(流中途错误 stream_interrupted /
		// 客户端断开 client_disconnected)— 预设优先,classifyError 只补未预设的。
		// 否则流式 200(头已发出,状态码锁死)会把中途失败伪装成 ok。
		if entry.ErrorType == "" {
			entry.ErrorType = classifyError(entry.StatusCode, lastProviderName == "", lastErr, gatewayValidation)
		}
		// 无论成功还是失败,只要命中过 provider 就记录 — 成功路径同样需要可观测性(spec §1.2 F2/F5)
		// P-provider-vendor: 按厂商归一 — 路由侧是注册名(deepseek-anthropic /
		// minimax-openai 协议面),日志/UI/导出统一显示厂商名,协议面看 protocol 列
		if lastProviderName != "" {
			entry.ProviderName = provider.Default().VendorFor(lastProviderName)
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
		gatewayValidation = true
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	// P-responses: /responses 透传前剥离推理块(跨厂商切换时 MiniMax 的
	// reasoning 会被 DeepSeek 400 拒收;剥离后目标模型重新推理)
	if isResponsesPath(c.Request.URL.Path) {
		if nb, _ := stripResponsesReasoning(body); nb != nil {
			body = nb
		}
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
		gatewayValidation = true
		writeJSONError(c, http.StatusBadRequest, "invalid_request", "request body must include non-empty 'model' field")
		return
	}
	isStream = bodyStream
	if entry != nil {
		// P-stream-flag: entry 在 body 解析前创建,IsStream 当时是零值 —
		// 解析完 body 后补写真实值(之前日志 stream 列恒为 False)
		entry.IsStream = isStream
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
		if gk, ok := gkVal.(*auth.GatewayKey); ok {
			if len(gk.ProviderKeyIDs) > 0 {
				routeOpts = append(routeOpts, router.WithProviderKeyIDs(gk.ProviderKeyIDs))
			}
			// P-whitelist-select: 白名单参与 catch_all 自动模式的候选模型选择 —
			// key 允许的模型就是链上实际服务的模型(通配 * 不参与选择)
			if len(gk.AllowedModels) > 0 && !(len(gk.AllowedModels) == 1 && gk.AllowedModels[0] == "*") {
				routeOpts = append(routeOpts, router.WithAllowedModels(gk.AllowedModels))
			}
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

	// 4.5 探针:provider 绑定检查(白名单已移入 tryOneCandidate 逐候选校验,
	// 入口不再单独拒绝 — 同 tier 乱序下白名单外候选会被跳过继续试白名单内的)。
	// 迭代器不支持 reset:探针通过的候选直接用 runWithFirstResult 作为第一个开始循环
	if gkVal, ok := c.Get("gateway_key"); ok {
		if gk, ok := gkVal.(*auth.GatewayKey); ok {
			probeResult, probeErr := iter.Next()
			if probeErr != nil {
				// 没更多候选
				e.handleAllFailed(c, req, lastErr, traceID)
				return
			}
			// P19: provider 绑定检查 — 若 key.Providers 非空,路由结果必须在列表里
			if len(gk.Providers) > 0 && e.authn != nil {
				if e.authn.CheckProvider(gk, probeResult.ProviderName) != nil {
					e.logger.Warn("key provider mismatch",
						zap.String("key", gk.Name),
						zap.Strings("key_providers", gk.Providers),
						zap.String("routed_provider", probeResult.ProviderName),
						zap.String("model", model),
						zap.String("trace_id", traceID),
					)
					c.JSON(http.StatusForbidden, gin.H{
						"error": gin.H{
							"type": "key_provider_mismatch",
							"message": fmt.Sprintf("key %q is bound to providers %v but request routes to %q",
								gk.Name, gk.Providers, probeResult.ProviderName),
						},
					})
					return
				}
			}
			// 记下第一个候选的 provider name,供 defer 写 ProviderName
			lastProviderName = probeResult.ProviderName
			// 检查通过,把这个候选放回 iterator(不太好做,所以重置当前 idx)
			// 简单做法:迭代器不支持 reset,改为手动用 probeResult 开始循环
			e.runWithFirstResult(c, ctx, req, iter, probeResult, &lastProviderName, &lastErr, entry)
			return
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
		// P-catch-all: 按当前候选重写 body 的 model 字段(与 tryOneCandidate 同一逻辑)
		if newBody, ok2 := rewriteModelField(req.Body, result.ModelID); ok2 {
			req.Body = newBody
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
				e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, entryErrorType(entry), isStream, streamUsage)
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
//   - gatewayValidation 标记 status 是 gateway 自己设的(如 400 invalid_request)而不是 upstream 返的
func classifyError(statusCode int, providerEmpty bool, upstreamErrType *provider.ProviderError, gatewayValidation bool) string {
	if statusCode == 0 {
		return "unknown"
	}
	if statusCode < 400 {
		return "ok"
	}
	// Gateway 自己的 validate 失败(model 缺失 / messages 不是 array 等)
	// status=400 + 没真正命中 provider → 标 invalid_request
	if gatewayValidation && statusCode == http.StatusBadRequest {
		return "invalid_request"
	}
	// Provider 返的 invalid_request(400) — 透传时也是 invalid_request,不是 upstream_4xx
	if upstreamErrType != nil && upstreamErrType.ErrorType == provider.ErrorTypeInvalidRequest {
		return "invalid_request"
	}
	// Client 断开连接 — 不是 upstream 错误,也不应 trigger failover
	if upstreamErrType != nil && upstreamErrType.ErrorType == provider.ErrorTypeClientDisconnected {
		return "client_disconnected"
	}
	// P-catch-all: 白名单拒绝 — gateway 自己返的 403,不是上游 auth 失败
	if upstreamErrType != nil && upstreamErrType.ErrorType == provider.ErrorTypeModelNotAllowed {
		return "model_not_allowed"
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
//
// Slot 申请在 chunk loop 开始之前一次性完成(doStream 顶部),不是 per-chunk
// 申请 — 这样 appendStreamChunk 后续的 per-chunk 调用只会做 lookup,
// 不会让 streamCnt 反复 +1。finalizeStream 的 defer 与 acquire 严格配对。
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

	// Task 7 / F4: 全局流上限由 acquireStreamSlot 内部处理 — 在 chunk
	// loop 开始之前一次性申请 slot(每个 stream 只 +1 一次)。
	// acquireStreamSlot 失败时不动计数器,appendStreamChunk 后续
	// 的 streamBuf.Load 会自然返回 nil → chunk 不入 buffer。
	// finalizeStream 用 defer 兜底,通过 LoadAndDelete 是否拿到判断
	// 是否需要 -1,与 acquire 严格配对。
	_, _ = e.acquireStreamSlot(req.TraceID)
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
	// 流式写超时续期:http.Server.WriteTimeout 是「响应整体绝对上限」,长生成
	// (>120s)会被服务器掐断;每个 chunk 后把写 deadline 续期到 write_timeout 之后,
	// 把绝对上限变成「空闲超时」— 活跃流永不掐断,上游卡死静默 write_timeout 后断。
	// gin 1.10 的 responseWriter 实现 Unwrap,ResponseController(Go 1.20+)可用。
	rc := http.NewResponseController(c.Writer)

	for chunk := range chunkCh {
		if chunk.Err != nil {
			if errors.Is(chunk.Err, io.EOF) {
				break
			}
			// 流中途错误(上游断流 / 连接被杀 / context canceled):
			// 写一个 error event 给客户端,然后退出。HTTP 头已发出、状态码锁死 200,
			// 无法改状态,但 access log 必须标 stream_interrupted,不能伪装成 ok。
			if entry != nil {
				entry.ErrorType = "stream_interrupted"
			}
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
		// Task 7: 累积到 access log buffer(lookup-only,slot 已由
		// doStream 开头的 acquireStreamSlot 一次性申请)
		e.appendStreamChunk(req.TraceID, chunk.Data)
		// chunk.Data 已经是 SSE data 行的内容(Provider 负责格式化)
		if _, err := c.Writer.Write(chunk.Data); err != nil {
			// 客户端断开 / 写失败 — 立刻 break 但要返 ProviderError 让
			// caller 知道这不是正常结束,access_log error_type 标 client_disconnected
			// (非 retryable,不需要 failover 到下一个 provider — 问题在 client)
			if entry != nil {
				entry.ErrorType = "client_disconnected"
			}
			e.logger.Warn("write stream chunk (client likely disconnected)",
				zap.String("provider", result.ProviderName),
				zap.Error(err))
			_ = canFlush
			return true, nil, &provider.ProviderError{
				ProviderName: result.ProviderName,
				ErrorType:    provider.ErrorTypeClientDisconnected,
				Message:      "client disconnected during stream: " + err.Error(),
			}
		}
		if canFlush {
			flusher.Flush()
		}
		// 续期写 deadline — 失败说明连接已不可写,后续 Write 会报错,忽略即可
		_ = rc.SetWriteDeadline(time.Now().Add(e.writeTimeout))
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

// isResponsesPath 判断请求路径是否是 OpenAI Responses API(Codex)
func isResponsesPath(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, "/responses")
}

// stripResponsesReasoning P-responses: 剥离 Responses input 里的推理块。
//
// 跨厂商切换时(如 token plan 耗尽从 MiniMax 切到 DeepSeek),客户端会把
// 上一家的推理块原样回带 — MiniMax 的 reasoning 项(含 encrypted_content)
// 会被 DeepSeek 以 400 "reasoning_text must be passed back" 拒收。
// 推理内容是展示性上下文,剥离后目标模型重新推理,行为正确。
//
// 实测(DeepSeek /v1/responses):请求带工具往返(function_call/output)
// 且处于 thinking 模式时,必须回带 reasoning_text,否则 400 — 剥离推理后
// 必须显式 reasoning.effort="none" 声明不启用 thinking,请求才能通过。
// 返回 (新 body, 是否剥离过推理)。
func stripResponsesReasoning(body []byte) ([]byte, bool) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}
	inp, ok := req["input"].([]any)
	if !ok {
		return body, false
	}
	out := make([]any, 0, len(inp))
	stripped := false
	hasToolRounds := false
	for _, item := range inp {
		it, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		t, _ := it["type"].(string)
		if t == "reasoning" {
			stripped = true
			continue // 整项剥离
		}
		if t == "function_call" || t == "function_call_output" {
			hasToolRounds = true
		}
		// message 内容块里的 reasoning_text 也剥掉
		if t == "message" {
			if content, ok := it["content"].([]any); ok {
				var blocks []any
				for _, b := range content {
					bm, ok := b.(map[string]any)
					if !ok || bm["type"] != "reasoning_text" {
						blocks = append(blocks, b)
					}
				}
				it["content"] = blocks
			}
		}
		out = append(out, it)
	}
	req["input"] = out
	// 跨厂商续接:剥离了推理 + 带工具往返 → 强制 effort=none
	// (DeepSeek 校验:thinking 模式 + 工具往返必须回带 reasoning_text,否则 400)
	if stripped && hasToolRounds {
		req["reasoning"] = map[string]any{"effort": "none"}
	}
	res, err := json.Marshal(req)
	if err != nil {
		return body, stripped
	}
	return res, stripped
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

	// P-catch-all: 链上所有候选都被 key 白名单排除 → 403 model_not_allowed。
	// (ModelNotAllowed 在 IsRetryable 里保持 retryable 让 failover 继续,
	// 但最终全失败时必须透传 403 而不是 502)
	if lastErr.ErrorType == provider.ErrorTypeModelNotAllowed {
		c.Writer.Header().Set("X-Request-Id", traceID)
		writeJSONError(c, http.StatusForbidden, "model_not_allowed", lastErr.Message)
		return
	}

	// invalid_request / auth 等不应 failover 的错误:直接返回 Provider 原始错误
	if !errorIsRetryable(lastErr) {
		// 尽量按 Provider 状态码透传 — 但 0 是不合法的(没从 Provider 拿到的)
		statusCode := lastErr.StatusCode
		if statusCode < 100 || statusCode > 599 {
			// client_disconnected / 没具体 status → 返 499(nginx "client closed request" 标准)
			// 或 502 gateway error 看语义;client_disconnected → 499 更准确
			if lastErr.ErrorType == provider.ErrorTypeClientDisconnected {
				statusCode = 499
			} else {
				statusCode = http.StatusBadGateway
			}
		}
		c.Writer.Header().Set("X-Request-Id", traceID)
		c.Writer.WriteHeader(statusCode)
		if len(lastErr.RawError) > 0 {
			c.Writer.Write(lastErr.RawError)
		} else {
			writeJSONError(c, statusCode, string(lastErr.ErrorType), lastErr.Message)
		}
		return
	}

	writeJSONError(c, http.StatusBadGateway, "gateway_error",
		fmt.Sprintf("all providers failed: %s", lastErr.Message))
}

// entryErrorType 取 access entry 上已预设的 error_type。
// 流中途错误由 doStream 预设(stream_interrupted / client_disconnected),
// 正常完成时为空字符串。entry 可为 nil(未启用 accesslog)。
func entryErrorType(entry *accesslog.AccessEntry) string {
	if entry == nil {
		return ""
	}
	return entry.ErrorType
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
		// P37 + P40 + P-quota-512k: 算 cost(支持 cache pricing + 长上下文悬崖,单位 CNY ¥)
		// 逻辑见 provider.ComputeCost — cost = prompt*input + cache_creation*cache_create
		// (0 则 fallback 到 input)+ cache_read*cache_read + completion*output;
		// 输入含缓存超过 long_context_input_threshold 时全项乘 multiplier(M3 512k 悬崖)
		if mgr := e.router.Manager(); mgr != nil {
			c := mgr.CostFor(result.ProviderName, result.ModelID)
			r.Cost = provider.ComputeCost(c, u)
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
		// P-empty200: 非重试错误也要写响应 — 之前静默 return,客户端收到
		// 200 + 空 body/空流(Codex 报 "stream closed before response.completed")
		e.handleAllFailed(c, req, *outLastErr, req.TraceID)
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
			e.handleAllFailed(c, req, *outLastErr, req.TraceID)
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
	// P-catch-all: 白名单按候选逐个校验 — 白名单外的候选像没 key 一样跳过,
	// 继续试链上其他候选。同 tier 内 provider 顺序不定,不能因第一个候选
	// 不符就整请求 403;链上全部候选都被排除时由 handleAllFailed 收尾返 403
	if gkVal, ok := c.Get("gateway_key"); ok {
		if gk, ok := gkVal.(*auth.GatewayKey); ok && e.authn != nil {
			if err := e.authn.CheckAllowed(gk, result.ModelID); err != nil {
				*outProviderName = result.ProviderName
				*lastErr = &provider.ProviderError{
					ProviderName: result.ProviderName,
					StatusCode:   http.StatusForbidden,
					ErrorType:    provider.ErrorTypeModelNotAllowed,
					Message:      fmt.Sprintf("key %q does not allow model %q", gk.Name, result.ModelID),
				}
				e.logger.Debug("candidate skipped (not in key whitelist)",
					zap.String("key", gk.Name),
					zap.String("provider", result.ProviderName),
					zap.String("model", result.ModelID),
					zap.Strings("allowed", gk.AllowedModels),
					zap.String("trace_id", req.TraceID))
				return false
			}
		}
	}
	// P-catch-all: 记录实际使用的上游模型(候选的目标模型,如 MiniMax-M3 —
	// 客户端请求名可能只是标签)。failover 时每次尝试都覆盖,最后一次成功者胜出
	if entry != nil {
		entry.FinalModel = result.ModelID
	}
	req.Headers.Set("X-Request-Id", req.TraceID)
	if result.Key != nil {
		req.Headers.Set("Authorization", "Bearer "+result.Key.Key)
	}
	// P-catch-all: 上游必须收到真实 model 名,不能发客户端原始名。
	// 长格式 alias / catch_all 的每个候选带各自的目标 model(如 MiniMax-M3 与
	// deepseek-v4-flash 并存),按当前候选重写 body — 否则 failover 到 DeepSeek
	// 这类严格校验的 provider 会 400 model_not_found(MiniMax 宽容别名所以一直没暴露)。
	// 真实 model 直连时 result.ModelID == 客户端名,重写为 no-op
	if newBody, ok2 := rewriteModelField(req.Body, result.ModelID); ok2 {
		req.Body = newBody
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
			e.recordUsageWithTokens(req, result, time.Since(start), http.StatusOK, entryErrorType(entry), true, streamUsage)
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
// 计数与占位是绑定的:ok==true 路径必然两者都发生,ok==false 路径
// 两者都不保留(计数器短暂 +1/-1 后回到原值)。这保证 finalizeStream
// 可以用 streamBuf.LoadAndDelete 的结果作为唯一信号判断是否需要 -1,
// 严格配对,无泄漏无 underflow。
//
// 调用方应只从 doStream 调用一次(在 chunk loop 开始之前)。
// appendStreamChunk 是 lookup-only,不申请 slot,不会让计数器再 +1。
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
// Lookup-only:slot 必须已由 doStream 通过 acquireStreamSlot 申请过。
// lookup 失败(slot 被 F4 上限拒绝,或 stream 已被 finalize 清掉)
// → 直接返回,不留任何状态。这样 per-chunk 调用不会让 streamCnt
// 反复 +1(原 bug: 旧实现每 chunk 都调 acquire,一个 N-chunk 的 stream
// 会泄漏 N-1 的计数)。
//
// 写入过程中达到 MaxBodyBytes → 标记 truncated,但不再继续追加。
// (BodyFileWriter.Write 会再次校验 data 长度并打 .truncated.json 后缀。)
func (e *Engine) appendStreamChunk(traceID string, chunk []byte) {
	accAny, ok := e.streamBuf.Load(traceID)
	if !ok {
		// slot 未申请成功(或已被 finalize) — 不累积 body,metadata 仍照写
		return
	}
	acc := accAny.(*streamAcc)
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
// 计数严格配对:acquireStreamSlot 在 ok==true 时必然 +1 并占位 streamBuf;
// 这里通过 LoadAndDelete 是否拿到作为"是否需要 -1"的唯一信号。
//   - LoadAndDelete 返回 ok=true → 配对的 +1 在 finalize 之前一定发生过
//     (否则 streamBuf 里不可能有占位)→ 减 1
//   - LoadAndDelete 返回 ok=false → acquire 没成功(被 F4 上限拒绝),
//     也没占位 → 不动计数器
//
// 这样 increment / decrement 是严格一一对应,无泄漏,无 underflow。
//
// 幂等性:streamBuf.LoadAndDelete 保证只调用一次 WriteBody,
// 即使调用方多次 defer 也安全(defer 顺序 LIFO,但 LoadAndDelete 本身原子)。
// 二次调用时 LoadAndDelete 返回 ok=false → 直接 return → 不会重复 -1。
//
// 调用方应负责:正常 EOF / message_stop / 客户端断开 / 错误路径都调一次。
func (e *Engine) finalizeStream(traceID string, entry *accesslog.AccessEntry) {
	accAny, ok := e.streamBuf.LoadAndDelete(traceID)
	if !ok {
		// 没累积过(acquire 被 F4 上限拒绝,或 streamBuf 已被清) —
		// 配对规则下,既然 acquire 没成功,这里也不动计数器。
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
