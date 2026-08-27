package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
	"github.com/wang546673478/native-llm-gateway/internal/provider"
)

// P-relay-cascade 守卫组(handler 编排层)。
// 删中转站要做三件事:删站 → 清该站全部面的归属行 → 热重载。
// 按第一要素,清理**不**塞进 relayStationStore.Delete(那样它就干两件事),
// 而由 handler 编排两个各自内聚的 store。这组测试锁死编排顺序与完整性。

// fakeRelayStationStore 最小 dbpkg.RelayStationStore 替身。
type fakeRelayStationStore struct {
	station   *dbpkg.RelayStation
	getErr    error
	deleteErr error
	deletedID uint
	// deleteCalled 记录 Delete 是否发生 —— 用于断言「先取站后删站」的顺序
	deleteCalled bool
	// faceRowsAtDelete 快照删站瞬间的归属行数,证明清理发生在删站之后
	snapshotFaces func() int
	facesAtDelete int
}

func (s *fakeRelayStationStore) List(ctx context.Context) ([]dbpkg.RelayStation, error) {
	if s.station == nil {
		return nil, nil
	}
	return []dbpkg.RelayStation{*s.station}, nil
}
func (s *fakeRelayStationStore) Get(ctx context.Context, id uint) (*dbpkg.RelayStation, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.station == nil || s.station.ID != id {
		return nil, errors.New("record not found")
	}
	return s.station, nil
}
func (s *fakeRelayStationStore) Create(ctx context.Context, st *dbpkg.RelayStation) error { return nil }
func (s *fakeRelayStationStore) Update(ctx context.Context, st *dbpkg.RelayStation) error { return nil }
func (s *fakeRelayStationStore) Delete(ctx context.Context, id uint) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleteCalled = true
	s.deletedID = id
	if s.snapshotFaces != nil {
		s.facesAtDelete = s.snapshotFaces()
	}
	s.station = nil
	return nil
}

// fakeRouteOrderStore 最小 dbpkg.RouteOrderStore 替身。
// 只有 DeleteByProvider 有真实行为,其余三个方法删站路径不走。
type fakeRouteOrderStore struct {
	// rows 用 "scope/provider/name" 压平表示,便于断言剩余集合
	rows []string
	// deletedProviders 记录 DeleteByProvider 的调用序列(顺序敏感)
	deletedProviders []string
	deleteErr        error
	// perName 指定某个名字返回的删除行数,不在表里的返回 0
	perName map[string]int64
}

func (s *fakeRouteOrderStore) ListByScope(ctx context.Context, scope, provider, billingSource string) ([]dbpkg.RouteOrder, error) {
	return nil, nil
}
func (s *fakeRouteOrderStore) Replace(ctx context.Context, scope, provider, billingSource string, names []string) error {
	return nil
}
func (s *fakeRouteOrderStore) ResetScope(ctx context.Context, scope, provider string) error {
	return nil
}
func (s *fakeRouteOrderStore) DeleteByProvider(ctx context.Context, provider string) (int64, error) {
	if s.deleteErr != nil {
		return 0, s.deleteErr
	}
	s.deletedProviders = append(s.deletedProviders, provider)
	// 同库里的语义:两个 scope 都要命中(provider 序名字在 name 列,
	// key 序名字在 provider 列)。这里按压平串的两种形状过滤。
	kept := make([]string, 0, len(s.rows))
	deleted := int64(0)
	for _, r := range s.rows {
		if r == dbpkg.RouteScopeProvider+"//"+provider ||
			strings.HasPrefix(r, dbpkg.RouteScopeKey+"/"+provider+"/") {
			deleted++
			continue
		}
		kept = append(kept, r)
	}
	s.rows = kept
	if n, ok := s.perName[provider]; ok {
		return n, nil
	}
	return deleted, nil
}

// fakeProviderKeyPurger 最小 ProviderKeyPurger 替身(handler 侧窄接口)。
type fakeProviderKeyPurger struct {
	// purged 记录调用序列 —— 断言「面名 ∪ 站名」都被覆盖
	purged    []string
	deleteErr error
	perName   map[string]int64
}

func (p *fakeProviderKeyPurger) DeleteByProvider(ctx context.Context, providerName string) (int64, error) {
	if p.deleteErr != nil {
		return 0, p.deleteErr
	}
	p.purged = append(p.purged, providerName)
	return p.perName[providerName], nil
}

// newDeleteCtx 造一个带 :id 路径参数的 DELETE 上下文。
func newDeleteCtx(t *testing.T, id string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/relay-stations/"+id, nil)
	ginCtx.Params = gin.Params{{Key: "id", Value: id}}
	return ginCtx, rec
}

// TestDeleteRelayStation_CascadesSingleFace 单协议站:清站名那一个面。
func TestDeleteRelayStation_CascadesSingleFace(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 7, Name: "tokenmarket", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolAnthropic),
	}}
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "tokenmarket", Face: "tokenmarket", ModelID: "claude-opus-5"},
		{Vendor: "tokenmarket", Face: "tokenmarket", ModelID: "claude-sonnet-5"},
		{Vendor: "other", Face: "other", ModelID: "m1"},
	}}

	ginCtx, rec := newDeleteCtx(t, "7")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !stations.deleteCalled || stations.deletedID != 7 {
		t.Errorf("站未被删除(deleteCalled=%v id=%d)", stations.deleteCalled, stations.deletedID)
	}
	if len(models.deletedFaces) != 1 || models.deletedFaces[0] != "tokenmarket" {
		t.Fatalf("清理的面 = %v, want [tokenmarket] —— 漏清会留孤儿归属行",
			models.deletedFaces)
	}
	for _, r := range models.faceRows {
		if r.Face == "tokenmarket" {
			t.Errorf("tokenmarket 归属行仍残留: %+v", r)
		}
	}
	if len(models.faceRows) != 1 || models.faceRows[0].Face != "other" {
		t.Errorf("无关面被波及, 剩 %+v, want 只剩 other", models.faceRows)
	}
}

// TestDeleteRelayStation_CascadesEveryFaceOfMultiProtocol 多协议站:
// 每个协议面都要清。只清一个是最容易犯的错(rightapi 三面场景)。
func TestDeleteRelayStation_CascadesEveryFaceOfMultiProtocol(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 3, Name: "rightapi", BaseURL: "https://x.com",
		ProtocolMode:       "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	}}
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "rightapi", Face: "rightapi-openai", ModelID: "gpt-5.4"},
		{Vendor: "rightapi", Face: "rightapi-anthropic", ModelID: "claude-opus-5"},
		{Vendor: "keepme", Face: "keepme", ModelID: "m1"},
	}}

	ginCtx, rec := newDeleteCtx(t, "3")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := map[string]bool{}
	for _, f := range models.deletedFaces {
		got[f] = true
	}
	for _, want := range []string{"rightapi-openai", "rightapi-anthropic"} {
		if !got[want] {
			t.Errorf("面 %q 没被清理 —— 多协议站漏清该协议的归属行", want)
		}
	}
	if len(models.faceRows) != 1 || models.faceRows[0].Face != "keepme" {
		t.Errorf("剩余归属行 = %+v, want 只剩 keepme", models.faceRows)
	}
}

// TestDeleteRelayStation_CleanupHappensAfterDelete 顺序:先删站,再清面。
// 反了的话,清面成功但删站失败会留下「有站无归属」的站(该站全部候选消失 → 503),
// 比留孤儿行更糟。
func TestDeleteRelayStation_CleanupHappensAfterDelete(t *testing.T) {
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "tm", Face: "tm", ModelID: "m1"},
		{Vendor: "tm", Face: "tm", ModelID: "m2"},
	}}
	stations := &fakeRelayStationStore{
		station: &dbpkg.RelayStation{
			ID: 1, Name: "tm", BaseURL: "https://x.com",
			ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
		},
		snapshotFaces: func() int { return len(models.faceRows) },
	}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stations.facesAtDelete != 2 {
		t.Errorf("删站瞬间归属行 = %d, want 2 —— 清面跑在删站之前了", stations.facesAtDelete)
	}
	if len(models.faceRows) != 0 {
		t.Errorf("删站后归属行 = %d, want 0", len(models.faceRows))
	}
}

// TestDeleteRelayStation_StationDeleteFailsSkipsCleanup 删站失败时
// 绝不能清面 —— 站还在,清了面它就没候选了。
func TestDeleteRelayStation_StationDeleteFailsSkipsCleanup(t *testing.T) {
	stations := &fakeRelayStationStore{
		station: &dbpkg.RelayStation{
			ID: 1, Name: "tm", BaseURL: "https://x.com",
			ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
		},
		deleteErr: errors.New("db down"),
	}
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "tm", Face: "tm", ModelID: "m1"},
	}}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if len(models.deletedFaces) != 0 {
		t.Errorf("删站失败却清了面 %v —— 站还在但没了归属行 → 该站 503",
			models.deletedFaces)
	}
	if len(models.faceRows) != 1 {
		t.Errorf("归属行被删了, 剩 %d, want 1", len(models.faceRows))
	}
}

// TestDeleteRelayStation_ReloadRunsAfterCleanup 热重载在清理之后跑,
// 且能看到清理后的状态(否则 Registry 重建时又把孤儿面读回来)。
func TestDeleteRelayStation_ReloadRunsAfterCleanup(t *testing.T) {
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "tm", Face: "tm", ModelID: "m1"},
	}}
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}

	facesAtReload := -1
	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{
		RelayStationStore: stations,
		ModelStore:        models,
		RelayReloadFunc: func() error {
			facesAtReload = len(models.faceRows)
			return nil
		},
	}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if facesAtReload != 0 {
		t.Errorf("热重载时归属行 = %d, want 0 —— reload 跑在清理之前会读回孤儿面",
			facesAtReload)
	}
}

// TestDeleteRelayStation_CleanupErrorSurfaces 清理失败要报错,不能静默 200。
// 静默会让用户以为清干净了,孤儿行悄悄堆积(正是历史欠账的成因)。
func TestDeleteRelayStation_CleanupErrorSurfaces(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}
	models := &fakeProviderModelStore{deleteErr: errors.New("constraint violation")}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 —— 清理失败静默返回 200 会让孤儿行悄悄堆积", rec.Code)
	}
}

// TestDeleteRelayStation_ModelStoreNilStillDeletes ModelStore 缺失(降级)时
// 删站本身仍要成功 —— 级联是增强,不是删站的前置条件。
func TestDeleteRelayStation_ModelStoreNilStillDeletes(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !stations.deleteCalled {
		t.Error("ModelStore=nil 时站没被删 —— 级联不该阻塞删站")
	}
}

// TestDeleteRelayStation_MissingStationStillDeletes 取站失败(并发删 / 不存在)时
// 仍走删除保持幂等,只是没有面可清。
func TestDeleteRelayStation_MissingStationStillDeletes(t *testing.T) {
	stations := &fakeRelayStationStore{getErr: errors.New("record not found")}
	models := &fakeProviderModelStore{}

	ginCtx, rec := newDeleteCtx(t, "9")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(models.deletedFaces) != 0 {
		t.Errorf("取不到站却清了面 %v —— 面名无从得知,清了就是瞎删",
			models.deletedFaces)
	}
}

// TestDeleteRelayStation_StoreNilReturns503 前置守卫不回退。
func TestDeleteRelayStation_StoreNilReturns503(t *testing.T) {
	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{}).deleteRelayStation(ginCtx)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestDeleteRelayStation_InvalidIDReturns400 非法 id 不该走到任何 store。
func TestDeleteRelayStation_InvalidIDReturns400(t *testing.T) {
	stations := &fakeRelayStationStore{}
	models := &fakeProviderModelStore{}
	ginCtx, rec := newDeleteCtx(t, "not-a-number")
	(&Admin{RelayStationStore: stations, ModelStore: models}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if stations.deleteCalled || len(models.deletedFaces) != 0 {
		t.Error("非法 id 却动了 store")
	}
}

// ── route_order 级联(P-relay-cascade 第二级) ─────────────────────────
// 孤儿排序改写的危害比孤儿归属行更大:scope=provider 的孤儿仍占着层内
// seq 名次,把活着的候选整体往后挤(实测两个已删厂商占了 api 层 seq 0/1)。

// TestDeleteRelayStation_CascadesRouteOrderPerFace 多协议站的每个面都要清排序改写,
// 且两个 scope 都清(provider 名次 + 该 provider 下的 key 顺序)。
func TestDeleteRelayStation_CascadesRouteOrderPerFace(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 3, Name: "rightapi", BaseURL: "https://x.com",
		ProtocolMode:       "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	}}
	orders := &fakeRouteOrderStore{rows: []string{
		dbpkg.RouteScopeProvider + "//rightapi-openai",
		dbpkg.RouteScopeProvider + "//rightapi-anthropic",
		dbpkg.RouteScopeKey + "/rightapi-openai/k1",
		dbpkg.RouteScopeKey + "/rightapi-openai/k2",
		dbpkg.RouteScopeProvider + "//deepseek",
		dbpkg.RouteScopeKey + "/deepseek/dk1",
	}}

	ginCtx, rec := newDeleteCtx(t, "3")
	(&Admin{RelayStationStore: stations, RouteOrderStore: orders}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := []string{"rightapi-openai", "rightapi-anthropic"}
	if len(orders.deletedProviders) != len(want) {
		t.Fatalf("清理的面 = %v, want %v —— 漏一个面就留下占位的 seq 名次",
			orders.deletedProviders, want)
	}
	for i, w := range want {
		if orders.deletedProviders[i] != w {
			t.Errorf("第 %d 个清理的面 = %q, want %q", i, orders.deletedProviders[i], w)
		}
	}
	// 剩下的必须正好是无关 provider 的两行
	if len(orders.rows) != 2 {
		t.Fatalf("剩余改写 = %v, want 只剩 deepseek 的两行", orders.rows)
	}
	for _, r := range orders.rows {
		if strings.Contains(r, "rightapi") {
			t.Errorf("rightapi 改写仍残留: %s", r)
		}
	}
}

// TestDeleteRelayStation_RouteOrderStoreNilStillDeletes RouteOrderStore 缺失(降级)
// 时删站仍要成功 —— 级联是增强,不是删站的前置条件。
func TestDeleteRelayStation_RouteOrderStoreNilStillDeletes(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ModelStore: &fakeProviderModelStore{}}).
		deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !stations.deleteCalled {
		t.Error("RouteOrderStore=nil 时站没被删 —— 级联不该阻塞删站")
	}
}

// TestDeleteRelayStation_RouteOrderCleanupErrorSurfaces 排序清理失败要 500,
// 且错误里带上是哪个面失败的(否则多协议站无从定位)。
func TestDeleteRelayStation_RouteOrderCleanupErrorSurfaces(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}
	orders := &fakeRouteOrderStore{deleteErr: errors.New("deadlock detected")}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, RouteOrderStore: orders}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 —— 静默 200 会让占位的 seq 名次悄悄留着", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "route order cleanup failed for tm") {
		t.Errorf("错误信息 = %s, want 含失败的面名", rec.Body.String())
	}
}

// ── provider_api_keys 级联(P-relay-cascade 第三级) ────────────────────
// 清理集是「面名 ∪ 站名」:syncRelayStationKeys 按**站名**写 provider_api_keys,
// 手工在「上游 Key」页加的是按**面名**存的,两条来路都要覆盖。

// TestDeleteRelayStation_PurgesKeysForStationNameToo 多协议站的关键用例:
// 站名不在 FaceNames 里,只清面名会漏掉同步写入的那批 key
// —— 留下幽灵条目 + 上游 key 明文无限期留库。
func TestDeleteRelayStation_PurgesKeysForStationNameToo(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 3, Name: "rightapi", BaseURL: "https://x.com",
		ProtocolMode:       "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	}}
	keys := &fakeProviderKeyPurger{perName: map[string]int64{
		"rightapi-openai":    2, // 手工按面名加的
		"rightapi-anthropic": 1,
		"rightapi":           3, // syncRelayStationKeys 按站名写的
	}}

	ginCtx, rec := newDeleteCtx(t, "3")
	(&Admin{RelayStationStore: stations, ProviderKeyPurge: keys}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := strings.Join(keys.purged, ",")
	if !strings.Contains(got, "rightapi-openai") || !strings.Contains(got, "rightapi-anthropic") {
		t.Errorf("清理集 = %v, 缺协议面 —— 手工按面名加的 key 会残留", keys.purged)
	}
	// 站名单独成项,不能被面名前缀「顺带」覆盖
	found := false
	for _, n := range keys.purged {
		if n == "rightapi" {
			found = true
		}
	}
	if !found {
		t.Errorf("清理集 = %v, 缺站名 rightapi —— syncRelayStationKeys 按站名写的那批 key 会残留(含明文)",
			keys.purged)
	}
	if len(keys.purged) != 3 {
		t.Errorf("清理了 %d 个名字, want 3(两个面 + 站名)", len(keys.purged))
	}
}

// TestDeleteRelayStation_PurgesKeysSingleProtocolNoDuplicate 单协议站的面名 == 站名,
// 清理集必须去重成一项。重复调一次只是多一次 0 行 DELETE,不算错;
// 但去重是 appendUnique 的契约,一起锁死防它退化成裸 append。
func TestDeleteRelayStation_PurgesKeysSingleProtocolNoDuplicate(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tokenmarket", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}
	keys := &fakeProviderKeyPurger{perName: map[string]int64{"tokenmarket": 5}}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ProviderKeyPurge: keys}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(keys.purged) != 1 || keys.purged[0] != "tokenmarket" {
		t.Errorf("清理集 = %v, want [tokenmarket] —— 面名与站名相同应去重", keys.purged)
	}
}

// TestDeleteRelayStation_KeyPurgeNilStillDeletes ProviderKeyPurge 缺失(降级)时
// 删站仍成功。
func TestDeleteRelayStation_KeyPurgeNilStillDeletes(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, RouteOrderStore: &fakeRouteOrderStore{}}).
		deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !stations.deleteCalled {
		t.Error("ProviderKeyPurge=nil 时站没被删 —— 级联不该阻塞删站")
	}
}

// TestDeleteRelayStation_KeyPurgeErrorSurfaces key 清理失败要 500 并带上名字。
// 这一级失败尤其不能静默:残留行里带着上游 key 明文。
func TestDeleteRelayStation_KeyPurgeErrorSurfaces(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}
	keys := &fakeProviderKeyPurger{deleteErr: errors.New("connection reset")}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{RelayStationStore: stations, ProviderKeyPurge: keys}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 —— 静默 200 会让上游 key 明文悄悄留库", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "provider key cleanup failed for tm") {
		t.Errorf("错误信息 = %s, want 含失败的名字", rec.Body.String())
	}
}

// ── 三级级联的编排(顺序 + 响应契约) ─────────────────────────────────

// TestDeleteRelayStation_AllThreeCascadesAfterDelete 三级清理都必须发生在删站**之后**。
// 清在前面而删站失败,就把活站的归属/名次/key 清光了(比留孤儿严重得多)。
func TestDeleteRelayStation_AllThreeCascadesAfterDelete(t *testing.T) {
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "tm", Face: "tm", ModelID: "m1"},
	}}
	orders := &fakeRouteOrderStore{rows: []string{dbpkg.RouteScopeProvider + "//tm"}}
	keys := &fakeProviderKeyPurger{perName: map[string]int64{"tm": 1}}

	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 1, Name: "tm", BaseURL: "https://x.com",
		ProtocolMode: "single", PrimaryProtocol: string(provider.ProtocolOpenAI),
	}}
	// 删站瞬间三张表都还没被动过
	ordersAtDelete, keysAtDelete := -1, -1
	stations.snapshotFaces = func() int {
		ordersAtDelete = len(orders.deletedProviders)
		keysAtDelete = len(keys.purged)
		return len(models.faceRows)
	}

	ginCtx, rec := newDeleteCtx(t, "1")
	(&Admin{
		RelayStationStore: stations,
		ModelStore:        models,
		RouteOrderStore:   orders,
		ProviderKeyPurge:  keys,
	}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if stations.facesAtDelete != 1 {
		t.Errorf("删站瞬间归属行 = %d, want 1 —— 清理跑在删站之前", stations.facesAtDelete)
	}
	if ordersAtDelete != 0 {
		t.Errorf("删站瞬间已清 %d 个面的排序 —— 清理跑在删站之前", ordersAtDelete)
	}
	if keysAtDelete != 0 {
		t.Errorf("删站瞬间已清 %d 个名字的 key —— 清理跑在删站之前", keysAtDelete)
	}
	// 三级都实际跑到了
	if len(models.deletedFaces) != 1 || len(orders.deletedProviders) != 1 || len(keys.purged) != 1 {
		t.Errorf("三级级联未全部执行: faces=%v orders=%v keys=%v",
			models.deletedFaces, orders.deletedProviders, keys.purged)
	}
}

// TestDeleteRelayStation_ResponseReportsAllCounts 响应要回报三级各清了多少行。
// 这是用户唯一能看到「到底清没清」的地方 —— 少一个字段就等于没法自查。
func TestDeleteRelayStation_ResponseReportsAllCounts(t *testing.T) {
	stations := &fakeRelayStationStore{station: &dbpkg.RelayStation{
		ID: 3, Name: "rightapi", BaseURL: "https://x.com",
		ProtocolMode:       "multi",
		PrimaryProtocol:    string(provider.ProtocolOpenAI),
		SupportedProtocols: `["openai","anthropic"]`,
	}}
	models := &fakeProviderModelStore{faceRows: []dbpkg.ProviderModelFace{
		{Vendor: "rightapi", Face: "rightapi-openai", ModelID: "gpt-5.4"},
		{Vendor: "rightapi", Face: "rightapi-anthropic", ModelID: "claude-opus-5"},
	}}
	orders := &fakeRouteOrderStore{perName: map[string]int64{
		"rightapi-openai":    3,
		"rightapi-anthropic": 4,
	}}
	keys := &fakeProviderKeyPurger{perName: map[string]int64{
		"rightapi-openai": 1,
		"rightapi":        6,
	}}

	ginCtx, rec := newDeleteCtx(t, "3")
	(&Admin{
		RelayStationStore: stations,
		ModelStore:        models,
		RouteOrderStore:   orders,
		ProviderKeyPurge:  keys,
	}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Message          string   `json:"message"`
		DeletedFaceRows  int64    `json:"deleted_face_rows"`
		DeletedOrderRows int64    `json:"deleted_order_rows"`
		DeletedKeyRows   int64    `json:"deleted_key_rows"`
		CleanedFaces     []string `json:"cleaned_faces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期 JSON: %v; body=%s", err, rec.Body.String())
	}
	if body.DeletedFaceRows != 2 {
		t.Errorf("deleted_face_rows = %d, want 2", body.DeletedFaceRows)
	}
	if body.DeletedOrderRows != 7 {
		t.Errorf("deleted_order_rows = %d, want 7(3+4 逐面累加)", body.DeletedOrderRows)
	}
	if body.DeletedKeyRows != 7 {
		t.Errorf("deleted_key_rows = %d, want 7(面 1 + 站名 6 累加)", body.DeletedKeyRows)
	}
	if len(body.CleanedFaces) != 2 {
		t.Errorf("cleaned_faces = %v, want 两个协议面", body.CleanedFaces)
	}
}

// TestDeleteRelayStation_MissingStationSkipsAllCascades 取不到站时面名/站名都无从得知,
// 三级清理必须**一个都不跑** —— 空名字落到 DELETE 上会清空整张表。
func TestDeleteRelayStation_MissingStationSkipsAllCascades(t *testing.T) {
	stations := &fakeRelayStationStore{getErr: errors.New("record not found")}
	orders := &fakeRouteOrderStore{}
	keys := &fakeProviderKeyPurger{}

	ginCtx, rec := newDeleteCtx(t, "9")
	(&Admin{
		RelayStationStore: stations,
		ModelStore:        &fakeProviderModelStore{},
		RouteOrderStore:   orders,
		ProviderKeyPurge:  keys,
	}).deleteRelayStation(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(orders.deletedProviders) != 0 {
		t.Errorf("取不到站却清了排序 %v —— 名字无从得知,清了就是瞎删",
			orders.deletedProviders)
	}
	if len(keys.purged) != 0 {
		t.Errorf("取不到站却清了 key %v —— 空串落到 DELETE 会清空整张表",
			keys.purged)
	}
}
