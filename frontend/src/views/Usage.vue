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
        :remote="true"
        :pagination="pagination"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
      />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, ref } from 'vue'
import { usePagination } from '../composables/usePagination'
import { NButton, NCard, NDataTable, NInput, NSpace, NSpin, NText, NTag } from 'naive-ui'
import { api, type AggregateRow, type ModelProviderRow } from '../api/client'
import { fmtDateTime } from '../utils/time'
import { fmtNum } from '../utils/status'

const rows = ref<AggregateRow[]>([])
const records = ref<any[]>([])
const loading = ref(true)

// P65: provider 分布缓存(model_id → providers 列表)
const providerMap = ref<Record<string, ModelProviderRow[]>>({})
const loadingProvider = ref<Record<string, boolean>>({})

const start = ref('')
const end = ref('')

// P66: 最近请求后端分页状态
// 分页状态 + handlers 收敛到共享 usePagination(消除此前内联 reactive + 两份
// onPageChange/onPageSizeChange;魔数 20/[20,50,100,200] 单源)。
const { pagination, onPageChange, onPageSizeChange } = usePagination(load)

// query 是「重新查询」(用户改时间窗) — 重置 page=1
async function query() {
  pagination.value.page = 1
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
  {
    title: '平均延迟(ms)',
    key: 'avg_latency_ms',
    render: (row: AggregateRow) => Number(row.avg_latency_ms ?? 0).toFixed(2),
  },
]

const recordColumns = [
  { title: '时间', key: 'created_at', render: (row: any) => fmtDateTime(row.created_at) },
  { title: 'Provider', key: 'provider_name' },
  { title: 'Model', key: 'model_id' },
  { title: 'Protocol', key: 'protocol' },
  { title: '状态', key: 'status_code' },
  { title: '延迟(ms)', key: 'latency_ms' },
  // P-token-split: 缓存输入 = total - input - output(MiniMax 语义 total 另计缓存,
  // 精确;DeepSeek 命中已在 input 内 → 缓存列 0)。DB 未存 cache 拆分,这是精确可得的分解
  {
    title: '缓存输入',
    key: 'cache_input_tokens',
    render: (row: any) => fmtNum(Math.max(0, row.total_tokens - row.input_tokens - row.output_tokens)),
  },
  { title: '未缓存输入', key: 'input_tokens', render: (row: any) => fmtNum(row.input_tokens) },
  { title: '输出', key: 'output_tokens', render: (row: any) => fmtNum(row.output_tokens) },
  { title: 'Trace', key: 'trace_id' },
]

async function load() {
  loading.value = true
  try {
    const params: any = {
      // P66: 后端分页 — 带 limit/offset
      limit: pagination.value.pageSize,
      offset: (pagination.value.page - 1) * pagination.value.pageSize,
    }
    if (start.value) params.start = start.value
    if (end.value) params.end = end.value
    const [agg, rec] = await Promise.all([
      api.aggregateUsage(params),
      api.usage(params),
    ])
    rows.value = agg.rows
    records.value = rec.records
    pagination.value.itemCount = rec.total // P66: 总数驱动分页器
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
