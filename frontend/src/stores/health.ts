// Pinia store:健康检查
import { defineStore } from 'pinia'
import axios from 'axios'

export const useHealthStore = defineStore('health', {
  state: () => ({
    ok: false,
    lastCheck: '',
  }),
  actions: {
    async check() {
      try {
        await axios.get('/healthz', { timeout: 3000 })
        this.ok = true
      } catch {
        this.ok = false
      }
      this.lastCheck = new Date().toISOString()
    },
  },
})
