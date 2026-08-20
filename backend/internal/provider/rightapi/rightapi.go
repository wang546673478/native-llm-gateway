// Package rightapi 实现 Right Code（https://rightapi.ai/）API 中转站 Provider。
//
// Right Code 是一个 AI Agent API 中转站：流量经其网关反向到 OpenAI / Anthropic /
// Grok 上游官网，一个统一 API Key 访问所有渠道（后端平台按余额按量扣费，
// 余额不足返回 429）。
//
// 本包接入 3 个协议面（共享 vendor="rightapi"，共享同一个 key 池）：
//   - rightapi-codex   → OpenAI 协议，endpoint /codex/v1，Responses API + chat/completions
//   - rightapi-grok    → OpenAI 协议，endpoint /grok/v1，走 Responses API
//   - rightapi-claude  → Anthropic 协议，endpoint /claude，/v1/messages
//
// 鉴权：Authorization: Bearer <key> 或 x-api-key（Gemini 渠道才用 x-goog-api-key，
// 本包不接 Gemini）。单一 key 对多渠道通用 → 同 vendor 共享 key 池（buildKeyPools
// 按 vendor 复用）。
//
// 无官方余额查询 API → 不写 balancer，落到 QuotaRecoveryProbe 模式（vendorHasBalancer
// 返回 false，不会永久死 key，余额不足由上游 429 错误码驱动降档）。
package rightapi

import (
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// 三个注册面名 —— vendor 都是 "rightapi"（共享 key 池）
const (
	codexName  = "rightapi-codex"
	grokName   = "rightapi-grok"
	claudeName = "rightapi-claude"

	// Vendor 三种注册面归到同一厂商名
	vendor = "rightapi"
)

// init 注册三协议面。vendor 统一是 "rightapi"，server.buildKeyPools 据此共享同一个 key 池。
func init() {
	provider.RegisterGlobalWithProtocolVendor(codexName, newCodex, provider.ProtocolOpenAI, vendor)
	provider.RegisterGlobalWithProtocolVendor(grokName, newGrok, provider.ProtocolOpenAI, vendor)
	provider.RegisterGlobalWithProtocolVendor(claudeName, newClaude, provider.ProtocolAnthropic, vendor)
}
