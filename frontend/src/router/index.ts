import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/overview' },
    { path: '/overview', component: () => import('../views/Overview.vue') },
    { path: '/providers', component: () => import('../views/Providers.vue') },
    { path: '/keys', component: () => import('../views/Keys.vue') },
    { path: '/routing', component: () => import('../views/Routing.vue') },
    { path: '/usage', component: () => import('../views/Usage.vue') },
  ],
})

export default router
