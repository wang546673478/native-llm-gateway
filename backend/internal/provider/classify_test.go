package provider

import (
	"net/http"
	"testing"
)

// P-quota-minimax-429: MiniMax anthropic 面的套餐耗尽错误是
// HTTP 429 + {"error":{"type":"rate_limit_error","message":"已达到 Token Plan
// 用量上限:...(2056)"}}(实测 2026-08-05),不是文档说的 200+base_resp。
// 若被误分类成 rate_limit → key 标 COOLING 而非 QUOTA_EXCEEDED → 无轮询恢复,
// 整链掉到 api 层(deepseek)。这组测试钉住 429+quota 关键字的识别。
func TestClassifyErrorWithBody_MiniMax429Quota(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"已达到 Token Plan 用量上限：请升级 Token Plan 套餐或购买积分补充用量。 (2056)"},"request_id":"x"}`)

	got := ClassifyErrorWithBody(http.StatusTooManyRequests, body)
	if got != ErrorTypeQuotaExceeded {
		t.Errorf("classify(429, MiniMax quota body) = %q, want quota_exceeded", got)
	}
}

// 普通 429(无 quota 关键字)保持 rate_limit — 不破坏现有冷却语义
func TestClassifyErrorWithBody_Plain429StaysRateLimit(t *testing.T) {
	body := []byte(`{"error":{"message":"Too many requests, slow down"}}`)

	got := ClassifyErrorWithBody(http.StatusTooManyRequests, body)
	if got != ErrorTypeRateLimit {
		t.Errorf("classify(429, plain body) = %q, want rate_limit", got)
	}
}

// 回归:GLM 1113 余额不足(429 + 中文 body)仍识别为 quota
func TestClassifyErrorWithBody_Glm1113Quota(t *testing.T) {
	body := []byte(`{"error":{"code":"1113","message":"余额不足或无可用资源包,请充值。"}}`)

	got := ClassifyErrorWithBody(http.StatusTooManyRequests, body)
	if got != ErrorTypeQuotaExceeded {
		t.Errorf("classify(429, GLM 1113 body) = %q, want quota_exceeded", got)
	}
}

// P-quota-minimax-429-fix 回归:纯限流 429("rate limit exceeded")不能升级成
// quota_exceeded — 否则 healthy key 被误杀到 poll 恢复,期间 token_plan 桶空
// 整链掉到 api 层(2026-08-05 实测:双 key 同时不可用 → 直接 failover 到 deepseek)
func TestClassifyErrorWithBody_PlainRateLimit429StaysRateLimit(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_error","message":"rate limit exceeded, please retry later"}}`)
	got := ClassifyErrorWithBody(429, body)
	if got != ErrorTypeRateLimit {
		t.Errorf("classify(429, plain rate-limit body) = %q, want rate_limit", got)
	}
}
