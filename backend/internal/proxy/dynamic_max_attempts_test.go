// dynamic_max_attempts_test.go — max_attempts=0(动态候选预算)守卫测试。
//
// 背景(2026-08-28 实测事故):max_attempts 写死 3,api 层有 8 个候选时只试前 3 个
// (tokenmarket-codex / codex1 / plus3)就整单 503,路由顺序里排在后面的 pro3 /
// plus / pro2 / pro / pro+plus 一次都没被碰过 —— 用户按路由规则的预期是
// 「就算全部端点挂了,也应该停在 pro+plus」。
//
// 根因:maxRetry 被当成「防死循环的封顶」,但真正的终止边界是 RouteIterator
// 有限且单调(candidates 切片 + current++ 无回退)。封顶只会砍掉「有但没试」的候选。
//
// 这些测试**直接驱动 runCandidateLoop**。包里原有的 failover 测试都是手工
// iter.Next() + doRequest 逐个走,压根不经过候选循环 —— 封顶截断因此从没被覆盖到。
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// attemptRecorder 记录候选被实际发请求的顺序(跨 provider 共享一份)。
type attemptRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *attemptRecorder) note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, name)
}

func (r *attemptRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// distinctCount 实际试过的**候选**个数(maxRetry 计的就是这个,不是请求数)。
func (r *attemptRecorder) distinctCount() int {
	seen := map[string]bool{}
	for _, n := range r.snapshot() {
		seen[n] = true
	}
	return len(seen)
}

// recordingProvider 每次收到请求就记名字,然后按固定错误类型失败。
type recordingProvider struct {
	name string
	rec  *attemptRecorder
	// errType 失败类型;用 server_error 是为了落进 isNetworkClass —— 它会先试
	// swapToOtherKey(单 key pool 换不到)再推进下一候选,最贴近线上 503 的走法。
	errType provider.ErrorType
	// succeed 为 true 时返回 200(用来验证「走到了这一站」)
	succeed bool
}

func (p *recordingProvider) Name() string                { return p.name }
func (p *recordingProvider) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (p *recordingProvider) SendRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	p.rec.note(p.name)
	if p.succeed {
		return &provider.Response{
			StatusCode: 200,
			Body:       []byte(`{"choices":[{"message":{"content":"ok"}}]}`),
			Headers:    http.Header{},
		}, nil
	}
	return nil, &provider.ProviderError{
		ProviderName: p.name,
		ErrorType:    p.errType,
		Message:      "upstream unavailable",
		StatusCode:   503,
	}
}

func (p *recordingProvider) SendStreamRequest(ctx context.Context, req *provider.Request) (<-chan *provider.StreamChunk, *provider.Response, error) {
	return nil, nil, &provider.ProviderError{
		ProviderName: p.name,
		ErrorType:    p.errType,
		Message:      "stream not used in these tests",
	}
}

func (p *recordingProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *recordingProvider) ListModels(ctx context.Context) ([]string, error) {
	return []string{"gpt-4"}, nil
}
func (p *recordingProvider) SetPool(*keypool.Pool) {}
func (p *recordingProvider) Close() error          { return nil }

// runTierScenario 造 names 里的每个 provider 各 1 把 key(单 key → swapToOtherKey
// 换不到,直接推进下一候选,与线上 tokenmarket 每站一把 key 的拓扑一致),
// 全放同一层,然后**真的跑一遍 runCandidateLoop**。
// succeedLast=true 时最后一个 provider 返 200,用来验证「预算够时能走到最后一站」。
func runTierScenario(t *testing.T, maxRetry int, tier keypool.BillingSource, names []string, succeedLast bool) *attemptRecorder {
	t.Helper()
	rec := &attemptRecorder{}

	provs := make([]provider.Provider, 0, len(names))
	pools := make(map[string]*keypool.Pool, len(names))
	for i, n := range names {
		provs = append(provs, &recordingProvider{
			name:    n,
			rec:     rec,
			errType: provider.ErrorTypeServerError,
			succeed: succeedLast && i == len(names)-1,
		})
		pools[n] = newTestPoolWithTier(n, 1, tier)
	}

	mgr := newTestManager(t, provs...)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})
	engine := NewEngine(Config{
		Router:   rtr,
		Logger:   zap.NewNop(),
		MaxRetry: maxRetry,
	})

	gin.SetMode(gin.TestMode)
	// 用真 httptest recorder 而不是 fakeResponseWriter:handleAllFailed 会写响应,
	// 这里只关心候选走了多少个,但仍需一个能正常吞写入的 writer。
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	req := &provider.Request{
		TraceID:  "test-dynamic-budget",
		Model:    "gpt-4",
		Path:     "/v1/chat/completions",
		IsStream: false,
		Body:     []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		Headers:  http.Header{},
	}

	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, iter)

	var lastProviderName string
	var lastErr *provider.ProviderError
	engine.runCandidateLoop(c, context.Background(), req, iter, nil, &lastProviderName, &lastErr, nil)
	return rec
}

// eightAPIFaces 复刻线上 api 层的 8 个候选(顺序即 route_order 里的 seq 序)。
// 用户的原话:「就算全部端点挂了,那么也应该是停在 tokenmarket-pro+plus」。
func eightAPIFaces() []string {
	return []string{
		"tokenmarket-codex",
		"tokenmarket-codex1",
		"tokenmarket-plus3",
		"tokenmarket-pro3",
		"tokenmarket-plus",
		"tokenmarket-pro2",
		"tokenmarket-pro",
		"tokenmarket-pro+plus",
	}
}

// TestDynamicBudget_ZeroWalksEveryCandidateInTier 核心断言:maxRetry=0 时
// 8 个候选全部试到,最后一站是 pro+plus(用户明确的预期)。
func TestDynamicBudget_ZeroWalksEveryCandidateInTier(t *testing.T) {
	faces := eightAPIFaces()
	rec := runTierScenario(t, 0, keypool.BillingSourceAPI, faces, false)

	got := rec.snapshot()
	assert.Equal(t, len(faces), rec.distinctCount(),
		"maxRetry=0 应把该层 %d 个候选全试一遍,实际只试了 %d 个:%v",
		len(faces), rec.distinctCount(), got)

	for _, f := range faces {
		assert.Contains(t, got, f, "候选 %q 一次都没被尝试 —— 封顶又把层截断了", f)
	}
	require.NotEmpty(t, got)
	// 刻意**不**断言「最后一个是 pro+plus」:层内候选的先后由 route_order(DB 表)
	// 决定,这个测试装置没有 route_order,router 会按自己的默认序给候选。
	// 预算要保证的是「一个都不落」,顺序是路由层的职责,别把两件事混在一条断言里。
	assert.Contains(t, got, "tokenmarket-pro+plus",
		"排在路由顺序最末的候选必须也被试到(用户的原始预期):%v", got)
}

// TestDynamicBudget_ZeroDoesNotStopAtThree 回归锁死:复现事故现场 —— 写死 3 的
// 老行为会停在第 3 个候选 tokenmarket-plus3(access log 里 provider key 就停在这)。
func TestDynamicBudget_ZeroDoesNotStopAtThree(t *testing.T) {
	rec := runTierScenario(t, 0, keypool.BillingSourceAPI, eightAPIFaces(), false)
	got := rec.snapshot()

	assert.Greater(t, rec.distinctCount(), 3,
		"只试了 %d 个候选就收手 —— max_attempts 写死 3 的老 bug 回潮了:%v",
		rec.distinctCount(), got)
	assert.NotEqual(t, "tokenmarket-plus3", got[len(got)-1],
		"停在 tokenmarket-plus3 = 事故现场重现(api 层第 3 个候选)")
}

// TestDynamicBudget_ZeroReachesLateSuccess 预算够时能真的用上 pro+plus 这一站:
// 只有它健康,其余全挂。写死 3 的话这个请求会 503,而它本该成功。
//
// 只断言「试到了 pro+plus 且它是最后一个」——**不**断言总共走了 8 站:
// 装置没有 route_order,pro+plus 可能排在默认序的第 6 位,成功即停,后面的
// 候选本就不该再被碰(不试才是对的)。
func TestDynamicBudget_ZeroReachesLateSuccess(t *testing.T) {
	rec := runTierScenario(t, 0, keypool.BillingSourceAPI, eightAPIFaces(), true)

	got := rec.snapshot()
	require.NotEmpty(t, got)
	assert.Contains(t, got, "tokenmarket-pro+plus",
		"唯一健康的一站没被试到 —— 这个请求会白白 503:%v", got)
	assert.Equal(t, "tokenmarket-pro+plus", got[len(got)-1],
		"成功的一站应该是最后一个被尝试的(成功即停):%v", got)
	assert.Greater(t, rec.distinctCount(), 3,
		"健康站排在第 3 位之后,写死 3 就永远够不到它:%v", got)
}

// TestDynamicBudget_ExplicitCapStillTruncates 显式封顶仍然生效 —— 这是**故意保留**
// 的能力(给最坏延迟设上界),不是 bug。maxRetry=3 就该只试 3 个。
// 这条测试和上面几条一起,把「0 = 动态 / 正整数 = 封顶」两种语义都钉住。
func TestDynamicBudget_ExplicitCapStillTruncates(t *testing.T) {
	rec := runTierScenario(t, 3, keypool.BillingSourceAPI, eightAPIFaces(), false)

	assert.Equal(t, 3, rec.distinctCount(),
		"显式 maxRetry=3 应只试 3 个候选,实际 %d 个:%v",
		rec.distinctCount(), rec.snapshot())
}

// TestDynamicBudget_ExplicitCapOfOne 边界:封顶 1 只试 1 个。
// 顺带证明 0 和 1 是**不同**语义(若哪天有人把 0 又夹成 1,这条会红)。
func TestDynamicBudget_ExplicitCapOfOne(t *testing.T) {
	rec := runTierScenario(t, 1, keypool.BillingSourceAPI, eightAPIFaces(), false)
	assert.Equal(t, 1, rec.distinctCount(),
		"maxRetry=1 应只试 1 个候选:%v", rec.snapshot())
}

// TestDynamicBudget_NegativeClampsToZero 负数按 0(动态)处理,不能被夹成写死值。
func TestDynamicBudget_NegativeClampsToZero(t *testing.T) {
	faces := eightAPIFaces()
	rec := runTierScenario(t, -5, keypool.BillingSourceAPI, faces, false)
	assert.Equal(t, len(faces), rec.distinctCount(),
		"负 maxRetry 应归一到 0(动态)而不是旧默认值 3:%v", rec.snapshot())
}

// TestDynamicBudget_ConfigZeroSurvivesNewEngine 语义开关点的直接守卫:
// NewEngine 必须让 0 **原样活下来**。这里断言的是字段值本身 ——
// 一旦有人把 `< 0` 改回 `<= 0`,这条立刻红,不用等行为测试。
func TestDynamicBudget_ConfigZeroSurvivesNewEngine(t *testing.T) {
	cases := []struct {
		in   int
		want int
		desc string
	}{
		{in: 0, want: 0, desc: "0 是有效值(动态),不能被改写成 3"},
		{in: -1, want: 0, desc: "负数归一到 0"},
		{in: 3, want: 3, desc: "正整数原样保留"},
		{in: 99, want: 99, desc: "大正整数原样保留"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			e := NewEngine(Config{Logger: zap.NewNop(), MaxRetry: tc.in})
			assert.Equal(t, tc.want, e.maxRetry, tc.desc)
		})
	}
}

// TestDynamicBudget_QuotaEvidenceDowngradeStillWorks 不变式回归:maxRetry=0 之后
// 「有额度证据才降档」必须仍然成立。token_plan 层 3 站全 quota_exceeded → 降到
// api 层继续试;顺带确认动态预算没把 peekNextTier 的层切换判定搞坏。
func TestDynamicBudget_QuotaEvidenceDowngradeStillWorks(t *testing.T) {
	rec := &attemptRecorder{}
	tokenPlanNames := []string{"tp-a", "tp-b", "tp-c"}
	apiNames := []string{"api-a", "api-b"}

	provs := make([]provider.Provider, 0, len(tokenPlanNames)+len(apiNames))
	pools := map[string]*keypool.Pool{}
	for _, n := range tokenPlanNames {
		provs = append(provs, &recordingProvider{
			name: n, rec: rec, errType: provider.ErrorTypeQuotaExceeded,
		})
		pools[n] = newTestPoolWithTier(n, 1, keypool.BillingSourceTokenPlan)
	}
	for i, n := range apiNames {
		provs = append(provs, &recordingProvider{
			name: n, rec: rec, errType: provider.ErrorTypeServerError,
			succeed: i == len(apiNames)-1, // api 层最后一站好
		})
		pools[n] = newTestPoolWithTier(n, 1, keypool.BillingSourceAPI)
	}

	mgr := newTestManager(t, provs...)
	rtr := router.NewRouter(zap.NewNop(), mgr, pools, router.Config{
		CatchAll: &router.AliasConfig{},
	})
	engine := NewEngine(Config{
		Router: rtr, Logger: zap.NewNop(), MaxRetry: 0,
	})

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req := &provider.Request{
		TraceID: "test-downgrade", Model: "gpt-4", Path: "/v1/chat/completions",
		Body:    []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{},
	}
	iter, err := rtr.Route(context.Background(), req)
	require.NoError(t, err)

	var lastProviderName string
	var lastErr *provider.ProviderError
	engine.runCandidateLoop(c, context.Background(), req, iter, nil, &lastProviderName, &lastErr, nil)

	got := rec.snapshot()
	for _, n := range tokenPlanNames {
		assert.Contains(t, got, n, "token_plan 层候选 %q 应被试过:%v", n, got)
	}
	assert.Contains(t, got, "api-b",
		"token_plan 层集齐额度证据后应降档到 api 层并走到健康的一站:%v", got)
}
