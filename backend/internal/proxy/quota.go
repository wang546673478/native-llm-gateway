package proxy

import (
	"context"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
)

// QuotaChecker 配额探测接口 — proxy 不再直接 import quotacheck 包
// 由 server 注入默认实现(内部调 quotacheck.CheckQuota),也可以注入 mock 测试
// 单一职责:proxy 只依赖窄接口,不依赖 quotacheck 实现细节
type QuotaChecker interface {
	// CheckQuota 探测指定 provider + key 是否还有额度(token_plan 层降档决策用)
	// 返回 has=true 表示还有额度;err 非 nil 表示探测失败(按未耗尽处理)
	CheckQuota(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error)
}

// CheckQuotaFunc 函数式实现 — 便于 server 用 quotacheck.CheckQuota 直接注入
type CheckQuotaFunc func(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error)

// CheckQuota 实现 QuotaChecker 接口
func (f CheckQuotaFunc) CheckQuota(ctx context.Context, providerName, baseURL string, k *keypool.Key) (bool, error) {
	return f(ctx, providerName, baseURL, k)
}
