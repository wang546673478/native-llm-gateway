<template>
  <n-card title="活跃请求(实时)">
    <template #header-extra>
      <n-tag :type="rows.length > 0 ? 'success' : 'default'" size="small">
        {{ rows.length }} 条
      </n-tag>
    </template>
    <n-data-table :columns="columns" :data="rows" :bordered="false" :pagination="false" />
    <n-empty v-if="rows.length === 0" description="当前无活跃请求" style="margin-top: 24px" />
  </n-card>
</template>

<script setup lang="ts">
import { h, onMounted, onUnmounted, ref } from 'vue'
import { NCard, NDataTable, NEmpty, NTag } from 'naive-ui'
import { api, type InflightRequest } from '../api/client'

const rows = ref<InflightRequest[]>([])
let timer: ReturnType<typeof setInterval> | undefined

function fmtElapsed(ms: number): string {
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return `${m}m${s % 60}s`
}

async function load() {
  try {
    const r = await api.inflight()
    rows.value = r.requests
  } catch (e) {
    console.error('inflight load failed', e)
  }
}

const columns = [
  { title: 'Trace', key: 'trace_id', ellipsis: true },
  { title: 'Model', key: 'model' },
  { title: 'Provider', key: 'provider_name', render: (r: InflightRequest) => r.provider_name || '路由中…' },
  { title: 'Gateway Key', key: 'gateway_key_name', render: (r: InflightRequest) => r.gateway_key_name || '—' },
  {
    title: '流式',
    key: 'is_stream',
    render: (r: InflightRequest) => (r.is_stream ? '是' : '否'),
  },
  {
    title: '已耗时',
    key: 'elapsed_ms',
    render: (r: InflightRequest) => fmtElapsed(r.elapsed_ms),
  },
]

onMounted(() => {
  load()
  timer = setInterval(load, 1000) // 1s 轮询
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>
