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
        <n-form-item label="Provider" path="provider_name">
          <n-select
            v-model:value="form.provider_name"
            :options="providerOptions"
            placeholder="选择 Provider"
            :disabled="editing"
          />
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
import axios from 'axios'
import { api } from '../api/client'

interface ProviderKeyView {
  id: number
  provider_name: string
  name: string
  key_masked: string
  enabled: boolean
  // P48: 计费来源 — token_plan / api / free
  billing_source: string
  created_at: string
  updated_at: string
}

interface ProviderInfo {
  name: string
  protocol: string
}

const keys = ref<ProviderKeyView[]>([])
const providers = ref<ProviderInfo[]>([])
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
const message = useMessage()

const form = ref({
  provider_name: '',
  name: '',
  key: '',
  enabled: true,
  // P48: 计费来源 — token_plan / api / free,默认 api
  billing_source: 'api',
})

const rules = {
  provider_name: { required: true, message: '选择 Provider', trigger: 'blur' },
  key: { required: true, message: 'Key 必填', trigger: 'blur' },
}

const providerOptions = computed(() =>
  providers.value.map(p => ({ label: `${p.name} (${p.protocol})`, value: p.name }))
)

const billingSourceOptions = [
  { label: '💰 按量计费 (api)', value: 'api' },
  { label: '📦 Token Plan (token_plan)', value: 'token_plan' },
  { label: '🎁 免费层 (free)', value: 'free' },
]

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
        token_plan: { color: '#2080f0', label: '📦 token_plan' },
        api:        { color: '#f0a020', label: '💰 api' },
        free:       { color: '#18a058', label: '🎁 free' },
      }
      const m = map[row.billing_source] ?? { color: '#999', label: row.billing_source }
      return h('span', { style: { color: m.color, fontWeight: 500 } }, m.label)
    },
  },
  {
    title: '状态',
    key: 'enabled',
    width: 100,
    render: (row) =>
      h(
        'span',
        { style: { color: row.enabled ? '#18a058' : '#999' } },
        row.enabled ? '● 启用' : '○ 禁用',
      ),
  },
  { title: '创建时间', key: 'created_at', width: 200 },
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
    // 一次性加载 provider 列表 + keys(对每个 provider 都拉一次 key)
    const provResp = await api.providers()
    providers.value = provResp.providers
    // 后端 /api/v1/providers 不返回 loaded 字段,默认所有列出来的 provider 都是 loaded 的
    // (不列出的 provider 是没启用的)
    const list = provResp.providers || []
    const allKeys = await Promise.all(
      list.map(async p => {
        try {
          const r = await axios.get<{ keys: ProviderKeyView[] }>(`/api/v1/providers/${encodeURIComponent(p.name)}/api-keys`)
          return r.data.keys || []
        } catch (e) {
          return []
        }
      })
    )
    keys.value = allKeys.flat()
  } catch (e: any) {
    message.error('加载失败: ' + (e.message ?? e))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  form.value = {
    provider_name: providers.value[0]?.name ?? '',
    name: '',
    key: '',
    enabled: true,
    billing_source: 'api',
  }
  modalVisible.value = true
}

async function save() {
  if (!form.value.provider_name) {
    message.error('选择 Provider')
    return
  }
  if (!form.value.key) {
    message.error('Key 必填')
    return
  }
  saving.value = true
  try {
    await axios.post(
      `/api/v1/providers/${encodeURIComponent(form.value.provider_name)}/api-keys`,
      {
        name: form.value.name,
        key: form.value.key,
        enabled: form.value.enabled,
        billing_source: form.value.billing_source,
      },
    )
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
    await axios.delete(
      `/api/v1/providers/${encodeURIComponent(row.provider_name)}/api-keys/${row.id}`,
    )
    message.success('已删除')
    await load()
  } catch (e: any) {
    message.error('删除失败: ' + (e.response?.data?.error ?? e.message))
  }
}

onMounted(load)
</script>
