// 主题状态机 —— 单一职责:管理 system/light/dark 三态并同步到 DOM。
//
// 低耦合:各 view 不碰 document / localStorage / matchMedia,只消费 useTheme()。
// 换持久化方式(如改存后端)只改这一个文件。
//
// 模块级单例:状态定义在模块作用域而非 useTheme() 内部 —— 多个组件调用
// useTheme() 共享同一份 ref,不各自持一份(否则顶栏切换器与页面样式会脱节)。
// matchMedia 监听器同理只注册一次,不随组件挂载重复注册。

import { computed, ref, watchEffect } from 'vue'

export type ThemeMode = 'system' | 'light' | 'dark'

const STORAGE_KEY = 'llm-gateway-theme'
const DARK_QUERY = '(prefers-color-scheme: dark)'

function readStored(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // localStorage 不可用(隐私模式 / 禁用)— 退回跟随系统,不阻断渲染
  }
  return 'system'
}

const mode = ref<ThemeMode>(readStored())

// 系统偏好。matchMedia 在极老浏览器可能缺失 — 缺失时恒为 false(亮色)
const mql = typeof window !== 'undefined' && window.matchMedia
  ? window.matchMedia(DARK_QUERY)
  : null
const systemDark = ref(mql?.matches ?? false)

// 系统主题变化时更新(用户在 macOS/GNOME 里切深色,页面即时跟随)。
// 只在 mode==='system' 时影响最终结果,但 systemDark 始终保持真实值 —
// 这样用户从 light 切回 system 时立刻拿到正确的当前系统偏好。
mql?.addEventListener('change', e => {
  systemDark.value = e.matches
})

/** 最终生效的暗色状态:system 跟随系统,light/dark 为用户显式选择 */
const isDark = computed(() =>
  mode.value === 'system' ? systemDark.value : mode.value === 'dark',
)

// 单一副作用点:把暗色状态同步成 html.dark class,供 tokens.css 切换变量。
// 这是本模块唯一触碰 DOM 的地方。
watchEffect(() => {
  if (typeof document === 'undefined') return
  document.documentElement.classList.toggle('dark', isDark.value)
  // 让浏览器原生控件(滚动条、表单默认样式)也跟随
  document.documentElement.style.colorScheme = isDark.value ? 'dark' : 'light'
})

function setMode(next: ThemeMode) {
  mode.value = next
  try {
    localStorage.setItem(STORAGE_KEY, next)
  } catch {
    // 存不进去也不影响本次会话生效
  }
}

export function useTheme() {
  return { mode, isDark, setMode }
}
