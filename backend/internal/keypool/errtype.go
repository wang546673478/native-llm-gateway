// Package keypool — 错误类型常量
//
// 单一职责:集中定义 ReportError 需要区分/触发的上游错误类型。
//
// 为什么在 keypool 定义而不是引用 provider:keypool 是零内部依赖的低层包(低耦合
// 标杆),import provider.ErrorType 会把 keypool 拉低、方向反转。所以 keypool
// 自己定义等价常量,值必须与 provider.ErrorType 一致 —— 一致性由
// TestErrTypeMatchesProvider 守卫测试强制(见 errtype_test.go),发现漂移立即失败,
// 避免熔断/冷却误判这种静默故障。
//
// 这些常量同时消除了 ReportError switch 里的裸字符串字面量("改一处要改多处"
// 隐患):加/改名错误类型,只需改这里 + provider + circuit,守卫测试兜底。
package keypool

// ErrorType 上游/网关错误类型 — 值须与 provider.ErrorType 保持一致
type ErrorType string

const (
	ErrorTypeRateLimit          ErrorType = "rate_limit"          // 429:限流(计费率),不触发熔断
	ErrorTypeAuth               ErrorType = "auth"                // 401/403 auth:key 本身问题 → 冷却
	ErrorTypeInvalidRequest     ErrorType = "invalid_request"     // 400:请求内容不支持,只计数
	ErrorTypeServerError        ErrorType = "server_error"        // 5xx:熔断计数
	ErrorTypeTimeout            ErrorType = "timeout"             // 超时:熔断计数
	ErrorTypeConnection         ErrorType = "connection"          // 连接/网络错误:熔断计数
	ErrorTypeClientDisconnected ErrorType = "client_disconnected" // 客户端取消:不属于 key/upstream 故障
	ErrorTypeQuotaExceeded      ErrorType = "quota_exceeded"      // 配额耗尽:标 QE 等 poll 恢复

	// breakerErrs 触发 per-key 熔断的错误类型集合(circuit 默认可计数集合同一个集合)
)

// TripsBreaker 报告一个错误类型是否计入 per-key 熔断
// breakerErrs 用 slice(map 不能做常量),查询时量小,线性扫即可
var breakerErrs = []ErrorType{ErrorTypeServerError, ErrorTypeTimeout, ErrorTypeConnection}

func TripsBreaker(errType string) bool {
	for _, e := range breakerErrs {
		if string(e) == errType {
			return true
		}
	}
	return false
}
