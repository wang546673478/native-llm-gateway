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
          <div class="big-num">¥{{ (data?.total?.total_cost ?? 0).toFixed(4) }}</div>
        </n-card>
      </n-gi>
    </n-grid>

    <!-- P47: 按计费来源分组 -->
    <n-card title="按计费来源 (24h)" style="margin-top: 16px">
      <n-grid :cols="3" :x-gap="16" :y-gap="16">
        <n-gi v-for="bs in data?.by_billing_source ?? []" :key="bs.billing_source">
          <div class="bs-card" :class="`bs-${bs.billing_source}`">
            <div class="bs-label">
              <span class="bs-tag">{{ bsLabel(bs.billing_source) }}</span>
              <span class="bs-desc">{{ bsDesc(bs.billing_source) }}</span>
            </div>
            <div class="bs-stats">
              <div class="bs-row">
                <span class="bs-key">请求</span>
                <span class="bs-val">{{ fmtNum(bs.total_requests) }}</span>
              </div>
              <div class="bs-row">
                <span class="bs-key">Token</span>
                <span class="bs-val">{{ fmtNum(bs.total_tokens) }}</span>
              </div>
              <div class="bs-row big">
                <span class="bs-key">费用</span>
                <span class="bs-val">¥{{ bs.total_cost.toFixed(4) }}</span>
              </div>
            </div>
          </div>
        </n-gi>
      </n-grid>
    </n-card>

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

// P47: 计费来源标签和说明
function bsLabel(s: string): string {
  switch (s) {
    case 'token_plan': return '📦 Token Plan'
    case 'api':        return '💰 按量计费'
    case 'free':       return '🎁 免费层'
    default:           return s
  }
}
function bsDesc(s: string): string {
  switch (s) {
    case 'token_plan': return '包月套餐,quota 优先用'
    case 'api':        return '按 token 收费(deepseek/openai 等)'
    case 'free':       return '永久免费(GLM-4-flash 等)'
    default:           return ''
  }
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

/* P47: 计费来源卡片 */
.bs-card {
  border: 1px solid #e0e0e6;
  border-radius: 6px;
  padding: 16px;
  background: #fafafa;
}
.bs-token_plan { border-left: 4px solid #2080f0; }
.bs-api        { border-left: 4px solid #f0a020; }
.bs-free       { border-left: 4px solid #18a058; }
.bs-label {
  display: flex;
  flex-direction: column;
  margin-bottom: 12px;
}
.bs-tag {
  font-size: 16px;
  font-weight: 600;
}
.bs-desc {
  font-size: 12px;
  color: #888;
  margin-top: 2px;
}
.bs-stats {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.bs-row {
  display: flex;
  justify-content: space-between;
  font-size: 13px;
}
.bs-row .bs-key { color: #888; }
.bs-row .bs-val { font-weight: 500; }
.bs-row.big {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px dashed #ddd;
  font-size: 14px;
}
.bs-row.big .bs-val {
  font-weight: 600;
  color: #18a058;
  font-size: 16px;
}
</style>
