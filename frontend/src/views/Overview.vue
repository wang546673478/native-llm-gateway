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

    <!-- P65: 按 Model 卡片(每张卡显示一个 Model 的用量,不再按 provider 归类) -->
    <n-card title="按 Model 用量 (24h)" style="margin-top: 16px">
      <n-grid :cols="3" :x-gap="16" :y-gap="16">
        <n-gi v-for="row in data?.by_model ?? []" :key="row.model_id">
          <div class="bs-card">
            <div class="bs-label">
              <span class="bs-tag">{{ row.model_id }}</span>
            </div>
            <div class="bs-stats">
              <div class="bs-row">
                <span class="bs-key">请求</span>
                <span class="bs-val">{{ fmtNum(row.total_requests) }}</span>
              </div>
              <div class="bs-row">
                <span class="bs-key">Token</span>
                <span class="bs-val">{{ fmtNum(row.total_tokens) }}</span>
              </div>
              <div class="bs-row big">
                <span class="bs-key">费用</span>
                <span class="bs-val">¥{{ row.total_cost.toFixed(4) }}</span>
              </div>
            </div>
          </div>
        </n-gi>
      </n-grid>
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

// P65: 移除过时的 columns(P48 卡片替代了它,模板从未引用)

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

// P47 helper removed in P48 — billing_source 已不在 dashboard 顶层展示
// billing_source 现在是 key 级别的,可以在 Provider Keys 页面看每把 key 的 tier

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

/* P65: 按 Model 卡片(替代之前按 provider 归类) */
.bs-card {
  border: 1px solid #e0e0e6;
  border-radius: 6px;
  padding: 16px;
  background: #fafafa;
  border-left: 4px solid #2080f0;
}
.bs-label {
  display: flex;
  flex-direction: column;
  margin-bottom: 12px;
}
.bs-tag {
  font-size: 16px;
  font-weight: 600;
  color: #2080f0;
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
