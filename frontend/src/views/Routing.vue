<template>
  <n-spin :show="loading">
    <!-- P-catch-all 自动模式:唯一的"路由规则" — 无路由表,所有 provider 自动参与 -->
    <n-card v-if="data?.catch_all" title="兜底路由 *(任意未知模型)" style="margin-bottom: 12px">
      <template v-if="(data.catch_all.Providers ?? []).length === 0">
        <n-space align="center" style="margin-bottom: 12px">
          <n-tag type="success">自动模式</n-tag>
          <n-text depth="3" style="font-size: 12px">
            无路由表:客户端发任何 model 名都一律走这条链(名字只是标签),
            所有 enabled provider 自动参与(按请求路径选协议面),每个 provider
            用其默认模型承接,token_plan → api → free 计费;链上能用哪些模型
            由 gateway key 白名单细化。加 provider + key 即自动进链。
          </n-text>
        </n-space>

        <!-- 树状排序图(2026-08-10):Level 1 token_plan/api 层 → Level 2 provider → Level 3 key -->
        <n-space justify="space-between" align="center" style="margin: 4px 0 8px">
          <n-text depth="2" style="font-size: 13px; font-weight: 600">
            调度顺序树(默认按加入时间,可上/下移改写优先级)
          </n-text>
          <n-space>
            <n-button v-if="hasDirty" size="small" type="primary" :loading="saving" @click="saveOrders">
              保存排序
            </n-button>
            <n-button v-if="hasDirty" size="small" @click="cancelOrders">取消</n-button>
          </n-space>
        </n-space>

        <!-- 每层一个卡片组 -->
        <n-space vertical size="small" v-for="layerMeta in layerMetas" :key="layerMeta.key">
          <n-card
            size="small"
            :title="`${layerMeta.label} 层`"
            :style="{ borderColor: layerMeta.color }"
          >
            <template #header-extra>
              <n-text depth="3" style="font-size: 12px">
                {{ layerOrder(layerMeta.key).length }} 个 provider · 默认按最早 key 加入时间排序
              </n-text>
            </template>

            <!-- 层内 provider(Level 2)-->
            <div
              v-for="(prov, pi) in layerOrder(layerMeta.key)"
              :key="prov.provider"
              class="route-node"
              style="border-left: 3px solid #ccc; padding-left: 8px; margin-bottom: 6px"
            >
              <n-space align="center" justify="space-between">
                <n-space align="center">
                  <n-button size="tiny" quaternary :disabled="pi === 0" @click="moveProv(layerMeta.key, pi, -1)">
                    ▲
                  </n-button>
                  <n-button size="tiny" quaternary :disabled="pi === layerOrder(layerMeta.key).length - 1" @click="moveProv(layerMeta.key, pi, 1)">
                    ▼
                  </n-button>
                  <n-tag :bordered="false" :color="{ color: layerMeta.color, textColor: '#fff' }">
                    {{ prov.provider }}
                  </n-tag>
                  <n-text depth="3" style="font-size: 12px">
                    {{ prov.protocol || '' }} · {{ prov.defaultModel || '' }}
                  </n-text>
                </n-space>
                <n-text depth="3" style="font-size: 12px">{{ prov.keys.length }} key</n-text>
              </n-space>

              <!-- provider 内 key(Level 3)-->
              <div v-if="prov.keys.length" style="margin-left: 28px; margin-top: 4px">
                <div
                  v-for="(k, ki) in prov.keys"
                  :key="k.name"
                  style="display: flex; align-items: center; padding: 2px 0"
                >
                  <span style="width: 16px; display: inline-block; color: #888; font-size: 12px">
                    {{ ki + 1 }}
                  </span>
                  <n-button size="tiny" text :disabled="ki === 0" @click="moveKey(layerMeta.key, prov.provider, ki, -1)">
                    ↑
                  </n-button>
                  <n-button size="tiny" text :disabled="ki === prov.keys.length - 1" @click="moveKey(layerMeta.key, prov.provider, ki, 1)">
                    ↓
                  </n-button>
                  <n-text style="font-size: 13px">
                    {{ k.name }}
                  </n-text>
                  <n-tag v-if="k.billing_source === 'token_plan'" size="tiny" style="margin-left: 6px">📦</n-tag>
                  <n-tag v-else size="tiny" style="margin-left: 6px" type="info">api</n-tag>
                </div>
              </div>
              <n-text v-else depth="3" style="font-size: 12px; margin-left: 28px">(无可用 key)</n-text>
            </div>
          </n-card>
        </n-space>
      </template>
      <template v-else>
        <n-space align="center" style="margin-bottom: 12px">
          <n-tag type="warning">显式列表</n-tag>
          <n-tag type="info">策略: {{ data.catch_all.Strategy }}</n-tag>
          <n-tag>{{ data.catch_all.Providers.length }} 个候选</n-tag>
        </n-space>
        <n-data-table
          :columns="columns"
          :data="data.catch_all.Providers"
          :bordered="false"
          :pagination="false"
        />
      </template>
    </n-card>

    <n-empty v-else-if="!loading" description="未配置 catch_all — 未知模型名将返回 503" />

    <!-- alias 表已退役:精细映射由 config.yaml 的 routing.aliases 配置,页面不再展示 -->
  </n-spin>
</template>

<script setup lang="ts">
// 树状排序图(Routing 优先度编辑)
// Level 1: token_plan / api 两层(由 key 的 billing_source 决定,不可手改)
// Level 2: 层内 provider 顺序(默认按最早 key 加入时间,可上/下移改写)
// Level 3: provider 内 key 顺序(默认按加入时间,可上/下移改写)
// 保存 → PUT /routing/order 落库 + 后端热生效(sticky 与 429 机制不变)
import { computed, onMounted, ref } from 'vue'
import { NCard, NButton, NDataTable, NEmpty, NSpace, NSpin, NTag, NText } from 'naive-ui'
import { api, type RoutingResp } from '../api/client'
import { useProvidersStore } from '../stores/providers'

const data = ref<RoutingResp | null>(null)
const loading = ref(true)
const saving = ref(false)

const store = useProvidersStore()

interface TreeNodeProvider {
  provider: string
  protocol: string
  defaultModel: string
  keys: { name: string; billing_source: string }[]
}
interface TreeLayer {
  key: string
  label: string
  color: string
  providers: TreeNodeProvider[]
}

// 本组件局部编辑的树;每个 key 引用 route_order 改写或默认(加入时间)序
const layers = ref<TreeLayer[]>([
  { key: 'token_plan', label: 'token_plan', color: '#18a058', providers: [] },
  { key: 'api', label: 'api 付费', color: '#2080f0', providers: [] },
])
const dirty = ref(false)

const hasDirty = computed(() => dirty.value)

const layerMetas = computed(() =>
  layers.value.map(l => ({ key: l.key, label: l.label, color: l.color })),
)

const layerOrder = (layerKey: string) => {
  const l = layers.value.find(x => x.key === layerKey)
  return l ? l.providers : []
}

function moveProv(layerKey: string, idx: number, dir: number) {
  const l = layers.value.find(x => x.key === layerKey)
  if (!l) return
  const next = idx + dir
  if (next < 0 || next >= l.providers.length) return
  const arr = l.providers
  ;[arr[idx], arr[next]] = [arr[next], arr[idx]]
  dirty.value = true
}

function moveKey(layerKey: string, provider: string, idx: number, dir: number) {
  const l = layers.value.find(x => x.key === layerKey)
  const p = l?.providers.find(pp => pp.provider === provider)
  if (!p) return
  const next = idx + dir
  if (next < 0 || next >= p.keys.length) return
  ;[p.keys[idx], p.keys[next]] = [p.keys[next], p.keys[idx]]
  dirty.value = true
}

function cancelOrders() {
  // 从 route_order 覆写 + 默认序重新装配初始状态(re-fetch)
  buildTree()
  dirty.value = false
}

async function saveOrders() {
  saving.value = true
  try {
    // 逐层保存 provider 改写(Level 2)
    for (const l of layers.value) {
      const provOrder = l.providers.map(p => p.provider)
      await api.routeOrder.put('provider', '', l.key, provOrder)
      // 逐 provider 保存 key 改写(Level 3)
      for (const p of l.providers) {
        const keyOrder = p.keys.map(k => k.name)
        await api.routeOrder.put('key', p.provider, l.key, keyOrder)
      }
    }
    // 已保存,清 dirty
    dirty.value = false
  } finally {
    saving.value = false
  }
}

async function buildTree() {
  // 拉 provider 参与方与各自 key,按 billing_source 归层(token_plan / api)
  // 顺序 = 后端返回序(已是「route_order 改写序,否则加入时间序」),前端按此展示并可上/下移改写
  await store.load()
  const byLayer: Record<string, Map<string, TreeNodeProvider>> = {
    token_plan: new Map(),
    api: new Map(),
  }
  for (const v of store.vendors) {
    for (const n of v.names ?? []) {
      const faceName = n.name
      try {
        const res = await api.providerKeys.list(faceName)
        const kws = (res.keys ?? []).filter(k => k.enabled !== false)
        for (const k of kws) {
          const bs = k.billing_source || 'api'
          if (!byLayer[bs]) byLayer[bs] = new Map()
          const node = byLayer[bs].get(faceName) || {
            provider: faceName,
            protocol: n.protocol || '',
            defaultModel: v.models?.[0] || '',
            keys: [],
          }
          node.keys.push({ name: k.name, billing_source: bs })
          byLayer[bs].set(faceName, node)
        }
      } catch {
        // provider 无 pool/key —— 自动模式下无 key 自然不参与,跳过
      }
    }
  }
  layers.value = layers.value.map(l => {
    const m = byLayer[l.key]
    l.providers = m ? Array.from(m.values()) : []
    return l
  })
  dirty.value = false
}

const columns = [
  { title: 'Provider', key: 'Name' },
  { title: 'Model', key: 'Model' },
  { title: 'Priority', key: 'Priority' },
  { title: 'Weight', key: 'Weight' },
]

onMounted(async () => {
  loading.value = true
  try {
    data.value = await api.routing()
    if (data.value?.catch_all && (data.value.catch_all.Providers ?? []).length === 0) {
      await buildTree()
    }
  } finally {
    loading.value = false
  }
})
</script>

<style scoped>
.route-node {
  border-radius: 6px;
}
</style>
