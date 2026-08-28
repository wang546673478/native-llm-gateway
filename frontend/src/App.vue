<template>
  <n-config-provider :theme="naiveTheme" :theme-overrides="themeOverrides">
    <!-- NGlobalStyle: 让 naive-ui 接管 body 背景/文字色,暗色切换时不留白底 -->
    <n-global-style />
    <n-message-provider>
      <!-- NDialogProvider: useDialog() 的宿主 —— 缺它则 useDialog() 返回 undefined,
           调用方 setup 阶段抛错、整个页面白屏(ModelManager 的「清理无归属」二次确认用) -->
      <n-dialog-provider>
      <!-- 登录页：独立布局，无侧边栏 -->
      <router-view v-if="isLoginPage" />
      <!-- 主应用：带侧边栏导航 -->
      <n-layout v-else has-sider style="min-height: 100vh">
        <n-layout-sider bordered :width="220" collapse-mode="width" :collapsed-width="64" show-trigger="bar" style="display: flex; flex-direction: column;">
          <div class="logo">LLM Gateway</div>
          <n-menu
            :options="menuOptions"
            :value="activeKey"
            @update:value="onMenuSelect"
            style="flex: 1; overflow-y: auto;"
          />
          <!-- 用户信息 & 登出 -->
          <div v-if="authStore.user" class="user-info">
            <div class="username">{{ authStore.user.username }}</div>
            <button @click="handleLogout" class="logout-button">登出</button>
          </div>
        </n-layout-sider>
        <n-layout>
          <n-layout-header bordered style="padding: 12px 24px">
            <n-space justify="space-between" align="center">
              <span class="page-title">
                {{ currentTitle }}
              </span>
              <n-space align="center" :size="12">
                <theme-switch />
                <n-tag :type="healthOk ? 'success' : 'error'" size="small">
                  {{ healthOk ? '● Healthy' : '● Unhealthy' }}
                </n-tag>
              </n-space>
            </n-space>
          </n-layout-header>
          <n-layout-content style="padding: 24px">
            <router-view />
          </n-layout-content>
        </n-layout>
      </n-layout>
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>

<script setup lang="ts">
import { computed, h, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider, NDialogProvider, NGlobalStyle, NLayout, NLayoutSider,
  NLayoutHeader, NLayoutContent, NMenu, NMessageProvider, NSpace, NTag,
  darkTheme,
} from 'naive-ui'
import type { GlobalThemeOverrides, MenuOption } from 'naive-ui'
import { RouterLink } from 'vue-router'
import { useHealthStore } from './stores/health'
import { useAuthStore } from './stores/auth'
import { authApi } from './api/client'
import { useTheme } from './composables/useTheme'
import ThemeSwitch from './components/ThemeSwitch.vue'

const route = useRoute()
const router = useRouter()
const healthStore = useHealthStore()
const authStore = useAuthStore()
const { isDark } = useTheme()

// naive-ui 内置主题:暗色传 darkTheme,亮色传 null(库的默认亮色)
const naiveTheme = computed(() => (isDark.value ? darkTheme : null))

// themeOverrides 让 naive-ui 组件的主色与 tokens.css 对齐 —— 否则库按钮/标签
// 仍用它自己的绿,与手写样式的 var(--c-primary) 分叉。
// 这不是双真相源:naive-ui 的 API 只接受 JS 值(不吃 CSS 变量),两边引用同一组
// 语义色常量是库的约束。改主色时两处一起改(值相同),由下方注释锁定关联。
// 值必须与 styles/tokens.css 的 --c-primary / --c-info / --c-warning / --c-error 一致。
const LIGHT = {
  primary: '#18a058', primaryHover: '#36ad6a', primaryPressed: '#0c7a43',
  info: '#2080f0', warning: '#f0a020', error: '#d03050',
}
const DARK = {
  primary: '#4ade80', primaryHover: '#63e894', primaryPressed: '#35c96b',
  info: '#60a5fa', warning: '#fbbf24', error: '#f87171',
}

const themeOverrides = computed<GlobalThemeOverrides>(() => {
  const c = isDark.value ? DARK : LIGHT
  return {
    common: {
      primaryColor: c.primary,
      primaryColorHover: c.primaryHover,
      primaryColorPressed: c.primaryPressed,
      primaryColorSuppl: c.primaryHover,
      infoColor: c.info,
      infoColorHover: c.info,
      warningColor: c.warning,
      warningColorHover: c.warning,
      errorColor: c.error,
      errorColorHover: c.error,
      successColor: c.primary,
      successColorHover: c.primaryHover,
      borderRadius: '8px',
      borderRadiusSmall: '6px',
      fontFamilyMono: 'var(--font-mono)',
    },
    Card: { borderRadius: '10px' },
    DataTable: { thFontWeight: '600' },
  }
})

const isLoginPage = computed(() => route.path === '/login')
const activeKey = computed(() => route.path)
const healthOk = computed(() => healthStore.ok)
const currentTitle = computed(() => {
  const map: Record<string, string> = {
    '/overview': '📊 总览',
    '/providers': '🏭 厂商管理 - 厂商列表',
    '/provider-keys': '🏭 厂商管理 - 密钥管理',
    '/keys': '🎫 Gateway Keys',
    '/relay-stations': '🚉 中转站',
    '/routing': '🗺️ 路由规则',
    '/usage': '📈 用量',
    '/access-logs': '📋 Access Logs',
    '/inflight': '⚡ 活跃请求',
    '/models': '🧩 模型管理',
    '/admin-users': '👥 管理员用户',
  }
  return map[route.path] ?? 'LLM Gateway'
})

function renderMenuLabel(to: string, label: string) {
  return () => h(RouterLink, { to }, { default: () => label })
}

const menuOptions = computed<MenuOption[]>(() => {
  const options: MenuOption[] = [
    { key: '/overview', label: renderMenuLabel('/overview', '📊 总览') },
    {
      key: 'vendor-management',
      label: '🏭 厂商管理',
      children: [
        { key: '/providers', label: renderMenuLabel('/providers', '厂商列表') },
        { key: '/provider-keys', label: renderMenuLabel('/provider-keys', '密钥管理') },
      ],
    },
    { key: '/keys', label: renderMenuLabel('/keys', '🎫 Gateway Keys') },
    { key: '/relay-stations', label: renderMenuLabel('/relay-stations', '🚉 中转站') },
    { key: '/routing', label: renderMenuLabel('/routing', '🗺️ 路由规则') },
    { key: '/usage', label: renderMenuLabel('/usage', '📈 用量') },
    { key: '/access-logs', label: renderMenuLabel('/access-logs', '📋 Access Logs') },
    { key: '/inflight', label: renderMenuLabel('/inflight', '⚡ 活跃请求') },
    { key: '/models', label: renderMenuLabel('/models', '🧩 模型管理') },
  ]

  // 只有 root 用户可见管理员用户菜单
  if (authStore.isRoot) {
    options.push({ key: '/admin-users', label: renderMenuLabel('/admin-users', '👥 管理员用户') })
  }

  return options
})

function onMenuSelect(key: string) {
  router.push(key)
}

async function handleLogout() {
  try {
    await authApi.logout()
  } catch {
    // 忽略错误，清除本地 token 即可
  } finally {
    authStore.clearToken()
    router.push('/login')
  }
}

onMounted(() => {
  healthStore.check()
  setInterval(() => healthStore.check(), 10_000)

  // 如果已登录但 user 信息为空，获取用户信息
  if (authStore.isAuthenticated && !authStore.user && route.path !== '/login') {
    authApi.me()
      .then(userInfo => {
        authStore.setUser({ username: userInfo.username, role: userInfo.role })
      })
      .catch(() => {
        // 获取失败，清除 token
        authStore.clearToken()
        router.push('/login')
      })
  }
})
</script>

<style>
/* body 基线已移到 styles/tokens.css(与 CSS 变量同源),这里只留布局件样式 */

.logo {
  padding: var(--sp-4);
  font-size: 16px;
  font-weight: 700;
  color: var(--c-primary);
  border-bottom: 1px solid var(--b-1);
  letter-spacing: -0.01em;
}

.page-title {
  font-size: 18px;
  font-weight: 600;
  color: var(--t-1);
}

.user-info {
  padding: var(--sp-3) var(--sp-4);
  border-top: 1px solid var(--b-1);
  background: var(--s-sunken);
  flex-shrink: 0;
}

.username {
  font-size: 14px;
  font-weight: 500;
  color: var(--t-1);
  margin-bottom: var(--sp-2);
}

.logout-button {
  width: 100%;
  padding: 6px 12px;
  background: var(--s-card);
  color: var(--t-2);
  border: 1px solid var(--b-1);
  border-radius: var(--r-sm);
  cursor: pointer;
  font-size: 13px;
  font-family: inherit;
  transition: background var(--tr-fast), border-color var(--tr-fast),
    color var(--tr-fast);
}

.logout-button:hover {
  background: var(--c-primary-soft);
  border-color: var(--c-primary);
  color: var(--c-primary);
}
</style>
