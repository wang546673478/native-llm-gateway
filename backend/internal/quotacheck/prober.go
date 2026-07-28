// Package quotacheck 实现 token plan 配额恢复检测
// P68 — 配合 keypool.Pool 自动恢复 quota_exceeded 的 Key
//
// 架构:
//   - 有 balance API 的 provider(deepseek/glm)走主动 polling
//   - 没 balance API 的 provider(kimi/minimax/qwen/gemini)走 probe-on-cooldown
//   - 所有 candidate 入一个 min-heap(按 nextAt 排序),单 goroutine tick 处理
package quotacheck

import (
	"context"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// Result 单次探测的归类结果
type Result int

const (
	// ResultRestored 2xx — 配额恢复,key 重新可用
	ResultRestored Result = iota
	// ResultStillExhausted 4xx 含 quota 关键字 — 配额仍耗尽
	ResultStillExhausted
	// ResultAuthFailed 401/403 无 quota 关键字 — key 废了,不再探测
	ResultAuthFailed
	// ResultTransportError timeout / connection refused — 网络问题,继续探测
	ResultTransportError
)

// Prober 知道如何问"这个 key 的 quota 恢复了吗?"针对某个具体 provider
// Prober 实例应该是无状态的(global,共享);每次 Probe 调一次 HTTP
type Prober interface {
	// Probe 探测一次 key 的 quota 状态
	// baseURL 是 provider 的 endpoint(可能跟 cfg 略有不同)
	// 返回探测结果(由 Prober 自己解析 HTTP 响应)
	Probe(ctx context.Context, baseURL string, k *keypool.Key) Result
}

// Balancer 是 Prober 的快路径:provider 提供余额查询 API,直接 GET 拿 balance
// 用于主动 polling(不需要发请求试,也不需要计入配额)
type Balancer interface {
	// FetchBalance 拉取当前余额
	// 返回 (balance, hasQuota, error) — hasQuota 表示余额 > 0
	FetchBalance(ctx context.Context, baseURL string, k *keypool.Key) (Balance, error)
}

// Balance 余额查询结果
type Balance struct {
	Raw      float64 // 解析出的余额数值(单位由 source 决定)
	HasQuota bool    // true 表示余额 > 0,key 可用
	Source   string  // "deepseek:/user/balance" 之类,用于日志/metrics
}

// global registry — provider 包在 init() 里 Register
var (
	proberRegistry   = map[string]Prober{}   // provider name → Prober
	balancerRegistry = map[string]Balancer{} // provider name → Balancer
)

// RegisterProber 注册一个 provider 的 Prober(后注册的覆盖前者)
func RegisterProber(providerName string, p Prober) {
	proberRegistry[providerName] = p
}

// RegisterBalancer 注册一个 provider 的 Balancer
func RegisterBalancer(providerName string, b Balancer) {
	balancerRegistry[providerName] = b
}

// LookupProber 查 Prober(没有返 nil,调用方需自己 fallback)
func LookupProber(providerName string) Prober {
	return proberRegistry[providerName]
}

// LookupBalancer 查 Balancer(没有返 nil)
func LookupBalancer(providerName string) Balancer {
	return balancerRegistry[providerName]
}

// hasQuotaKeyword 检查 status code + body 是否含 quota_exceeded 信号
// 用于把 HTTP 响应归类到 Result
// 关键字列表与 provider.ClassifyErrorWithBody 保持一致
var quotaKeywords = []string{
	"insufficient_quota",
	"quota_exceeded",
	"insufficient_balance",
	"insufficient credits",
	"insufficient credit",
	"exceeded_current_quota",
	"余额不足",
	"quota exceeded",
	"rate_limit_reached",
	"billing_not_active",
	"payment_required",
	"plan required",
	"plan_required",
}

func hasQuotaKeyword(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := string(body)
	for _, kw := range quotaKeywords {
		if contains(s, kw) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// isAuthLikeStatus 401/403 不带 quota 关键字 → key 废了
func isAuthLikeStatus(status int) bool {
	return status == 401 || status == 403
}

// DefaultProbeTimeout 探测默认超时(单次 HTTP 请求)
const DefaultProbeTimeout = 10 * time.Second
