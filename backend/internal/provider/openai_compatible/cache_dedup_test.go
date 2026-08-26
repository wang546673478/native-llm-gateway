package openai_compatible

import (
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// ── P-cache-dedup 守卫 ────────────────────────────────────────────────────
//
// 事故:ComputeCost 把 PromptTokens 按 input 价、CacheReadTokens 按 cache 价
// 各计一次,而本包两个解析器直接塞上游的 prompt_tokens / input_tokens
// ——那是**含缓存的完整输入**。缓存部分因此被收两遍钱。
// 线上 170 条记录中招,tokenmarket-codex 一站多算约 4.4 千万 cached token。
//
// 根因不是漏了分支,而是契约与实现互相矛盾:provider.Usage 的注释一直写着
// PromptTokens 是「不计 cache 的输入」,且 provider.TestComputeCost 早就
// 按互斥来算钱。是本包两个解析器违约。
//
// 下面的测试锁死换算本身,而不是锁某个厂商的具体数字 —— 数字类断言散在
// openai_compatible_test.go 各个形状用例里,这里只管不变式。

// TestUncachedInput 换算函数的边界。
//
// 重点是最后两个:上游偶发不自洽(命中量 > 完整输入)时必须 floor 到 0。
// 若返回负数,ComputeCost 会算出负费用,把同一账期别的请求的钱冲掉
// —— 那比多收钱更难查(总额看起来"差不多对")。
func TestUncachedInput(t *testing.T) {
	cases := []struct {
		name         string
		prompt, read int
		want         int
	}{
		{"无缓存:原样返回", 1000, 0, 1000},
		{"部分命中:相减", 1200, 800, 400},
		{"DeepSeek 口径与官方 miss 字段一致", 100, 80, 20},
		{"全部命中:未命中输入为 0", 500, 500, 0},
		{"上游不自洽(命中 > 输入):floor 到 0,不返回负数", 100, 150, 0},
		{"零值输入", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uncachedInput(c.prompt, c.read); got != c.want {
				t.Errorf("uncachedInput(%d, %d) = %d, want %d", c.prompt, c.read, got, c.want)
			}
		})
	}
}

// TestParsers_PromptAndCacheAreDisjoint 跨两套形状锁互斥不变式。
//
// 判据是 prompt + cache_read + completion == 上游 total_tokens。
// 这是**可从上游 body 独立验算**的等式:上游自己给了 total,
// 若解析后三项相加超出 total,超出的量恰好就是被重复计费的缓存量。
//
// 刻意用表驱动覆盖两个解析器 + 两种 cache 字段风格,因为违约点是"每个
// 解析器各写一遍取值",单测一个形状挡不住另一个回潮。
func TestParsers_PromptAndCacheAreDisjoint(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantTotal int // 上游 body 里的 total_tokens,独立于解析结果
	}{
		{
			name: "chat completions / DeepSeek 风格 hit+miss",
			body: `{"model":"deepseek-v4-pro","usage":{
				"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,
				"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`,
			wantTotal: 150,
		},
		{
			name: "chat completions / OpenAI 标准 cached_tokens",
			body: `{"model":"MiniMax-M3","usage":{
				"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,
				"prompt_tokens_details":{"cached_tokens":800}}}`,
			wantTotal: 1500,
		},
		{
			name: "chat completions / 无缓存(减法必须是恒等变换)",
			body: `{"model":"deepseek-v4-pro","usage":{
				"prompt_tokens":700,"completion_tokens":30,"total_tokens":730}}`,
			wantTotal: 730,
		},
		{
			name:      "responses 流式末帧(97% 命中,重复计费最严重的形状)",
			body:      responsesStreamCompletedBody,
			wantTotal: 341953,
		},
		{
			name: "responses 非流式",
			body: `{"model":"gpt-5.6-sol","usage":{
				"input_tokens":4390,"input_tokens_details":{"cached_tokens":3840},
				"output_tokens":6,"total_tokens":4396}}`,
			wantTotal: 4396,
		},
	}

	p := &DefaultOpenAIUsageParser{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := p.Parse([]byte(c.body))
			if u == nil {
				t.Fatal("Parse returned nil")
			}
			if u.TotalTokens != c.wantTotal {
				t.Fatalf("TotalTokens = %d, want %d — TotalTokens 应保持上游原值(不参与换算)",
					u.TotalTokens, c.wantTotal)
			}
			sum := u.PromptTokens + u.CacheReadTokens + u.CacheCreationTokens + u.CompletionTokens
			if sum != c.wantTotal {
				t.Errorf("prompt(%d)+cache_read(%d)+cache_creation(%d)+completion(%d) = %d, want %d\n"+
					"  超出 %d 个 token —— 这些就是会被 input 价和 cache 价重复收费的量",
					u.PromptTokens, u.CacheReadTokens, u.CacheCreationTokens, u.CompletionTokens,
					sum, c.wantTotal, sum-c.wantTotal)
			}
			if u.PromptTokens < 0 {
				t.Errorf("PromptTokens = %d,负数会算出负费用冲掉别的账", u.PromptTokens)
			}
		})
	}
}

// TestParsers_NoDoubleBillingEndToEnd 直接用 ComputeCost 验金额,而不只验 token。
//
// 单验 token 不够:有人可能"修"成把缓存量塞进 CacheCreationTokens
// (那一项当前不参与计费)——token 不变式仍绿,但钱又错了。
// 这里按真实定价算出预期金额,金额对不上就红。
func TestParsers_NoDoubleBillingEndToEnd(t *testing.T) {
	// 定价刻意让 input 远贵于 cache_read(真实 cache 折扣就是这个量级),
	// 这样重复计费会造成显著差额,不会被浮点误差掩盖。
	cost := provider.ModelCost{
		CostPerMillionInput:     10.0,
		CostPerMillionCacheRead: 1.0,
		CostPerMillionOutput:    30.0,
	}

	// 上游:完整输入 1200(其中 800 命中缓存),输出 300
	body := []byte(`{"model":"MiniMax-M3","usage":{
		"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,
		"prompt_tokens_details":{"cached_tokens":800}}}`)

	u := (&DefaultOpenAIUsageParser{}).Parse(body)
	if u == nil {
		t.Fatal("Parse returned nil")
	}

	// 正确账:未命中 400 按 input 价 + 命中 800 按 cache 价 + 输出 300 按 output 价
	want := 400*10.0/1e6 + 800*1.0/1e6 + 300*30.0/1e6
	// 违约账(修复前):完整输入 1200 全按 input 价,命中 800 再按 cache 价收一次
	buggy := 1200*10.0/1e6 + 800*1.0/1e6 + 300*30.0/1e6

	got := provider.ComputeCost(cost, u)
	const eps = 1e-12
	if diff := got - want; diff > eps || diff < -eps {
		t.Errorf("ComputeCost = %.12f, want %.12f", got, want)
	}
	if diff := got - buggy; diff < eps && diff > -eps {
		t.Errorf("ComputeCost = %.12f,等于重复计费的金额 —— 缓存部分仍被收了两遍", got)
	}
}
