package provider

// ProviderLookup 路由/代理层需要的 provider 查询窄接口。
//
// 低耦合:router 与 proxy 此前直接依赖 *provider.Manager 具体类型(违反
// "Router 只接收窄接口" 的 CLAUDE.md 建议)。抽成窄接口后,router/proxy 只依赖
// 这 6 个方法,不依赖 Manager 的具体实现细节 —— 替换/测试 Manager 更容易,router
// 也不会因 Manager 新增方法而耦合。Manager 实现该接口。
type ProviderLookup interface {
	// Get 按注册名取 Provider(实例;不存在 ok=false)
	Get(name string) (Provider, bool)
	// GetAll 返回所有已加载 Provider(注册名 → Provider)
	GetAll() map[string]Provider
	// BillingSourceFor 查 provider 的计费来源("token_plan"/"api"/"free")
	BillingSourceFor(provider string) string
	// DefaultModelFor 返回 provider 承接未知模型名的默认模型
	DefaultModelFor(name string) string
	// SupportsResponsesAPI 该 provider 是否原生支持 OpenAI Responses API
	SupportsResponsesAPI(name string) bool
	// CostFor 查 model 的单价(计费用),返回 ModelCost
	CostFor(providerName, modelID string) ModelCost
	// EndpointFor 查 provider 的 baseURL(quotacheck 探测用;未加载返回空)
	EndpointFor(providerName string) string
	// VendorFor 查注册名的厂商(vendor)。路由 Level 2 排序的改写键是厂商名
	// (route_order provider 作用域),而候选是注册面名,需归 vendor 再查改写。
	VendorFor(name string) string
	// ModelsFor 返回某注册面按 vendor 归位后的模型 id 列表(见 Manager.ModelsFor)。
	ModelsFor(name string) []string
}
