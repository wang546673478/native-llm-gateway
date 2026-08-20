<template>
  <n-spin :show="loading">
    <n-space vertical size="large">
      <n-card title="模型管理">
        <template #header-extra>
          <n-space>
            <n-button size="small" type="primary" :loading="syncingAll" @click="onSyncAll">
              全部同步
            </n-button>
            <n-tag :type="totalModels > 0 ? 'success' : 'default'" size="small">
              {{ totalModels }} 个模型
            </n-tag>
          </n-space>
        </template>
        <template #default>
          每个厂商的上游模型与三档每百万 token 定价(单位:元/百万 token)。价格可直接编辑,blur 时保存;「同步」从上游拉取该厂商最新模型清单。
        </template>
      </n-card>

      <n-card v-for="(rows, vendor) in vendors" :key="vendor" :title="vendor">
        <template #header-extra>
          <n-button size="small" type="primary" :loading="syncingVendor === vendor" @click="onSync(vendor)">
            同步
          </n-button>
        </template>
        <n-data-table
          :columns="columns"
          :data="rows"
          :bordered="false"
          :pagination="false"
        />
      </n-card>

      <n-empty v-if="!loading && vendorNames.length === 0" description="暂无模型数据" />
    </n-space>
  </n-spin>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import {
  NButton,
  NCard,
  NDataTable,
  NEmpty,
  NInputNumber,
  NSpace,
  NSpin,
  NTag,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { useMessage } from 'naive-ui'
import { api, type ProviderModelRow } from '../api/client'

const message = useMessage()

const vendors = ref<Record<string, ProviderModelRow[]>>({})
const loading = ref(true)
const syncingVendor = ref<string | null>(null)
const syncingAll = ref(false)

const totalModels = computed(() =>
  Object.values(vendors.value).reduce((acc, rows) => acc + rows.length, 0),
)
const vendorNames = computed(() => Object.keys(vendors.value))

function isUnpriced(r: ProviderModelRow): boolean {
  return (
    r.cost_per_million_input === 0 &&
    r.cost_per_million_cache_read === 0 &&
    r.cost_per_million_output === 0
  )
}

async function load() {
  try {
    const r = await api.models.list()
    vendors.value = r.vendors
  } catch (e) {
    console.error('models load failed', e)
  } finally {
    loading.value = false
  }
}

async function onSync(vendor: string) {
  syncingVendor.value = vendor
  try {
    const r = await api.models.sync(vendor)
    message.success(`同步完成: ${vendor} 新增 ${r.synced_models} 个模型`)
    await load()
  } catch (e) {
    console.error('sync failed', e)
    message.error(`同步失败: ${vendor}`)
  } finally {
    syncingVendor.value = null
  }
}

async function onSyncAll() {
  syncingAll.value = true
  try {
    const r = await api.models.syncAll()
    if (r.failed > 0) {
      const failedVendors = r.results.filter((x) => x.error).map((x) => `${x.vendor}(${x.error})`)
      message.warning(`同步完成: ${r.total} 个厂商, ${r.failed} 个失败 — ${failedVendors.join('; ')}`)
    } else {
      message.success(`同步完成: ${r.total} 个厂商全部成功`)
    }
    await load()
  } catch (e) {
    console.error('sync-all failed', e)
    message.error('全部同步失败')
  } finally {
    syncingAll.value = false
  }
}

async function onSavePrice(row: ProviderModelRow) {
  try {
    await api.models.save({
      vendor: row.vendor,
      model_id: row.model_id,
      cost_per_million_input: row.cost_per_million_input,
      cost_per_million_cache_read: row.cost_per_million_cache_read,
      cost_per_million_output: row.cost_per_million_output,
    })
    message.success(`已保存: ${row.vendor}/${row.model_id}`)
  } catch (e) {
    console.error('save price failed', e)
    message.error(`保存失败: ${row.model_id}`)
  }
}

function priceCell(key: 'cost_per_million_input' | 'cost_per_million_cache_read' | 'cost_per_million_output') {
  return (row: ProviderModelRow) =>
    h(NInputNumber, {
      value: row[key],
      min: 0,
      size: 'small',
      style: 'width: 140px',
      showButton: false,
      onUpdateValue: (v: number | null) => {
        row[key] = v ?? 0
      },
      onBlur: () => onSavePrice(row),
    })
}

const columns: DataTableColumns<ProviderModelRow> = [
  {
    title: '模型 ID',
    key: 'model_id',
    render: (row: ProviderModelRow) =>
      h('span', [
        row.model_id,
        isUnpriced(row)
          ? h(NTag, { type: 'warning', size: 'small', style: 'margin-left: 8px' }, () => '未定价')
          : null,
      ]),
  },
  { title: '输入价', key: 'cost_per_million_input', render: priceCell('cost_per_million_input') },
  { title: '缓存命中价', key: 'cost_per_million_cache_read', render: priceCell('cost_per_million_cache_read') },
  { title: '输出价', key: 'cost_per_million_output', render: priceCell('cost_per_million_output') },
  {
    title: '同步时间',
    key: 'synced_at',
    render: (row: ProviderModelRow) => row.synced_at || '—',
  },
  {
    title: '来源',
    key: 'source',
    render: (row: ProviderModelRow) =>
      h(
        NTag,
        { type: row.source === 'upstream' ? 'info' : 'default', size: 'small' },
        () => (row.source === 'upstream' ? '上游' : '手工'),
      ),
  },
]

onMounted(load)
</script>
