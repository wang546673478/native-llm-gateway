package usage

// ── P-token-split: 「缓存输入 / 未缓存输入」拆分规则的唯一真相源 ──────────────
//
// 为什么需要「规则」而不是直接读列:库里 input_tokens 与两个 cache 列的口径
// 随时间漂移过三次,任何单一列都不能独立解释一行。实测(3.4 万行全表扫描):
//
//	时代 A(~2026-08-23 之前,31656 行,全是 anthropic 面)
//	  cache_read / cache_creation 两列**从未写入**(恒为 0),
//	  但缓存量已经算进了 total → total > input + output。
//	  此时缓存量只能靠 total - input - output 反推;读明细列会得到 0(丢数据)。
//	时代 B(2026-08-23 起,明细列开始有值)
//	  anthropic 系:cache 在 input 之**外**另计,total 含全部四项。
//	时代 C(P-cache-dedup 修复之前写入的 openai 系,191 行)
//	  input_tokens 是**含缓存**的完整输入,total = input + output,
//	  缓存量只在 cache_read 列里 → 反推得 0,必须读明细列。
//	时代 D(P-cache-dedup 修复之后的 openai 系)
//	  input_tokens 已扣缓存,与 anthropic 同构,反推即可得缓存量。
//
// 于是判据是「反推值是否为正」,而不是判 protocol / 看日期 / 猜版本:
//   - 反推为正 → input_tokens 已是未缓存输入(时代 A/B/D),缓存量 = 反推值
//   - 反推为 0 → input_tokens 含缓存(时代 C),缓存量 = cache_read 列
// 差值本身就是口径的直接证据,不需要知道这行是谁在哪个版本写的。
//
// 曾经评估并**否决**的更简写法:
//	uncached = total - output - cache_read - cache_creation
// 它依赖明细列,在时代 A 的 31656 行上把缓存量整体错算进未缓存输入
// (实测最大单行偏差 744192 token)。不要改回去。
//
// 这里是全项目唯一定义处。聚合(SUM)必须在 SQL 里算,所以规则用 SQL 表达;
// 明细行也走同一片段返回拆好的字段,前端不再自己算一遍 —— 避免 SQL 与 TS
// 两份实现各自漂移(本项目历史上多次事故的同一根因)。
//
// 方言约束:生产 PostgreSQL、测试 SQLite,故不能用 LEAST/GREATEST(SQLite 无),
// 一律用 CASE 表达。

const (
	// SQLCachedInput 该行的「缓存输入」token 数。
	SQLCachedInput = `CASE
		WHEN total_tokens - input_tokens - output_tokens > 0
			THEN total_tokens - input_tokens - output_tokens
		WHEN cache_read_tokens < input_tokens THEN cache_read_tokens
		ELSE input_tokens
	END`

	// SQLUncachedInput 该行的「未缓存输入」token 数。
	//
	// 与 SQLCachedInput 严格互补:两者相加恒等于该行「输入侧」的总量,
	// 不重不漏。这正是 ComputeCost 计费所依赖的互斥性(见 provider.Usage 契约)。
	SQLUncachedInput = `CASE
		WHEN total_tokens - input_tokens - output_tokens > 0
			THEN input_tokens
		WHEN cache_read_tokens < input_tokens THEN input_tokens - cache_read_tokens
		ELSE 0
	END`
)

// SplitTokens 按上面的规则拆一行,供测试与 Go 侧复核用。
//
// 存在的意义不是给业务代码调用(业务走 SQL 片段),而是让守卫测试能用
// 独立于 SQL 的表达方式复核同一规则 —— 两边对不上就说明有一侧漂了。
func SplitTokens(inputTokens, outputTokens, totalTokens, cacheReadTokens int) (cached, uncached int) {
	if derived := totalTokens - inputTokens - outputTokens; derived > 0 {
		return derived, inputTokens
	}
	if cacheReadTokens < inputTokens {
		return cacheReadTokens, inputTokens - cacheReadTokens
	}
	return inputTokens, 0
}
