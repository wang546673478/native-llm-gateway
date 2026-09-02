package provider

import (
	"context"
	"errors"
)

// ClassifyTransportError 判网络上/HTTP 传输错误归类。
//
// 单一职责:三个协议 base(openai_compatible / anthropic_compatible / google)
// 此前各自 inline 相同逻辑 `errType := Connection; if ctx.Err()==DeadlineExceeded
// { errType=Timeout }`(6 处复制)。抽出单源,消除复制粘贴型耦合 —— 一旦传输类
// 判定的语义要改(如加 DNS/连接重置细分),只改这一处,不再漏改其余。
//
// 语义:客户端取消或 error 包含 context.Canceled → ClientDisconnected；请求截止
// → Timeout；其他传输错误 → Connection。ClientDisconnected 不属于上游/key 故障。
func ClassifyTransportError(ctx context.Context, err error) ErrorType {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return ErrorTypeClientDisconnected
		case context.DeadlineExceeded:
			return ErrorTypeTimeout
		}
	}
	if errors.Is(err, context.Canceled) {
		return ErrorTypeClientDisconnected
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTypeTimeout
	}
	return ErrorTypeConnection
}

// ShouldReportKeyPool reports whether a provider-side transport result may
// update key-pool health. Once the caller's context is terminal there is no
// reliable evidence that the upstream failed: the client (or the gateway
// request deadline) ended the attempt. Skipping the report prevents a parent
// cancellation/deadline from cooling or opening the circuit for an otherwise
// healthy key. Provider-owned HTTP client timeouts still report because the
// original context remains active.
func ShouldReportKeyPool(ctx context.Context, errType ErrorType) bool {
	if errType == ErrorTypeClientDisconnected {
		return false
	}
	if ctx == nil {
		return true
	}
	return ctx.Err() == nil
}
