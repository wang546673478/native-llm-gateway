// Package keypool — 计费来源(tier)常量
//
// 单一职责:集中定义 billing source / tier 的枚举值(token_plan / api / free)。
// 消除跨包散落的裸字符串字面量 —— 之前 "token_plan" 在 keypool/proxy/router/
// quotacheck/mimo/auth/policy/config 等 8 包散落 77 处,加/改名计费档要手动改
// 多处,漏一处就路由/计费走错档,编译器不抓。
//
// 为何不求把载体(string)改成类型:Key.BillingSource / router.Result.Tier /
// config billing_source / DB 字段 / 前端 JSON 都已是 string,整体改类型会大范围
// 波及持久化+API+前端,正是第一要素要避免的"改一处坏一片"。所以保守做法:
// 数据仍是 string,但**逻辑比较**统一改用本包常量(单一命名源),跨包值一致性由
// TestBillingSourceAlignment 守卫测试兜底。
package keypool

// BillingSource 计费来源 / tier 枚举值
type BillingSource string

const (
	// BillingSourceTokenPlan 包月套餐(如 minimax token plan),优先路由,
	// quota 用完自动 failover 到 api/free
	BillingSourceTokenPlan BillingSource = "token_plan"
	// BillingSourceAPI 按量付费(默认)
	BillingSourceAPI BillingSource = "api"
	// BillingSourceFree 免费档
	BillingSourceFree BillingSource = "free"
	// BillingSourceDefault 未显式声明时的兜底值
	BillingSourceDefault BillingSource = "api"
)

// TierOrder 跨包一致的 tier 优先级(用于迭代降档顺序:token_plan → api → free)
// 集中在这里,替代各处重复的 []string{"token_plan","api","free"}
var TierOrder = []string{
	string(BillingSourceTokenPlan),
	string(BillingSourceAPI),
	string(BillingSourceFree),
}

// IsTokenPlan 判断 tier 是否 token_plan(逻辑比较统一走常量,避免裸字面量)
func IsTokenPlan(tier string) bool { return tier == string(BillingSourceTokenPlan) }

// Normalize 空值兜底为默认("api"),并校验合法性;非法返回 false
func Normalize(bs string) (string, bool) {
	if bs == "" {
		return string(BillingSourceDefault), true
	}
	switch BillingSource(bs) {
	case BillingSourceTokenPlan, BillingSourceAPI, BillingSourceFree:
		return bs, true
	}
	return "", false
}
