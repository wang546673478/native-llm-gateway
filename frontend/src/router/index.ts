import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('../views/Overview.vue') },
    { path: '/providers', component: () => import('../views/Providers.vue') },
    { path: '/provider-keys', component: () => import('../views/ProviderKeys.vue') },
    { path: '/keys', component: () => import('../views/Keys.vue') },
    { path: '/routing', component: () => import('../views/Routing.vue') },
    { path: '/usage', component: () => import('../views/Usage.vue') },
    { path: '/access-logs', name: 'access-logs', component: () => import('../views/AccessLogs.vue') },
  ],
})

export default router
