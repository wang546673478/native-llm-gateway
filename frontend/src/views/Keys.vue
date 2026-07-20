<template>
  <n-spin :show="loading">
    <n-data-table :columns="columns" :data="keys" :bordered="false" :pagination="false" />
  </n-spin>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NDataTable, NSpin } from 'naive-ui'
import { api, type GatewayKeyInfo } from '../api/client'

const keys = ref<GatewayKeyInfo[]>([])
const loading = ref(true)

const columns = [
  { title: 'Name', key: 'name' },
  {
    title: 'Allowed Models',
    key: 'allowed_models',
    render: (row: GatewayKeyInfo) => row.allowed_models.join(', '),
  },
  { title: 'RPM', key: 'rpm' },
  { title: 'TPM', key: 'tpm' },
]

onMounted(async () => {
  loading.value = true
  try {
    const resp = await api.keys()
    keys.value = resp.keys
  } finally {
    loading.value = false
  }
})
</script>
