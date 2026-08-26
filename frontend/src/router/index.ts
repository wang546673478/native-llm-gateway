import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: () => import('../views/Login.vue'), meta: { public: true } },
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('../views/Overview.vue') },
    { path: '/providers', component: () => import('../views/Providers.vue') },
    { path: '/provider-keys', component: () => import('../views/ProviderKeys.vue') },
    { path: '/keys', component: () => import('../views/Keys.vue') },
    { path: '/relay-stations', component: () => import('../views/RelayStations.vue') },
    { path: '/routing', component: () => import('../views/Routing.vue') },
    { path: '/usage', component: () => import('../views/Usage.vue') },
    { path: '/access-logs', name: 'access-logs', component: () => import('../views/AccessLogs.vue') },
    { path: '/inflight', name: 'inflight', component: () => import('../views/Inflight.vue') },
    { path: '/models', component: () => import('../views/ModelManager.vue') },
    { path: '/admin-users', component: () => import('../views/AdminUsers.vue'), meta: { requiresRoot: true } },
  ],
})

// 路由守卫：检查认证状态
router.beforeEach(async (to, from, next) => {
  const authStore = useAuthStore()

  // 公开路由直接放行
  if (to.meta.public) {
    // 已登录用户访问登录页，跳转到首页
    if (to.path === '/login' && authStore.isAuthenticated) {
      next('/')
      return
    }
    next()
    return
  }

  // 未登录，跳转到登录页
  if (!authStore.isAuthenticated) {
    next('/login')
    return
  }

  // 需要 root 权限的路由
  if (to.meta.requiresRoot) {
    // 首次访问或刷新页面，user 可能为空，需要先获取用户信息
    if (!authStore.user) {
      try {
        const userInfo = await import('../api/client').then(m => m.authApi.me())
        authStore.setUser({ username: userInfo.username, role: userInfo.role })
      } catch {
        // 获取用户信息失败，清除 token 并跳转到登录页
        authStore.clearToken()
        next('/login')
        return
      }
    }

    if (!authStore.isRoot) {
      // 非 root 用户，跳转到首页
      next('/')
      return
    }
  }

  next()
})

export default router
