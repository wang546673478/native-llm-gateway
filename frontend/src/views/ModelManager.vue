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
          <br />
          中转站类厂商有多个协议面(不同后缀端点、模型互不相通),用面 tab 切换查看各面的模型。
        </template>
      </n-card>

      <n-card v-for="(rows, vendor) in vendors" :key="vendor" :title="vendor">
        <template #header-extra>
          <n-space>
            <n-button
              v-if="orphansOf(vendor).length > 0"
              size="small"
              @click="onPrune(vendor)"
              :loading="pruningVendor === vendor"
            >
              清理无归属 ({{ orphansOf(vendor).length }})
            </n-button>
            <n-button size="small" type="primary" :loading="syncingVendor === vendor" @click="onSync(vendor)">
              同步
            </n-button>
          </n-space>
        </template>

        <!-- P-model-face: 面 tab — 只在该厂商有归属数据时显示(未同步过的厂商无面信息) -->
        <n-space v-if="faceNamesOf(vendor).length > 0" size="small" style="margin-bottom: 12px">
          <n-tag
            :type="activeFace[vendor] ? 'default' : 'primary'"
            size="small"
            checkable
            :checked="!activeFace[vendor]"
            @click="activeFace[vendor] = ''"
          >
            全部 {{ rows.length }}
          </n-tag>
          <n-tag
            v-for="face in faceNamesOf(vendor)"
            :key="face"
            size="small"
            checkable
            :checked="activeFace[vendor] === face"
            @click="activeFace[vendor] = face"
          >
            {{ shortFace(vendor, face) }} {{ facesOf(vendor)[face].length }}
          </n-tag>
          <n-tag
            v-if="orphansOf(vendor).length > 0"
            type="warning"
            size="small"
            checkable
            :checked="activeFace[vendor] === ORPHAN_FACE"
            @click="activeFace[vendor] = ORPHAN_FACE"
          >
            ⚠ 无归属 {{ orphansOf(vendor).length }}
          </n-tag>
        </n-space>

        <n-data-table
          :columns="columns"
          :data="filteredRows(vendor, rows)"
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
import { useDialog, useMessage } from 'naive-ui'
import { api, type ProviderModelRow } from '../api/client'

const message = useMessage()
const dialog = useDialog()

// ORPHAN_FACE 是「无归属」这个伪面的 tab 值(不会与真实注册面名冲突)
const ORPHAN_FACE = '__orphan__'

const vendors = ref<Record<string, ProviderModelRow[]>>({})
const faces = ref<Record<string, Record<string, string[]>>>({})
// activeFace: vendor → 当前选中的面('' = 全部)
const activeFace = ref<Record<string, string>>({})
const loading = ref(true)
const syncingVendor = ref<string | null>(null)
const syncingAll = ref(false)
const pruningVendor = ref<string | null>(null)

const totalModels = computed(() =>
  Object.values(vendors.value).reduce((acc, rows) => acc + rows.length, 0),
)
const vendorNames = computed(() => Object.keys(vendors.value))

function facesOf(vendor: string): Record<string, string[]> {
  return faces.value[vendor] ?? {}
}

function faceNamesOf(vendor: string): string[] {
  return Object.keys(facesOf(vendor)).sort()
}

// shortFace 去掉 vendor 前缀显示(rightapi-codex → codex),tab 排更紧凑
function shortFace(vendor: string, face: string): string {
  return face.startsWith(`${vendor}-`) ? face.slice(vendor.length + 1) : face
}

// facesForModel 该模型归属的面(去 vendor 前缀);空 = 无归属
function facesForModel(vendor: string, modelID: string): string[] {
  const out: string[] = []
  for (const [face, models] of Object.entries(facesOf(vendor))) {
    if (models.includes(modelID)) out.push(shortFace(vendor, face))
  }
  return out.sort()
}

// orphansOf 该厂商无归属的模型(上游下架 / 换 channel 残留)。
// 该厂商完全没有归属数据时返回空 —— 那是 fallback 模式(未同步过 / 所有面都无
// 模型列表端点),此时所有模型都隐式可用,不能当成「全部无归属」误导用户去清理。
function orphansOf(vendor: string): ProviderModelRow[] {
  if (faceNamesOf(vendor).length === 0) return []
  const rows = vendors.value[vendor] ?? []
  return rows.filter((r) => facesForModel(vendor, r.model_id).length === 0)
}

function filteredRows(vendor: string, rows: ProviderModelRow[]): ProviderModelRow[] {
  const face = activeFace.value[vendor]
  if (!face) return rows
  if (face === ORPHAN_FACE) return orphansOf(vendor)
  const allowed = facesOf(vendor)[face] ?? []
  return rows.filter((r) => allowed.includes(r.model_id))
}

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
    faces.value = r.faces ?? {}
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

// onPrune 清理无归属模型 —— 会连带删掉这些模型的手工价格,所以二次确认里列出模型名
function onPrune(vendor: string) {
  const orphans = orphansOf(vendor)
  if (orphans.length === 0) return
  dialog.warning({
    title: `清理 ${vendor} 的无归属模型`,
    content: `以下 ${orphans.length} 个模型已不在该厂商任何协议面的上游清单里(通常是上游下架或换了 channel),将连同其手工价格一并删除:\n\n${orphans.map((r) => r.model_id).join('\n')}`,
    style: 'white-space: pre-line',
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      pruningVendor.value = vendor
      try {
        const r = await api.models.prune(vendor)
        message.success(`已清理 ${vendor}: 删除 ${r.deleted} 个无归属模型`)
        activeFace.value[vendor] = ''
        await load()
      } catch (e) {
        console.error('prune failed', e)
        message.error(`清理失败: ${vendor}`)
      } finally {
        pruningVendor.value = null
      }
    },
  })
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
  {
    // P-model-face: 该模型由哪个协议面提供 —— 中转站厂商一眼看出 gpt-* 走 codex 端点、
    // claude-* 走 claude 端点。无归属 = 上游已下架(可用「清理无归属」删除)。
    // 该厂商整体无归属数据时显示 — (fallback 模式:所有面共享全量清单)
    title: '面',
    key: 'face',
    render: (row: ProviderModelRow) => {
      if (faceNamesOf(row.vendor).length === 0) return '—'
      const fs = facesForModel(row.vendor, row.model_id)
      if (fs.length === 0) {
        return h(NTag, { type: 'warning', size: 'small' }, () => '⚠ 无归属')
      }
      return h(
        NSpace,
        { size: 4 },
        () => fs.map((f) => h(NTag, { size: 'small', type: 'info' }, () => f)),
      )
    },
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
