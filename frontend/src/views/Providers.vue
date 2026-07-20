<template>
  <n-spin :show="loading">
    <n-data-table
      :columns="columns"
      :data="providers"
      :bordered="false"
      :pagination="false"
    />
  </n-spin>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NDataTable, NSpin, NTag } from 'naive-ui'
import { api, type ProviderInfo } from '../api/client'

const providers = ref<ProviderInfo[]>([])
const loading = ref(true)

const columns = [
  { title: 'Name', key: 'name' },
  {
    title: 'Protocol',
    key: 'protocol',
    render: (row: ProviderInfo) => h(NTag, { type: 'info', size: 'small' }, () => row.protocol),
  },
  {
    title: 'Models',
    key: 'models',
    render: (row: ProviderInfo) => row.models.join(', '),
  },
  {
    title: 'Key Pool',
    key: 'key_pool',
    render: (row: ProviderInfo) => {
      const kp = row.key_pool
      if (!kp) return '—'
      return `${kp.active_keys}/${kp.total_keys} active, ${kp.cooling_keys} cooling, ${kp.disabled_keys} disabled`
    },
  },
  {
    title: 'Circuit Breaker',
    key: 'circuit_breaker',
    render: (row: ProviderInfo) => {
      const cb = row.circuit_breaker
      if (!cb) return '—'
      const type = cb.state === 'CLOSED' ? 'success' : cb.state === 'OPEN' ? 'error' : 'warning'
      return h(NTag, { type, size: 'small' }, () => `${cb.state} (${cb.failures_in_window} fails)`)
    },
  },
]

import { h } from 'vue'

onMounted(async () => {
  loading.value = true
  try {
    const resp = await api.providers()
    providers.value = resp.providers
  } finally {
    loading.value = false
  }
})
</script>
