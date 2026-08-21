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
      style="width: 800px"
      :mask-closable="false"
    >
      <n-form ref="formRef" :model="form" :rules="rules" label-placement="top">
        <n-form-item label="名称" path="name">
          <n-input v-model:value="form.name" :disabled="editing" placeholder="例如 prod-team-a" />
        </n-form-item>

        <n-alert v-if="!editing" type="info" :show-icon="false" style="margin-bottom: 12px">
          密钥将由系统自动生成,创建后会展示在列表里,可随时复制。
        </n-alert>

        <!-- P34: 多 Provider 绑定(限制 provider 类型) -->
        <n-form-item label="绑定 Provider(可多选,限制可用的 provider 类型)" path="providers">
          <n-select
            v-model:value="form.providers"
            multiple
            :options="providerOptions"
            placeholder="不选 = 不限制,可用于任意 Provider"
            clearable
          />
        </n-form-item>

        <!-- P34: 第二级 — 绑定具体 Provider Key 凭证(多租户隔离) -->
        <n-form-item label="绑定的 Provider Key(可多选,精准锁定上游凭证)" path="provider_key_ids">
          <n-select
            v-model:value="form.provider_key_ids"
            multiple
            :options="filteredProviderKeyOptions"
            :disabled="availableProviderKeys.length === 0"
            placeholder="不选 = 用该 provider 的所有 key 池"
            clearable
            filterable
          />
          <n-text depth="3" style="font-size: 12px; display: block; margin-top: 4px">
            已绑定 {{ form.provider_key_ids.length }} 个 Provider Key ·
            <span v-if="form.providers.length === 0">
              (所有 provider 的 keys 都可选,共 {{ availableProviderKeys.length }} 个)
            </span>
            <span v-else>
              (从已绑 Provider 中选,共 {{ availableProviderKeys.length }} 个)
            </span>
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
            用 <code>*</code> 表示允许所有模型。当前可选 {{ selectableModelCount }} 个;
            按渠道面分组 —— <b>〔仅此面〕</b>的模型选中即锁定该渠道,<b>〔多面共有〕</b>的任一渠道都可能命中
          </n-text>
        </n-form-item>

        <!-- P-catch-all: default_model 已退役 — 未知模型名由 catch_all 自动模式接管,
             此处不再配置。后端字段保留,key 不设置即不触发 fallback -->

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

    <!-- 创建后的明文密钥一次性展示对话框。
         SECURITY: 关掉后丢失,用户必须现在复制保存。这是替代 list/get 泄露的唯一办法。 -->
    <n-modal
      v-model:show="newKeyModalVisible"
      preset="card"
      :title="`新密钥已生成: ${newKeyName}`"
      style="width: 560px"
      :mask-closable="false"
      :closable="true"
    >
      <n-alert type="warning" style="margin-bottom: 16px">
        这是密钥明文,<strong>只会展示这一次</strong>。关闭此弹窗后无法再次查看,请立即复制保存到安全位置。
      </n-alert>
      <n-input
        :value="newKeySecret"
        readonly
        type="text"
        style="font-family: monospace"
      >
        <template #suffix>
          <n-button text @click="copyNewKeySecret">📋 复制</n-button>
        </template>
      </n-input>
      <template #footer>
        <n-space justify="end">
          <n-button type="primary" @click="newKeyModalVisible = false">我已保存</n-button>
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
import { api, type ProviderKeyView } from '../api/client'
import { useProvidersStore } from '../stores/providers'
import { copyText } from '../utils/clipboard'

interface ProviderInfo {
  name: string
  protocol: string
  loaded: boolean
  models: string[]
}

// P34: KeyView 含 provider_key_ids + 明文 key(用户要求 list 也能直接复制)
// 注意:这是内部 LLM Gateway,UI 部署在内网/反代后面,管理员需要随时复制 key 来分发/重新部署。
// SECURITY trade-off:list 返明文 key 让任何能访问 UI 的人都可拿到所有客户端 token。
// 外部部署必须在反代层加 basic auth + IP allowlist 保护 admin endpoints。
interface KeyView {
  id?: number
  name: string
  key: string
  providers: string[]
  provider_key_ids: number[]
  allowed_models: string[]
  rpm: number
  tpm: number
  enabled: boolean
  created_at?: string
}

const keys = ref<KeyView[]>([])
const providers = ref<ProviderInfo[]>([])
// P-model-face: vendor → 面 → 该面提供的模型 id(来自 /providers/models 的 faces)。
// 空 = 该厂商无面归属数据(未同步 / 所有面都无模型列表端点)→ 下拉退回扁平模式
const modelFaces = ref<Record<string, Record<string, string[]>>>({})
const providerKeysMap = ref<Record<string, ProviderKeyView[]>>({}) // P34: provider_name → ProviderKey[]
// P-provider-vendor: store 持 vendors 单一来源;load() 内填充 providers
const provStore = useProvidersStore()
// P-provider-vendor: 注册名(deepseek-anthropic)→ 厂商(deepseek)映射 —
// 编辑/展示旧数据(绑定仍存注册名)时归一,保存后后端统一存厂商名
const regToVendor = ref<Record<string, string>>({})
const loading = ref(false)
const saving = ref(false)
const modalVisible = ref(false)
const editing = ref(false)
// SECURITY: 创建后的明文 key 仅一次性展示在 newKeyModal,关闭后丢失
const newKeySecret = ref('')
const newKeyModalVisible = ref(false)
const newKeyName = ref('')
const message = useMessage()

const form = ref({
  name: '',
  providers: [] as string[],
  provider_key_ids: [] as number[], // P34: 绑定的 ProviderKey ID
  allowed_models: ['*'] as string[],
  rpm: 100,
  tpm: 500000,
  enabled: true,
})

const rules = {
  name: { required: true, message: '名称必填', trigger: 'blur' },
}

const providerOptions = computed<SelectOption[]>(() => provStore.vendorOptions)

// P34: 当前可选的 ProviderKey(根据已绑定的 providers 过滤)
// form.providers 为空时显示所有;否则只显示绑定 provider 的 keys
const availableProviderKeys = computed<ProviderKeyView[]>(() => {
  if (form.value.providers.length === 0) {
    return Object.values(providerKeysMap.value).flat()
  }
  const out: ProviderKeyView[] = []
  for (const p of form.value.providers) {
    out.push(...(providerKeysMap.value[p] ?? []))
  }
  return out
})

const filteredProviderKeyOptions = computed<SelectOption[]>(() =>
  availableProviderKeys.value.map(k => ({
    label: `${k.provider_name} → ${k.name} (${k.key_masked})${k.enabled ? '' : ' [已禁用]'}`,
    value: k.id,
    disabled: !k.enabled,
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

// P-model-face: 模型 → 提供它的面(去掉 vendor 前缀的短名),按当前已选 vendor 过滤。
// 同一模型可能由同厂商多个面提供(如 claude-opus-4-8 官渠和 AWSQ 都有);
// 只由一个面提供的模型(如 claude-fable-5 仅官渠)= 选它就锁定了那个渠道。
const modelFaceMap = computed<Record<string, string[]>>(() => {
  const selected = form.value.providers
  const out: Record<string, string[]> = {}
  for (const [vendor, faces] of Object.entries(modelFaces.value)) {
    if (selected.length > 0 && !selected.includes(vendor)) continue
    for (const [face, models] of Object.entries(faces)) {
      const short = face.startsWith(`${vendor}-`) ? face.slice(vendor.length + 1) : face
      const label = `${vendor} · ${short}`
      for (const m of models) {
        ;(out[m] ??= []).push(label)
      }
    }
  }
  for (const m of Object.keys(out)) out[m] = Array.from(new Set(out[m])).sort()
  return out
})

// availableModelOptions 白名单模型下拉。
//
// P-model-face: 按「提供该模型的面组合」分组 —— 组名即归属,一眼看出选某个模型
// 会命中哪些渠道。不按单个面分组,是因为同一模型可能属于多个面,那样会在
// multi-select 里出现重复 value(选一个全亮 + naive-ui 告警)。
// 无归属数据的模型(未同步的厂商 / 面无模型端点)落到「未标注归属」组,行为同旧版。
const availableModelOptions = computed<SelectOption[]>(() => {
  const wildcard: SelectOption = { label: '* (通配,所有模型)', value: '*' }
  const models = providerModelsUnion.value.filter(m => m !== '*')
  const faceMap = modelFaceMap.value
  // 有任何归属信息才分组,否则保持旧的扁平列表(向后兼容)
  if (!models.some(m => (faceMap[m] ?? []).length > 0)) {
    return [wildcard, ...models.sort().map(m => ({ label: m, value: m }))]
  }
  // 按「面组合」聚类:key = 归属面排序后拼接
  const groups = new Map<string, string[]>()
  for (const m of models) {
    const key = (faceMap[m] ?? []).join(' + ') || '未标注归属'
    const bucket = groups.get(key)
    if (bucket) bucket.push(m)
    else groups.set(key, [m])
  }
  const sortedKeys = Array.from(groups.keys()).sort((a, b) => {
    if (a === '未标注归属') return 1
    if (b === '未标注归属') return -1
    return a.localeCompare(b)
  })
  return [
    wildcard,
    ...sortedKeys.map(key => {
      const items = (groups.get(key) ?? []).sort()
      const exclusive = !key.includes(' + ') && key !== '未标注归属'
      return {
        type: 'group' as const,
        // 组名带提示:单一面 = 选中即锁定该渠道;多面 = 任一渠道都可能命中
        label: `${key}${exclusive ? '  〔仅此面〕' : key === '未标注归属' ? '' : '  〔多面共有〕'} · ${items.length}`,
        key,
        children: items.map(m => ({ label: m, value: m })),
      }
    }),
  ]
})

// selectableModelCount 真实可选模型数(不含通配项与分组标题 —— 分组后
// availableModelOptions.length 是「组数+1」,拿它当模型数会显示成个位数)
const selectableModelCount = computed(() => providerModelsUnion.value.filter(m => m !== '*').length)

// renderModelTag 已选模型的 tag —— 后缀标真实归属面。
// 修复:原实现用 p.models.includes() 判断,而 /providers 的 models 是 vendor 级
// 合并列表(各面共用一份),导致每个模型都匹配到该厂商的全部注册面,后缀等于没标。
// 现在读 provider_model_faces 的真实归属。
function renderModelTag({ option }: any) {
  const value = String(option.value)
  const faces = modelFaceMap.value[value] ?? []
  const suffix = faces.length > 0 ? ` (${faces.join(', ')})` : ''
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
    // 1. 拿 keys + providers
    // P-provider-vendor: 绑定 Provider 按厂商 — 用 /providers(vendor 聚合,
    // 经共享 store 拉取)而非 /providers/registered(注册名);同时建注册名→厂商映射
    const [keysResp] = await Promise.all([
      api.keys.list().catch(() => ({ keys: [], count: 0 })),
      provStore.load(),
      // P-model-face: 拉「面→模型」归属,让白名单下拉能按渠道分组 + 标「仅此面/多面共有」。
      // 中转站厂商同一 vendor 下各面是不同上游、模型互不相通(如 claude-fable-5 只有
      // 官渠有),扁平列表看不出选某个模型会不会锁死渠道。拉失败则退回扁平模式。
      api.models
        .list()
        .then(r => {
          modelFaces.value = r.faces ?? {}
        })
        .catch(() => {
          modelFaces.value = {}
        }),
    ])
    keys.value = keysResp.keys
    // reg-name → vendor 映射来自 store getter(单一来源,不再本地手撸循环)
    regToVendor.value = provStore.regToVendor
    const vendors: any[] = provStore.vendors ?? []
    providers.value = vendors.map((v: any) => ({
      name: v.vendor,
      protocol: v.names?.[0]?.protocol ?? '',
      loaded: true,
      models: v.models ?? [],
    }))

    // 2. 拿所有 provider 的 keys(P34: 用于 ProviderKey 下拉)
    const map: Record<string, ProviderKeyView[]> = {}
    await Promise.all(
      providers.value.map(async p => {
        try {
          const r = await api.providerKeys.list(p.name)
          map[p.name] = r.keys ?? []
        } catch {
          map[p.name] = []
        }
      })
    )
    providerKeysMap.value = map
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
    provider_key_ids: [],
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
    // P-provider-vendor: 旧数据可能存了注册名(deepseek-anthropic),映射回厂商
    // 使下拉能选中;保存时后端再统一归一为厂商名
    providers: row.providers.map(p => regToVendor.value[p] ?? p),
    provider_key_ids: [...(row.provider_key_ids ?? [])],
    allowed_models: row.allowed_models.length > 0 ? [...row.allowed_models] : ['*'],
    rpm: row.rpm,
    tpm: row.tpm,
    enabled: row.enabled,
  }
  modalVisible.value = true
}

// P34: 把绑定的 ID 翻译成可读的 "minimax → key-1 (sk-...)"
import type { VNode } from 'vue'
function describeProviderKeys(ids: number[]): VNode | string {
  if (!ids || ids.length === 0) return h('span', { style: 'color: #999' }, '全部')
  // 从 providerKeysMap 找
  const all = Object.values(providerKeysMap.value).flat()
  const items = ids.map(id => all.find(k => k.id === id)).filter(Boolean) as ProviderKeyView[]
  if (items.length === 0) return `${ids.length} 个 (失效)`
  return items.map(k => `${k.provider_name}→${k.name}`).join(', ')
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
      provider_key_ids: form.value.provider_key_ids,
      allowed_models: form.value.allowed_models,
      rpm: form.value.rpm,
      tpm: form.value.tpm,
      enabled: form.value.enabled,
    }
    if (editing.value) {
      await api.keys.update(form.value.name, body)
      message.success('已更新')
      modalVisible.value = false
    } else {
      body.name = form.value.name
      // 创建响应包含明文 key(KeyView 不含,用 any 临时接住)
      const resp = await api.keys.create(body)
      // SECURITY: 创建后明文仅此一次返回,弹窗展示给用户复制保存
      modalVisible.value = false
      newKeySecret.value = resp.key ?? ''
      newKeyName.value = form.value.name
      newKeyModalVisible.value = true
    }
    await load()
  } catch (e: any) {
    message.error('保存失败: ' + (e.response?.data?.error ?? e.message))
  } finally {
    saving.value = false
  }
}

async function copyNewKeySecret() {
  if (!newKeySecret.value) return
  const ok = await copyText(newKeySecret.value)
  message.success(ok ? '已复制到剪贴板' : '复制失败')
}

// 复制密钥到剪贴板
async function copyKey(row: KeyView) {
  if (!row.key) {
    message.error('密钥为空,无法复制')
    return
  }
  const ok = await copyText(row.key)
  message.success(ok ? `已复制 ${row.name} 的密钥` : '复制失败')
}

async function confirmDelete(row: KeyView) {
  if (!confirm(`确认删除 Key "${row.name}" ?此操作不可撤销`)) return
  try {
    await api.keys.delete(row.name)
    message.success('已删除')
    await load()
  } catch (e: any) {
    message.error('删除失败: ' + (e.response?.data?.error ?? e.message))
  }
}

const columns: DataTableColumns<KeyView> = [
  { title: '名称', key: 'name', width: 130 },
  {
    title: '密钥',
    key: 'key',
    width: 240,
    render: (row) =>
      h('code', {
        style: 'font-size: 11px; padding: 2px 6px; background: rgba(24,160,88,0.08); border-radius: 4px; user-select: all; cursor: pointer;',
        onClick: () => copyKey(row),
        title: '点击复制',
      }, row.key),
  },
  {
    title: 'Provider 绑定',
    key: 'providers',
    width: 140,
    render: (row) => {
      if (!row.providers || row.providers.length === 0) {
        return h('span', { style: 'color: #999' }, '任意')
      }
      // P-provider-vendor: 旧数据存注册名,展示归一为厂商
      return h('span', {}, row.providers.map((p, i) =>
        h('span', { key: i, style: 'color: #2080f0; margin-right: 4px' }, `🔒 ${regToVendor.value[p] ?? p}`)
      ))
    },
  },
  {
    title: 'Provider Key 绑定',
    key: 'provider_key_ids',
    width: 220,
    render: (row) => {
      const desc = describeProviderKeys(row.provider_key_ids ?? [])
      if (typeof desc === 'string') {
        return h('span', {
          style: row.provider_key_ids?.length
            ? 'color: #2080f0; font-size: 12px'
            : 'color: #999; font-size: 12px',
        }, desc)
      }
      return desc
    },
  },
  {
    title: '允许模型',
    key: 'allowed_models',
    width: 150,
    render: (row) =>
      row.allowed_models.length === 0
        ? '*'
        : row.allowed_models.length > 3
        ? `${row.allowed_models.slice(0, 3).join(', ')} +${row.allowed_models.length - 3}`
        : row.allowed_models.join(', '),
  },
  { title: 'RPM', key: 'rpm', width: 60 },
  { title: 'TPM', key: 'tpm', width: 70 },
  {
    title: '状态',
    key: 'enabled',
    width: 70,
    render: (row) =>
      h('span',
        { style: { color: row.enabled ? '#18a058' : '#999' } },
        row.enabled ? '● 启用' : '○ 禁用'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 210,
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