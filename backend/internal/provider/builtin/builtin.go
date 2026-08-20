// Package builtin 触发所有内置 Provider 包的 init() 注册
//
// 单一职责:这就是个"装载点",通过一次 import 让所有内置厂商生效
// 替代 cmd/gateway/main.go 里的 blank import 列表
// 加新厂商时只需在这里加一行(blank import)
//
// 自动注册:加 init() 在这里用 go:generate 扫描更彻底,但引入复杂度;
// 当前厂商数量手动维护成本低。如果想自动,看 docs/architecture.md 里
// "go:generate 替代 blank import" 设计
//
// 2026-08-20 下线 gemini / qwen / glm 三家(历史用量:gemini 0、qwen 0、glm 53 次
// 且已无 key)。gemini 是 provider/google 的唯一消费者,google 协议层暂留待评估。
package builtin

import (
	// 每个 blank import 触发对应包的 init() 调用,注册到 provider.Default() Registry
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/mimo"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/minimax"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/rightapi"
)
