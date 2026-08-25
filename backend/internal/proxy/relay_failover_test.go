package proxy

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/wang546673478/native-llm-gateway/internal/keypool"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
	"github.com/wang546673478/native-llm-gateway/internal/router"
)

// engineWithRelayFlags 构造一个只够 candidateUnfitNotFatal 用的 Engine:
// 把 relays 里的名字注册成中转站,others 注册成普通厂商。
// candidateUnfitNotFatal 只读 e.router.Manager().IsRelay(),不碰其他依赖。
func engineWithRelayFlags(t *testing.T, relays, others []string) *Engine {
	t.Helper()
	reg := provider.NewRegistry()
	cfg := &provider.ManagerConfig{Providers: map[string]provider.ManagerProviderConfig{}}
	mk := func(name string) func(provider.ProviderConfig) (provider.Provider, error) {
		return func(provider.ProviderConfig) (provider.Provider, error) {
			return &fakeProvider{name: name, proto: provider.ProtocolOpenAI}, nil
		}
	}
	for _, n := range relays {
		reg.RegisterWithProtocolVendorRelay(n, mk(n), provider.ProtocolOpenAI, n, true)
		cfg.Providers[n] = provider.ManagerProviderConfig{Enabled: true, Protocol: provider.ProtocolOpenAI}
	}
	for _, n := range others {
		reg.Register(n, mk(n))
		cfg.Providers[n] = provider.ManagerProviderConfig{Enabled: true, Protocol: provider.ProtocolOpenAI}
	}
	mgr := provider.NewManager(reg, zap.NewNop())
	if err := mgr.LoadFromConfig(context.Background(), cfg); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	r := router.NewRouter(zap.NewNop(), mgr, map[string]*keypool.Pool{}, router.Config{})
	return &Engine{router: r}
}

// TestCandidateUnfitNotFatal_RelayModelErrorContinues 中转站的 404/400 必须「继续下一站」,
// 不能 fatal。
//
// 为什么这个守卫必须存在:P-relay-model-first 让中转站永远透传,于是
// result.ModelID == req.Model 恒成立,原来那个「ModelID != req.Model 才继续」的
// 逃生口对中转站永远不触发 —— 一个站 404 会把整条 failover 链掐断,它后面本来
// 能用的站一个都试不到(2026-08-25 实测:codex 之后 8 个 tokenmarket 站全试不到)。
func TestCandidateUnfitNotFatal_RelayModelErrorContinues(t *testing.T) {
	e := engineWithRelayFlags(t, []string{"tm-codex"}, []string{"deepseek"})

	const clientModel = "gpt-5.6-sol"
	// 中转站:透传 → ModelID == clientModel(正是逃生口失效的形态)
	relayResult := &router.RouteResult{ProviderName: "tm-codex", ModelID: clientModel}

	for _, et := range []provider.ErrorType{
		provider.ErrorTypeModelNotFound,
		provider.ErrorTypeInvalidRequest,
	} {
		pe := &provider.ProviderError{ErrorType: et}
		if !e.candidateUnfitNotFatal(relayResult, clientModel, pe) {
			t.Errorf("中转站 %s + ModelID==clientModel → 应继续下一站,却判 fatal"+
				"(整条 failover 链被一个站掐断)", et)
		}
	}
}

// TestCandidateUnfitNotFatal_BuiltinDirectModelStaysFatal 回归守卫:内建厂商直连
// 真实模型名时,404 仍然 fatal —— 模型真不存在,换候选也没用。
// 这条是上面那条的边界:不能为了救中转站把内建厂商的 fatal 语义一起放开。
func TestCandidateUnfitNotFatal_BuiltinDirectModelStaysFatal(t *testing.T) {
	e := engineWithRelayFlags(t, []string{"tm-codex"}, []string{"deepseek"})

	const clientModel = "deepseek-v4-flash"
	res := &router.RouteResult{ProviderName: "deepseek", ModelID: clientModel}
	pe := &provider.ProviderError{ErrorType: provider.ErrorTypeModelNotFound}

	if e.candidateUnfitNotFatal(res, clientModel, pe) {
		t.Error("内建厂商直连真实模型名 + 404 → 应保持 fatal(模型真不存在),却判继续")
	}
}

// TestCandidateUnfitNotFatal_LabelModelStillContinues 回归守卫:原有的
// 「候选目标模型 ≠ 客户端模型名 → 继续」语义不变(catch_all 标签模型场景,
// 2026-08-07 mimo 404 image input 那条)。
func TestCandidateUnfitNotFatal_LabelModelStillContinues(t *testing.T) {
	e := engineWithRelayFlags(t, nil, []string{"minimax"})

	res := &router.RouteResult{ProviderName: "minimax", ModelID: "MiniMax-M3"}
	pe := &provider.ProviderError{ErrorType: provider.ErrorTypeInvalidRequest}

	if !e.candidateUnfitNotFatal(res, "claude-opus-5", pe) {
		t.Error("ModelID != clientModel(标签模型)+ 400 → 应继续下一候选,却判 fatal")
	}
}

// TestCandidateUnfitNotFatal_OtherErrorTypesNotSwallowed 守卫边界:只有模型类
// (404/400)才算「候选不适配」。别的错误类型不能被这个 helper 吞掉 —— 否则
// 会绕过 quota/网络 那些各有各的处置分支(降档、换 key、熔断)。
func TestCandidateUnfitNotFatal_OtherErrorTypesNotSwallowed(t *testing.T) {
	e := engineWithRelayFlags(t, []string{"tm-codex"}, nil)
	res := &router.RouteResult{ProviderName: "tm-codex", ModelID: "gpt-5.6-sol"}

	for _, et := range []provider.ErrorType{
		provider.ErrorTypeAuth,
		provider.ErrorTypeQuotaExceeded,
		provider.ErrorTypeRateLimit,
		provider.ErrorTypeConnection,
		provider.ErrorTypeTimeout,
		provider.ErrorTypeServerError,
	} {
		pe := &provider.ProviderError{ErrorType: et}
		if e.candidateUnfitNotFatal(res, "gpt-5.6-sol", pe) {
			t.Errorf("%s 不是模型类错误,不该被 candidateUnfitNotFatal 认领"+
				"(会绕过它自己的处置分支)", et)
		}
	}
}
