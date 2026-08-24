// Package provider - UsageParser 接口定义
package provider

// UsageParser 从响应体中提取 Usage 信息
// 不同厂商/中转商可以有不同的解析逻辑,但不影响请求的透传
type UsageParser interface {
	// Parse 从原始响应 body 中提取 Usage
	// 返回 nil 表示响应中没有 usage 信息
	Parse(body []byte) *Usage
}
