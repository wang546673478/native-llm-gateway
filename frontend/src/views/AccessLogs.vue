<template>
  <n-spin :show="loading">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">
          Access Logs (24h)
          <n-tag style="margin-left: 8px" type="info">总 {{ stats.total_24h }}</n-tag>
          <n-tag style="margin-left: 4px" type="error">错 {{ stats.errors_24h }}</n-tag>
          <n-tag style="margin-left: 4px">{{ stats.active_keys }} 活跃 key</n-tag>
        </n-h3>
        <n-button @click="load">刷新</n-button>
      </n-space>

      <n-space style="margin-bottom: 12px" :wrap="false">
        <n-input
          v-model:value="filterTraceId"
          placeholder="Trace ID"
          clearable
          style="width: 220px"
        />
        <n-select
          v-model:value="filterKey"
          :options="keyOptions"
          placeholder="Gateway Key"
          clearable
          style="width: 180px"
          @update:value="resetAndLoad"
        />
        <n-select
          v-model:value="filterStatus"
          :options="statusOptions"
          placeholder="状态"
          multiple
          clearable
          :max-tag-count="1"
          style="width: 240px"
          @update:value="resetAndLoad"
        />
        <n-button type="primary" @click="resetAndLoad">查询</n-button>
      </n-space>

      <n-data-table
        :columns="columns"
        :data="records"
        :remote="true"
        :pagination="pagination"
        :bordered="false"
        :row-props="rowProps"
        @update:page="onPageChange"
        @update:page-size="onPageSizeChange"
      />
    </n-card>

    <n-drawer v-model:show="drawerVisible" :width="700" placement="right">
      <n-drawer-content :title="`Trace ${detail?.metadata.trace_id ?? ''}`" closable>
        <div v-if="detail">
          <n-descriptions :column="1" bordered size="small" style="margin-bottom: 16px">
            <n-descriptions-item label="时间">{{ detail.metadata.created_at }}</n-descriptions-item>
            <n-descriptions-item label="状态">
              <n-tag :type="statusTagType(detail.metadata.status_code)">
                {{ detail.metadata.status_code }} {{ detail.metadata.error_type }}
              </n-tag>
            </n-descriptions-item>
            <n-descriptions-item label="Gateway Key">
              {{ detail.metadata.gateway_key_name || '—' }}
            </n-descriptions-item>
            <n-descriptions-item label="Provider">
              {{ detail.metadata.provider_name || '—' }}
            </n-descriptions-item>
            <n-descriptions-item label="Model">
              {{ detail.metadata.requested_model }} → {{ detail.metadata.final_model }}
            </n-descriptions-item>
            <n-descriptions-item label="延迟">
              {{ detail.metadata.latency_ms }} ms
            </n-descriptions-item>
            <n-descriptions-item label="Client IP">
              {{ detail.metadata.client_ip }}
            </n-descriptions-item>
          </n-descriptions>

          <n-space align="center">
            <n-h4>请求体 ({{ formatSize(detail.metadata.req_body_size) }})</n-h4>
            <n-tag v-if="detail.req_body_trunc" type="warning" size="small">已截断</n-tag>
          </n-space>
          <n-card embedded>
            <n-input
              type="textarea"
              :value="detail.req_body || '— 不可用 —'"
              :autosize="{ minRows: 4, maxRows: 16 }"
              readonly
            />
            <n-button size="tiny" style="margin-top: 4px" @click="copy(detail.req_body)">
              复制
            </n-button>
          </n-card>

          <n-space align="center">
            <n-h4>响应体 ({{ formatSize(detail.metadata.resp_body_size) }})</n-h4>
            <n-tag v-if="detail.resp_body_trunc" type="warning" size="small">已截断</n-tag>
          </n-space>
          <n-card embedded>
            <n-input
              type="textarea"
              :value="detail.resp_body || '— 不可用 —'"
              :autosize="{ minRows: 4, maxRows: 16 }"
              readonly
            />
            <n-button size="tiny" style="margin-top: 4px" @click="copy(detail.resp_body)">
              复制
            </n-button>
          </n-card>
        </div>
      </n-drawer-content>
    </n-drawer>
  </n-spin>
</template>

<script setup lang="ts">
import { h, onMounted, reactive, ref } from 'vue'
import {
  NButton,
  NCard,
  NDataTable,
  NDescriptions,
  NDescriptionsItem,
  NDrawer,
  NDrawerContent,
  NH3,
  NH4,
  NInput,
  NSelect,
  NSpace,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns, DataTableCreateRowProps } from 'naive-ui'
import { api, type AccessLog, type AccessLogDetailResp } from '../api/client'

const message = useMessage()
const records = ref<AccessLog[]>([])
const stats = ref({ total_24h: 0, errors_24h: 0, active_keys: 0 })
const loading = ref(false)
const pagination = reactive({
  page: 1,
  pageSize: 20,
  itemCount: 0,
  showSizePicker: true,
  pageSizes: [20, 50, 100, 200] as number[],
})

const filterTraceId = ref('')
const filterKey = ref<string | null>(null)
const filterStatus = ref<string[]>([])
const keyOptions = ref<{ label: string; value: string }[]>([])

const statusOptions = [
  { label: '成功 (2xx/3xx)', value: 'ok' },
  { label: '4xx', value: '4xx' },
  { label: '5xx', value: '5xx' },
  { label: '认证失败', value: 'auth_failed' },
  { label: '无路由', value: 'no_route' },
  { label: '模型未授权', value: 'model_not_allowed' },
  { label: 'Key/Provider 不匹配', value: 'key_provider_mismatch' },
  { label: '上游 4xx', value: 'upstream_4xx' },
  { label: '上游 429', value: 'upstream_429' },
  { label: '上游 5xx', value: 'upstream_5xx' },
  { label: '连接错误', value: 'connection_error' },
  { label: '超时', value: 'timeout' },
  { label: '未知错误', value: 'unknown' },
]

const drawerVisible = ref(false)
const detail = ref<AccessLogDetailResp | null>(null)

function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

function statusTagType(code: number): 'error' | 'warning' | 'success' {
  if (code >= 500) return 'error'
  if (code >= 400) return 'warning'
  return 'success'
}

const columns: DataTableColumns<AccessLog> = [
  {
    title: '时间',
    key: 'created_at',
    width: 180,
    render: row => row.created_at.substring(11, 19),
  },
  {
    title: '状态',
    key: 'status_code',
    width: 90,
    render: row =>
      h(NTag, { type: statusTagType(row.status_code), size: 'small' }, () => `${row.status_code}`),
  },
  { title: 'Key', key: 'gateway_key_name', width: 120 },
  {
    title: 'Model',
    key: 'requested_model',
    width: 180,
    render: row => {
      if (row.requested_model === row.final_model) return row.requested_model
      return h('span', {}, [
        h('span', { style: 'color: #999' }, row.requested_model),
        h('span', { style: 'margin: 0 4px' }, '→'),
        h('span', { style: 'color: #2080f0' }, row.final_model),
      ])
    },
  },
  { title: 'Provider', key: 'provider_name', width: 120 },
  { title: '延迟', key: 'latency_ms', width: 70 },
  { title: 'Trace', key: 'trace_id', render: row => row.trace_id.substring(0, 8) },
]

async function load() {
  loading.value = true
  try {
    const params: Record<string, string | number> = {
      limit: pagination.pageSize,
      offset: (pagination.page - 1) * pagination.pageSize,
    }
    if (filterTraceId.value) params.trace_id = filterTraceId.value
    if (filterKey.value) params.gateway_key = filterKey.value
    if (filterStatus.value.length > 0) params.status = filterStatus.value.join(',')

    const [listResp, statsResp] = await Promise.all([
      api.accessLogs.list(params),
      api.accessLogs.stats(),
    ])
    records.value = listResp.records
    pagination.itemCount = listResp.total
    stats.value = statsResp
  } catch (error: unknown) {
    message.error(`加载失败: ${errorMessage(error)}`)
  } finally {
    loading.value = false
  }
}

async function loadKeyOptions() {
  const response = await api.keys.list().catch(() => ({ keys: [], count: 0 }))
  keyOptions.value = response.keys.map(key => ({ label: key.name, value: key.name }))
}

function onPageChange(page: number) {
  pagination.page = page
  load()
}

function onPageSizeChange(pageSize: number) {
  pagination.pageSize = pageSize
  pagination.page = 1
  load()
}

function resetAndLoad() {
  pagination.page = 1
  load()
}

async function openDetail(row: AccessLog) {
  try {
    detail.value = await api.accessLogs.detail(row.id)
    drawerVisible.value = true
  } catch (error: unknown) {
    message.error(`加载详情失败: ${errorMessage(error)}`)
  }
}

const rowProps: DataTableCreateRowProps<AccessLog> = row => ({
  style: 'cursor: pointer',
  onClick: () => openDetail(row),
})

async function copy(text: string) {
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    message.success('已复制')
  } catch {
    message.error('复制失败')
  }
}

function errorMessage(error: unknown) {
  const requestError = error as {
    message?: string
    response?: { data?: { error?: string } }
  }
  return requestError.response?.data?.error ?? requestError.message ?? String(error)
}

onMounted(() => {
  load()
  loadKeyOptions()
})
</script>
