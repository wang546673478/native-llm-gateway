package relay

import (
	"context"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// fakeMgr 最小 ProviderManager 实现,只记录 AddProvider 的注册名。
type fakeMgr struct {
	added []string
}

func (m *fakeMgr) AddProvider(ctx context.Context, name string, p provider.Provider) error {
	m.added = append(m.added, name)
	return nil
}
func (m *fakeMgr) RemoveProvider(name string)           {}
func (m *fakeMgr) GetAll() map[string]provider.Provider { return nil }

// TestSingleProtocol_RegistersOneFaceUnderStationName 守卫:
// 单协议站只注册一个面,注册名就是站名本身(不带协议后缀)。
// 面名是路由候选的最小单元 + route_order / provider_model_faces 的键,
// 改了会让排序覆盖和模型归属行全部对不上。
func TestSingleProtocol_RegistersOneFaceUnderStationName(t *testing.T) {
	mgr := &fakeMgr{}
	s := database.RelayStation{
		Name:            "tm-single",
		BaseURL:         "https://example.com",
		ProtocolMode:    "single",
		PrimaryProtocol: string(provider.ProtocolOpenAI),
	}
	if err := registerAndLoadRelayStation(context.Background(), s, mgr); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(mgr.added) != 1 || mgr.added[0] != "tm-single" {
		t.Errorf("AddProvider 注册名 = %v, want [tm-single]", mgr.added)
	}
}

// TestMultiProtocol_RegistersOneFacePerProtocol 守卫:
// 多协议站按 "<站名>-<协议>" 拆出独立的面,每个协议一个。
// 拆面是中转站多协议的立足点 —— 同一个站的 openai / anthropic 端点模型互不相通,
// 合成一个面会让某协议拿到另一协议的模型发给自己端点(404 model not found)。
func TestMultiProtocol_RegistersOneFacePerProtocol(t *testing.T) {
	mgr := &fakeMgr{}
	s := database.RelayStation{
		Name:               "tm-multi",
		BaseURL:            "https://example.com",
		ProtocolMode:       "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	}
	if err := registerAndLoadRelayStation(context.Background(), s, mgr); err != nil {
		t.Fatalf("register: %v", err)
	}

	want := map[string]bool{"tm-multi-openai": false, "tm-multi-anthropic": false}
	for _, name := range mgr.added {
		if _, ok := want[name]; !ok {
			t.Errorf("注册了预期外的面 %q", name)
			continue
		}
		want[name] = true
	}
	for face, seen := range want {
		if !seen {
			t.Errorf("面 %s 没注册(多协议拆面漏了该协议)", face)
		}
	}
	if len(mgr.added) != 2 {
		t.Errorf("AddProvider 调用 %d 次(%v), want 2 个面", len(mgr.added), mgr.added)
	}
}
