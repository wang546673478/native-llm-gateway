package relay

import (
	"context"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// fakeMgr 最小 ProviderManager 实现,只记录 SetResponsesAPISupport 的调用。
// 用 slice 而非 map 记录:要能区分「没调用」和「调用了但传 false」——
// 这两者正是本组守卫测试要盯的 bug(漏设 vs 设错)。
type fakeMgr struct {
	added []string
	calls []responsesCall
}

type responsesCall struct {
	name      string
	supported bool
}

func (m *fakeMgr) AddProvider(ctx context.Context, name string, p provider.Provider) error {
	m.added = append(m.added, name)
	return nil
}
func (m *fakeMgr) RemoveProvider(name string)           {}
func (m *fakeMgr) GetAll() map[string]provider.Provider { return nil }
func (m *fakeMgr) SetResponsesAPISupport(name string, supported bool) {
	m.calls = append(m.calls, responsesCall{name: name, supported: supported})
}

// callFor 返回某个注册名的 SetResponsesAPISupport 调用(ok=false 表示压根没调用)
func (m *fakeMgr) callFor(name string) (responsesCall, bool) {
	for _, c := range m.calls {
		if c.name == name {
			return c, true
		}
	}
	return responsesCall{}, false
}

// TestSingleProtocol_SetsResponsesFlagUnconditionally P-responses 守卫:
// 单协议站必须无条件 set 标志,不能只在 true 时 set。
// 只在 true 时 set 会让热重载时「DB 改回 false」清不掉内存里的旧 true ——
// 关掉某站的 /responses 得重启进程才生效(2026-08-25 实测)。
func TestSingleProtocol_SetsResponsesFlagUnconditionally(t *testing.T) {
	for _, want := range []bool{true, false} {
		mgr := &fakeMgr{}
		s := database.RelayStation{
			Name:                 "tm-single",
			BaseURL:              "https://example.com",
			ProtocolMode:         "single",
			PrimaryProtocol:      string(provider.ProtocolOpenAI),
			SupportsResponsesAPI: want,
		}
		if err := registerAndLoadRelayStation(context.Background(), s, mgr); err != nil {
			t.Fatalf("register: %v", err)
		}
		got, ok := mgr.callFor("tm-single")
		if !ok {
			t.Fatalf("supports_responses_api=%v 时没调用 SetResponsesAPISupport"+
				"(热重载改回 false 将清不掉内存旧值)", want)
		}
		if got.supported != want {
			t.Errorf("SetResponsesAPISupport(%v), want %v", got.supported, want)
		}
	}
}

// TestMultiProtocol_SetsResponsesFlagPerFace P-responses 守卫:
// 多协议站拆出的每个面都是独立注册名,router 按注册名查标志。
// 这个分支原本压根没调用 SetResponsesAPISupport —— 多协议站配了
// supports_responses_api=true 也永远拿不到 /responses(候选被筛空 → 503)。
func TestMultiProtocol_SetsResponsesFlagPerFace(t *testing.T) {
	mgr := &fakeMgr{}
	s := database.RelayStation{
		Name:                 "tm-multi",
		BaseURL:              "https://example.com",
		ProtocolMode:         "multi",
		PrimaryProtocol:      string(provider.ProtocolOpenAI),
		SupportedProtocols:   `["openai","anthropic"]`,
		SupportsResponsesAPI: true,
	}
	if err := registerAndLoadRelayStation(context.Background(), s, mgr); err != nil {
		t.Fatalf("register: %v", err)
	}

	// 两个面都要注册,且都要带标志
	for _, face := range []string{"tm-multi-openai", "tm-multi-anthropic"} {
		got, ok := mgr.callFor(face)
		if !ok {
			t.Errorf("面 %s 没设置 Responses 标志(多协议分支漏设 → 该面永远拿不到 /responses)", face)
			continue
		}
		if !got.supported {
			t.Errorf("面 %s 标志 = false, want true", face)
		}
	}
	if len(mgr.added) != 2 {
		t.Errorf("AddProvider 调用 %d 次(%v), want 2 个面", len(mgr.added), mgr.added)
	}
}
