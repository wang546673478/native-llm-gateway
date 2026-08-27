package auth

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	dbpkg "github.com/wang546673478/native-llm-gateway/internal/database"
)

// P-relay-cascade 守卫组:ProviderKeyStore.DeleteByProvider。
//
// provider_api_keys.provider_name 是普通字符串列(非外键)。中转站被删后 key 行
// 会留下,用户实测的症状就是这个:「claude-aws 和 codex 应该都被我删除了,
// 为什么你还能找到?」—— 站早没了,行还在,查得到。
// 除了幽灵条目,更要紧的是上游 key 明文(存在 key_hash 列,列名是历史遗留)
// 会无限期留库。
func newProviderKeyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dbpkg.ProviderAPIKey{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func seedProviderKeys(t *testing.T, db *gorm.DB, pairs [][2]string) {
	t.Helper()
	for _, p := range pairs {
		row := dbpkg.ProviderAPIKey{
			ProviderName:  p[0],
			Name:          p[1],
			KeyHash:       "sk-" + p[0] + "-" + p[1],
			Enabled:       dbpkg.BoolPtr(true),
			BillingSource: "api",
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("seed provider key %s/%s: %v", p[0], p[1], err)
		}
	}
}

func remainingProviderNames(t *testing.T, db *gorm.DB) map[string]int {
	t.Helper()
	var rows []dbpkg.ProviderAPIKey
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("list provider keys: %v", err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.ProviderName]++
	}
	return out
}

// TestProviderKeyDeleteByProvider_RemovesAllKeysOfFace 一个面下多把 key 全清。
func TestProviderKeyDeleteByProvider_RemovesAllKeysOfFace(t *testing.T) {
	ctx := context.Background()
	db := newProviderKeyTestDB(t)
	store := NewProviderKeyStore(db)

	// 复刻实测形状:tokenmarket-kiro3 有两把 key(GteszdUN / ieQroWa0)
	seedProviderKeys(t, db, [][2]string{
		{"tokenmarket-kiro3", "GteszdUN"},
		{"tokenmarket-kiro3", "ieQroWa0"},
		{"tokenmarket-kiro", "aliveKey"},
	})

	n, err := store.DeleteByProvider(ctx, "tokenmarket-kiro3")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 2 {
		t.Errorf("删除行数 = %d, want 2", n)
	}
	left := remainingProviderNames(t, db)
	if left["tokenmarket-kiro3"] != 0 {
		t.Errorf("tokenmarket-kiro3 还剩 %d 行 —— 幽灵条目+key 明文留库", left["tokenmarket-kiro3"])
	}
	if left["tokenmarket-kiro"] != 1 {
		t.Errorf("误伤了活面 tokenmarket-kiro,剩余 = %v", left)
	}
}

// TestProviderKeyDeleteByProvider_NoPrefixMatching 精确匹配。
// kiro3 被删、kiro/kiro2/kiro4 还活着,是线上真实拓扑 ——
// 退化成前缀匹配会把三个活站的 key 一起删掉,那三站立刻全部 401。
func TestProviderKeyDeleteByProvider_NoPrefixMatching(t *testing.T) {
	ctx := context.Background()
	db := newProviderKeyTestDB(t)
	store := NewProviderKeyStore(db)

	seedProviderKeys(t, db, [][2]string{
		{"tokenmarket-kiro", "k1"},
		{"tokenmarket-kiro2", "k2"},
		{"tokenmarket-kiro3", "k3"},
		{"tokenmarket-kiro4", "k4"},
		{"tokenmarket", "station-level"},
	})

	n, err := store.DeleteByProvider(ctx, "tokenmarket-kiro")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除行数 = %d, want 1 —— 疑似前缀匹配,连着删了 kiro2/3/4", n)
	}
	left := remainingProviderNames(t, db)
	for _, want := range []string{"tokenmarket-kiro2", "tokenmarket-kiro3", "tokenmarket-kiro4", "tokenmarket"} {
		if left[want] != 1 {
			t.Errorf("共前缀的活面 %q 的 key 被误删,剩余 = %v", want, left)
		}
	}
}

// TestProviderKeyDeleteByProvider_EmptyIsNoOp 空串必须 no-op。
// 危险点:List(ctx, "") 的语义是「不过滤,返回全表」;同样的空串落到 Delete
// 上会**清空整张 provider_api_keys 表** —— 全部厂商瞬间没 key,网关全 503。
func TestProviderKeyDeleteByProvider_EmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newProviderKeyTestDB(t)
	store := NewProviderKeyStore(db)

	seedProviderKeys(t, db, [][2]string{
		{"deepseek", "k1"},
		{"minimax", "k2"},
		{"mimo", "k3"},
	})

	n, err := store.DeleteByProvider(ctx, "")
	if err != nil {
		t.Fatalf("DeleteByProvider(\"\"): %v", err)
	}
	if n != 0 {
		t.Errorf("空串删了 %d 行 —— 这会清空整表,全部厂商瞬间无 key", n)
	}
	if left := remainingProviderNames(t, db); len(left) != 3 {
		t.Errorf("空串调用后剩 %d 个 provider, want 3:%v", len(left), left)
	}
}

// TestProviderKeyDeleteByProvider_UnknownIsNoOp 未知名字 → 0 行不报错。
// 删站级联对「面名 ∪ 站名」逐个调用,多算的名字必须安静 no-op
// (single 模式下面名 == 站名,去重后只调一次;multi 模式站名不在面名里,
// 但 syncRelayStationKeys 按站名写行,所以两者都要调)。
func TestProviderKeyDeleteByProvider_UnknownIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := newProviderKeyTestDB(t)
	store := NewProviderKeyStore(db)

	seedProviderKeys(t, db, [][2]string{{"deepseek", "k1"}})

	n, err := store.DeleteByProvider(ctx, "never-existed")
	if err != nil {
		t.Fatalf("未知名字不该报错: %v", err)
	}
	if n != 0 {
		t.Errorf("删除行数 = %d, want 0", n)
	}
	if left := remainingProviderNames(t, db); left["deepseek"] != 1 {
		t.Error("误删了活行")
	}
}

// TestProviderKeyDeleteByProvider_IgnoresEnabledFlag disabled 的行也要清 —
// 「禁用」不等于「已删除」,留着照样是幽灵条目 + 明文留库。
func TestProviderKeyDeleteByProvider_IgnoresEnabledFlag(t *testing.T) {
	ctx := context.Background()
	db := newProviderKeyTestDB(t)
	store := NewProviderKeyStore(db)

	rows := []dbpkg.ProviderAPIKey{
		{ProviderName: "dead-face", Name: "on", KeyHash: "sk-1", Enabled: dbpkg.BoolPtr(true), BillingSource: "api"},
		{ProviderName: "dead-face", Name: "off", KeyHash: "sk-2", Enabled: dbpkg.BoolPtr(false), BillingSource: "api"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := store.DeleteByProvider(ctx, "dead-face")
	if err != nil {
		t.Fatalf("DeleteByProvider: %v", err)
	}
	if n != 2 {
		t.Errorf("删除行数 = %d, want 2(enabled 与 disabled 都要清)", n)
	}
}

// TestProviderKeyDeleteByProvider_SatisfiesNarrowPurgerContract 契约守卫:
// handler 侧定义了窄接口 ProviderKeyPurger(只要 DeleteByProvider),
// 由 auth.ProviderKeyStore 结构化满足。这里用一个同形匿名接口断言签名不漂移 ——
// 改了参数/返回值,handler 的注入会在编译期断,但那是另一个包;
// 这条测试让签名变更在本包立刻可见。
func TestProviderKeyDeleteByProvider_SatisfiesNarrowPurgerContract(t *testing.T) {
	var purger interface {
		DeleteByProvider(ctx context.Context, providerName string) (int64, error)
	} = NewProviderKeyStore(newProviderKeyTestDB(t))

	if purger == nil {
		t.Fatal("ProviderKeyStore 不再满足 handler.ProviderKeyPurger 的形状")
	}
}
