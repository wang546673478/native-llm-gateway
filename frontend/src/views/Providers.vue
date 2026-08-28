<template>
  <n-spin :show="loading && !firstLoad">
    <table-skeleton v-if="firstLoad" />
    <n-data-table
      v-else
      :columns="columns"
      :data="providers"
      :bordered="false"
      :pagination="false"
    />
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import { NDataTable, NSpin, NTag } from 'naive-ui'
import type { VendorInfo } from '../api/client'
import { useProvidersStore } from '../stores/providers'
import { useFirstLoad } from '../composables/useFirstLoad'
import TableSkeleton from '../components/TableSkeleton.vue'

// P-provider-vendor: 一行 = 一个厂商(vendor),names 列出该厂商的全部注册名(协议面)
// 数据源 = 共享 providers store(全字段 VendorInfo,含 key_pool/circuit_breaker)
const providers = ref<VendorInfo[]>([])
const loading = ref(true)
const { firstLoad } = useFirstLoad(loading)
const store = useProvidersStore()

const columns = [
  { title: 'Name', key: 'vendor' },
  {
    title: 'Protocol',
    key: 'protocol',
    render: (row: VendorInfo) =>
      row.names.map(n =>
        h(NTag, { type: 'info', size: 'small', style: { marginRight: '4px' } }, () => n.protocol),
      ),
  },
  {
    title: 'Models',
    key: 'models',
    render: (row: VendorInfo) => row.models.join(', '),
  },
  {
    title: 'Key Pool',
    key: 'key_pool',
    render: (row: VendorInfo) => {
      const kp = row.key_pool
      if (!kp) return '—'
      return `${kp.active_keys}/${kp.total_keys} active, ${kp.cooling_keys} cooling, ${kp.disabled_keys} disabled`
    },
  },
  {
    title: 'Circuit Breaker',
    key: 'circuit_breaker',
    render: (row: VendorInfo) => {
      const cb = row.circuit_breaker
      if (!cb) return '—'
      const type = cb.state === 'CLOSED' ? 'success' : cb.state === 'OPEN' ? 'error' : 'warning'
      return h(NTag, { type, size: 'small' }, () => `${cb.state} (${cb.failures_in_window} fails)`)
    },
  },
]

onMounted(async () => {
  loading.value = true
  try {
    await store.load() // 共享 fetch + 缓存;vendor 清单一次
    // P-relay-independent: 厂商列表只显示真厂商,不含中转站(中转站在「中转站管理」页)
    providers.value = store.vendors.filter(v => !v.is_relay)
  } finally {
    loading.value = false
  }
})
</script>
