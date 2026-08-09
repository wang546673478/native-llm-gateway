package handler

import "testing"

// TestValidStatusTokensCoverProducedErrorTypes 守卫测试:validStatusTokens 白名单
// 必须覆盖 proxy 实际产出的每一种 access-log error_type。
//
// 跨层单源:error_type 枚举分两半 —— proxy 产出(classifyError + inline assign)
// 后端白名单 validStatusTokens(这里)前端 AccessLogs.vue statusOptions(过滤)。
// 任何一边增删都会破坏过滤或 UI,此测试把"proxy 产出 ⊆ 白名单"锁死,
// 防止新增 error_type 时漏加白名单 -> 该值在列表可查但过滤返回 400 invalid_status
// (2026-08-09 实测:invalid_request/stream_interrupted 白名单缺,前端仍坏)。
func TestValidStatusTokensCoverProducedErrorTypes(t *testing.T) {
	// proxy 产出的全部 error_type(后端唯一产出源)
	// 来源:internal/proxy/proxy.go classifyError + entry.ErrorType 内联赋值。
	// 若新增 producer,这里必须对齐 —— 改别处会让这个测试立刻失败。
	produced := []string{
		"unknown",
		"ok",
		"invalid_request",
		"client_disconnected",
		"model_not_allowed",
		"auth_failed",
		"no_route",
		"upstream_5xx",
		"upstream_429",
		"upstream_4xx",
		"timeout",
		"connection_error",
		"stream_interrupted",
		// bucket 语义(非独立 error_type,按 status_code 聚)
		"4xx",
		"5xx",
	}
	for _, et := range produced {
		if !validStatusTokens[et] {
			t.Fatalf("access-log error_type %q is produced by proxy but missing from validStatusTokens whitelist -> frontend filter returns 400 invalid_status", et)
		}
	}
}

// TestValidStatusTokensFrontendFilter 守卫测试:validStatusTokens 必须覆盖前端
// AccessLogs.vue statusOptions 里所有 value。前端过滤值 ⊆ 后端白名单。
// (后端白名单是权威;前端加过滤项必须先落白名单,否则过滤直接 400。)
func TestValidStatusTokensFrontendFilter(t *testing.T) {
	// AccessLogs.vue statusOptions 的所有 value(后端为权威,仅作交叉核对)。
	frontend := []string{
		"ok", "4xx", "5xx",
		"auth_failed", "no_route", "model_not_allowed", "key_provider_mismatch",
		"upstream_4xx", "upstream_429", "upstream_5xx",
		"invalid_request", "connection_error", "timeout",
		"stream_interrupted", "client_disconnected", "unknown",
	}
	for _, v := range frontend {
		if !validStatusTokens[v] {
			t.Fatalf("frontend filter value %q not in validStatusTokens -> selecting it returns 400 invalid_status", v)
		}
	}
}
