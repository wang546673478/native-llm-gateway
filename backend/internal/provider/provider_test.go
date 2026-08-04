// Package provider — 共享类型单元测试
package provider

import "testing"

// TestParseMiniMaxBaseResp P-quota-minimax:
// base_resp 是 MiniMax 专属错误载体(HTTP 200 也可能带错误)。
// 1008(余额不足)/ 2056(超 Token Plan)→ 非零;成功(0)或缺失 → (0,"")
func TestParseMiniMaxBaseResp(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"余额不足 1008", `{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`, 1008},
		{"超 Token Plan 2056", `{"base_resp":{"status_code":2056,"status_msg":"plan limit"}}`, 2056},
		{"其他错误码", `{"base_resp":{"status_code":2001,"status_msg":"whatever"}}`, 2001},
		{"status_code=0 成功", `{"base_resp":{"status_code":0,"status_msg":"success"}}`, 0},
		{"无 base_resp 正常响应", `{"choices":[{"message":{"content":"hi"}}]}`, 0},
		{"非 JSON body", `not-json`, 0},
		{"空 body", ``, 0},
	}
	for _, c := range cases {
		code, msg := ParseMiniMaxBaseResp([]byte(c.body))
		if code != c.want {
			t.Errorf("%s: code = %d, want %d (msg=%q)", c.name, code, c.want, msg)
		}
	}
	if !IsMiniMaxQuotaCode(1008) || !IsMiniMaxQuotaCode(2056) {
		t.Error("IsMiniMaxQuotaCode should accept 1008 and 2056")
	}
	if IsMiniMaxQuotaCode(2001) || IsMiniMaxQuotaCode(0) {
		t.Error("IsMiniMaxQuotaCode should reject other codes")
	}
}
