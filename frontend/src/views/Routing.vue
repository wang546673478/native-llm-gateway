<template>
  <n-spin :show="loading">
    <!-- P-catch-all: 兜底路由卡片 — 任何 alias 表外的模型名都按它路由 -->
    <n-card
      v-if="data?.catch_all"
      title="*(任意未知模型)"
      style="margin-bottom: 12px"
    >
      <n-space align="center" style="margin-bottom: 12px">
        <n-tag type="warning">兜底路由 catch_all</n-tag>
        <n-tag type="info">策略: {{ data.catch_all.Strategy }}</n-tag>
        <n-tag>{{ data.catch_all.Providers.length }} 个候选</n-tag>
        <n-text depth="3" style="font-size: 12px">
          客户端发任何 alias 表外且无 provider 声明的 model 名时按此路由,
          仍按 token_plan → api → free 计费
        </n-text>
      </n-space>
      <n-data-table :columns="columns" :data="data.catch_all.Providers" :bordered="false" :pagination="false" />
    </n-card>

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
import { NCard, NDataTable, NSpace, NSpin, NTag, NText } from 'naive-ui'
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
