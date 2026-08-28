// 前端状态/类型展示的高内聚渲染辅助。
//
// 低耦合:ProviderKeys.vue/Overview.vue/Keys.vue/AccessLogs.vue 此前各自硬编码同一
// 套颜色调色板(#18a058 绿 / #f0a020 黄 / #d03050 红 / #2080f0 蓝 / #999 灰)
// 和状态→标签映射(3 处独立 map),改一个颜色要改多个 view 且漏一处 UI 不一致。
// 集中到这里,各 view 统一引用。

// statusPalette 单一调色板(颜色 + 语义),供 status/billing/balance 等渲染共享。
// 与 naive-ui 标签色一致:success/info/warning/error/undefined。
//
// 色值本身**不在这里定义** —— 真相源是 styles/tokens.css 的语义色变量。
// 这里用 getter 在运行时读 CSS 变量:naive-ui 的 render() 每次重绘都重新取值,
// 所以暗色切换后颜色自动跟上,各 view 不需要加 watch/key 强制重渲染。
// 调用方写法(STATUS_PALETTE.green.color)完全不变。
// cssToken 读一个设计 token 的当前值。导出是因为 canvas 渲染(echarts)不能吃
// CSS 变量,只能拿到具体色值 —— 图表构造 option 时用它取色,与 CSS 走同一真相源。
export function cssToken(name: string, fallback: string): string {
  if (typeof window === 'undefined' || typeof document === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

export const STATUS_PALETTE = {
  green: {
    get color() { return cssToken('--c-primary', '#18a058') },
    tagType: 'success',
  },
  blue: {
    get color() { return cssToken('--c-info', '#2080f0') },
    tagType: 'info',
  },
  yellow: {
    get color() { return cssToken('--c-warning', '#f0a020') },
    tagType: 'warning',
  },
  red: {
    get color() { return cssToken('--c-error', '#d03050') },
    tagType: 'error',
  },
  gray: {
    get color() { return cssToken('--c-neutral', '#909399') },
    tagType: 'default',
  },
} as const

// quotaDisplay 按 quota_kind(percent/currency/空)渲染余额值。
// ProviderKeys.vue 余额列 与 Overview.vue 用量汇总 用同一套语义:
//   percent → 显示 '—'(避免把 e.g. 63% 当金额);非 percent(currency) → ¥N。
// percent 用 QUOTA_KIND.PERCENT 常量(单一来源,防裸串漂移)。
import { QUOTA_KIND } from '../api/constants'

export function quotaDisplay(quotaKind: string | undefined, amount: number | null | undefined): string {
  if (amount === null || amount === undefined || Number.isNaN(amount)) return '—'
  if (quotaKind === QUOTA_KIND.PERCENT) return '—'
  return `¥${amount.toFixed(2)}`
}

// fmtNum 通用数字格式化(Usage.vue / Overview.vue 各一份 fmtNum 收敛)。
// 对齐 Overview 的防御语义:undefined/null → '—'(不显示假 0)。
export function fmtNum(n: number | null | undefined): string {
  if (n === undefined || n === null) return '—'
  return n.toLocaleString('en-US')
}
