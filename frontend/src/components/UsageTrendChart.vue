<template>
  <n-card>
    <template #header>
      <n-space align="center" :size="8">
        <span>请求趋势</span>
        <n-tag size="small" :bordered="false" type="info">{{ bucketLabel }}</n-tag>
      </n-space>
    </template>
    <template #header-extra>
      <n-text depth="3" class="caliber">{{ caliber }}</n-text>
    </template>

    <n-skeleton v-if="loading" height="300px" :sharp="false" />
    <n-empty
      v-else-if="buckets.length === 0"
      description="所选时间窗内没有请求记录"
      style="padding: 72px 0"
    />
    <v-chart v-else class="chart" :option="option" autoresize />
  </n-card>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NEmpty, NSkeleton, NSpace, NTag, NText } from 'naive-ui'
import VChart from 'vue-echarts'
import { use } from 'echarts/core'
import { BarChart, LineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { cssToken } from '../utils/status'
import { useTheme } from '../composables/useTheme'

// 按需注册(echarts tree-shaking 入口)—— 不 import 整个 'echarts'
use([BarChart, LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

// 只声明本组件真正读的字段,不复用后端完整 UsageRecord 类型,
// 免得图表被无关字段的变动波及。
interface TrendRecord {
  created_at?: string
  total_tokens?: number
  status_code?: number
  error_type?: string
}

const props = withDefaults(
  defineProps<{ records: TrendRecord[]; loading?: boolean; sampleLimit?: number }>(),
  { loading: false, sampleLimit: 0 },
)

const { isDark } = useTheme()

const HOUR_MS = 3600_000
const DAY_MS = 86_400_000

interface Bucket {
  t: number
  requests: number
  errors: number
  tokens: number
}

// 有效时间戳集合(解析失败的记录直接丢,不当成 epoch 0 污染跨度)
const stamps = computed<number[]>(() =>
  props.records
    .map(r => (r.created_at ? new Date(r.created_at).getTime() : NaN))
    .filter(t => Number.isFinite(t)),
)

const spanMs = computed(() =>
  stamps.value.length === 0 ? 0 : Math.max(...stamps.value) - Math.min(...stamps.value),
)

// 分桶步长自适应:跨度 ≤48h 按小时,否则按天。
// 为什么要自适应:样本是「最近 N 条」而不是固定时间窗,低流量网关这 N 条可能
// 横跨几个月 —— 固定按小时会产生上千个空桶,图形完全不可读。
const step = computed(() => (spanMs.value > 48 * HOUR_MS ? DAY_MS : HOUR_MS))
const bucketLabel = computed(() => (step.value === DAY_MS ? '按天' : '按小时'))

// isError 与后端聚合口径**必须一致**:usage/repository.go 的
//   status_code >= 400 OR error_type != ''
// 否则图上的错误数和「按 Model 聚合」表的错误列对不上账。
// 特别注意 error_type:HTTP 200 也可能是失败(流内上游错误,踩坑 #31),
// 只看状态码会漏判。
function isError(r: TrendRecord): boolean {
  return (r.status_code ?? 0) >= 400 || !!r.error_type
}

const buckets = computed<Bucket[]>(() => {
  if (stamps.value.length === 0) return []
  const s = step.value
  const acc = new Map<number, Bucket>()

  for (const r of props.records) {
    const raw = r.created_at ? new Date(r.created_at).getTime() : NaN
    if (!Number.isFinite(raw)) continue
    const t = Math.floor(raw / s) * s
    let b = acc.get(t)
    if (!b) {
      b = { t, requests: 0, errors: 0, tokens: 0 }
      acc.set(t, b)
    }
    b.requests++
    b.tokens += r.total_tokens ?? 0
    if (isError(r)) b.errors++
  }

  // 补空桶:没有请求的时间段必须显示为 0 而不是被跳过,
  // 否则折线会把两个相隔很远的点连起来,看着像"一直有流量"。
  const first = Math.floor(Math.min(...stamps.value) / s) * s
  const last = Math.floor(Math.max(...stamps.value) / s) * s
  const out: Bucket[] = []
  for (let t = first; t <= last; t += s) {
    out.push(acc.get(t) ?? { t, requests: 0, errors: 0, tokens: 0 })
  }
  return out
})

// 口径说明必须写在图上 —— 后端没有时间序列端点,样本就是「最近 N 条请求」,
// 不是完整时间窗的全量。不写清楚会让人把它当成 24h 全量曲线读。
const caliber = computed(() => {
  const n = props.records.length
  if (n === 0) return '无样本'
  const capped = props.sampleLimit > 0 && n >= props.sampleLimit
  return `样本:最近 ${n} 条请求${capped ? '(上限)' : ''}`
})

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

function axisLabel(t: number): string {
  const d = new Date(t)
  const md = `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  return step.value === DAY_MS ? md : `${md} ${pad(d.getHours())}:00`
}

function fmtCompact(n: number): string {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}k`
  return String(n)
}

const option = computed(() => {
  // 读 isDark 是为了让 computed 在主题切换时失效重算 —— token 值存在 DOM 上,
  // 不是响应式的,没有这一行图表会停在旧配色。别当成无用变量删掉。
  void isDark.value

  const cReq = cssToken('--c-info', '#2080f0')
  const cErr = cssToken('--c-error', '#d03050')
  const cTok = cssToken('--c-primary', '#18a058')
  const cText = cssToken('--t-2', '#4b5158')
  const cSubtle = cssToken('--t-3', '#8b9096')
  const cSplit = cssToken('--b-1', '#e5e7eb')
  const cCard = cssToken('--s-card', '#ffffff')

  return {
    backgroundColor: 'transparent',
    animationDuration: 300,
    tooltip: {
      trigger: 'axis',
      backgroundColor: cCard,
      borderColor: cSplit,
      textStyle: { color: cText },
      axisPointer: { type: 'shadow' },
    },
    legend: {
      data: ['请求数', '错误数', '总 Token'],
      textStyle: { color: cText },
      top: 0,
    },
    grid: { left: 8, right: 8, bottom: 8, top: 40, containLabel: true },
    xAxis: {
      type: 'category',
      data: buckets.value.map(b => axisLabel(b.t)),
      axisLine: { lineStyle: { color: cSplit } },
      axisLabel: { color: cSubtle, hideOverlap: true },
      axisTick: { show: false },
    },
    yAxis: [
      {
        type: 'value',
        name: '请求',
        nameTextStyle: { color: cSubtle },
        axisLabel: { color: cSubtle },
        splitLine: { lineStyle: { color: cSplit, type: 'dashed' } },
      },
      {
        type: 'value',
        name: 'Token',
        nameTextStyle: { color: cSubtle },
        axisLabel: { color: cSubtle, formatter: (v: number) => fmtCompact(v) },
        splitLine: { show: false },
      },
    ],
    series: [
      {
        name: '请求数',
        type: 'bar',
        stack: 'count',
        data: buckets.value.map(b => b.requests - b.errors),
        itemStyle: { color: cReq, borderRadius: [0, 0, 0, 0] },
        barMaxWidth: 28,
      },
      {
        // 错误堆在请求上 —— 柱子总高 = 总请求数,红色部分是其中失败的
        name: '错误数',
        type: 'bar',
        stack: 'count',
        data: buckets.value.map(b => b.errors),
        itemStyle: { color: cErr, borderRadius: [3, 3, 0, 0] },
        barMaxWidth: 28,
      },
      {
        name: '总 Token',
        type: 'line',
        yAxisIndex: 1,
        smooth: true,
        showSymbol: false,
        data: buckets.value.map(b => b.tokens),
        lineStyle: { color: cTok, width: 2 },
        itemStyle: { color: cTok },
      },
    ],
  }
})
</script>

<style scoped>
.chart {
  height: 300px;
  width: 100%;
}
.caliber {
  font-size: 12px;
  font-family: var(--font-mono);
}
</style>
