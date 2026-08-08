// Package builtin 触发所有内置 Provider 包的 init() 注册
//
// 单一职责:这就是个"装载点",通过一次 import 让所有内置厂商生效
// 替代 cmd/gateway/main.go 里的 6 个 blank import 列表
// 加新厂商时只需在这里加一行(blank import)
//
// 自动注册:加 init() 在这里用 go:generate 扫描更彻底,但引入复杂度;
// 当前 6 个厂商手动维护成本低。如果想自动,看 docs/architecture.md 里
// "go:generate 替代 blank import" 设计
package builtin

import (
	// 每个 blank import 触发对应包的 init() 调用,注册到 provider.Default() Registry
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/deepseek"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/gemini"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/glm"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/mimo"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/minimax"
	_ "github.com/wang546673478/native-llm-gateway/internal/provider/qwen"
)
