package provider

import "context"

// ClassifyTransportError 判网络上/HTTP 传输错误归类。
//
// 单一职责:三个协议 base(openai_compatible / anthropic_compatible / google)
// 此前各自 inline 相同逻辑 `errType := Connection; if ctx.Err()==DeadlineExceeded
// { errType=Timeout }`(6 处复制)。抽出单源,消除复制粘贴型耦合 —— 一旦传输类
// 判定的语义要改(如加 DNS/连接重置细分),只改这一处,不再漏改其余。
//
// 语义:连接错误默认 Connection;若 context 已 DeadlineExceeded(请求超时被砍)→
// 归类 Timeout(让它进熔断/重试判定而非普通连接错)。
func ClassifyTransportError(ctx context.Context, err error) ErrorType {
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return ErrorTypeTimeout
	}
	return ErrorTypeConnection
}
