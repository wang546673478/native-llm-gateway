<template>
  <n-spin :show="loading">
    <n-card>
      <n-space justify="space-between" align="center" style="margin-bottom: 16px">
        <n-h3 style="margin: 0">Gateway Keys({{ keys.length }})</n-h3>
        <n-space>
          <n-button type="primary" @click="openCreate">+ 新建</n-button>
          <n-button @click="load">刷新</n-button>
        </n-space>
      </n-space>

      <n-data-table :columns="columns" :data="keys" :bordered="false" :pagination="false" />
    </n-card>

    <!-- 新建/编辑 模态框 -->
    <n-modal
      v-model:show="modalVisible"
      preset="card"
      :title="editing ? '编辑 Gateway Key' : '新建 Gateway Key'"
      style="width: 700px"
      :mask-closable="false"
    >
      <n-form ref="formRef" :model="form" :rules="rules" label-placement="top">
        <n-form-item label="名称" path="name">
          <n-input v-model:value="form.name" :disabled="editing" placeholder="例如 prod-team-a" />
        </n-form-item>

        <!-- P32-B:密钥由系统自动生成 -->
        <n-alert v-if="!editing" type="info" :show-icon="false" style="margin-bottom: 12px">
          密钥将由系统自动生成,创建后会展示在列表里,可随时复制。
        </n-alert>

        <!-- 多 Provider 绑定 -->
        <n-form-item label="绑定 Provider(可多选)" path="providers">
          <n-select
            v-model:value="form.providers"
            multiple
            :options="providerOptions"
            placeholder="不选 = 不限制,可用于任意 Provider"
            clearable
          />
          <n-text depth="3" style="font-size: 12px; display: block; margin-top: 4px">
            同一把 API key 可同时用于多个 Provider(例如 DeepSeek 的 OpenAI 和 Anthropic 兼容端点)
          </n-text>
        </n-form-item>

        <n-form-item label="允许的模型" path="allowed_models">
          <n-select
            v-model:value="form.allowed_models"
            multiple
            :options="availableModelOptions"
            :render-tag="renderModelTag"
            :placeholder="availableModelOptions.length === 0 ? '先选 Provider 才能选模型' : '从已选 Provider 的模型中选,默认 * 通配'"
            :disabled="availableModelOptions.length === 0"
            clearable
          />
          <n-text depth="3" style="font-size: 12px; display: block; margin-top: 4px">
            用 <code>*</code> 表示允许所有模型。当前可选 {{ availableModelOptions.length }} 个
          </n-text>
        </n-form-item>

        <n-form-item label="RPM 限制" path="rpm">
          <n-input-number v-model:value="form.rpm" :min="0" style="width: 100%" />
        </n-form-item>
        <n-form-item label="TPM 限制" path="tpm">
          <n-input-number v-model:value="form.tpm" :min="0" style="width: 100%" />
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
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem,
  NInput, NInputNumber, NModal, NSpace, NSpin, NSwitch, NSelect,
  NH3, NText, useMessage,
} from 'naive-ui'
import type { DataTableColumns, SelectOption } from 'naive-ui'
import axios from 'axios'

interface ProviderInfo {
  name: string
  protocol: string
  loaded: boolean
  models: string[]
}

// P32-B: KeyView 含明文 key(列表直接展示,可复制)
interface KeyView {
  name: string
  key: string
  providers: string[]
  allowed_models: string[]
  rpm: number
  tpm: number
  enabled: boolean
  created_at?: string
}

const keys = ref<KeyView[]>([])
const providers = ref<ProviderInfo[]>([])
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
const message = useMessage()

const form = ref({
  name: '',
  providers: [] as string[],
  allowed_models: ['*'] as string[],
  rpm: 100,
  tpm: 500000,
  enabled: true,
})

const rules = {
  name: { required: true, message: '名称必填', trigger: 'blur' },
}

const providerOptions = computed<SelectOption[]>(() =>
  providers.value.map(p => ({
    label: p.name,
    value: p.name,
  })),
)

const providerModelsUnion = computed<string[]>(() => {
  if (form.value.providers.length === 0) {
    return Array.from(new Set(providers.value.flatMap(p => p.models)))
  }
  return Array.from(new Set(
    form.value.providers.flatMap(name => {
      const p = providers.value.find(x => x.name === name)
      return p ? p.models : []
    })
  ))
})

const availableModelOptions = computed<SelectOption[]>(() => {
  const opts: SelectOption[] = [
    { label: '* (通配,所有模型)', value: '*' },
    ...providerModelsUnion.value
      .filter(m => m !== '*')
      .sort()
      .map(m => ({ label: m, value: m })),
  ]
  return opts
})

function renderModelTag({ option, handleClose }: any) {
  const matched = providers.value
    .filter(p => form.value.providers.includes(p.name) || form.value.providers.length === 0)
    .filter(p => p.models.includes(String(option.value)))
    .map(p => p.name)
  const suffix = matched.length > 0 ? ` (${matched.join(', ')})` : ''
  return h(
    'span',
    {
      style: 'background: rgba(24, 160, 88, 0.1); padding: 2px 8px; border-radius: 4px; font-size: 12px; margin-right: 4px;',
    },
    `${option.label}${suffix} ×`,
  )
}

async function load() {
  loading.value = true
  try {
    const [keysResp, regResp] = await Promise.all([
      axios.get('/api/v1/keys'),
      axios.get('/api/v1/providers/registered'),
    ])
    keys.value = keysResp.data.keys
    providers.value = (regResp.data.providers ?? []).map((p: any) => ({
      name: p.name,
      protocol: p.protocol,
      loaded: true, // /registered 返回的都是已注册(即可用)的 provider
      models: p.models ?? [],
    }))
  } catch (e: any) {
    message.error('加载失败: ' + (e.message ?? e))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  form.value = {
    name: '',
    providers: [],
    allowed_models: ['*'],
    rpm: 100,
    tpm: 500000,
    enabled: true,
  }
  modalVisible.value = true
}

function openEdit(row: KeyView) {
  editing.value = true
  form.value = {
    name: row.name,
    providers: [...row.providers],
    allowed_models: row.allowed_models.length > 0 ? [...row.allowed_models] : ['*'],
    rpm: row.rpm,
    tpm: row.tpm,
    enabled: row.enabled,
  }
  modalVisible.value = true
}

async function save() {
  if (!form.value.name) {
    message.error('名称必填')
    return
  }
  saving.value = true
  try {
    const body: any = {
      providers: form.value.providers,
      allowed_models: form.value.allowed_models,
      rpm: form.value.rpm,
      tpm: form.value.tpm,
      enabled: form.value.enabled,
    }
    if (editing.value) {
      await axios.put(`/api/v1/keys/${encodeURIComponent(form.value.name)}`, body)
      message.success('已更新')
    } else {
      body.name = form.value.name
      // P32-B: 后端直接返回 key 在响应里;列表加载后会自动展示
      await axios.post('/api/v1/keys', body)
      message.success('已创建,密钥已展示在列表中')
    }
    modalVisible.value = false
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    saving.value = false
  }
}

async function copyKey(row: KeyView) {
  try {
    await navigator.clipboard.writeText(row.key)
    message.success(`已复制 ${row.name} 的密钥`)
  } catch (e) {
    // fallback
    const ta = document.createElement('textarea')
    ta.value = row.key
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
    message.success('已复制')
  }
}

async function confirmDelete(row: KeyView) {
  if (!confirm(`确认删除 Key "${row.name}" ?此操作不可撤销`)) return
  try {
    await axios.delete(`/api/v1/keys/${encodeURIComponent(row.name)}`)
    message.success('已删除')
    await load()
  } catch (e: any) {
    message.error('删除失败: ' + (e.response?.data?.error ?? e.message))
  }
}

const columns: DataTableColumns<KeyView> = [
  { title: '名称', key: 'name', width: 140 },
  {
    title: '密钥',
    key: 'key',
    render: (row) =>
      h('code', {
        style: 'font-size: 12px; padding: 2px 6px; background: rgba(24,160,88,0.08); border-radius: 4px; user-select: all; cursor: pointer;',
        onClick: () => copyKey(row),
        title: '点击复制',
      }, row.key),
  },
  {
    title: 'Provider 绑定',
    key: 'providers',
    width: 200,
    render: (row) => {
      if (!row.providers || row.providers.length === 0) {
        return h('span', { style: 'color: #999' }, '任意')
      }
      return h(
        'span',
        {},
        row.providers.map((p, i) =>
          h('span', { key: i, style: 'color: #2080f0; margin-right: 4px' }, `🔒 ${p}`)
        )
      )
    },
  },
  {
    title: '允许模型',
    key: 'allowed_models',
    width: 180,
    render: (row) =>
      row.allowed_models.length === 0
        ? '*'
        : row.allowed_models.length > 3
        ? `${row.allowed_models.slice(0, 3).join(', ')} +${row.allowed_models.length - 3}`
        : row.allowed_models.join(', '),
  },
  { title: 'RPM', key: 'rpm', width: 70 },
  { title: 'TPM', key: 'tpm', width: 80 },
  {
    title: '状态',
    key: 'enabled',
    width: 80,
    render: (row) =>
      h(
        'span',
        { style: { color: row.enabled ? '#18a058' : '#999' } },
        row.enabled ? '● 启用' : '○ 禁用',
      ),
  },
  {
    title: '操作',
    key: 'actions',
    width: 220,
    render: (row) =>
      h(NSpace, {}, () => [
        h(NButton, { size: 'small', onClick: () => copyKey(row) }, () => '📋 复制'),
        h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑'),
        h(NButton, { size: 'small', type: 'error', onClick: () => confirmDelete(row) }, () => '删除'),
      ]),
  },
]

onMounted(load)
</script>