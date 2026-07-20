<template>
  <n-spin :show="loading">
    <n-card
      v-for="(rule, alias) in data?.aliases ?? {}"
      :key="alias"
      :title="alias"
      style="margin-bottom: 12px"
    >
      <n-space align="center" style="margin-bottom: 12px">
        <n-tag type="info">策略: {{ rule.Strategy }}</n-tag>
        <n-tag>{{ rule.Providers.length }} 个候选</n-tag>
      </n-space>
      <n-data-table :columns="columns" :data="rule.Providers" :bordered="false" :pagination="false" />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NCard, NDataTable, NSpace, NSpin, NTag } from 'naive-ui'
import { api, type RoutingResp } from '../api/client'

const data = ref<RoutingResp | null>(null)
const loading = ref(true)

const columns = [
  { title: 'Provider', key: 'Name' },
  { title: 'Model', key: 'Model' },
  { title: 'Priority', key: 'Priority' },
  { title: 'Weight', key: 'Weight' },
]

onMounted(async () => {
  loading.value = true
  try {
    data.value = await api.routing()
  } finally {
    loading.value = false
  }
})
</script>
