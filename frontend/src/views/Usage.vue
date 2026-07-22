<template>
  <n-spin :show="loading">
    <n-card title="按 Model 聚合(可调时间窗)">
      <n-space style="margin-bottom: 12px">
        <n-text>开始:</n-text>
        <n-input v-model:value="start" placeholder="RFC3339" style="width: 280px" />
        <n-text>结束:</n-text>
        <n-input v-model:value="end" placeholder="RFC3339" style="width: 280px" />
        <n-button type="primary" @click="query">查询</n-button>
      </n-space>
      <n-data-table :columns="columns" :data="rows" :bordered="false" :pagination="false" />
    </n-card>

    <n-card title="最近请求" style="margin-top: 16px">
      <n-data-table
        :columns="recordColumns"
        :data="records"
        :bordered="false"
        :pagination="pagination"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
      />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, reactive, ref } from 'vue'
import { NButton, NCard, NDataTable, NInput, NSpace, NSpin, NText, NTag } from 'naive-ui'
import { api, type AggregateRow, type ModelProviderRow } from '../api/client'

const rows = ref<AggregateRow[]>([])
const records = ref<any[]>([])
const loading = ref(true)

// P65: provider 分布缓存(model_id → providers 列表)
const providerMap = ref<Record<string, ModelProviderRow[]>>({})
const loadingProvider = ref<Record<string, boolean>>({})

const start = ref('')
const end = ref('')

// P66: 最近请求后端分页状态
// 用 reactive 包一个对象,这样 n-data-table 通过 :pagination 拿到的
// 是响应式对象,内部 page/pageSize/itemCount 变化会触发分页器重渲
const pagination = reactive({
  page: 1,
  pageSize: 20,
  itemCount: 0,
  showSizePicker: true,
  pageSizes: [20, 50, 100, 200] as number[],
})

function onPageChange(page: number) {
  pagination.page = page
  load()
}
function onPageSizeChange(pageSize: number) {
  pagination.pageSize = pageSize
  pagination.page = 1
  load()
}

// query 是「重新查询」(用户改时间窗) — 重置 page=1
async function query() {
  pagination.page = 1
  providerMap.value = {} // P65: 时间窗变了 provider 缓存也清
  await load()
}

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
    render(row: AggregateRow) {
      const list = providerMap.value[row.model_id]
      if (!list) {
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
  try {
    const params: any = {
      // P66: 后端分页 — 带 limit/offset
      limit: pagination.pageSize,
      offset: (pagination.page - 1) * pagination.pageSize,
    }
    if (start.value) params.start = start.value
    if (end.value) params.end = end.value
    const [agg, rec] = await Promise.all([
      api.aggregateUsage(params),
      api.usage(params),
    ])
    rows.value = agg.rows
    records.value = rec.records
    pagination.itemCount = rec.total // P66: 总数驱动分页器
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
