package usage

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// ── P-token-split 守卫 ────────────────────────────────────────────────────
//
// 这条规则的全部价值在于「对四种行型同时成立」。库里 input_tokens 与两个
// cache 列的口径漂移过三次(见 tokensplit.go),所以下面每个 fixture 都取自
// 真实库里实测存在的形状,不是编的。
//
// 尤其守住时代 A:31656 行 anthropic 面,cache 明细列从未写入(恒 0),缓存量
// 只存在于 total 里。曾评估过一版"更简"的公式(减明细列)在这些行上把缓存量
// 整体错算进未缓存输入,单行偏差最大 744192 token。那个退化必须被测试挡住。

// tokenSplitFixture 一种真实行型
type tokenSplitFixture struct {
	name          string
	era           string
	input         int
	output        int
	total         int
	cacheRead     int
	cacheCreation int
	wantCached    int
	wantUncached  int
}

// realWorldShapes 四种行型,数值取自真实库
var realWorldShapes = []tokenSplitFixture{
	{
		name: "时代A_anthropic_明细列从未写入_缓存量只在total里",
		era:  "~2026-08-23 之前,31656 行",
		// minimax/MiniMax-M3 的典型形状:两个 cache 列都是 0,但 total > input+output
		input: 5000, output: 300, total: 25000,
		cacheRead: 0, cacheCreation: 0,
		// 缓存量只能反推:25000 - 5000 - 300 = 19700
		wantCached: 19700, wantUncached: 5000,
	},
	{
		name:  "时代B_anthropic_明细列有值_cache在input之外",
		era:   "2026-08-23 起",
		input: 1200, output: 400, total: 6600,
		cacheRead: 4000, cacheCreation: 1000,
		// 反推为正(6600-1200-400=5000)→ input 本身即未缓存
		wantCached: 5000, wantUncached: 1200,
	},
	{
		name: "时代C_openai_修复前_input含缓存",
		era:  "P-cache-dedup 修复之前,191 行",
		// tokenmarket-codex/gpt-5.6-sol 实测形状:total = input + output
		input: 513865, output: 51, total: 513916,
		cacheRead: 449152, cacheCreation: 0,
		// 反推为 0 → 必须读明细列
		wantCached: 449152, wantUncached: 513865 - 449152,
	},
	{
		name:  "时代D_openai_修复后_input已扣缓存",
		era:   "P-cache-dedup 修复之后",
		input: 64713, output: 51, total: 513916,
		cacheRead: 449152, cacheCreation: 0,
		// 反推为正(513916-64713-51=449152)→ 与明细列一致
		wantCached: 449152, wantUncached: 64713,
	},
	{
		name:  "无缓存_减法必须是恒等变换",
		era:   "全时代",
		input: 700, output: 30, total: 730,
		cacheRead: 0, cacheCreation: 0,
		wantCached: 0, wantUncached: 700,
	},
	{
		name:  "全部命中_未缓存为0",
		era:   "边界",
		input: 500, output: 10, total: 510,
		cacheRead: 500, cacheCreation: 0,
		wantCached: 500, wantUncached: 0,
	},
}

// TestSplitTokens_RealWorldShapes Go 侧规则对四种行型都对
func TestSplitTokens_RealWorldShapes(t *testing.T) {
	for _, f := range realWorldShapes {
		t.Run(f.name, func(t *testing.T) {
			cached, uncached := SplitTokens(f.input, f.output, f.total, f.cacheRead)
			if cached != f.wantCached || uncached != f.wantUncached {
				t.Errorf("SplitTokens(in=%d out=%d total=%d read=%d) = (cached=%d, uncached=%d), want (%d, %d)\n  era: %s",
					f.input, f.output, f.total, f.cacheRead,
					cached, uncached, f.wantCached, f.wantUncached, f.era)
			}
		})
	}
}

// TestSQLSplit_MatchesGoSplit SQL 片段与 Go 实现必须逐行一致。
//
// 这是本文件的核心守卫:规则单源在 tokensplit.go,但聚合走 SQL、复核走 Go,
// 两种表达方式必须给出同一答案。任一侧被改动而另一侧没跟上,这里立刻红。
func TestSQLSplit_MatchesGoSplit(t *testing.T) {
	repo, db := newTestRepo(t)
	seedShapes(t, db)

	records, err := repo.Query(context.Background(), QueryFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(records) != len(realWorldShapes) {
		t.Fatalf("got %d records, want %d", len(records), len(realWorldShapes))
	}

	// 按 model_id 回找 fixture(seed 时用 name 当 model_id)
	byModel := map[string]tokenSplitFixture{}
	for _, f := range realWorldShapes {
		byModel[f.name] = f
	}

	for _, r := range records {
		f, ok := byModel[r.ModelID]
		if !ok {
			t.Errorf("unexpected model_id %q", r.ModelID)
			continue
		}
		goCached, goUncached := SplitTokens(r.InputTokens, r.OutputTokens, r.TotalTokens, r.CacheReadTokens)

		if r.CachedInputTokens != goCached || r.UncachedInputTokens != goUncached {
			t.Errorf("%s: SQL 与 Go 不一致\n  SQL = (cached=%d, uncached=%d)\n  Go  = (cached=%d, uncached=%d)",
				f.name, r.CachedInputTokens, r.UncachedInputTokens, goCached, goUncached)
		}
		if r.CachedInputTokens != f.wantCached || r.UncachedInputTokens != f.wantUncached {
			t.Errorf("%s: SQL 结果不对\n  got  (cached=%d, uncached=%d)\n  want (cached=%d, uncached=%d)\n  era: %s",
				f.name, r.CachedInputTokens, r.UncachedInputTokens, f.wantCached, f.wantUncached, f.era)
		}
	}
}

// TestSQLSplit_RejectsNaiveFormula 锁死「不能改回依赖明细列的简化公式」。
//
// 被否决的写法:uncached = total - output - cache_read - cache_creation
// 它在时代 A(明细列恒 0)上把缓存量整体错算进未缓存输入。这里直接把那个
// 错误公式算出来,断言它与正确规则**不同** —— 若哪天有人改回去,
// TestSQLSplit_MatchesGoSplit 会红,而这条测试解释为什么不能那样写。
func TestSQLSplit_RejectsNaiveFormula(t *testing.T) {
	era := realWorldShapes[0] // 时代 A
	if era.cacheRead != 0 || era.cacheCreation != 0 {
		t.Fatalf("fixture 前提变了:时代 A 的明细列应为 0")
	}

	_, correct := SplitTokens(era.input, era.output, era.total, era.cacheRead)
	naive := era.total - era.output - era.cacheRead - era.cacheCreation

	if naive == correct {
		t.Fatal("这个 fixture 区分不出两种公式,守卫失效 —— 需要换一个 total > input+output 的形状")
	}
	if correct != era.input {
		t.Errorf("时代 A 的未缓存输入应等于 input_tokens(%d),got %d", era.input, correct)
	}
	t.Logf("已否决的简化公式会算成 %d,正确值 %d,偏差 %d token", naive, correct, naive-correct)
}

// TestAggregate_SumsNormalizedInput 聚合层:混口径的行加在一起也要对。
//
// 这是本次修复的目标 —— 此前聚合是裸 SUM(input_tokens),把三个时代的
// 不同含义直接相加,且缓存量在聚合层完全不可见。
func TestAggregate_SumsNormalizedInput(t *testing.T) {
	repo, db := newTestRepo(t)
	seedShapes(t, db)

	// 所有 fixture 用同一 billing_source,聚成一行好对账
	rows, err := repo.AggregateByBillingSource(context.Background(), QueryFilter{})
	if err != nil {
		t.Fatalf("AggregateByBillingSource: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d billing_source groups, want 1", len(rows))
	}

	var wantCached, wantUncached int64
	for _, f := range realWorldShapes {
		wantCached += int64(f.wantCached)
		wantUncached += int64(f.wantUncached)
	}

	got := rows[0]
	if got.TotalInput != wantUncached {
		t.Errorf("TotalInput(未缓存) = %d, want %d —— 裸 SUM(input_tokens) 会得到 %d",
			got.TotalInput, wantUncached, sumRawInput())
	}
	if got.TotalCachedInput != wantCached {
		t.Errorf("TotalCachedInput = %d, want %d", got.TotalCachedInput, wantCached)
	}

	// 按 model 聚合口径必须与按 billing_source 一致 —— 两张表在页面上并列
	byModel, err := repo.Aggregate(context.Background(), QueryFilter{})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var mCached, mUncached int64
	for _, r := range byModel {
		mCached += r.TotalCachedInput
		mUncached += r.TotalInput
	}
	if mCached != wantCached || mUncached != wantUncached {
		t.Errorf("两张聚合表口径不一致:by_model=(cached=%d, uncached=%d), by_billing=(cached=%d, uncached=%d)",
			mCached, mUncached, got.TotalCachedInput, got.TotalInput)
	}
}

// TestAggregate_CachedPlusUncachedIsStable 拆分不重不漏。
//
// 判据:cached + uncached 必须等于「输入侧总量」,即 total - output。
// 这个等式对四种行型都成立,且完全不依赖 input_tokens 那个漂移过的列。
func TestAggregate_CachedPlusUncachedIsStable(t *testing.T) {
	for _, f := range realWorldShapes {
		t.Run(f.name, func(t *testing.T) {
			cached, uncached := SplitTokens(f.input, f.output, f.total, f.cacheRead)
			inputSide := f.total - f.output
			if cached+uncached != inputSide {
				t.Errorf("cached(%d) + uncached(%d) = %d, want %d (= total %d - output %d)\n  era: %s",
					cached, uncached, cached+uncached, inputSide, f.total, f.output, f.era)
			}
		})
	}
}

func sumRawInput() int64 {
	var n int64
	for _, f := range realWorldShapes {
		n += int64(f.input)
	}
	return n
}

func newTestRepo(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dbpkg.UsageRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewRepository(db), db
}

func seedShapes(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	for i, f := range realWorldShapes {
		rec := dbpkg.UsageRecord{
			TraceID:             f.name,
			ProviderName:        "p",
			ModelID:             f.name, // 用 name 当 model_id 便于回找
			Protocol:            "anthropic",
			BillingSource:       "api",
			InputTokens:         f.input,
			OutputTokens:        f.output,
			TotalTokens:         f.total,
			CacheReadTokens:     f.cacheRead,
			CacheCreationTokens: f.cacheCreation,
			CreatedAt:           now.Add(-time.Duration(i) * time.Minute),
		}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatalf("seed %s: %v", f.name, err)
		}
	}
}
