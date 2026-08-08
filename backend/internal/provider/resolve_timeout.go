package provider

import "time"

// resolveProviderTimeout provider 请求超时解析:
//
//	providerTimeout > 0 → 用 provider 自己的;
//	否则 fallback 到全局默认(defaultTimeout,来自 config.timeouts.provider_default);
//	仍 0 → 返回 0,由各协议 base 的 NewBase 落到自身 60/90s 默认。
//
// 低耦合修复:config 的 provider_default 此前定义了但无任何消费方(silent no-op),
// 而 provider timeout 默认硬编码在各 base。这里让全局默认在 provider 层生效,
// 保持 base 层的 per-protocol 默认兜底。
func resolveProviderTimeout(providerTimeout, defaultTimeout time.Duration) time.Duration {
	if providerTimeout > 0 {
		return providerTimeout
	}
	return defaultTimeout
}
