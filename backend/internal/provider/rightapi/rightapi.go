// Package rightapi 实现 Right Code（https://rightapi.ai/）API 中转站 Provider。
//
// Right Code 是一个 AI Agent API 中转站：流量经其网关反向到 OpenAI / Anthropic /
// Gemini 上游官网，一个统一 API Key 访问所有渠道（后端平台按余额按量扣费，
// 余额不足返回 429）。
//
// 本包接入的协议面（共享 vendor="rightapi"，共享同一个 key 池），每个后缀端点是一个面：
//   - rightapi-codex       → OpenAI 协议，endpoint /codex/v1，Responses API + chat/completions
//   - rightapi-grok        → OpenAI 协议，endpoint /grok/v1，走 Responses API
//   - rightapi-claude      → Anthropic 协议，endpoint /claude（官渠）
//   - rightapi-claude-aws  → Anthropic 协议，endpoint /claude-aws（AWSQ 逆向渠道）
//   - rightapi-gemini      → OpenAI 协议，endpoint /gemini（Google Gemini，走 chat/completions）
//
// 面名约定 = rightapi-<后缀>（endpoint 的路径段），一眼对得上「这个面指向哪个上游渠道」。
//
// 鉴权：Authorization: Bearer <key> 或 x-api-key（Gemini 渠道也走 Bearer，不走
// x-goog-api-key —— rightapi 中转站上层已收敛鉴权）。单一 key 对多渠道通用 →
// 同 vendor 共享 key 池（buildKeyPools 按 vendor 复用）。
//
// 注意：rightapi 还有一个 /claude-sale（官渠特惠）渠道，**不接入** —— 它要求
// 「标准 Claude Code 客户端」的账号组（实测返回 503 `this group only allows
// Claude Code clients`），是 key 在平台上的账户层级开关，网关伪造 header/指纹
// 也绕不过。用官渠 /claude 即可拿到相同模型（含 claude-fable-5）。
//
// 无官方余额查询 API → 不写 balancer，落到 QuotaRecoveryProbe 模式（vendorHasBalancer
// 返回 false，不会永久死 key，余额不足由上游 429 错误码驱动降档）。
package rightapi

import (
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// 注册面名 —— vendor 都是 "rightapi"（共享 key 池）。
// 命名 = rightapi-<endpoint 后缀>，与 config.yaml 的 providers.<name> 一一对应。
const (
	codexName     = "rightapi-codex"
	grokName      = "rightapi-grok"
	claudeName    = "rightapi-claude"     // 官渠，endpoint /claude
	claudeAWSName = "rightapi-claude-aws" // AWSQ 逆向渠道，endpoint /claude-aws
	geminiName    = "rightapi-gemini"     // Google Gemini，endpoint /gemini

	// Vendor 所有注册面归到同一厂商名
	vendor = "rightapi"
)

// init 注册所有协议面。vendor 统一是 "rightapi"，server.buildKeyPools 据此共享同一个 key 池。
func init() {
	provider.RegisterGlobalWithProtocolVendor(codexName, newCodex, provider.ProtocolOpenAI, vendor)
	provider.RegisterGlobalWithProtocolVendor(grokName, newGrok, provider.ProtocolOpenAI, vendor)
	provider.RegisterGlobalWithProtocolVendor(claudeName, newClaude, provider.ProtocolAnthropic, vendor)
	provider.RegisterGlobalWithProtocolVendor(claudeAWSName, newClaudeAWS, provider.ProtocolAnthropic, vendor)
	provider.RegisterGlobalWithProtocolVendor(geminiName, newGemini, provider.ProtocolOpenAI, vendor)
}
