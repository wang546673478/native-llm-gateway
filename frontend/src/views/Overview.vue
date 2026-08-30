<template>
  <div>
    <!-- 首次加载用骨架屏(不是全页 spin 遮罩):切页面不白闪。
         后续 15s 轮询静默刷新,不再骨架化已有内容。 -->
    <n-grid v-if="firstLoad" cols="1 s:2 l:4" responsive="screen" :x-gap="16" :y-gap="16">
      <n-gi v-for="i in 4" :key="i">
        <n-skeleton height="112px" :sharp="false" />
      </n-gi>
    </n-grid>
    <n-grid v-else cols="1 s:2 l:4" responsive="screen" :x-gap="16" :y-gap="16">
      <n-gi>
        <stat-card
          icon="📊"
          label="总请求数(24h)"
          tone="primary"
          :value="fmtNum(data?.total?.total_requests)"
        />
      </n-gi>
      <n-gi>
        <stat-card
          icon="🎯"
          label="总 Token 数"
          tone="info"
          :value="fmtNum(data?.total?.total_tokens)"
        />
      </n-gi>
      <n-gi>
        <stat-card
          icon="⚠"
          label="错误数"
          :tone="(data?.total?.error_count ?? 0) > 0 ? 'error' : 'primary'"
          :value="fmtNum(data?.total?.error_count)"
        />
      </n-gi>
      <n-gi>
        <stat-card
          icon="💰"
          label="总费用"
          tone="warning"
          :value="`¥${(data?.total?.total_cost ?? 0).toFixed(4)}`"
        />
      </n-gi>
    </n-grid>

    <!-- P65: 按 Model 卡片(每张卡显示一个 Model 的用量,不再按 provider 归类) -->
    <n-card title="按 Model 用量 (24h)" style="margin-top: 16px">
      <n-grid cols="1 s:2 l:3" responsive="screen" :x-gap="16" :y-gap="16">
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

    <!-- 复用 keypools 数据，展示每池已 poll key 的 Remaining 之和
         （整池可用额度粗略值，CNY，保留两位小数）。 -->
    <n-card title="整池可用额度 (QuotaKnownSum)" style="margin-top: 16px">
      <n-grid cols="1 s:2 l:3" responsive="screen" :x-gap="16" :y-gap="16">
        <n-gi v-for="row in data?.keypools ?? []" :key="row.provider_name">
          <div class="bs-card">
            <div class="bs-label">
              <span class="bs-tag">{{ row.provider_name }}</span>
              <span class="bs-desc">
                {{ row.quota_polled_keys }} / {{ row.total_keys }} keys polled
              </span>
            </div>
            <div class="bs-stats">
              <div class="bs-row big">
                <span class="bs-key">可用额度</span>
                <span class="bs-val">
                  {{ quotaDisplay(row.quota_kind, row.quota_known_sum) }}
                </span>
              </div>
            </div>
          </div>
        </n-gi>
      </n-grid>
    </n-card>

    <n-card title="设备指纹归一化" style="margin-top: 16px">
      <div class="fp-row">
        <div class="fp-desc">
          多台机器经网关共用一把上游 key 时，把 device_id / 平台 / shell / 系统版本
          归一成网关固定值，抹平「多头设备」信号，降低封号风险。仅改无副作用的纯指纹，
          不碰工作目录与对话内容。
        </div>
        <n-switch
          :value="fpEnabled"
          :loading="fpLoading"
          @update:value="toggleFingerprint"
        />
      </div>
      <div v-if="fpCanonical" class="fp-canonical">
        统一 device_id：<code class="mono">{{ fpCanonical }}</code>
      </div>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import {
  NCard, NDataTable, NGi, NGrid, NSkeleton, NSwitch,
} from 'naive-ui'
import { api, type DashboardResp } from '../api/client'
import { fmtNum, quotaDisplay } from '../utils/status'
import StatCard from '../components/StatCard.vue'

const data = ref<DashboardResp | null>(null)
// firstLoad 只在「还没拿到任何数据」时为 true —— 15s 轮询刷新不该把已有内容
// 换成骨架(会周期性闪烁)。这与旧的 loading 全页遮罩语义不同,是有意的。
const firstLoad = ref(true)
let timer: number | undefined

// P-fingerprint: 设备指纹归一化开关状态
const fpEnabled = ref(false)
const fpCanonical = ref('')
const fpLoading = ref(false)

async function toggleFingerprint(v: boolean) {
  fpLoading.value = true
  try {
    await api.fingerprint.set(v)
    fpEnabled.value = v
  } catch (e) {
    console.error('fingerprint toggle failed', e)
  } finally {
    fpLoading.value = false
  }
}

async function loadFingerprint() {
  try {
    const f = await api.fingerprint.get()
    fpEnabled.value = f.enabled
    fpCanonical.value = f.canonical_device_id
  } catch (e) {
    console.error('fingerprint load failed', e)
  }
}

// P65: 移除过时的 columns(P48 卡片替代了它,模板从未引用)

const poolColumns = [
  { title: 'Provider', key: 'provider_name' },
  { title: '总数', key: 'total_keys' },
  { title: 'Active', key: 'active_keys' },
  { title: 'Cooling', key: 'cooling_keys' },
  { title: 'Disabled', key: 'disabled_keys' },
]

// P47 helper removed in P48 — billing_source 已不在 dashboard 顶层展示
// billing_source 现在是 key 级别的,可以在 Provider Keys 页面看每把 key 的 tier

async function load() {
  try {
    data.value = await api.dashboard()
  } catch (e) {
    console.error('dashboard load failed', e)
  } finally {
    firstLoad.value = false
  }
}

onMounted(() => {
  load()
  loadFingerprint()
  timer = window.setInterval(load, 15_000)
})
onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<style scoped>
/* 色值全部走 tokens.css 变量 —— 暗色模式自动跟随,改主色只改 token 一处 */

/* P65: 按 Model 卡片(替代之前按 provider 归类) */
.bs-card {
  border: 1px solid var(--b-dash);
  border-radius: var(--r-md);
  padding: var(--sp-4);
  background: var(--s-sunken);
  border-left: 3px solid var(--c-info);
  transition: box-shadow var(--tr-fast), transform var(--tr-fast);
}
.bs-card:hover {
  box-shadow: var(--sh-2);
  transform: translateY(-1px);
}
.bs-label {
  display: flex;
  flex-direction: column;
  margin-bottom: var(--sp-3);
}
.bs-tag {
  font-family: var(--font-mono);
  font-size: 15px;
  font-weight: 600;
  color: var(--c-info);
  word-break: break-all;
}
.bs-desc {
  font-size: 12px;
  color: var(--t-3);
  margin-top: 2px;
}
.bs-stats {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}
.bs-row {
  display: flex;
  justify-content: space-between;
  font-size: 13px;
}
.bs-row .bs-key { color: var(--t-3); }
.bs-row .bs-val {
  font-weight: 500;
  color: var(--t-1);
  font-variant-numeric: tabular-nums;
}
.bs-row.big {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px dashed var(--b-dash);
  font-size: 14px;
}
.bs-row.big .bs-val {
  font-weight: 650;
  color: var(--c-primary);
  font-size: 16px;
}

/* P-fingerprint: 设备指纹归一化开关 */
.fp-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-4);
}
.fp-desc {
  font-size: 13px;
  color: var(--t-3);
  line-height: 1.65;
}
.fp-canonical {
  margin-top: 10px;
  font-size: 12px;
  color: var(--t-2);
}
.fp-canonical code {
  background: var(--s-chip);
  color: var(--t-1);
  padding: 2px 6px;
  border-radius: var(--r-sm);
}
</style>
