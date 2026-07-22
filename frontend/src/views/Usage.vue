<template>
  <n-spin :show="loading">
    <n-card title="按 Model 聚合(可调时间窗)">
      <n-space style="margin-bottom: 12px">
        <n-text>开始:</n-text>
        <n-input v-model:value="start" placeholder="RFC3339" style="width: 280px" />
        <n-text>结束:</n-text>
        <n-input v-model:value="end" placeholder="RFC3339" style="width: 280px" />
        <n-button type="primary" @click="load">查询</n-button>
      </n-space>
      <n-data-table :columns="columns" :data="rows" :bordered="false" :pagination="false" />
    </n-card>

    <n-card title="最近请求" style="margin-top: 16px">
      <n-data-table
        :columns="recordColumns"
        :data="records"
        :bordered="false"
        :pagination="{ pageSize: 20 }"
      />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import { NButton, NCard, NDataTable, NInput, NSpace, NSpin, NText, NTag } from 'naive-ui'
import { api, type AggregateRow, type ModelProviderRow } from '../api/client'

const rows = ref<AggregateRow[]>([])
const records = ref<any[]>([])
const loading = ref(true)

// P65: provider 分布缓存(model_id → providers 列表)
//  - 后端只按 model 聚合,Provider 列渲染时按需拉 + 缓存
const providerMap = ref<Record<string, ModelProviderRow[]>>({})
const loadingProvider = ref<Record<string, boolean>>({})

const start = ref('')
const end = ref('')

async function fetchProviders(modelId: string) {
  if (providerMap.value[modelId] || loadingProvider.value[modelId]) return
  loadingProvider.value[modelId] = true
  try {
    const params: any = {}
    if (start.value) params.start = start.value
    if (end.value) params.end = end.value
    const r = await api.modelProviders(modelId, params)
    providerMap.value[modelId] = r.providers
  } catch (e) {
    console.error('modelProviders failed', modelId, e)
    providerMap.value[modelId] = []
  } finally {
    loadingProvider.value[modelId] = false
  }
}

const columns = [
  {
    title: 'Provider',
    key: 'providers',
    // P65: 异步渲染 — Provider 标签列表
    render(row: AggregateRow) {
      const list = providerMap.value[row.model_id]
      if (!list) {
        // 首次渲染触发拉取
        fetchProviders(row.model_id)
        return h('span', { style: 'color:#888' }, '加载中…')
      }
      if (list.length === 0) {
        return h('span', { style: 'color:#888' }, '—')
      }
      return h(
        'div',
        { style: 'display:flex;gap:4px;flex-wrap:wrap' },
        list.map(p =>
          h(NTag, { type: 'info', size: 'small', bordered: false }, () =>
            `${p.provider_name} (${p.request_count})`,
          ),
        ),
      )
    },
  },
  { title: 'Model', key: 'model_id' },
  { title: '请求', key: 'total_requests' },
  { title: 'Input', key: 'total_input_tokens' },
  { title: 'Output', key: 'total_output_tokens' },
  { title: '总 Token', key: 'total_tokens' },
  { title: '错误', key: 'error_count' },
  { title: '平均延迟(ms)', key: 'avg_latency_ms' },
]

const recordColumns = [
  { title: '时间', key: 'created_at' },
  { title: 'Provider', key: 'provider_name' },
  { title: 'Model', key: 'model_id' },
  { title: 'Protocol', key: 'protocol' },
  { title: '状态', key: 'status_code' },
  { title: '延迟(ms)', key: 'latency_ms' },
  { title: 'Token', key: 'total_tokens' },
  { title: 'Trace', key: 'trace_id' },
]

async function load() {
  loading.value = true
  // 清空 provider 缓存,时间窗变了就重新拉
  providerMap.value = {}
  try {
    const params: any = { limit: 20 }
    if (start.value) params.start = start.value
    if (end.value) params.end = end.value
    const [agg, rec] = await Promise.all([
      api.aggregateUsage(params),
      api.usage(params),
    ])
    rows.value = agg.rows
    records.value = rec.records
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
