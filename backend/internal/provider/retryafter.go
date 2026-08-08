package provider

import (
	"fmt"
	"time"
)

// ParseRetryAfter 解析 HTTP Retry-After 头为冷却时长(duration)。
//
// 单一职责:统一 openai_compatible / anthropic_compatible 两处字节相同的
// parseRetryAfter(消除复制粘贴型耦合)。当前只支持秒数(整数),不支持
// HTTP-date 格式(RFC 7231);解析失败/空返回 0(保守方向)。
func ParseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}
