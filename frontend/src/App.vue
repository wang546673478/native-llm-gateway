<template>
  <n-config-provider :theme="null">
    <n-message-provider>
      <n-layout has-sider style="min-height: 100vh">
        <n-layout-sider bordered :width="220" collapse-mode="width" :collapsed-width="64" show-trigger="bar">
          <div class="logo">LLM Gateway</div>
          <n-menu
            :options="menuOptions"
            :value="activeKey"
            @update:value="onMenuSelect"
          />
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
    </n-message-provider>
  </n-config-provider>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider, NLayout, NLayoutSider, NLayoutHeader, NLayoutContent,
  NMenu, NMessageProvider, NSpace, NTag,
} from 'naive-ui'
import type { MenuOption } from 'naive-ui'
import { RouterLink } from 'vue-router'
import { useHealthStore } from './stores/health'

const route = useRoute()
const router = useRouter()
const healthStore = useHealthStore()

const activeKey = computed(() => route.path)
const healthOk = computed(() => healthStore.ok)
const currentTitle = computed(() => {
  const map: Record<string, string> = {
    '/overview': '总览',
    '/providers': 'Providers',
    '/provider-keys': 'Provider Keys',
    '/keys': 'Gateway Keys',
    '/routing': '路由规则',
    '/usage': '用量',
    '/access-logs': 'Access Logs',
  }
  return map[route.path] ?? 'LLM Gateway'
})

function renderMenuLabel(to: string, label: string) {
  return () => h(RouterLink, { to }, { default: () => label })
}

const menuOptions: MenuOption[] = [
  { key: '/overview', label: renderMenuLabel('/overview', '总览') },
  { key: '/providers', label: renderMenuLabel('/providers', 'Providers') },
  { key: '/provider-keys', label: renderMenuLabel('/provider-keys', 'Provider Keys') },
  { key: '/keys', label: renderMenuLabel('/keys', 'Gateway Keys') },
  { key: '/routing', label: renderMenuLabel('/routing', '路由规则') },
  { key: '/usage', label: renderMenuLabel('/usage', '用量') },
  { key: '/access-logs', label: renderMenuLabel('/access-logs', '📋 Access Logs') },
]

function onMenuSelect(key: string) {
  router.push(key)
}

onMounted(() => {
  healthStore.check()
  setInterval(() => healthStore.check(), 10_000)
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
body {
  margin: 0;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  background: #f5f7fa;
}
</style>
