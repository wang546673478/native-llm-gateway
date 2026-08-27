package relay

import (
	"context"
	"sort"
	"testing"

	"github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// P-relay-cascade 守卫组:FaceNames 是「删站时该清哪些面」的唯一依据。
// 它必须和 registerAndLoadRelayStation 实际注册的面名**逐字一致** ——
// 少一个 → 留孤儿归属行(历史欠账 81 行的成因);
// 多一个/拼错 → 删到别站的归属行(如清 tokenmarket 波及 tokenmarket-cc)。

// stationShapes 覆盖全部 protocol_mode / supported_protocols 组合。
func stationShapes() []struct {
	desc string
	s    database.RelayStation
} {
	return []struct {
		desc string
		s    database.RelayStation
	}{
		{"single 模式", database.RelayStation{
			Name: "tm", ProtocolMode: "single",
			PrimaryProtocol: string(provider.ProtocolOpenAI),
		}},
		{"single 模式但填了 supported(应忽略 supported)", database.RelayStation{
			Name: "tm", ProtocolMode: "single",
			PrimaryProtocol:    string(provider.ProtocolAnthropic),
			SupportedProtocols: `["openai","anthropic"]`,
		}},
		{"multi 双协议", database.RelayStation{
			Name: "tm", ProtocolMode: "multi",
			PrimaryProtocol:    string(provider.ProtocolOpenAI),
			SupportedProtocols: `["openai","anthropic"]`,
		}},
		{"multi 三协议", database.RelayStation{
			Name: "tm", ProtocolMode: "multi",
			PrimaryProtocol:    string(provider.ProtocolOpenAI),
			SupportedProtocols: `["openai","anthropic","google"]`,
		}},
		{"multi 但 supported 为空(退回 primary 单面)", database.RelayStation{
			Name: "tm", ProtocolMode: "multi",
			PrimaryProtocol: string(provider.ProtocolAnthropic),
		}},
		{"protocol_mode 为空(非 multi → 按站名单面)", database.RelayStation{
			Name: "tm", PrimaryProtocol: string(provider.ProtocolOpenAI),
		}},
	}
}

// TestFaceNames_IsSupersetOfActualRegistration 核心不变式(超集,非等集):
//
//	FaceNames(s) ⊇ registerAndLoadRelayStation(s) 实际注册的面名
//
// 期望值**从注册器现场推导**,不手抄第二份清单 —— 手抄会和实现漂移
// (同 CLAUDE.md 里热路径预筛必须是解析器命中集超集的写法)。
//
// 为什么是超集而不是等集:注册可能整站失败(如 multi 含 google —— 中转站
// 尚不支持该协议,NewGenericRelayProvider 直接报错,零面落地),此时
// FaceNames 仍会报出 3 个名。方向性是刻意的:
//   - 少报一个面 → 删站后留孤儿归属行(历史欠账 81 行的成因),静默且难查
//   - 多报一个面 → DeleteFaceModels 按 face = ? 精确删,没有该面就是 0 行 no-op
//
// 所以只有「漏」是 bug,「多」是安全侧。
func TestFaceNames_IsSupersetOfActualRegistration(t *testing.T) {
	for _, tc := range stationShapes() {
		t.Run(tc.desc, func(t *testing.T) {
			// BaseURL 是注册的必填项,和面名推导无关 —— 就地补齐,
			// 让 stationShapes 只聚焦 protocol_mode / supported_protocols。
			s := tc.s
			s.BaseURL = "https://example.com"

			mgr := &fakeMgr{}
			// 注册失败(如 google 不支持)也要继续断言:
			// 那种站零面落地,超集自然成立,但不能因此跳过检查。
			regErr := registerAndLoadRelayStation(context.Background(), s, mgr)

			derived := make(map[string]bool, len(FaceNames(s)))
			for _, f := range FaceNames(s) {
				derived[f] = true
			}

			var missing []string
			for _, reg := range mgr.added {
				if !derived[reg] {
					missing = append(missing, reg)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("实际注册了 %v 但 FaceNames 漏报 %v(regErr=%v)—— "+
					"删站时这些面的归属行会成孤儿", mgr.added, missing, regErr)
			}
		})
	}
}

// TestFaceNames_MultiWithGoogleStillReportsAllNames 锁死上面的方向性:
// multi 含 google 时整站注册失败(零面),但 FaceNames 仍报全部名字。
// 这是有意的 —— 万一将来 google 落地、或历史上曾写入过归属行,删站要能清掉。
// 若哪天改成「只报能注册成功的面」,这条会红。
func TestFaceNames_MultiWithGoogleStillReportsAllNames(t *testing.T) {
	s := database.RelayStation{
		Name: "tm", BaseURL: "https://example.com", ProtocolMode: "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic","google"]`,
	}
	if err := registerAndLoadRelayStation(context.Background(), s, &fakeMgr{}); err == nil {
		t.Skip("google 已被中转站支持,该用例的前提(整站注册失败)不再成立")
	}
	got := FaceNames(s)
	if len(got) != 3 {
		t.Errorf("FaceNames = %v(%d 个), want 3 个 —— "+
			"注册失败不该让 FaceNames 少报,否则残留归属行永远清不掉", got, len(got))
	}
}

// TestFaceNames_SingleReturnsStationName single 模式面名就是站名本身,
// 不带协议后缀(路由候选键 / route_order / 归属行都用它)。
func TestFaceNames_SingleReturnsStationName(t *testing.T) {
	got := FaceNames(database.RelayStation{
		Name: "tokenmarket", ProtocolMode: "single",
		PrimaryProtocol: string(provider.ProtocolAnthropic),
	})
	if len(got) != 1 || got[0] != "tokenmarket" {
		t.Errorf("FaceNames = %v, want [tokenmarket]", got)
	}
}

// TestFaceNames_MultiUsesNameProtocolSuffix multi 模式按 name-协议 拆面。
func TestFaceNames_MultiUsesNameProtocolSuffix(t *testing.T) {
	got := FaceNames(database.RelayStation{
		Name: "rightapi", ProtocolMode: "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	})
	want := map[string]bool{"rightapi-openai": false, "rightapi-anthropic": false}
	for _, f := range got {
		if _, ok := want[f]; !ok {
			t.Errorf("多出预期外的面 %q", f)
			continue
		}
		want[f] = true
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("缺面 %q(该面归属行会成孤儿)", f)
		}
	}
}

// TestFaceNames_MalformedJSONFallsBackToStationName 协议列是坏 JSON 时
// 退回站名单面 —— 宁可少删也不错删(错删会波及别站归属行)。
func TestFaceNames_MalformedJSONFallsBackToStationName(t *testing.T) {
	got := FaceNames(database.RelayStation{
		Name: "broken", ProtocolMode: "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai",`, // 截断的 JSON
	})
	if len(got) != 1 || got[0] != "broken" {
		t.Errorf("FaceNames = %v, want [broken](坏 JSON 应退回站名)", got)
	}
}

// TestFaceNames_NeverReturnsEmpty 任何输入都不能返回空集 ——
// 空集意味着删站时一个面都不清,静默留下全部孤儿行。
func TestFaceNames_NeverReturnsEmpty(t *testing.T) {
	cases := []database.RelayStation{
		{Name: "a"},
		{Name: "a", ProtocolMode: "multi"},
		{Name: "a", ProtocolMode: "multi", SupportedProtocols: `[]`},
		{Name: "a", ProtocolMode: "multi", SupportedProtocols: `garbage`},
		{Name: "a", ProtocolMode: "single"},
		{Name: "a", ProtocolMode: "weird-future-mode"},
	}
	for _, s := range cases {
		if got := FaceNames(s); len(got) == 0 {
			t.Errorf("FaceNames(%+v) 返回空集 —— 删站会漏清全部归属行", s)
		}
	}
}

// TestFaceNames_DoesNotMatchSiblingStations 守卫「前缀匹配」的诱惑:
// 清 tokenmarket 不能牵连 tokenmarket-cc(独立的另一个站)。
// FaceNames 给的是精确面名,配合 DeleteFaceModels 的 face = ? 精确删。
func TestFaceNames_DoesNotMatchSiblingStations(t *testing.T) {
	got := FaceNames(database.RelayStation{
		Name: "tokenmarket", ProtocolMode: "single",
		PrimaryProtocol: string(provider.ProtocolAnthropic),
	})
	for _, f := range got {
		if f == "tokenmarket-cc" || f == "tokenmarket-codex" {
			t.Errorf("FaceNames 含兄弟站面 %q —— 删 tokenmarket 会误删它的归属行", f)
		}
	}
}
