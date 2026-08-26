// Pinia store: 管理员认证状态
import { defineStore } from 'pinia'

export interface AdminUser {
  username: string
  role: string
}

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('admin_token') || null as string | null,
    user: null as AdminUser | null,
    loading: false,
  }),
  getters: {
    isAuthenticated(): boolean {
      return !!this.token
    },
    isRoot(): boolean {
      return this.user?.role === 'root'
    },
  },
  actions: {
    setToken(token: string) {
      this.token = token
      localStorage.setItem('admin_token', token)
    },
    clearToken() {
      this.token = null
      this.user = null
      localStorage.removeItem('admin_token')
    },
    setUser(user: AdminUser) {
      this.user = user
    },
  },
})
