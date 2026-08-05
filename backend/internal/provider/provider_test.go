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

// TestComputeCost P-quota-512k: 单次请求费用计算
// 覆盖:基础四线(cache 定价)、cache_creation 缺省 fallback 到 input、
// 无定价 → 0、512k 长上下文悬崖(输入含缓存超阈值 → 全项乘 multiplier)
func TestComputeCost(t *testing.T) {
	// M3 官方价(元/1k tokens)+ 512k 悬崖 2x
	m3 := ModelCost{
		CostPer1kInput:            0.0021,
		CostPer1kOutput:           0.0084,
		CostPer1kCacheRead:        0.00042,
		LongContextInputThreshold: 512000,
		LongContextMultiplier:     2,
	}

	t.Run("基础四线 cache 定价", func(t *testing.T) {
		u := &Usage{PromptTokens: 1000, CacheCreationTokens: 500, CacheReadTokens: 1000, CompletionTokens: 1000}
		got := ComputeCost(m3, u)
		// 1*0.0021 + 0.5*0.0021(fallback input) + 1*0.00042 + 1*0.0084
		want := 0.0021 + 0.5*0.0021 + 0.00042 + 0.0084
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})

	t.Run("cache_creation 显式价", func(t *testing.T) {
		c := m3
		c.CostPer1kCacheCreation = 0.002625
		u := &Usage{PromptTokens: 1000, CacheCreationTokens: 1000}
		got := ComputeCost(c, u)
		want := 0.0021 + 0.002625
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})

	t.Run("无定价 → 0", func(t *testing.T) {
		if got := ComputeCost(ModelCost{}, &Usage{PromptTokens: 1000}); got != 0 {
			t.Errorf("cost = %v, want 0", got)
		}
	})

	t.Run("输入含缓存超 512k → 全项乘 2", func(t *testing.T) {
		u := &Usage{PromptTokens: 100, CacheReadTokens: 600000, CompletionTokens: 50}
		got := ComputeCost(m3, u)
		base := 0.1*0.0021 + 600*0.00042 + 0.05*0.0084
		want := base * 2
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v (base %v)", got, want, base)
		}
	})

	t.Run("未超阈值不变", func(t *testing.T) {
		u := &Usage{PromptTokens: 100, CacheReadTokens: 400000, CompletionTokens: 50}
		got := ComputeCost(m3, u)
		want := 0.1*0.0021 + 400*0.00042 + 0.05*0.0084
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})

	t.Run("阈值未配置 → 悬崖不生效", func(t *testing.T) {
		c := m3
		c.LongContextInputThreshold = 0
		u := &Usage{PromptTokens: 100, CacheReadTokens: 900000, CompletionTokens: 50}
		got := ComputeCost(c, u)
		want := 0.1*0.0021 + 900*0.00042 + 0.05*0.0084
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v", got, want)
		}
	})
}
