package provider

import "github.com/wang546673478/native-llm-gateway/internal/keypool"

// ToPool 从 provider 拿到的 Pool 注入值(interface{})安全还原成 *keypool.Pool。
//
// 单一职责:统一 6 个 vendor 包(toPool)重复的 `interface{}(*keypool.Pool)` 还原,
// 消除复制粘贴型耦合 —— 之前 deepseek/glm/qwen/minimax/mimo/gemini 各有一份
// 字节相同的 toPool,若注入契约变化需 6 处同步,漏一处则静默返 nil Pool → 运行时
// "keypool not configured"。集中到 provider(已依赖 keypool)一个实现。
func ToPool(p interface{}) *keypool.Pool {
	if p == nil {
		return nil
	}
	if pp, ok := p.(*keypool.Pool); ok {
		return pp
	}
	return nil
}
