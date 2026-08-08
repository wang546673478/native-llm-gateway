// Pinia store:provider/vendor 注册表 —— 消除 5 个 view 各自独立 fetch 同一批数据
//
// 低耦合:Providers.vue / ProviderKeys.vue / Keys.vue / AccessLogs.vue / Routing.vue
// 此前各自 onMounted 调 api.providers() 并把 .vendors remap 成局部结构,backend 改
// VendorInfo 形状会同时破坏 5 个 view。集中 fetch 一次 + 暴露切片,各 view 消费
// 所需子集。
import { defineStore } from 'pinia'
import { api, type ProvidersResponse } from '../api/client'

export const useProvidersStore = defineStore('providers', {
  state: () => ({
    vendors: [] as ProvidersResponse['vendors'],
    loaded: false,
    loading: false,
    error: false,
  }),
  getters: {
    // 注册名 → 厂商映射(Keys.vue 旧数据归一用)
    regToVendor(): Record<string, string> {
      const map: Record<string, string> = {}
      for (const v of this.vendors) {
        for (const n of v.names ?? []) map[n.name] = v.vendor
      }
      return map
    },
    // 按 vendor 聚合的注册名(Providers.vue 表格用)
    vendorNames(): Record<string, string[]> {
      const m: Record<string, string[]> = {}
      for (const v of this.vendors) {
        m[v.vendor] = (v.names ?? []).map(n => n.name)
      }
      return m
    },
  },
  actions: {
    async load() {
      if (this.loading) return
      this.loading = true
      this.error = false
      try {
        const resp = await api.providers()
        this.vendors = resp.vendors ?? []
        this.loaded = true
      } catch {
        this.error = true
      } finally {
        this.loading = false
      }
    },
  },
})
