package database

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// P-relay-cascade 守卫组:RouteOrderStore.DeleteByProvider。
//
// route_order.provider / .name 都是普通字符串列(非外键),厂商/中转站被硬删后
// 排序改写行会留下。这里的孤儿比归属行更凶:scope=provider 的孤儿**仍占着层内
// seq 名次**,把活着的候选整体往后挤 —— 2026-08-28 实测已删的 claude-aws /
// codex 占了 api 层 seq 0 和 1 两个最高优先级位。
//
// 删的粒度必须精确到「两个 scope 各自的名字列」:
//
//	scope=provider → 名字在 name 列   (provider 列为空)
//	scope=key      → 名字在 provider 列(name 列是 key 名)
//
// 少查一处就留孤儿;查宽了(如前缀匹配 / 空串)会删掉活数据。
func newRouteOrderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&RouteOrder{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func seedRouteOrders(t *testing.T, db *gorm.DB, rows []RouteOrder) {
	t.Helper()
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed route_order: %v", err)
		}
	}
}

// remainingKeys 把剩余行压成 "scope/provider/name" 便于比对
func remainingKeys(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var rows []RouteOrder
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("list route_order: %v", err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Scope+"/"+r.Provider+"/"+r.Name] = true
	}
	return out
}

// TestDeleteByProvider_RemovesBothScopes 核心:一次调用要同时清掉
// scope=provider(层内名次)和 scope=key(该 provider 内的 key 序)两处。
// 这是 2026-08-28 现场的真实形状 —— codex 在两个 scope 都有行。
func TestDeleteByProvider_RemovesBothScopes(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		// scope=provider:名字在 name 列,provider 列空
		{Scope: RouteScopeProvider, Provider: "", Name: "codex", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeProvider, Provider: "", Name: "tokenmarket-pro", BillingSource: "api", Seq: 1},
		// scope=key:名字在 provider 列
		{Scope: RouteScopeKey, Provider: "codex", Name: "292f20db", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeKey, Provider: "tokenmarket-pro", Name: "key-1", BillingSource: "api", Seq: 0},
	})

	n, err := store.DeleteByProvider(ctx, "codex")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 2 {
		t.Errorf("删除行数 = %d, want 2(scope=provider 1 行 + scope=key 1 行)", n)
	}

	left := remainingKeys(t, db)
	if left["provider//codex"] {
		t.Error("scope=provider 的 codex 行还在 —— 它仍占着层内 seq 名次,会把活候选往后挤")
	}
	if left["key/codex/292f20db"] {
		t.Error("scope=key 的 codex 行还在 —— 少查一个 scope 就留孤儿")
	}
	if !left["provider//tokenmarket-pro"] || !left["key/tokenmarket-pro/key-1"] {
		t.Errorf("动了别人的行,剩余 = %v", left)
	}
}

// TestDeleteByProvider_ProviderScopeOnly 只在 scope=provider 有行的形状
// (实测 claude-aws / codex 的 provider 行就是这样单独存在的)。
func TestDeleteByProvider_ProviderScopeOnly(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		{Scope: RouteScopeProvider, Provider: "", Name: "claude-aws", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeProvider, Provider: "", Name: "mimo", BillingSource: "api", Seq: 1},
	})

	n, err := store.DeleteByProvider(ctx, "claude-aws")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 1 {
		t.Errorf("删除行数 = %d, want 1", n)
	}
	if left := remainingKeys(t, db); !left["provider//mimo"] || len(left) != 1 {
		t.Errorf("剩余 = %v, want 只剩 mimo", left)
	}
}

// TestDeleteByProvider_NoPrefixMatching 精确匹配,不做前缀/LIKE。
// 线上有大量共前缀的面名(tokenmarket-pro / -pro2 / -pro3 / -pro+plus),
// 一旦退化成 LIKE 'name%' 会连着删掉活面的排序。
func TestDeleteByProvider_NoPrefixMatching(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		{Scope: RouteScopeProvider, Name: "tokenmarket-pro", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeProvider, Name: "tokenmarket-pro2", BillingSource: "api", Seq: 1},
		{Scope: RouteScopeProvider, Name: "tokenmarket-pro3", BillingSource: "api", Seq: 2},
		{Scope: RouteScopeProvider, Name: "tokenmarket-pro+plus", BillingSource: "api", Seq: 3},
		{Scope: RouteScopeKey, Provider: "tokenmarket-pro2", Name: "k1", BillingSource: "api", Seq: 0},
	})

	n, err := store.DeleteByProvider(ctx, "tokenmarket-pro")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除行数 = %d, want 1 —— 疑似退化成前缀匹配,连着删了 pro2/pro3/pro+plus", n)
	}

	left := remainingKeys(t, db)
	for _, want := range []string{
		"provider//tokenmarket-pro2",
		"provider//tokenmarket-pro3",
		"provider//tokenmarket-pro+plus",
		"key/tokenmarket-pro2/k1",
	} {
		if !left[want] {
			t.Errorf("共前缀的活面行 %q 被误删", want)
		}
	}
}

// TestDeleteByProvider_EmptyIsNoOp 空串必须是 no-op。
// 危险点:scope=provider 的行 provider 列**本来就是空串**,
// 若不拦空串,按空串等值匹配 provider 列会一把删光全部层内 provider 排序。
func TestDeleteByProvider_EmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		{Scope: RouteScopeProvider, Provider: "", Name: "mimo", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeProvider, Provider: "", Name: "deepseek", BillingSource: "api", Seq: 1},
		{Scope: RouteScopeKey, Provider: "mimo", Name: "k1", BillingSource: "api", Seq: 0},
	})

	n, err := store.DeleteByProvider(ctx, "")
	if err != nil {
		t.Fatalf("DeleteByProvider(\"\"): %v", err)
	}
	if n != 0 {
		t.Errorf("空串删了 %d 行 —— scope=provider 的 provider 列本就是空串,这会删光全部层内排序", n)
	}
	if left := remainingKeys(t, db); len(left) != 3 {
		t.Errorf("空串调用后剩余 %d 行, want 3:%v", len(left), left)
	}
}

// TestDeleteByProvider_UnknownIsNoOp 不存在的名字 → 0 行,不报错(幂等)。
// 删站级联会对「面名 ∪ 站名」逐个调用,多算的名字必须安静地 no-op。
func TestDeleteByProvider_UnknownIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		{Scope: RouteScopeProvider, Name: "mimo", BillingSource: "api", Seq: 0},
	})

	n, err := store.DeleteByProvider(ctx, "never-existed")
	if err != nil {
		t.Fatalf("未知名字不该报错: %v", err)
	}
	if n != 0 {
		t.Errorf("删除行数 = %d, want 0", n)
	}
	if left := remainingKeys(t, db); !left["provider//mimo"] {
		t.Error("误删了活行")
	}
}

// TestDeleteByProvider_AcrossBillingSources 跨层清干净:同一个面在
// token_plan 和 api 两层都可能有 provider 名次行,删站要两层都清。
// (Replace 是按 billing_source 分层写的,所以一个面确实会有多层行。)
func TestDeleteByProvider_AcrossBillingSources(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		{Scope: RouteScopeProvider, Name: "dead-face", BillingSource: "token_plan", Seq: 0},
		{Scope: RouteScopeProvider, Name: "dead-face", BillingSource: "api", Seq: 0},
		{Scope: RouteScopeProvider, Name: "dead-face", BillingSource: "free", Seq: 0},
		{Scope: RouteScopeProvider, Name: "alive", BillingSource: "api", Seq: 1},
	})

	n, err := store.DeleteByProvider(ctx, "dead-face")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 3 {
		t.Errorf("删除行数 = %d, want 3(三层各一行)—— 漏层会留孤儿继续占名次", n)
	}
	if left := remainingKeys(t, db); len(left) != 1 || !left["provider//alive"] {
		t.Errorf("剩余 = %v, want 只剩 alive", left)
	}
}

// TestDeleteByProvider_KeyScopeNameCollision 名字碰撞防线:
// 某 provider 的 **key 名**恰好等于另一个 provider 的名字时,不能连坐。
// 造得出来:key 名是 key 尾 8 位,人工也能改成任意串。
func TestDeleteByProvider_KeyScopeNameCollision(t *testing.T) {
	ctx := context.Background()
	db := newRouteOrderTestDB(t)
	store := NewRouteOrderStore(db)

	seedRouteOrders(t, db, []RouteOrder{
		// 要删的目标
		{Scope: RouteScopeProvider, Name: "codex", BillingSource: "api", Seq: 0},
		// 活着的 provider,其 key 恰好叫 "codex" —— 不能被带走
		{Scope: RouteScopeKey, Provider: "tokenmarket-pro", Name: "codex", BillingSource: "api", Seq: 0},
	})

	n, err := store.DeleteByProvider(ctx, "codex")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 1 {
		t.Errorf("删除行数 = %d, want 1", n)
	}
	if left := remainingKeys(t, db); !left["key/tokenmarket-pro/codex"] {
		t.Error("活 provider 里恰好叫 codex 的 key 行被连坐删掉了 —— scope=key 必须只按 provider 列匹配")
	}
}
