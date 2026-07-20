<template>
  <n-spin :show="loading">
    <n-grid :cols="4" :x-gap="16" :y-gap="16">
      <n-gi>
        <n-card title="总请求数(24h)">
          <div class="big-num">{{ fmtNum(data?.total?.total_requests) }}</div>
        </n-card>
      </n-gi>
      <n-gi>
        <n-card title="总 Token 数">
          <div class="big-num">{{ fmtNum(data?.total?.total_tokens) }}</div>
        </n-card>
      </n-gi>
      <n-gi>
        <n-card title="错误数">
          <div class="big-num" :class="{ danger: (data?.total?.error_count ?? 0) > 0 }">
            {{ fmtNum(data?.total?.error_count) }}
          </div>
        </n-card>
      </n-gi>
      <n-gi>
        <n-card title="总费用">
          <div class="big-num">${{ (data?.total?.total_cost ?? 0).toFixed(4) }}</div>
        </n-card>
      </n-gi>
    </n-grid>

    <n-card title="Provider / Model 用量明细" style="margin-top: 16px">
      <n-data-table
        :columns="columns"
        :data="data?.by_provider_model ?? []"
        :bordered="false"
        :pagination="false"
      />
    </n-card>

    <n-card title="Key Pool 状态" style="margin-top: 16px">
      <n-data-table
        :columns="poolColumns"
        :data="data?.keypools ?? []"
        :bordered="false"
        :pagination="false"
      />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import {
  NCard, NDataTable, NGi, NGrid, NSpin,
} from 'naive-ui'
import { api, type DashboardResp } from '../api/client'

const data = ref<DashboardResp | null>(null)
const loading = ref(true)
let timer: number | undefined

const columns = [
  { title: 'Provider', key: 'provider_name' },
  { title: 'Model', key: 'model_id' },
  { title: '请求数', key: 'total_requests' },
  { title: 'Input', key: 'total_input_tokens' },
  { title: 'Output', key: 'total_output_tokens' },
  { title: '总 Token', key: 'total_tokens' },
  { title: '错误数', key: 'error_count' },
  {
    title: '平均延迟',
    key: 'avg_latency_ms',
    render: (row: any) => `${(row.avg_latency_ms ?? 0).toFixed(0)} ms`,
  },
]

const poolColumns = [
  { title: 'Provider', key: 'provider_name' },
  { title: '总数', key: 'total_keys' },
  { title: 'Active', key: 'active_keys' },
  { title: 'Cooling', key: 'cooling_keys' },
  { title: 'Disabled', key: 'disabled_keys' },
]

function fmtNum(n: number | undefined): string {
  if (n === undefined || n === null) return '—'
  return n.toLocaleString()
}

async function load() {
  loading.value = true
  try {
    data.value = await api.dashboard()
  } catch (e) {
    console.error('dashboard load failed', e)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  timer = window.setInterval(load, 15_000)
})
onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<style scoped>
.big-num {
  font-size: 28px;
  font-weight: 600;
  color: #18a058;
}
.danger { color: #d03050; }
</style>
