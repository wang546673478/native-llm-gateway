<template>
  <n-spin :show="loading">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">中转站配置 ({{ stations.length }})</n-h3>
        <n-space>
          <n-button type="primary" @click="openCreate">+ 添加中转站</n-button>
          <n-button @click="load">刷新</n-button>
        </n-space>
      </n-space>

      <n-data-table :columns="columns" :data="stations" :bordered="false" />
    </n-card>

    <n-modal
      v-model:show="modalVisible"
      preset="card"
      :title="editing ? '编辑中转站' : '添加中转站'"
      style="width: 700px"
      :mask-closable="false"
    >
      <n-form ref="formRef" :model="form" :rules="rules" label-placement="top">
        <n-form-item label="名称(英文标识)" path="name">
          <n-input v-model:value="form.name" placeholder="如 tokenmarket" :disabled="editing" />
        </n-form-item>
        <n-form-item label="显示名称" path="display_name">
          <n-input v-model:value="form.display_name" placeholder="如 TokenMarket" />
        </n-form-item>
        <n-form-item label="Base URL" path="base_url">
          <n-input v-model:value="form.base_url" placeholder="https://api.example.com" />
        </n-form-item>
        <n-form-item label="协议模式" path="protocol_mode">
          <n-select
            v-model:value="form.protocol_mode"
            :options="protocolModeOptions"
            @update:value="onProtocolModeChange"
          />
        </n-form-item>
        <n-form-item label="主协议" path="primary_protocol">
          <n-select v-model:value="form.primary_protocol" :options="protocolOptions" />
        </n-form-item>
        <n-form-item v-if="form.protocol_mode === 'multi'" label="支持的协议" path="supported_protocols_array">
          <n-select
            v-model:value="form.supported_protocols_array"
            multiple
            :options="protocolOptions"
            placeholder="选择支持的协议"
          />
        </n-form-item>
        <n-form-item label="超时(秒)" path="timeout_seconds">
          <n-input-number v-model:value="form.timeout_seconds" :min="1" :max="300" style="width: 100%" />
        </n-form-item>
        <n-form-item label="计费来源" path="billing_source">
          <n-select v-model:value="form.billing_source" :options="billingSourceOptions" />
        </n-form-item>
        <n-form-item label="API Keys (每行一个)" path="keys_text">
          <n-input
            v-model:value="form.keys_text"
            type="textarea"
            placeholder="sk-xxx&#10;sk-yyy"
            :rows="4"
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
import { h, onMounted, ref } from 'vue'
import {
  NButton, NCard, NDataTable, NForm, NFormItem, NInput, NInputNumber,
  NModal, NSpace, NSpin, NSelect, NSwitch, NH3, NTag, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import {
  listRelayStations, createRelayStation, updateRelayStation, deleteRelayStation,
  type RelayStation,
} from '../api/client'
import { BILLING_SOURCE } from '../api/constants'
import { fmtDateTime } from '../utils/time'

const stations = ref<RelayStation[]>([])
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
const message = useMessage()

const form = ref({
  id: 0,
  name: '',
  display_name: '',
  base_url: '',
  protocol_mode: 'single' as 'single' | 'multi',
  primary_protocol: 'openai',
  supported_protocols_array: [] as string[],
  timeout_seconds: 60,
  billing_source: 'api',
  keys_text: '',
  enabled: true,
})

const protocolModeOptions = [
  { label: '单协议', value: 'single' },
  { label: '多协议', value: 'multi' },
]

const protocolOptions = [
  { label: 'OpenAI', value: 'openai' },
  { label: 'Anthropic', value: 'anthropic' },
  { label: 'Google', value: 'google' },
]

const billingSourceOptions = Object.entries(BILLING_SOURCE).map(([k, v]) => ({
  label: v,
  value: k,
}))

const rules = {
  name: { required: true, message: '请输入名称', trigger: 'blur' },
  display_name: { required: true, message: '请输入显示名称', trigger: 'blur' },
  base_url: { required: true, message: '请输入 Base URL', trigger: 'blur' },
  protocol_mode: { required: true, message: '请选择协议模式' },
  primary_protocol: { required: true, message: '请选择主协议' },
  timeout_seconds: { required: true, type: 'number' as const, message: '请输入超时时间' },
  billing_source: { required: true, message: '请选择计费来源' },
}

const columns: DataTableColumns<RelayStation> = [
  { title: '名称', key: 'name', width: 120 },
  { title: '显示名称', key: 'display_name', width: 120 },
  { title: 'Base URL', key: 'base_url', ellipsis: { tooltip: true } },
  {
    title: '协议模式',
    key: 'protocol_mode',
    width: 100,
    render: (row) => row.protocol_mode === 'single' ? '单协议' : '多协议',
  },
  { title: '主协议', key: 'primary_protocol', width: 100 },
  {
    title: '支持协议',
    key: 'supported_protocols',
    width: 150,
    render: (row) => {
      if (row.protocol_mode === 'single') return row.primary_protocol
      try {
        const protocols = JSON.parse(row.supported_protocols || '[]')
        return protocols.join(', ')
      } catch {
        return row.primary_protocol
      }
    },
  },
  {
    title: 'API Keys',
    key: 'keys',
    width: 100,
    render: (row) => {
      if (!row.keys) return '0 个'
      try {
        const keys = JSON.parse(row.keys)
        const count = Array.isArray(keys) ? keys.length : 0
        return `${count} 个`
      } catch {
        return '0 个'
      }
    },
  },
  {
    title: '计费来源',
    key: 'billing_source',
    width: 100,
    render: (row) => BILLING_SOURCE[row.billing_source as keyof typeof BILLING_SOURCE] || row.billing_source,
  },
  {
    title: '状态',
    key: 'enabled',
    width: 80,
    render: (row) =>
      h(NTag, { type: row.enabled ? 'success' : 'default', size: 'small' }, () =>
        row.enabled ? '启用' : '禁用'
      ),
  },
  {
    title: '创建时间',
    key: 'created_at',
    width: 160,
    render: (row) => (row.created_at ? fmtDateTime(row.created_at) : '-'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    render: (row) =>
      h(
        NSpace,
        {},
        {
          default: () => [
            h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑'),
            h(
              NButton,
              {
                size: 'small',
                type: 'error',
                onClick: () => confirmDelete(row),
              },
              () => '删除'
            ),
          ],
        }
      ),
  },
]

function onProtocolModeChange(mode: 'single' | 'multi') {
  if (mode === 'single') {
    form.value.supported_protocols_array = []
  } else {
    form.value.supported_protocols_array = [form.value.primary_protocol]
  }
}

async function load() {
  loading.value = true
  try {
    const resp = await listRelayStations()
    stations.value = resp.relay_stations || []
  } catch (e: any) {
    message.error('加载失败: ' + (e.message || String(e)))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  form.value = {
    id: 0,
    name: '',
    display_name: '',
    base_url: '',
    protocol_mode: 'single',
    primary_protocol: 'openai',
    supported_protocols_array: [],
    timeout_seconds: 60,
    billing_source: 'api',
    keys_text: '',
    enabled: true,
  }
  editing.value = false
  modalVisible.value = true
}

function openEdit(row: RelayStation) {
  let protocols: string[] = []
  if (row.protocol_mode === 'multi' && row.supported_protocols) {
    try {
      protocols = JSON.parse(row.supported_protocols)
    } catch {
      protocols = []
    }
  }

  let keysText = ''
  if (row.keys) {
    try {
      const keys = JSON.parse(row.keys)
      keysText = Array.isArray(keys) ? keys.join('\n') : ''
    } catch {
      keysText = ''
    }
  }

  form.value = {
    id: row.id || 0,
    name: row.name,
    display_name: row.display_name,
    base_url: row.base_url,
    protocol_mode: row.protocol_mode,
    primary_protocol: row.primary_protocol,
    supported_protocols_array: protocols,
    timeout_seconds: row.timeout_seconds,
    billing_source: row.billing_source,
    keys_text: keysText,
    enabled: row.enabled,
  }
  editing.value = true
  modalVisible.value = true
}

async function save() {
  const keysArray = form.value.keys_text
    .split('\n')
    .map(k => k.trim())
    .filter(k => k.length > 0)

  const payload: Partial<RelayStation> = {
    name: form.value.name,
    display_name: form.value.display_name,
    base_url: form.value.base_url,
    protocol_mode: form.value.protocol_mode,
    primary_protocol: form.value.primary_protocol,
    supported_protocols:
      form.value.protocol_mode === 'multi'
        ? JSON.stringify(form.value.supported_protocols_array)
        : '',
    timeout_seconds: form.value.timeout_seconds,
    billing_source: form.value.billing_source,
    keys: JSON.stringify(keysArray),
    enabled: form.value.enabled,
  }

  saving.value = true
  try {
    if (editing.value) {
      await updateRelayStation(form.value.id, payload)
      message.success('更新成功')
    } else {
      await createRelayStation(payload)
      message.success('创建成功')
    }
    modalVisible.value = false
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error || e.message || String(e)))
  } finally {
    saving.value = false
  }
}

function confirmDelete(row: RelayStation) {
  if (!row.id) return
  if (!confirm(`确定删除中转站 "${row.display_name || row.name}" 吗？`)) return
  doDelete(row.id)
}

async function doDelete(id: number) {
  try {
    await deleteRelayStation(id)
    message.success('删除成功')
    await load()
  } catch (e: any) {
    message.error('删除失败: ' + (e.response?.data?.error || e.message || String(e)))
  }
}

onMounted(() => {
  load()
})
</script>
