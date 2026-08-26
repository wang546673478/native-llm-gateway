<template>
  <n-config-provider :theme="null">
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
              <span style="font-size: 18px; font-weight: 600">
                {{ currentTitle }}
              </span>
              <n-tag :type="healthOk ? 'success' : 'error'" size="small">
                {{ healthOk ? '● Healthy' : '● Unhealthy' }}
              </n-tag>
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
import { computed, h, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider, NDialogProvider, NLayout, NLayoutSider, NLayoutHeader, NLayoutContent,
  NMenu, NMessageProvider, NSpace, NTag,
} from 'naive-ui'
import type { MenuOption } from 'naive-ui'
import { RouterLink } from 'vue-router'
import { useHealthStore } from './stores/health'
import { useAuthStore } from './stores/auth'
import { authApi } from './api/client'

const route = useRoute()
const router = useRouter()
const healthStore = useHealthStore()
const authStore = useAuthStore()

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
.logo {
  padding: 16px;
  font-size: 16px;
  font-weight: 700;
  color: #18a058;
  border-bottom: 1px solid #eee;
}

.user-info {
  padding: 12px 16px;
  border-top: 1px solid #eee;
  background: #fafafa;
  flex-shrink: 0;
}

.username {
  font-size: 14px;
  font-weight: 500;
  color: #333;
  margin-bottom: 8px;
}

.logout-button {
  width: 100%;
  padding: 6px 12px;
  background: white;
  border: 1px solid #ddd;
  border-radius: 4px;
  cursor: pointer;
  font-size: 13px;
  transition: all 0.2s;
}

.logout-button:hover {
  background: #f5f5f5;
  border-color: #999;
}

body {
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  background: #f5f7fa;
}
</style>
