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
      style="width: 600px"
      :mask-closable="false"
    >
      <n-form ref="formRef" :model="form" :rules="rules" label-placement="top">
        <n-form-item label="名称" path="name">
          <n-input v-model:value="form.name" :disabled="editing" placeholder="例如 prod-team-a" />
        </n-form-item>
        <n-form-item label="密钥" path="key">
          <n-input
            v-model:value="form.key"
            type="password"
            show-password-on="click"
            :placeholder="editing ? '留空表示不修改' : 'gw-...'"
          />
        </n-form-item>
        <n-form-item label="允许的模型" path="allowed_models">
          <n-dynamic-tags v-model:value="form.allowed_models" />
          <n-text depth="3" style="font-size: 12px; display: block; margin-top: 4px">
            用 * 表示允许所有模型
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
import { h, onMounted, ref } from 'vue'
import {
  NButton, NCard, NDataTable, NDynamicTags, NForm, NFormItem,
  NInput, NInputNumber, NModal, NSpace, NSpin, NSwitch, NH3, NText,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import axios from 'axios'

interface KeyView {
  name: string
  allowed_models: string[]
  rpm: number
  tpm: number
  enabled: boolean
}

const keys = ref<KeyView[]>([])
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
const message = useMessage()

const form = ref({
  name: '',
  key: '',
  allowed_models: ['*'] as string[],
  rpm: 100,
  tpm: 500000,
  enabled: true,
})

const rules = {
  name: { required: true, message: '名称必填', trigger: 'blur' },
  key: { required: true, message: '密钥必填', trigger: 'blur' },
}

const columns: DataTableColumns<KeyView> = [
  { title: '名称', key: 'name' },
  {
    title: '允许模型',
    key: 'allowed_models',
    render: (row) => row.allowed_models.join(', '),
  },
  { title: 'RPM', key: 'rpm' },
  { title: 'TPM', key: 'tpm' },
  {
    title: '状态',
    key: 'enabled',
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
    render: (row) =>
      h(NSpace, {}, () => [
        h(NButton, { size: 'small', onClick: () => openEdit(row) }, () => '编辑'),
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
    const resp = await axios.get('/api/v1/keys')
    keys.value = resp.data.keys
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
    key: '',
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
    key: '', // 编辑时不填,留空表示不改
    allowed_models: row.allowed_models,
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
  if (!editing.value && !form.value.key) {
    message.error('密钥必填')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await axios.put(`/api/v1/keys/${encodeURIComponent(form.value.name)}`, {
        key: form.value.key,
        allowed_models: form.value.allowed_models,
        rpm: form.value.rpm,
        tpm: form.value.tpm,
        enabled: form.value.enabled,
      })
      message.success('已更新')
    } else {
      await axios.post('/api/v1/keys', {
        name: form.value.name,
        key: form.value.key,
        allowed_models: form.value.allowed_models,
        rpm: form.value.rpm,
        tpm: form.value.tpm,
        enabled: form.value.enabled,
      })
      message.success('已创建')
    }
    modalVisible.value = false
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    saving.value = false
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

onMounted(load)
</script>
