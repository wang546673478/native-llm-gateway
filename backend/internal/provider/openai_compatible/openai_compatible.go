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
	// StreamTimeoutFloor 流式请求超时下限(默认 600s=10分钟)。
	// 见 NewBase:服务端 thinking/长上下文流式可能远大于非流式 Timeout,
	// 这里给流式单独保底,避免 120s/60s 的普通超时把长生成切断。
	// <=0 时按协议默认(openai/anthropic=600s,google=120s)。
	StreamTimeoutFloor time.Duration
	Pool               *keypool.Pool
	// ChatPath 是 chat completions 端点的路径,默认 /v1/chat/completions
	// DeepSeek 用 /chat/completions(无 /v1 前缀);其他 OpenAI 兼容家族都用默认
	ChatPath string
	// ResponsesPath 是 OpenAI Responses API 端点的路径(/v1/responses 透传,
	// Codex 客户端)。默认 /v1/responses;endpoint 已含 /v1 的 provider
	// (如 minimax-openai 的 https://api.minimaxi.com/v1)覆盖为 /responses。
	// 不支持 Responses API 的 provider(ResponsesAPI=false)不会收到此类请求
	ResponsesPath string
	// ModelsPath 是模型列表端点的路径(GET,供 ListModels / HealthCheck 用)。
	// 默认 /v1/models;endpoint 已含版本前缀的 provider(如 minimax-openai 的
	// https://api.minimaxi.com/v1、mimo 的 .../v1、glm 的 .../paas/v4)必须
	// 覆盖为 /models —— 否则拼出 /v1/v1/models 这类不存在的路径,上游回
	// HTML 404 / 空 body,同步报出的却是 "decode models: invalid character '<'"
	// 之类与真因无关的错(2026-08-20 根因)。与 ResponsesPath 同一套约定。
	ModelsPath string
	// BillingSource P47 计费面(token_plan / api / free),空 = 不限定。
	// ListModels / HealthCheck 用它按面取 key:同 vendor 多个面共享 key 池,
	// 但 key 与端点绑定(mimo 的 tp- key 只在 token-plan 端点有效,发到 api
	// 端点必 401)。不限定就会走 TierOrder 拿到别的面的 key(2026-08-20 实测)。
	BillingSource string
	// ModelsOverride 若非空,覆盖 cfg.Models(用于 DeepSeek v4 时代)
	ModelsOverride []string
	// StreamUsage 控制是否在流式请求里加 stream_options.include_usage=true
	// 默认 true:让响应最后一个 chunk 带 usage,Gateway 才能正确计费
	// DeepSeek / Qwen / Kimi / GLM 都支持
	StreamUsage bool
	// UsageParser 可选的 Usage 解析器,用于提取厂商特殊的计费字段
	// nil 时使用默认的 OpenAI 标准解析器
	UsageParser provider.UsageParser
}

// Base 是 OpenAI 兼容 Provider 的共享实现
// DeepSeek / GLM / Qwen / Kimi 等只需要薄薄一层 wrapper 即可
type Base struct {
	cfg         Config
	client      *http.Client
	usageParser provider.UsageParser
}

// NewBase 构造 Base
func NewBase(cfg Config) *Base {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if cfg.StreamTimeoutFloor <= 0 {
		cfg.StreamTimeoutFloor = 600 * time.Second // openai 协议默认 10 分钟
	}
	if cfg.ChatPath == "" {
		cfg.ChatPath = "/v1/chat/completions"
	}
	if cfg.ResponsesPath == "" {
		cfg.ResponsesPath = "/v1/responses"
	}
	if cfg.ModelsPath == "" {
		cfg.ModelsPath = "/v1/models"
	}
	// 默认开启 stream_options.include_usage:让流式响应最后一个 chunk 带 usage,
	// Gateway 才能正确计费。OpenAI 兼容家族(DeepSeek/Qwen/Kimi/GLM)都支持。
	cfg.StreamUsage = true

	// 如果没有指定 UsageParser,使用默认的标准 OpenAI 解析器
	usageParser := cfg.UsageParser
	if usageParser == nil {
		usageParser = &DefaultOpenAIUsageParser{}
	}

	return &Base{
		cfg:         cfg,
		client:      &http.Client{Timeout: timeout},
		usageParser: usageParser,
	}
}

// upstreamPath P-responses: 按客户端请求路径选上游端点路径。
//   - /responses、/v1/responses(Codex)→ {endpoint}{ResponsesPath},body 原样透传
//     (DeepSeek / MiniMax 官方原生支持 Responses API)
//   - 其他 → ChatPath(chat/completions)
// 自动适配 endpoint 是否已包含 /v1: 若 endpoint 以 /v1 结尾且路径以 /v1 开头,
// 则去掉路径的 /v1 前缀避免重复 (如 endpoint=http://x/v1 + path=/v1/responses → /responses)
func (b *Base) upstreamPath(req *provider.Request) string {
	p := strings.ToLower(req.Path)
	var path string
	if strings.HasSuffix(p, "/responses") {
		path = b.cfg.ResponsesPath
	} else {
		path = b.cfg.ChatPath
	}

	// 智能去重: 如果 endpoint 以 /v1 结尾且 path 以 /v1 开头, 去掉 path 的 /v1 前缀
	endpoint := strings.TrimRight(b.cfg.Endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1") && strings.HasPrefix(path, "/v1/") {
		return path[3:] // 去掉 "/v1" 前缀, 保留 "/responses" 或 "/chat/completions"
	}

	return path
}

// Name / Protocol / Models 由 wrapper 提供
// 这里把方法放在 wrapper 中,Base 只提供 HTTP 调用

// SendRequest 发送非流式请求
//  1. 从 Pool 取 Key
//  2. POST 到 {endpoint}{ChatPath}(Responses API 请求走 ResponsesPath 透传)
//  3. Authorization: Bearer {key}
//  4. body 原样透传
//  5. 解析 OpenAI 格式响应,提取 Usage
func (b *Base) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 优先用路由层已 acquire 的 key — 否则双 acquire 可能拿到
	// 不同 key,429 上报冷却标到没发过请求的 healthy key 上(2026-08-06 实测)
	key := req.Key
	var err error
	if key == nil {
		key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolOpenAI)) // P-provider-vendor: 按本包协议过滤
		if err != nil {
			return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+b.upstreamPath(req),
		bytes.NewReader(req.Body))
	if err != nil {
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
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
		errType := provider.ClassifyTransportError(ctx, err)
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, provider.NewError(b.cfg.Name, 0, errType, err.Error())
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		b.cfg.Pool.ReportError(key, string(provider.ErrorTypeConnection)) // io read 失败即连接型 — 与返回的 ErrorTypeConnection 一致,触发 per-key 熔断
		return nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
	}

	// P-quota-minimax: MiniMax 错误藏在 HTTP 200 的 body 里(base_resp.status_code≠0),
	// 下面 >= 400 分支看不到它。1008(余额不足)/ 2056(超 Token Plan)→ quota_exceeded,
	// 触发 failover 到下一 provider + key 标记 QUOTA_EXCEEDED;其他非零 → server_error
	if code, msg := provider.ParseMiniMaxBaseResp(body); code != 0 {
		errType := provider.ErrorTypeServerError
		if provider.IsMiniMaxQuotaCode(code) {
			errType = provider.ErrorTypeQuotaExceeded
		}
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream base_resp error %d: %s", code, msg), body)
	}

	if httpResp.StatusCode >= 400 {
		retryAfter := provider.ParseRetryAfter(httpResp.Header.Get("Retry-After"))
		// P49: 带 body 检测 quota exceeded(401/403 + body 含 quota 关键字 → 升级为 quota_exceeded,触发 failover)
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)

		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, retryAfter)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}

		pe := provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
		pe.RetryAfter = retryAfter // 429 冷却时常(failover 计冷却用);其他类型为 0
		return nil, pe
	}

	// 成功
	b.cfg.Pool.ReportSuccess(key)

	// 解析 Usage - 使用注入的 UsageParser
	usage := b.usageParser.Parse(body)

	return &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Usage:      usage,
	}, nil
}

// SendStreamRequest 发送流式请求,返回 chunk channel
// 流式响应是 SSE 格式:
//
//	data: {json}\n\n
//	data: [DONE]\n\n
func (b *Base) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	if b.cfg.Pool == nil {
		return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, "keypool not configured")
	}
	// P-key-mismatch: 同 SendRequest — 优先用路由层已 acquire 的 key
	key := req.Key
	var err error
	if key == nil {
		key, err = b.cfg.Pool.AcquireForProtocol(string(provider.ProtocolOpenAI)) // P-provider-vendor: 按本包协议过滤
		if err != nil {
			return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, fmt.Sprintf("no available key: %v", err))
		}
	}

	// 启用 StreamUsage 时,自动注入 stream_options.include_usage
	// (Responses API 透传不注入 — 该字段是 chat.completions 专有,
	// usage 在 response.completed 事件里自带)
	streamBody := req.Body
	if b.cfg.StreamUsage && !strings.HasSuffix(strings.ToLower(req.Path), "/responses") {
		streamBody = injectStreamUsage(streamBody)
	}

	// 流式超时:floor 可配置(默认 10 分钟)
	// http.Client.Timeout 限制整个请求生命周期(包括读 body)。
	// 对于流式响应,Anthropic / OpenAI 官方自家超时是 10 分钟,thinking / 长上下文
	// 模型可能更久。120s 太短,容易触发 context deadline exceeded,
	// 导致客户端报 "Connection closed mid-response"。
	streamTimeout := b.cfg.Timeout
	if streamTimeout < b.cfg.StreamTimeoutFloor {
		streamTimeout = b.cfg.StreamTimeoutFloor
	}
	client := &http.Client{Timeout: streamTimeout}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.Endpoint, "/")+b.upstreamPath(req),
		bytes.NewReader(streamBody))
	if err != nil {
		return nil, nil, provider.NewError(b.cfg.Name, 0, provider.ErrorTypeConnection, err.Error())
	}
	httpReq.Header.Set("Authorization", "Bearer "+key.Key)
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
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		// P49: 带 body 检测 quota
		errType := provider.ClassifyErrorWithBody(httpResp.StatusCode, body)
		if errType == provider.ErrorTypeRateLimit {
			b.cfg.Pool.ReportRateLimit(key, 0)
		} else {
			b.cfg.Pool.ReportError(key, string(errType))
		}
		return nil, nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream returned %d", httpResp.StatusCode), body)
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
		}
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream base_resp error %d: %s", code, msg), peeked)
	}
	// P-sse-stream-error: 上游 200 之后在流里发错误事件然后收流(实测
	// tokenmarket-codex:整条流只有一个 response.failed + rate_limit_exceeded)。
	// 与上面 base_resp 同一范式:错误落在 peek 窗口内 → 客户端还没收到任何字节,
	// 直接失败让 failover 换 key 重试;错误在窗口之外(流中途)由 proxy 兜底标记。
	if se := provider.ParseSSEStreamError(peeked); se != nil {
		httpResp.Body.Close()
		errType := provider.ClassifySSEStreamError(se)
		b.cfg.Pool.ReportError(key, string(errType))
		return nil, nil, provider.NewError(b.cfg.Name, httpResp.StatusCode, errType,
			fmt.Sprintf("upstream stream error %s: %s", se.Code, se.Message), peeked)
	}
	// 正常流:peeked 行已在 reader 外,用 MultiReader 接回
	streamReader := io.MultiReader(bytes.NewReader(peeked), reader)

	// 流式响应开始 — 上报 Key 成功
	b.cfg.Pool.ReportSuccess(key)

	ch := make(chan *provider.StreamChunk, 16)
	// P42: 收集流中的 usage — 最后一个带 usage 字段的 chunk 才是 totals
	// OpenAI 在 stream_options.include_usage=true 时才会在最后一个 chunk 发 usage
	var lastUsage *provider.Usage
	// resp.Usage 由 goroutine 在 close(ch) 之前填好;caller 必须先 drain channel 再读
	resp := &provider.Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
	}
	go func() {
		defer func() {
			// 在 channel 关闭前先填 usage,这样 caller drain 完就能安全读
			resp.SetUsage(lastUsage)
			close(ch)
		}()
		defer httpResp.Body.Close()
		reader := bufio.NewReader(streamReader)

		// SSE 格式:每行 "data: {...}" 直到 "data: [DONE]"
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
			// 只转发 data: 行(包含原始 SSE 格式)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[5:])
			if bytes.Equal(payload, []byte("[DONE]")) {
				if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Data: line}) {
					return
				}
				if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Err: io.EOF}) {
					return
				}
				return
			}
			// P42: 尝试从 chunk JSON 抽 usage - 使用注入的 UsageParser
			if u := b.usageParser.Parse(payload); u != nil {
				lastUsage = u
			}
			// 把 "data: {...}\n\n" 还原成 SSE 事件
			if !provider.SendOrAbort(ctx, ch, &provider.StreamChunk{Data: append(line, '\n', '\n')}) {
				return
			}
		}
	}()

	return ch, resp, nil
}

// HealthCheck 简单 GET 请求(检查 endpoint 可达)
func (b *Base) HealthCheck(ctx context.Context) error {
	hcTimeout := 5 * time.Second
	hctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet,
		strings.TrimRight(b.cfg.Endpoint, "/")+b.cfg.ModelsPath, nil)
	if err != nil {
		return err
	}
	if b.cfg.Pool != nil {
		if k, err := b.acquireOwnFaceKey(); err == nil { // P-provider-vendor: 按本面(协议+计费源)取 key
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

// acquireOwnFaceKey 取一把「属于本面」的 key。
// BillingSource 非空 → 只从该 tier 取;否则回退按协议取(TierOrder 顺序)。
// 为什么不能一律用 AcquireForProtocol:同 vendor 的多个协议面共享 key 池,
// 而 key 与端点是绑定的 —— mimo 的 tp- key 发到 api 端点、sk- key 发到
// token-plan 端点都会 401(2026-08-20 实测 2×2 全矩阵)。
func (b *Base) acquireOwnFaceKey() (*keypool.Key, error) {
	proto := string(provider.ProtocolOpenAI)
	if b.cfg.BillingSource != "" {
		return b.cfg.Pool.AcquireFromTier(b.cfg.BillingSource, nil, proto)
	}
	return b.cfg.Pool.AcquireForProtocol(proto)
}

// ListModels 调 GET {endpoint}{ModelsPath} 拉上游模型 id 列表。
func (b *Base) ListModels(ctx context.Context) ([]string, error) {
	endpoint := strings.TrimRight(b.cfg.Endpoint, "/")
	modelsPath := b.cfg.ModelsPath

	// P-relay-independent: 防止双重路径 — 如果 endpoint 已包含 /v1 且 modelsPath 以 /v1 开头，去掉 modelsPath 的 /v1
	if strings.HasSuffix(endpoint, "/v1") && strings.HasPrefix(modelsPath, "/v1/") {
		modelsPath = modelsPath[3:] // 去掉开头的 "/v1"，保留 "/models"
	}

	fullURL := endpoint + modelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	if b.cfg.Pool != nil {
		if k, err := b.acquireOwnFaceKey(); err == nil {
			req.Header.Set("Authorization", "Bearer "+k.Key)
			defer b.cfg.Pool.ReportSuccess(k)
		} else {
			// Log when we can't acquire a key for ListModels
			return nil, fmt.Errorf("list models: failed to acquire key: %w", err)
		}
	} else {
		return nil, fmt.Errorf("list models: pool is nil for provider %s", b.cfg.Name)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 先判状态码再解码:路径/鉴权错时上游回的是 HTML 404 或空 body,
	// 直接解码会把真因(404)埋成 "decode models: invalid character '<'"。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("list models: %s %s → status %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// Close 释放 http client
func (b *Base) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// SetPool P30: 让 Server 把从 DB 读出来的 Pool 注入到 Base
// 因为 Manager.LoadFromConfig 时 Pool 还是 nil(那时 DB 还没读),
// 启动后 Server.New 再注入
func (b *Base) SetPool(p *keypool.Pool) {
	b.cfg.Pool = p
}

// DefaultOpenAIUsageParser 标准 OpenAI 协议的 Usage 解析器
// 支持标准 OpenAI 格式 + DeepSeek 扩展字段 + MiniMax/qwen 等标准缓存字段
type DefaultOpenAIUsageParser struct{}

// Parse 按响应形状分派到对应解析器(本身不解析,只选形状)。
//
// P-responses-usage: OpenAI 有两套互不兼容的 usage 形状,同一个 openai 协议面
// 两者都会遇到(Chat Completions 走 /chat/completions,Codex 客户端走 /responses):
//
//	Chat Completions: {"usage":{"prompt_tokens":N,"completion_tokens":M,...}}
//	Responses:        {"response":{"usage":{"input_tokens":N,"output_tokens":M,...}}}
//
// 顺序是 Responses 先判、Chat Completions 回落 —— 不能反过来:
// Responses 非流式的 usage 也在顶层,但字段名是 input_tokens,而两套共用
// total_tokens。若先跑 Chat Completions,它只判「usage 键是否存在」就返回,
// 会得到 prompt/completion=0、total 有值的半条零值记录,且永不回落。
// 反向则安全:Chat Completions 没有 input_tokens,不会被 Responses 误判。
func (p *DefaultOpenAIUsageParser) Parse(body []byte) *provider.Usage {
	if u := parseResponsesUsage(body); u != nil {
		return u
	}
	return parseOpenAIUsage(body)
}

// parseOpenAIUsage 从 OpenAI Chat Completions 响应中抽取 usage
// 基础格式: {"usage": {"prompt_tokens": N, "completion_tokens": M, "total_tokens": T}}
//
// P65: 同时抽取顶层 "model" 字段(上游响应的真实 model 名,例如 "deepseek-v4-pro")
// proxy 写入 UsageRecord.ModelID 时优先用此字段覆盖客户端请求的 model
//
// DeepSeek 扩展:
//   - prompt_cache_hit_tokens / prompt_cache_miss_tokens (cache 命中/未命中)
//   - completion_tokens_details.reasoning_tokens (思考模式消耗)
//
// P-provider-vendor: OpenAI 标准缓存字段(MiniMax / OpenAI 官方 / qwen 等):
//   - prompt_tokens_details.cached_tokens — 与 DeepSeek 风格并存,CacheReadTokens = 两者之和
//
// 这些字段记在 RawUsage 里,Gateway 用作可选的精细计费输入。
func parseOpenAIUsage(body []byte) *provider.Usage {
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			TotalTokens           int `json:"total_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			// P-provider-vendor: OpenAI 标准缓存字段(MiniMax / OpenAI 官方 / qwen 等)
			// 与 DeepSeek 风格 prompt_cache_hit_tokens 并存,二者都算 CacheReadTokens
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}

	reasoningTokens := 0
	if resp.Usage.CompletionTokensDetails != nil {
		reasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
	}

	cachedTokens := 0
	if resp.Usage.PromptTokensDetails != nil {
		cachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
	}

	raw := map[string]interface{}{
		"prompt_tokens":            resp.Usage.PromptTokens,
		"completion_tokens":        resp.Usage.CompletionTokens,
		"total_tokens":             resp.Usage.TotalTokens,
		"prompt_cache_hit_tokens":  resp.Usage.PromptCacheHitTokens,
		"prompt_cache_miss_tokens": resp.Usage.PromptCacheMissTokens,
		"cached_tokens":            cachedTokens,
		"reasoning_tokens":         reasoningTokens,
	}

	cacheRead := resp.Usage.PromptCacheHitTokens + cachedTokens

	u := &provider.Usage{
		Model: resp.Model, // P65: 上游响应的真实 model 名
		// P-cache-dedup: 契约要求 PromptTokens = **不计 cache** 的输入(见 provider.Usage 注释),
		// 而 OpenAI 系的 prompt_tokens 是含缓存的完整输入 → 必须扣掉,否则缓存部分
		// 先按 input 价、再按 cache_read 价各计一次(重复计费)。
		PromptTokens:     uncachedInput(resp.Usage.PromptTokens, cacheRead),
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		// P40: DeepSeek 的 cache 模型 — prompt_cache_hit_tokens 视为 cache read。
		// P-provider-vendor: OpenAI 标准 cached_tokens(MiniMax 等)同样按缓存价计费,
		// 与 DeepSeek 风格并存相加(同一响应不会两种都给,相加等价于取到哪个算哪个)。
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: 0,
		RawUsage:            raw,
	}
	return u
}

// uncachedInput 把「含缓存的完整输入」换算成契约要求的「不计 cache 的输入」。
//
// 为什么用减法而不是直接取上游的 miss 字段:只有 DeepSeek 风格给
// prompt_cache_miss_tokens,OpenAI 标准形状(cached_tokens)和 Responses 形状
// 都只给「完整输入 + 命中量」两个数,uncached 必须自己算。用一个公式覆盖三种形状,
// 避免"按厂商分叉"退化成各写一份(那正是重复计费能藏这么久的原因)。
//
// 正确性依赖「命中量是完整输入的子集」。该前提已在 39532 份真实上游响应体上验证:
// DeepSeek prompt == hit+miss(11 例 0 反例)、OpenAI prompt >= cached(50 例 0 反例)、
// Responses input >= cached(837 例,唯一反例是 total 差 1 的上游舍入噪声,
// 子集性质本身仍成立)。
//
// floor 到 0 是防御上游偶发的不自洽(命中量 > 完整输入):宁可把该请求算成
// 「全部命中」少收一点,也不能返回负 token 让 cost 变成负数冲掉别的账。
func uncachedInput(promptTokens, cacheReadTokens int) int {
	if n := promptTokens - cacheReadTokens; n > 0 {
		return n
	}
	return 0
}

// parseResponsesUsage 从 OpenAI Responses API 响应中抽取 usage(P-responses-usage)
//
// 两个位置都要认 —— 流式和非流式 usage 挂的层级不一样:
//
//	非流式: {"usage":{"input_tokens":N,...}}                     顶层
//	流式:   data: {"type":"response.completed","response":{"usage":{...}}}  嵌在 response 里
//
// 实测 tokenmarket-codex(gpt-5.6-sol)流式末帧:
//
//	{"type":"response.completed","response":{...,"usage":{
//	   "input_tokens":341797,"input_tokens_details":{"cached_tokens":331648},
//	   "output_tokens":156,"output_tokens_details":{"reasoning_tokens":0},
//	   "total_tokens":341953}}}
//
// 语义与 Chat Completions 一致(**不同于** Anthropic):
//   - input_tokens 是完整输入,已含 cached_tokens(cached 是其子集)
//   - 故 PromptTokens = input_tokens、CacheReadTokens = cached_tokens,
//     与 parseOpenAIUsage 保持同一口径(total = input + output)
//   - Responses 无 cache 创建概念 → CacheCreationTokens = 0
//
// 流早期的 response.created / response.in_progress 事件带的是 "usage":null,
// 中间 delta 事件根本没有 usage → 两者都返回 nil,不会覆盖末帧的真实值。
func parseResponsesUsage(body []byte) *provider.Usage {
	type responsesUsage struct {
		InputTokens       int `json:"input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		TotalTokens       int `json:"total_tokens"`
		InputTokensDetail *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetail *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	var resp struct {
		Model    string          `json:"model"`
		Usage    *responsesUsage `json:"usage"`
		Response *struct {
			Model string          `json:"model"`
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}

	// 取到哪个算哪个:顶层(非流式)优先,否则钻进 response(流式末帧)
	u, model := resp.Usage, resp.Model
	if u == nil && resp.Response != nil {
		u, model = resp.Response.Usage, resp.Response.Model
	}
	// 关键守卫:usage 缺失 / null / 全零 一律返回 nil。
	// 判 input+output 而不判 total —— total 是两套形状共用的字段名,
	// 只判 total 会把 Chat Completions 的 body 认成 Responses(见 Parse 注释)。
	if u == nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}

	cachedTokens := 0
	if u.InputTokensDetail != nil {
		cachedTokens = u.InputTokensDetail.CachedTokens
	}
	reasoningTokens := 0
	if u.OutputTokensDetail != nil {
		reasoningTokens = u.OutputTokensDetail.ReasoningTokens
	}

	totalTokens := u.TotalTokens
	if totalTokens == 0 {
		totalTokens = u.InputTokens + u.OutputTokens
	}

	return &provider.Usage{
		Model: model, // P65: 上游响应的真实 model 名
		// P-cache-dedup: input_tokens 是含缓存的完整输入,扣掉命中量才符合
		// PromptTokens「不计 cache 的输入」契约(同 parseOpenAIUsage,理由见 uncachedInput)。
		PromptTokens:     uncachedInput(u.InputTokens, cachedTokens),
		CompletionTokens: u.OutputTokens,
		TotalTokens:      totalTokens,
		CacheReadTokens:  cachedTokens,
		RawUsage: map[string]interface{}{
			"input_tokens":     u.InputTokens,
			"output_tokens":    u.OutputTokens,
			"total_tokens":     totalTokens,
			"cached_tokens":    cachedTokens,
			"reasoning_tokens": reasoningTokens,
		},
	}
}

// injectStreamUsage 若请求 body 没有 stream_options,自动注入 include_usage=true
// 让流式响应末尾带 usage,便于在 Gateway 端记账
//
// 用 JSON 解析/序列化确保生成的 body 仍是合法 JSON(而不只是字符串拼接)
func injectStreamUsage(body []byte) []byte {
	// 空 body 直接返回默认值
	if len(bytes.TrimSpace(body)) == 0 {
		return []byte(`{"stream_options":{"include_usage":true}}`)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		// 解析失败:不修改,直接返回原 body
		return body
	}
	if _, exists := m["stream_options"]; exists {
		return body
	}
	m["stream_options"] = map[string]interface{}{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// parseRetryAfter 解析 Retry-After header(秒数或 HTTP 日期)
// 简化:只支持秒数
