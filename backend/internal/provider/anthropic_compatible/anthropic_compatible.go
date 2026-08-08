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
	// ForceThinkingDisabled P-deepseek-thinking: 上行前把 body 的 thinking 强制写成
	// {"type":"disabled"}。DeepSeek /anthropic 把 adaptive(Claude Code 发的)当 enabled 处理,
	// 严格校验历史里每个 assistant tool_use 消息必须回带 thinking 块 — Claude Code compact
	// 会剥离 thinking 块,导致 400 "content[].thinking ... must be passed back"(实测复现)。
	// deepseek-v4-flash 本来就是非 thinking 模型,显式 disabled 不损失能力(实测 200)
	ForceThinkingDisabled bool
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

// prepareBody 上行前的 body 预处理:ForceThinkingDisabled 时把 thinking 强制写成 disabled。
// 失败(非法 JSON)时原样返回 — 透传语义不变,让上游自己报错
func (b *Base) prepareBody(body []byte) []byte {
	if !b.cfg.ForceThinkingDisabled {
		return body
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	req["thinking"] = map[string]any{"type": "disabled"}
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// balanceGuardHealthy P-quota-guard: 守卫 — key 最近被 balancer 轮询过且显示有余额。
// 有余额还收到配额错误(MiniMax 瞬时限流 429 的 body 自带 2056 文本,关键词拦不住)
// → 判为限流而非配额耗尽,避免 healthy key 被误杀到 poll 恢复(≤60s)期间整链掉到 api 层。
// 从未轮询过(启动窗口,Remaining=0 是默认值不是真余额)→ 宽松放行 — 真耗尽由
// poll 连续 2 轮确认后标 QE,瞬态 2056 不误杀(2026-08-05 实测:重启后 60s 内
// key-1 被 2056 误标 QE,直到首轮 poll 恢复)
func (b *Base) balanceGuardHealthy(key *keypool.Key) bool {
	if key.LastPolledAt.IsZero() {
		return true // 启动窗口:无数据 → 不杀,宁可按限流冷却
	}
	if key.Remaining <= 0 || time.Since(key.LastPolledAt) > 5*time.Minute {
		return false // 有轮询数据(0 或过期)→ 信任上游错误码
	}
	return true
}

// rateLimitRetryDelay 限流重试等待:Retry-After 优先,默认 1s,上限 2s
// (避免瞬时限流把请求拖太久;超时场景 failover 更合适)
func rateLimitRetryDelay(retryAfter time.Duration) time.Duration {
	if retryAfter <= 0 {
		return time.Second
	}
	if retryAfter > 2*time.Second {
		return 2 * time.Second
	}
	return retryAfter
}

// classifyUpstream 统一分类 Anthropic 兼容上游的失败响应:
//  1. MiniMax base_resp 错误(藏在 HTTP 200 body)— 1008/2056 → quota,其余 → server_error
//  2. HTTP >= 400 → ClassifyErrorWithBody
//
// P-quota-guard: 分类结果为 quota_exceeded 但 balanceGuardHealthy(key) 通过 →
// 降级 rate_limit(MiniMax 瞬时限流误报 2056 的兜底)。
// 返回 (errType, 错误描述);成功(无错误)→ ("", "")
func (b *Base) classifyUpstream(status int, header http.Header, body []byte, key *keypool.Key) (provider.ErrorType, string) {
	if code, msg := provider.ParseMiniMaxBaseResp(body); code != 0 {
		errType := provider.ErrorTypeServerError
		if provider.IsMiniMaxQuotaCode(code) {
			errType = provider.ErrorTypeQuotaExceeded
			if b.balanceGuardHealthy(key) {
				errType = provider.ErrorTypeRateLimit
			}
		}
		return errType, fmt.Sprintf("upstream base_resp error %d: %s", code, msg)
	}
	if status >= 400 {
		errType := provider.ClassifyErrorWithBody(status, body)
		if errType == provider.ErrorTypeQuotaExceeded && b.balanceGuardHealthy(key) {
			errType = provider.ErrorTypeRateLimit
		}
		return errType, fmt.Sprintf("upstream returned %d", status)
	}
	return "", ""
}

// Name 由 wrapper 提供

// SendRequest 发送非流式 Anthropic Messages 请求
//
//	POST {endpoint}/v1/messages
//	Headers:
//	  x-api-key: {key}
//	  anthropic-version: 2023-06-01
//	  Content-Type: application/json
//	Body 原样透传
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 优先用路由层已 acquire 的 key — 否则双 acquire 可能
	// 拿到不同 key,429 时冷却标到没发过请求的 key 上(2026-08-06 实测)
	key := req.Key
	var err error
	if key == nil {
		key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic))
		if err != nil {
			return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	body := b.prepareBody(req.Body)
	// P-quota-guard-retry: 限流(含余额守卫降级)等 Retry-After(默认 1s,上限 2s)
	// 后用同一把 key 重试一次 — 瞬时限流下请求留在本 provider,不落 failover
	retried := false
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/messages",
			bytes.NewReader(body))
		if err != nil {
			return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
		}
		httpReq.Header.Set("x-api-key", key.Key)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("Content-Type", "application/json")
		if req.TraceID != "" {
			httpReq.Header.Set("X-Request-Id", req.TraceID)
		}
		httpResp, err := b.client.Do(httpReq)
		if err != nil {
			errType := provider.ClassifyTransportError(ctx, err)
			b.cfg.Pool.ReportError(key, string(errType))
			return nil, provider.NewError(b.cfg.Name, 0, errType, err.Error())
		}
		respBody, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if readErr != nil {
			b.cfg.Pool.ReportError(key, "io_error")
			return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, readErr.Error())
		}
		// P-quota-minimax: MiniMax 错误藏在 HTTP 200 的 body 里(base_resp.status_code≠0),
		// 1008(余额不足)/ 2056(超 Token Plan)→ quota_exceeded;P-quota-guard 见 classifyUpstream
		errType, msg := b.classifyUpstream(httpResp.StatusCode, httpResp.Header, respBody, key)
		if errType == provider.ErrorTypeRateLimit {
			retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
			b.cfg.Pool.ReportRateLimit(key, retryAfter)
			if !retried {
				retried = true
				select {
				case <-time.After(rateLimitRetryDelay(retryAfter)):
				case <-ctx.Done():
					return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeClientDisconnected, "client disconnected during rate-limit retry")
				}
				continue
			}
		} else if errType != "" {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		if errType != "" {
			return nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType, msg, respBody)
		}

		b.cfg.Pool.ReportSuccess(key)
		usage := parseAnthropicUsage(respBody)

		return &provider.Response{
			StatusCode: httpResp.StatusCode,
			Headers:    httpResp.Header,
			Body:       respBody,
			Usage:      usage,
		}, nil
	}
}

// SendStreamRequest 发送流式 Anthropic Messages 请求
// Anthropic SSE 格式:
//
//	event: message_start
//	data: {"type":"message_start","message":{...}}
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}
//
//	event: message_delta
//	data: {"type":"message_delta","usage":{"output_tokens":N}}
//
//	event: message_stop
//	data: {"type":"message_stop"}
func (b *Base) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 同 SendRequest — 优先用路由层已 acquire 的 key
	key := req.Key
	var err error
	if key == nil {
		key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic))
		if err != nil {
			return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	// 流式请求:超时拉长到 10 分钟
	// http.Client.Timeout 是整个请求生命周期(包括读 body)的硬上限。
	// 对于流式响应,Anthropic 官方 API 自家超时是 10 分钟,thinking 模型可能更久。
	// 之前 120s 太短 — upstream 思考类请求会触发 context deadline exceeded,
	// 导致 Claude Code 报 "Connection closed mid-response"。
	//
	// 10 分钟足够绝大多数 thinking 模型生成;超时由 context 控制
	// (调用方 ctx cancel 即触发中断)
	streamTimeout := b.cfg.Timeout
	if streamTimeout < 600*time.Second {
		streamTimeout = 600 * time.Second
	}
	client := &http.Client{Timeout: streamTimeout}

	body := b.prepareBody(req.Body)
	// P-quota-guard-retry: 与 SendRequest 相同 — 限流等 Retry-After(≤2s)重试一次
	retried := false
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(b.cfg.Endpoint, "/")+"/v1/messages",
			bytes.NewReader(body))
		if err != nil {
			return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
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
			errType := provider.ClassifyTransportError(ctx, err)
			b.cfg.Pool.ReportError(key, string(errType))
			return nil, nil, provider.NewError(b.cfg.Name, 0, errType, err.Error())
		}

		if httpResp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			// P49: 带 body 检测 quota + P-quota-guard 降级
			errType, msg := b.classifyUpstream(httpResp.StatusCode, httpResp.Header, respBody, key)
			if errType == provider.ErrorTypeRateLimit {
				retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
				b.cfg.Pool.ReportRateLimit(key, retryAfter)
				if !retried {
					retried = true
					select {
					case <-time.After(rateLimitRetryDelay(retryAfter)):
					case <-ctx.Done():
						return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeClientDisconnected, "client disconnected during rate-limit retry")
					}
					continue
				}
			} else if errType != "" {
				b.cfg.Pool.ReportError(key, string(errType))
			}
			return nil, nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType, msg, respBody)
		}

		// P-quota-minimax: 流式场景下 MiniMax 也可能 HTTP 200 + 首段 body 是
		// base_resp 错误。peek 前两行解析:确认是 base_resp 错误 → 在客户端收到
		// 任何字节前直接失败(failover 还来得及);否则把 peeked 行接回 reader 正常流式
		reader := bufio.NewReader(httpResp.Body)
		var peeked []byte
		for i := 0; i < 2; i++ {
			line, err := reader.ReadBytes('\n')
			peeked = append(peeked, line...)
			if err != nil {
				break
			}
		}
		if code, msg := provider.ParseMiniMaxStreamBaseResp(peeked); code != 0 {
			httpResp.Body.Close()
			errType := provider.ErrorTypeServerError
			if provider.IsMiniMaxQuotaCode(code) {
				errType = provider.ErrorTypeQuotaExceeded
				if b.balanceGuardHealthy(key) {
					errType = provider.ErrorTypeRateLimit
				}
			}
			if errType == provider.ErrorTypeRateLimit {
				b.cfg.Pool.ReportRateLimit(key, 0)
				if !retried {
					retried = true
					select {
					case <-time.After(rateLimitRetryDelay(0)):
					case <-ctx.Done():
						return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeClientDisconnected, "client disconnected during rate-limit retry")
					}
					continue
				}
			} else {
				b.cfg.Pool.ReportError(key, string(errType))
			}
			return nil, nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
				fmt.Sprintf("upstream base_resp error %d: %s", code, msg), peeked)
		}
		// 正常流:peeked 行已在 reader 外,用 MultiReader 接回
		streamReader := io.MultiReader(bytes.NewReader(peeked), reader)

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
			reader := bufio.NewReader(streamReader)

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
	} // P-quota-guard-retry: for 循环闭合
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
		if k, err := b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolAnthropic)); err == nil {
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
		Model:            resp.Model, // P65: 上游响应的真实 model 名
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens: resp.Usage.InputTokens + resp.Usage.OutputTokens +
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
