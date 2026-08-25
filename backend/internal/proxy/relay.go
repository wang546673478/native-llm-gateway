// Package proxy — 中转站直通模式辅助函数
package proxy

import (
	"github.com/wang546673478/native-llm-gateway/internal/auth"
)

// isRelayPassthrough 判断 Gateway Key 是否应该使用中转站直通模式
// 检查两个维度：
//   1. GatewayKey.Providers 绑定的 provider 是否全是中转站
//   2. GatewayKey.ProviderKeyIDs 反查出的 provider 是否全是中转站
// 任一维度满足即返回 true
func (e *Engine) isRelayPassthrough(gk *auth.GatewayKey) bool {
	if gk == nil {
		return false
	}

	mgr := e.router.Manager()

	// 维度 1: 检查 Providers 字段绑定（Provider 类型绑定）
	if len(gk.Providers) > 0 {
		allRelay := true
		for _, name := range gk.Providers {
			if !mgr.IsRelay(name) {
				allRelay = false
				break
			}
		}
		if allRelay {
			return true
		}
	}

	// 维度 2: 检查 ProviderKeyIDs 绑定（具体 key 绑定）
	if len(gk.ProviderKeyIDs) > 0 {
		boundProviders := e.getBoundProviders(gk.ProviderKeyIDs)
		allRelay := e.allAreRelays(boundProviders)
		if allRelay {
			return true
		}
	}

	// 都没绑定 = 不限制 = 不是中转站模式
	return false
}

// getBoundProviders 从 ProviderKeyIDs 反查归属的 provider（去重）
func (e *Engine) getBoundProviders(keyIDs []uint) []string {
	providers := make(map[string]bool)
	idSet := make(map[uint]bool)
	for _, id := range keyIDs {
		idSet[id] = true
	}

	// 遍历所有 pool，找出 keyIDs 归属的 provider
	for name := range e.router.Manager().GetAll() {
		if pool := e.router.Pool(name); pool != nil {
			for _, k := range pool.KeyPtrs() {
				id := parseKeyIDUint(k.ID)
				if idSet[id] {
					providers[name] = true
				}
			}
		}
	}

	result := make([]string, 0, len(providers))
	for name := range providers {
		result = append(result, name)
	}
	return result
}

// allAreRelays 检查所有 provider 是否都是中转站
func (e *Engine) allAreRelays(providers []string) bool {
	if len(providers) == 0 {
		return false
	}
	mgr := e.router.Manager()
	for _, name := range providers {
		if !mgr.IsRelay(name) {
			return false
		}
	}
	return true
}

// parseKeyIDUint 把 Key.ID (格式 "<provider>-key-<N>" 或纯数字字符串) 转 uint
// 与 keypool/pool.go parseKeyIDUint 逻辑一致
func parseKeyIDUint(id string) uint {
	var n uint
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint(c-'0')
	}
	return n
}
