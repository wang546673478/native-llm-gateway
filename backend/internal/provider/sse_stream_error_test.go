package provider

import "testing"

// 本文件的 payload 全部抄自生产 access log body 文件(2026-08-26 排障),
// 不是构造的样本 —— 手编样本证明不了真实上游的形状。

// tokenmarket-codex 整条流只有这一个事件(262 字节),然后收流。
// 网关此前把它记成 200/ok:不冷却 key、不 failover。
const realCodexFailedEvent = `data: {"type":"response.failed","response":{"id":"resp_1b4832e3503147b6839fabb63b27ba3c","object":"response","model":"gpt-5.6-sol","status":"failed","error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded for account, please retry later"}}}` + "\n\n"

// minimax anthropic 面:42 个正常事件之后才发的内容审核错误(流中途)。
const realMiniMaxMidStreamError = `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"output new_sensitive (1027)"}}` + "\n\n"

// tokenmarket-codex **成功**流的第一个事件 —— 抄自线上验证请求(2026-08-26 12:40,
// 200/9KB 正常回答)。它必然落在 peek 窗口里,所以必须放过:一旦误判,codex 面
// 每条成功流都会在开头被打成失败并换 key。
// 注意它含 limit_reached / allowed 这类"像错误"的字段,却既无 "error" 键也无
// failed 字样 —— 预筛就能拒掉,连 JSON 都不解析。
const realCodexRateLimitsHeader = `data: {"type":"codex.rate_limits","plan_type":"team","rate_limits":{"allowed":true,"limit_reached":false,"primary":{"used_percent":53,"window_minutes":300,"reset_after_seconds":16093,"reset_at":1787735303},"secondary":{"used_percent":26,"window_minutes":10080,"reset_after_seconds":553306,"reset_at":1788272516}},"code_review_rate_limits":null,"additional_rate_limits":null,"credits":{"has_credits":false,"unlimited":false,"balance":null},"promo":null}` + "\n\n"

func TestParseSSEStreamError_RealUpstreamPayloads(t *testing.T) {
	tests := []struct {
		name        string
		fragment    string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "codex response.failed(错误在 response 内层)",
			fragment:    realCodexFailedEvent,
			wantCode:    "rate_limit_exceeded",
			wantMessage: "Concurrency limit exceeded for account, please retry later",
		},
		{
			name:        "minimax error 事件(错误在顶层)",
			fragment:    realMiniMaxMidStreamError,
			wantCode:    "api_error", // 无 code 字段 → 回退用 error.type
			wantMessage: "output new_sensitive (1027)",
		},
		{
			name:        "只说 status=failed 不给细节",
			fragment:    `data: {"type":"response.failed","response":{"status":"failed"}}`,
			wantCode:    "",
			wantMessage: "upstream reported response status=failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSSEStreamError([]byte(tt.fragment))
			if got == nil {
				t.Fatalf("ParseSSEStreamError 漏判 — 这正是 200/ok 误记的根因")
			}
			if got.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tt.wantCode)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tt.wantMessage)
			}
		})
	}
}

// TestParseSSEStreamError_NormalEventsNotFlagged 误判防线:正常事件一条都不能命中。
// 判定认结构(顶层/response 内层 error 对象)不认关键词 —— 正文里出现 "error"
// 字样的正常回答必须放过,否则会把好流打成失败并冷却 healthy key。
func TestParseSSEStreamError_NormalEventsNotFlagged(t *testing.T) {
	normal := []struct {
		name     string
		fragment string
	}{
		{"openai chat delta", `data: {"id":"x","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`},
		{"responses created(error 显式 null)", `data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","error":null}}`},
		{"responses completed", `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","error":null}}`},
		{"anthropic message_start", `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10}}}`},
		{"anthropic content_block_delta", `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`},
		{"DONE 哨兵", `data: [DONE]`},
		{"正文里聊 error 这个词", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"you should handle the error and log error details"}}`},
		{"正文里贴 JSON 错误代码片段", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"例如 {\"error\":{\"code\":\"oops\"}} 这样的响应"}}`},
		{"空片段", ``},
		{"非 JSON", `data: not json at all`},
		{"codex 成功流首事件(线上实抓)", realCodexRateLimitsHeader},
	}
	for _, tt := range normal {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseSSEStreamError([]byte(tt.fragment)); got != nil {
				t.Errorf("误判正常事件为上游错误: code=%q msg=%q", got.Code, got.Message)
			}
			// 预筛也应放过(省掉热路径的 JSON 解析);它是超集,允许比解析器宽,
			// 但正常事件里最常见的这几种不该白解析一遍。
			if tt.fragment == realCodexRateLimitsHeader && MayContainSSEError([]byte(tt.fragment)) {
				t.Errorf("预筛没拒掉 codex 成功流首事件 → 每条成功流都白解析一次 JSON")
			}
		})
	}
}

// TestMayContainSSEError_IsSupersetOfParser 锁死预筛的超集不变式。
//
// 流式热路径先过 MayContainSSEError 再决定要不要 JSON 解析。预筛返回 false
// 就等于宣布"这段不可能有错误",解析器再没机会看 —— 所以凡是解析器能命中的
// 片段,预筛必须也返回 true。反向从解析器的正样本校验,不手抄一份清单。
//
// 这条测试红了 = 给解析器加了新错误形状但没同步预筛关键词,流中途那种形状会静默漏判
// (2026-08-26 首版就踩过:预筛只查 "error",漏掉只给 status=failed 的形状)。
func TestMayContainSSEError_IsSupersetOfParser(t *testing.T) {
	// 解析器能命中的全部已知形状
	positives := []string{
		realCodexFailedEvent,
		realMiniMaxMidStreamError,
		`data: {"type":"response.failed","response":{"status":"failed"}}`,
		`data: {"error":{"code":"insufficient_quota","message":"no balance"}}`,
		`{"error":{"message":"raw json 非 SSE 包装"}}`,
	}
	for _, p := range positives {
		if ParseSSEStreamError([]byte(p)) == nil {
			t.Fatalf("正样本清单过期,解析器已不认这段(先修清单): %.80s", p)
		}
		if !MayContainSSEError([]byte(p)) {
			t.Errorf("预筛漏判解析器能命中的片段 → 热路径永不解析: %.120s", p)
		}
	}
}

// TestClassifySSEStreamError_NotRateLimit 锁死刻意的分类决策。
//
// 上游 code 明明叫 rate_limit_exceeded,但**不能**映射到 ErrorTypeRateLimit:
// 那会走 retrySameKeyRateLimit(同一把 key 无延迟重试 10 次)。该策略假设
// "429 很便宜",而流内错误是上游挂住 ~32s 才失败(实测 latency 31.5-34.3s),
// 10 次 ≈ 320s,比不识别更难受。server_error 落 isNetworkClass → 立刻换 key。
//
// 这条测试红了 = 有人"顺手改成语义更像的 rate_limit",把 32s×10 的坑挖回来。
func TestClassifySSEStreamError_NotRateLimit(t *testing.T) {
	se := ParseSSEStreamError([]byte(realCodexFailedEvent))
	if se == nil {
		t.Fatal("前置解析失败")
	}
	got := ClassifySSEStreamError(se)
	if got == ErrorTypeRateLimit {
		t.Fatalf("不能分类成 rate_limit — 会触发同 key 重试 10 次 × 32s")
	}
	if got != ErrorTypeServerError {
		t.Errorf("ClassifySSEStreamError = %q, want %q", got, ErrorTypeServerError)
	}
	// server_error 必须可重试且落网络类,否则换不到 key
	pe := &ProviderError{ErrorType: got}
	if !pe.IsRetryable() {
		t.Error("server_error 必须 retryable,否则 failover 不启动")
	}
}

// TestClassifySSEStreamError_RealQuotaUpgrades 真额度耗尽仍升级 quota_exceeded
// (复用 LooksLikeQuotaError 的强配额词表,不另立一套关键词)。
func TestClassifySSEStreamError_RealQuotaUpgrades(t *testing.T) {
	tests := []struct {
		name string
		se   *SSEStreamError
		want ErrorType
	}{
		{"并发限制不是额度耗尽", &SSEStreamError{Code: "rate_limit_exceeded", Message: "Concurrency limit exceeded for account, please retry later"}, ErrorTypeServerError},
		{"余额不足", &SSEStreamError{Code: "insufficient_quota", Message: "You exceeded your current quota"}, ErrorTypeQuotaExceeded},
		{"中文余额", &SSEStreamError{Message: "账户余额不足"}, ErrorTypeQuotaExceeded},
		{"内容审核不是额度", &SSEStreamError{Code: "api_error", Message: "output new_sensitive (1027)"}, ErrorTypeServerError},
		{"nil 返空", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifySSEStreamError(tt.se); got != tt.want {
				t.Errorf("ClassifySSEStreamError = %q, want %q", got, tt.want)
			}
		})
	}
}
