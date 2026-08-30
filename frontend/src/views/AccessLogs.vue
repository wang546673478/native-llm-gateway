<template>
  <n-spin :show="loading && !firstLoad">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">
          Access Logs (24h)
          <n-tag style="margin-left: 8px" type="info">总 {{ stats.total_24h }}</n-tag>
          <n-tag style="margin-left: 4px" type="error">错 {{ stats.errors_24h }}</n-tag>
          <n-tag style="margin-left: 4px">{{ stats.active_keys }} 活跃 key</n-tag>
        </n-h3>
        <n-button @click="exportJsonl">导出 JSONL</n-button>
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
          v-model:value="filterProvider"
          :options="providerOptions"
          placeholder="Provider"
          filterable
          clearable
          style="width: 180px"
          @update:value="resetAndLoad"
        />
        <n-select
          v-model:value="filterModel"
          :options="modelOptions"
          placeholder="Model"
          filterable
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

      <table-skeleton v-if="firstLoad" :rows="8" />
      <n-data-table
        v-else
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
            <n-descriptions-item label="时间">{{ fmtDateTime(detail.metadata.created_at) }}</n-descriptions-item>
            <n-descriptions-item label="状态">
              <n-tag :type="statusTagType(detail.metadata.status_code)">
                {{ detail.metadata.status_code }} {{ detail.metadata.error_type }}
              </n-tag>
            </n-descriptions-item>
            <n-descriptions-item label="Gateway Key">
              {{ detail.metadata.gateway_key_name || detail.metadata.gateway_key_id || '—' }}
              <span v-if="detail.metadata.gateway_key_name && detail.metadata.gateway_key_id" style="color: var(--t-3)">
                (ID: {{ detail.metadata.gateway_key_id }})
              </span>
            </n-descriptions-item>
            <n-descriptions-item label="Provider">
              {{ detail.metadata.provider_name || '—' }}
            </n-descriptions-item>
            <n-descriptions-item label="Provider Key">
              {{ detail.metadata.provider_key_name || detail.metadata.provider_key_id || '—' }}
              <span v-if="detail.metadata.provider_key_name && detail.metadata.provider_key_id" style="color: var(--t-3)">
                (ID: {{ detail.metadata.provider_key_id }})
              </span>
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

          <!-- P-training: 可读 / 原始切换 -->
          <n-space align="center" style="margin-bottom: 8px">
            <n-radio-group v-model:value="viewMode" size="small">
              <n-radio-button value="readable">人类可读</n-radio-button>
              <n-radio-button value="raw">原始 JSON</n-radio-button>
            </n-radio-group>
          </n-space>

          <template v-if="viewMode === 'readable'">
            <n-space align="center">
              <n-h4>请求 ({{ formatSize(detail.metadata.req_body_size) }})</n-h4>
              <n-tag v-if="detail.req_body_trunc" type="warning" size="small">已截断</n-tag>
            </n-space>
            <n-card embedded size="small" style="margin-bottom: 16px">
              <template v-if="reqParsed">
                <n-space style="margin-bottom: 8px">
                  <n-tag type="info" size="small">model: {{ reqParsed.model ?? '—' }}</n-tag>
                  <n-tag v-if="reqParsed.stream" size="small">stream</n-tag>
                  <n-tag v-if="reqParsed.max_tokens" size="small">max_tokens: {{ reqParsed.max_tokens }}</n-tag>
                </n-space>
                <div v-if="reqMessages.length === 0" class="msg-content dim">
                  (消息列表为空或格式未识别 — 切原始 JSON 查看)
                </div>
                <div v-for="(m, i) in reqMessages" :key="i" class="msg-block">
                  <div class="msg-role" :class="`role-${m.role ?? 'unknown'}`">
                    {{ roleLabel(m.role) }}
                  </div>
                  <div class="msg-content">{{ msgText(m) }}</div>
                </div>
              </template>
              <n-text v-else depth="3">{{ detail.req_body || '— 不可用 —' }}</n-text>
            </n-card>

            <n-space align="center">
              <n-h4>响应 ({{ formatSize(detail.metadata.resp_body_size) }})</n-h4>
              <n-tag v-if="detail.resp_body_trunc" type="warning" size="small">已截断</n-tag>
            </n-space>
            <n-card embedded size="small">
              <template v-if="respParsed">
                <div v-if="respText" class="msg-content" style="white-space: pre-wrap">
                  {{ respText }}
                </div>
                <div v-if="respUsage" class="resp-usage">{{ respUsage }}</div>
                <n-text v-if="!respText && !respUsage" depth="3" style="font-size: 12px">
                  (响应无可读内容 — 切原始 JSON 查看)
                </n-text>
              </template>
              <n-text v-else depth="3">{{ detail.resp_body || '— 不可用 —' }}</n-text>
            </n-card>
          </template>

          <template v-else>
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
          </template>
        </div>
      </n-drawer-content>
    </n-drawer>
  </n-spin>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { usePagination } from '../composables/usePagination'
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
  NRadioButton,
  NRadioGroup,
  NSelect,
  NSpace,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns, DataTableCreateRowProps } from 'naive-ui'
import { api, type AccessLog, type AccessLogDetailResp } from '../api/client'
import { useProvidersStore } from '../stores/providers'
import { copyText } from '../utils/clipboard'
import { fmtDateTime, fmtTime } from '../utils/time'
import { STATUS_PALETTE } from '../utils/status'
import { useFirstLoad } from '../composables/useFirstLoad'
import TableSkeleton from '../components/TableSkeleton.vue'

const message = useMessage()
const records = ref<AccessLog[]>([])
const stats = ref({ total_24h: 0, errors_24h: 0, active_keys: 0 })
const loading = ref(false)
const { firstLoad } = useFirstLoad(loading)
// 分页状态 + handlers 收敛到共享 usePagination
const { pagination, onPageChange, onPageSizeChange } = usePagination(load)

function resetPage() {
  pagination.value.page = 1
}

const filterTraceId = ref('')
const filterKey = ref<string | null>(null)
const filterProvider = ref<string | null>(null)
const filterModel = ref<string | null>(null)
const filterStatus = ref<string[]>([])
const keyOptions = ref<{ label: string; value: string }[]>([])
const providerOptions = ref<{ label: string; value: string }[]>([])
const modelOptions = ref<{ label: string; value: string }[]>([])

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
  { label: '请求无效', value: 'invalid_request' },
  { label: '连接错误', value: 'connection_error' },
  { label: '超时', value: 'timeout' },
  { label: '流中断', value: 'stream_interrupted' },
  { label: '客户端断开', value: 'client_disconnected' },
  { label: '流内上游错误', value: 'upstream_stream_error' },
  { label: '未知错误', value: 'unknown' },
]

const drawerVisible = ref(false)
const detail = ref<AccessLogDetailResp | null>(null)
// P-training: 详情视图模式 — 人类可读(默认)/ 原始 JSON
const viewMode = ref<'readable' | 'raw'>('readable')

// ---- 人类可读渲染:请求/响应 body 解析 ----
interface MsgLike {
  role?: string
  content?: string | Array<Record<string, any>>
}

function parseBody(s: string | undefined): Record<string, any> | null {
  if (!s) return null
  try {
    return JSON.parse(s)
  } catch {
    return null
  }
}

const reqParsed = computed(() => parseBody(detail.value?.req_body))
const respParsed = computed(() => parseBody(detail.value?.resp_body))

// 请求消息列表(OpenAI / Anthropic 都是 messages 数组)
const reqMessages = computed<MsgLike[]>(() => {
  const msgs = reqParsed.value?.messages
  return Array.isArray(msgs) ? (msgs as MsgLike[]) : []
})

const roleLabel = (role: string | undefined): string => {
  switch (role) {
    case 'user': return '用户'
    case 'assistant': return '助手'
    case 'system': return '系统'
    case 'tool': return '工具'
    default: return role ?? '—'
  }
}

// 消息内容块 → 文本(content 可能是字符串或块数组:thinking/tool_use/tool_result)
function msgText(m: MsgLike): string {
  const c = m.content
  if (typeof c === 'string') return c
  if (Array.isArray(c)) {
    return c.map(b => {
      if (b.type === 'text') return b.text ?? ''
      if (b.type === 'thinking') return `[思考] ${b.thinking ?? ''}`
      if (b.type === 'tool_use') return `[工具调用] ${b.name}(${JSON.stringify(b.input ?? {})})`
      if (b.type === 'tool_result') return `[工具结果] ${JSON.stringify(b.content ?? '')}`
      return JSON.stringify(b)
    }).filter(Boolean).join('\n')
  }
  return c ? JSON.stringify(c) : ''
}

// 响应文本(Anthropic content[] / OpenAI choices[0].message / Gemini parts)
const respText = computed<string>(() => {
  const r = respParsed.value
  if (!r) return ''
  if (Array.isArray(r.content)) {
    return r.content.map((b: any) => {
      if (b.type === 'text') return b.text ?? ''
      if (b.type === 'thinking') return `[思考] ${b.thinking ?? ''}`
      return JSON.stringify(b)
    }).filter(Boolean).join('\n')
  }
  const msg = r.choices?.[0]?.message
  if (msg) return typeof msg.content === 'string' ? msg.content : JSON.stringify(msg.content ?? '')
  const parts = r.candidates?.[0]?.content?.parts
  if (parts) return parts.map((p: any) => p.text ?? '').filter(Boolean).join('\n')
  if (r.base_resp) return `[上游错误 ${r.base_resp.status_code}] ${r.base_resp.status_msg ?? ''}`
  return ''
})

// 响应 usage(Anthropic / OpenAI 两种结构)
const respUsage = computed<string>(() => {
  const r = respParsed.value
  if (!r) return ''
  const u = r.usage
  if (!u) return ''
  const parts: string[] = []
  if (u.input_tokens !== undefined) parts.push(`输入 ${u.input_tokens}`)
  if (u.output_tokens !== undefined) parts.push(`输出 ${u.output_tokens}`)
  if (u.prompt_tokens !== undefined) parts.push(`输入 ${u.prompt_tokens}`)
  if (u.completion_tokens !== undefined) parts.push(`输出 ${u.completion_tokens}`)
  if (u.cache_read_input_tokens !== undefined) parts.push(`缓存读 ${u.cache_read_input_tokens}`)
  if (u.cache_creation_input_tokens !== undefined) parts.push(`缓存写 ${u.cache_creation_input_tokens}`)
  return parts.join(' · ')
})

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
    // 后端返回 UTC(RFC3339),substring 直接抠字符串会显示 UTC 墙钟(差 8h)— 转本地
    render: row => fmtTime(row.created_at),
  },
  {
    title: '状态',
    key: 'status_code',
    width: 90,
    render: row =>
      h(NTag, { type: statusTagType(row.status_code), size: 'small' }, () => `${row.status_code}`),
  },
  {
    // P-gateway-key-name: 名字不落库(查询时按 ID 现查 gateway_keys)—
    // 名为主、ID 副标;key 被删则名字为空,回退显示 ID(与 Provider Key 同策略)
    title: 'Key',
    key: 'gateway_key_id',
    width: 120,
    render: row =>
      row.gateway_key_name
        ? h('span', { title: `ID: ${row.gateway_key_id}` }, row.gateway_key_name)
        : row.gateway_key_id || '—',
  },
  {
    // P-catch-all: 客户端请求名只是标签,实际使用模型(路由后)单独一列
    title: '客户端请求模型',
    key: 'requested_model',
    width: 150,
    render: row => row.requested_model,
  },
  {
    title: '实际使用模型',
    key: 'final_model',
    width: 150,
    render: row => {
      if (row.requested_model === row.final_model) {
        return h('span', { class: 'mono' }, row.final_model)
      }
      // 名字被路由改写(假名 → 真实模型)时蓝色标注
      return h('span', { class: 'mono', style: { color: STATUS_PALETTE.blue.color } }, row.final_model)
    },
  },
  { title: 'Provider', key: 'provider_name', width: 120 },
  {
    // P-key: 实际发请求的上游 key,显示名为主、ID 副标(名字可重复,ID 唯一)
    title: 'Provider Key',
    key: 'provider_key_id',
    width: 120,
    render: row =>
      row.provider_key_name
        ? h('span', { title: `ID: ${row.provider_key_id}` }, row.provider_key_name)
        : row.provider_key_id || '—',
  },
  { title: '延迟', key: 'latency_ms', width: 70 },
  {
    title: 'Trace',
    key: 'trace_id',
    render: row => h('span', { class: 'mono', title: row.trace_id }, row.trace_id.substring(0, 8)),
  },
]

// buildParams P-training: 当前过滤条件 → 查询参数(列表/导出共用)
function buildParams(extra: Record<string, string | number> = {}): Record<string, string | number> {
  const params: Record<string, string | number> = { ...extra }
  if (filterTraceId.value) params.trace_id = filterTraceId.value
  if (filterKey.value) params.gateway_key = filterKey.value
  if (filterProvider.value) params.provider = filterProvider.value
  if (filterModel.value) params.model = filterModel.value
  if (filterStatus.value.length > 0) params.status = filterStatus.value.join(',')
  return params
}

// exportJsonl P-training: 用当前过滤条件导出 JSONL 训练数据(新标签页下载)
// URL 由 client.ts 的 api.accessLogs.exportUrl 提供(单一 endpoint 源,防路径漂移)
function exportJsonl() {
  const params: Record<string, string | number> = {}
  for (const [k, v] of Object.entries(buildParams({ limit: 10000 }))) {
    params[k] = String(v)
  }
  const url = api.accessLogs.exportUrl(params)
  // url 不含 /api/v1 前缀,export 端点不在 axios client(需新标签页,直接拼前缀)
  window.open(`/api/v1${url}`, '_blank')
}

async function load() {
  loading.value = true
  try {
    const params = buildParams({
      limit: pagination.value.pageSize,
      offset: (pagination.value.page - 1) * pagination.value.pageSize,
    })

    const [listResp, statsResp] = await Promise.all([
      api.accessLogs.list(params),
      api.accessLogs.stats(),
    ])
    records.value = listResp.records
    pagination.value.itemCount = listResp.total
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

// 加载 provider / model 过滤选项。
// 复用 /providers/registered 接口,它返回 name + protocol + models,
// 省掉额外去 /providers 拉 key_pool/circuit 信息的开销。
async function loadProviderModelOptions() {
  const provStore = useProvidersStore()
  try {
    // P-provider-vendor: Provider 过滤按厂商(store.vendors 已共享 fetch 一次),
    // 日志 provider_name 已归一为厂商名;模型列表仍用 /providers/registered 并集
    const [, regResp] = await Promise.all([
      provStore.load(),
      api.providersRegistered().catch(() => ({ providers: [], count: 0 })),
    ])
    providerOptions.value = provStore.vendorOptions
    // dedupe models across providers
    const seen = new Set<string>()
    const models: { label: string; value: string }[] = []
    for (const p of regResp.providers ?? []) {
      for (const m of p.models ?? []) {
        if (!seen.has(m)) {
          seen.add(m)
          models.push({ label: m, value: m })
        }
      }
    }
    modelOptions.value = models
  } catch {
    providerOptions.value = []
    modelOptions.value = []
  }
}

function resetAndLoad() {
  resetPage()
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
  const ok = await copyText(text)
  if (ok) message.success('已复制')
  else message.error('复制失败')
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
  loadProviderModelOptions()
})
</script>

<style scoped>
/* P-training: 详情抽屉人类可读渲染 */
.msg-block {
  border: 1px solid var(--b-1);
  border-radius: var(--r-sm);
  padding: 6px 10px;
  margin-bottom: 6px;
  background: var(--s-sunken);
}
.msg-role {
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 4px;
}
.role-user {
  color: var(--c-info);
}
.role-assistant {
  color: var(--c-primary);
}
.role-system {
  color: var(--t-3);
}
.role-tool {
  color: var(--c-warning);
}
.msg-content {
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 300px;
  overflow-y: auto;
}
.msg-content.dim {
  color: var(--t-3);
  font-size: 12px;
}
.resp-usage {
  margin-top: 10px;
  padding-top: 8px;
  border-top: 1px dashed var(--b-dash);
  font-size: 12px;
  color: var(--t-3);
}
</style>
