// 前后端共享枚举 —— 前端唯一常量源。
//
// 低耦合:后端用 Go 常量定义这些枚举(token_plan / api / free, key status 等),
// 前端此前在多个 .vue 里硬编码同值(改 Go 枚举值 → UI 状态/颜色/过滤静默失效)。
// 集中到这里,各 view 统一引用;与 backend 保持一致(后端非 0 处已集中:
// keypool/billingsource.go、keypool/key.go)。
//
// ⚠️ 新增/重命名时须与 backend 同步:
//   - billing_source → backend/internal/keypool/billingsource.go
//   - key status     → backend/internal/keypool/key.go
//   - quota_kind     → backend/internal/provider/{deepseek,mimo,minimax}/balancer.go
//     (glm 面已于 2026-08-20 随 gemini/qwen 一起下线)

// BillingSource 计费来源枚举(值须与 backend keypool.BillingSource 一致)
export const BILLING_SOURCE = {
  TOKEN_PLAN: 'token_plan',
  API: 'api',
  FREE: 'free',
} as const
export const BILLING_SOURCE_DEFAULT = BILLING_SOURCE.API
export type BillingSourceValue = (typeof BILLING_SOURCE)[keyof typeof BILLING_SOURCE]

// KeyStatus key 运行时状态(值须与 backend keypool.KeyStatus 一致;P-no-disabled:无 DISABLED)
export const KEY_STATUS = {
  ACTIVE: 'ACTIVE',
  COOLING: 'COOLING',
  LIMITED: 'LIMITED',
  QUOTA_EXCEEDED: 'QUOTA_EXCEEDED',
} as const
// 前端对 enabled=false 的兜底展示用(非 backend 状态值)
export const KEY_STATUS_DISABLED_UI = 'DISABLED'

// QuotaKind 余额数值类型(值须与 backend quota_kind 一致;''=currency)
export const QUOTA_KIND = {
  PERCENT: 'percent',
  CURRENCY: 'currency',
  EMPTY: '',
} as const
