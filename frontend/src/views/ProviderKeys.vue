<template>
  <n-spin :show="loading">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">Provider API Keys({{ keys.length }})</n-h3>
        <n-space>
          <n-button type="primary" @click="openCreate">+ 添加 Key</n-button>
          <n-button @click="load">刷新</n-button>
        </n-space>
      </n-space>

      <n-data-table :columns="columns" :data="keys" :bordered="false" :pagination="false" />
    </n-card>

    <n-modal
      v-model:show="modalVisible"
      preset="card"
      :title="editing ? '编辑 Provider Key' : '添加 Provider Key'"
      style="width: 600px"
      :mask-closable="false"
    >
      <n-form ref="formRef" :model="form" :rules="rules" label-placement="top">
        <!-- P-provider-vendor: 两级选择 — 厂商 → 协议(默认全勾 = 全部) -->
        <n-form-item label="厂商" path="vendor">
          <n-select
            v-model:value="form.vendor"
            :options="vendorOptions"
            placeholder="选择厂商"
            :disabled="editing"
            @update:value="() => { form.protocols = protocolOptions.map(o => o.value) }"
          />
        </n-form-item>
        <n-form-item label="协议(不选 = 全部)" path="protocols">
          <n-select v-model:value="form.protocols" multiple :options="protocolOptions" placeholder="默认全勾" />
        </n-form-item>
        <n-form-item label="名称(可空,自动生成)" path="name">
          <n-input v-model:value="form.name" placeholder="如 prod-key-1,留空自动" />
        </n-form-item>
        <n-form-item label="API Key" path="key">
          <n-input
            v-model:value="form.key"
            type="password"
            show-password-on="click"
            :placeholder="editing ? '留空表示不修改' : 'sk-...'"
          />
        </n-form-item>
        <!-- P48: 计费来源 — 决定 Pool.Acquire 优先级 -->
        <n-form-item label="计费来源" path="billing_source">
          <n-select
            v-model:value="form.billing_source"
            :options="billingSourceOptions"
            placeholder="选择计费方式"
          />
        </n-form-item>
        <n-form-item label="启用" path="enabled">
          <n-switch v-model:value="form.enabled" />
        </n-form-item>
      </n-form>

      <template #footer>
        <n-space justify="end">
          <n-button @click="modalVisible = false">取消</n-button>
          <n-button type="primary" :loading="saving" @click="save">
            {{ editing ? '保存' : '创建' }}
          </n-button>
        </n-space>
      </template>
    </n-modal>
  </n-spin>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import {
  NButton, NCard, NDataTable, NForm, NFormItem,
  NInput, NModal, NSpace, NSpin, NSelect, NSwitch,
  NH3, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { api, type ProviderKeyView, type VendorInfo } from '../api/client'
import { useProvidersStore } from '../stores/providers'
import { BILLING_SOURCE, BILLING_SOURCE_DEFAULT, KEY_STATUS, KEY_STATUS_DISABLED_UI, QUOTA_KIND } from '../api/constants'
import { STATUS_PALETTE } from '../utils/status'
import { fmtDateTime } from '../utils/time'

const keys = ref<ProviderKeyView[]>([])
const providers = ref<VendorInfo[]>([])
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
const message = useMessage()
// P-provider-vendor: store 持 vendors 单一来源;load() 内填充 providers
const provStore = useProvidersStore()

const form = ref({
  // P-provider-vendor: 提交目标 = 选中 vendor 的第一个注册名(pool 共享,任意协议面可写)
  // (vendor/protocols 两级选择,provider_name 由 targetProviderName computed 推导,不落表单)
  vendor: '',
  protocols: [] as string[],
  name: '',
  key: '',
  enabled: true,
  // P48: 计费来源 — token_plan / api / free,默认 api(常量来自 constants.ts)
  billing_source: BILLING_SOURCE_DEFAULT,
})

const rules = {
  vendor: { required: true, message: '选择厂商', trigger: 'blur' },
  key: { required: true, message: 'Key 必填', trigger: 'blur' },
}

// P-provider-vendor: 两级下拉 — 厂商 → 协议面
const vendorOptions = computed(() =>
  providers.value.map(v => ({ label: v.vendor, value: v.vendor }))
)
const protocolOptions = computed(() => {
  const v = providers.value.find(p => p.vendor === form.value.vendor)
  if (!v) return []
  // 同一 vendor 可能多注册名共协议(如 mimo 有 openai×2 + anthropic×2 =
  // 两个端点 × 两个协议面)— 去重,下拉只显示协议种类
  return [...new Set(v.names.map(n => n.protocol))].map(p => ({ label: p, value: p }))
})
// 提交目标 provider_name = vendor 的第一个注册名(协议面任意,pool 共享)
const targetProviderName = computed(() => {
  const v = providers.value.find(p => p.vendor === form.value.vendor)
  return v?.names[0]?.name ?? ''
})

const billingSourceOptions = [
  { label: '💰 按量计费 (api)', value: BILLING_SOURCE.API },
  { label: '📦 Token Plan (token_plan)', value: BILLING_SOURCE.TOKEN_PLAN },
  { label: '🎁 免费层 (free)', value: BILLING_SOURCE.FREE },
]

// Task 10: balance column — tier-relative colour tier.
// 后端 default = 10;启动时拉 /api/v1/config/quota 拿当前生效值。
const warnThresholdPct = ref(10)

// Same provider & billing_source → max Remaining across all polled rows
// in the tier.  Polled rows are those with last_polled_at set.
//
// Spec §6.1: 同 provider 同 tier 桶内 Remaining 的最大值 — 必须同时
// 按 provider_name + billing_source 过滤,避免跨 provider token_plan
// keys 共享 tier_max 造成小余额 provider 显示错误的绿色。
function tierMaxFor(rows: ProviderKeyView[], tier: string, providerName: string): number {
  let max = 0
  for (const r of rows) {
    if (r.billing_source === tier && r.provider_name === providerName && r.last_polled_at) {
      if (r.remaining > max) max = r.remaining
    }
  }
  return max
}

function balanceColour(
  row: ProviderKeyView,
  tierMax: number,
  warnPct: number,
): 'green' | 'yellow' | 'red' | 'gray' {
  if (!row.last_polled_at) return 'gray'
  if (row.remaining === 0) return 'red'
  // P-quota-display: percent 行用绝对阈值(百分比本身是绝对值,tier-relative 会双重归一化);
  // currency 行保持 tier-relative 阈值(与既有行为一致)
  const threshold =
    row.quota_kind === QUOTA_KIND.PERCENT ? warnPct : (warnPct / 100) * tierMax
  if (row.remaining >= threshold) return 'green'
  return 'yellow'
}

// Source-of-truth list for tier-relativisation (matches backend pool view).
const allKeys = computed<ProviderKeyView[]>(() => keys.value)

function tierMaxForRow(row: ProviderKeyView): number {
  return tierMaxFor(allKeys.value, row.billing_source || '', row.provider_name || '')
}

const columns: DataTableColumns<ProviderKeyView> = [
  { title: 'ID', key: 'id', width: 60 },
  { title: 'Provider', key: 'provider_name', width: 160 },
  { title: '名称', key: 'name', width: 160 },
  { title: 'Key(脱敏)', key: 'key_masked' },
  {
    title: '计费来源',
    key: 'billing_source',
    width: 160,
    render: (row) => {
      const map: Record<string, { color: string; label: string }> = {
        [BILLING_SOURCE.TOKEN_PLAN]: { color: STATUS_PALETTE.blue.color, label: '📦 token_plan' },
        [BILLING_SOURCE.API]:        { color: STATUS_PALETTE.yellow.color, label: '💰 api' },
        [BILLING_SOURCE.FREE]:       { color: STATUS_PALETTE.green.color, label: '🎁 free' },
      }
      const m = map[row.billing_source] ?? { color: STATUS_PALETTE.gray.color, label: row.billing_source }
      return h('span', { style: { color: m.color, fontWeight: 500 } }, m.label)
    },
  },
  {
    title: '状态',
    key: 'status',
    width: 130,
    render: (row) => {
      // P68: 3 个运行时状态 — ACTIVE / COOLING / QUOTA_EXCEEDED
      // P-no-disabled: 没有 DISABLED 终端状态(全部可自动恢复);
      // DISABLED 显示仅保留给手动关闭(enabled=false)的 key
      // P-per-key-circuit: 熔断状态优先显示(熔断 = 该 key 暂时不参与调度,
      // 30s 后半开放行自动恢复;只影响这一把 key,不影响同 provider 其他 key)
      if (row.circuit_open) {
        return h('span', { style: { color: STATUS_PALETTE.red.color, fontWeight: 500 } }, '⚡ 熔断中')
      }
      const status = (row.status || (row.enabled ? KEY_STATUS.ACTIVE : KEY_STATUS_DISABLED_UI)).toUpperCase()
      const map: Record<string, { color: string; label: string }> = {
        [KEY_STATUS.ACTIVE]:          { color: STATUS_PALETTE.green.color, label: '● 启用' },
        [KEY_STATUS.COOLING]:         { color: STATUS_PALETTE.blue.color, label: '⏱ 冷却中' },
        [KEY_STATUS.QUOTA_EXCEEDED]:  { color: STATUS_PALETTE.yellow.color, label: '⚠ 配额耗尽' },
        [KEY_STATUS_DISABLED_UI]:     { color: STATUS_PALETTE.gray.color, label: '○ 已关闭' },
      }
      const m = map[status] ?? { color: STATUS_PALETTE.gray.color, label: status }
      return h('span', { style: { color: m.color, fontWeight: 500 } }, m.label)
    },
  },
  { title: '创建时间', key: 'created_at', width: 200, render: (row: any) => fmtDateTime(row.created_at) },
  {
    // P-quota-display: 列名"额度";渲染按 quota_kind:
    //   percent → "43%"(取整);currency/空 → "¥12.34"
    // 颜色:green/yellow/red/gray — percent 行按绝对阈值 warnPct;
    // currency 行按 tier_max 相对阈值(tierMaxForRow)
    title: '额度',
    key: 'remaining',
    width: 130,
    render: (row) => {
      if (!row.last_polled_at) {
        return h('span', { style: { color: STATUS_PALETTE.gray.color } }, '未轮询')
      }
      const colour = balanceColour(row, tierMaxForRow(row), warnThresholdPct.value)
      const text =
        row.quota_kind === QUOTA_KIND.PERCENT
          ? `${Math.floor(row.remaining)}%`
          : `¥${row.remaining.toFixed(2)}`
      return h(
        'span',
        { style: { color: STATUS_PALETTE[colour as keyof typeof STATUS_PALETTE]?.color ?? '#999', fontWeight: 500 } },
        text,
      )
    },
  },
  {
    title: '操作',
    key: 'actions',
    width: 120,
    render: (row) =>
      h(NSpace, {}, () => [
        h(
          NButton,
          { size: 'small', type: 'error', onClick: () => confirmDelete(row) },
          () => '删除',
        ),
      ]),
  },
]

async function load() {
  loading.value = true
  try {
    // P-provider-vendor: 厂商聚合列表来自共享 store → 按 vendor.names 展开,
    // 循环拉每个注册名的 api-keys
    // (同 vendor 的多注册名共享同一 key 池,列表天然按 provider_name 相邻)
    const provStore = useProvidersStore()
    await provStore.load()
    providers.value = provStore.vendors
    const allNames = provStore.vendors.flatMap(v => v.names.map(n => n.name))
    const allKeys = await Promise.all(
      allNames.map(async name => {
        try {
          const r = await api.providerKeys.list(name)
          return r.keys || []
        } catch (e) {
          return []
        }
      })
    )
    keys.value = allKeys.flat()
    // P-quota-balance: 拿后端当前生效的 warn_threshold_pct。
    // 失败就保留 ref(10),不影响列表渲染。
    try {
      const q = await api.quotaConfig()
      if (q && typeof q.warn_threshold_pct === 'number' && q.warn_threshold_pct > 0) {
        warnThresholdPct.value = q.warn_threshold_pct
      }
    } catch (e) {
      // ignore — fallback to default
    }
  } catch (e: any) {
    message.error('加载失败: ' + (e.message ?? e))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  form.value = {
    vendor: providers.value[0]?.vendor ?? '',
    protocols: providers.value[0]?.names.map(n => n.protocol) ?? [],
    name: '',
    key: '',
    enabled: true,
    billing_source: BILLING_SOURCE_DEFAULT,
  }
  modalVisible.value = true
}

async function save() {
  if (!form.value.vendor) {
    message.error('选择厂商')
    return
  }
  if (!form.value.key) {
    message.error('Key 必填')
    return
  }
  const target = targetProviderName.value
  if (!target) {
    message.error('厂商无可用注册名')
    return
  }
  saving.value = true
  try {
    // P-provider-vendor: 一把 key 存一条(厂商级一份),protocols 标勾选的协议;
    // 全勾 → 空(全部);勾选子集 → 逗号分隔。pool 共享,另一协议面的请求同样能取到
    const allProtocols = protocolOptions.value.map(o => o.value)
    const isAll =
      form.value.protocols.length === allProtocols.length &&
      allProtocols.every(p => form.value.protocols.includes(p))
    await api.providerKeys.create(target, {
      name: form.value.name,
      key: form.value.key,
      enabled: form.value.enabled,
      billing_source: form.value.billing_source,
      protocols: isAll ? '' : form.value.protocols.join(','),
    })
    message.success('已添加')
    modalVisible.value = false
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    saving.value = false
  }
}

async function confirmDelete(row: ProviderKeyView) {
  if (!confirm(`确认删除 ${row.provider_name} 的 Key "${row.name}" (${row.key_masked})?`)) return
  try {
    await api.providerKeys.delete(row.provider_name, row.id)
    message.success('已删除')
    await load()
  } catch (e: any) {
    message.error('删除失败: ' + (e.response?.data?.error ?? e.message))
  }
}

onMounted(load)
</script>
