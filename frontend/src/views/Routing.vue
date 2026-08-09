<template>
  <n-spin :show="loading">
    <!-- P-catch-all 自动模式:唯一的"路由规则" — 无路由表,所有 provider 自动参与 -->
    <n-card v-if="data?.catch_all" title="兜底路由 *(任意未知模型)" style="margin-bottom: 12px">
      <template v-if="(data.catch_all.Providers ?? []).length === 0">
        <n-space align="center" style="margin-bottom: 8px">
          <n-tag type="success">自动模式</n-tag>
          <n-text depth="3" style="font-size: 13px">
            无路由表:所有 enabled provider 自动参与(按请求路径选协议面) · token_plan → api →
            free 计费 · 加 provider + key 即自动进链。下面拖拽可改写优先级(默认按加入时间)。
          </n-text>
        </n-space>

        <!-- 保存/取消条 -->
        <n-space justify="space-between" align="center" style="margin: 12px 0">
          <n-text depth="2" style="font-size: 15px; font-weight: 600">
            🗂 调度顺序树<span style="font-weight: 400; color: #888"> — 拖拽 provider / key 调整优先级,点保存落库并即时生效</span>
          </n-text>
          <n-space>
            <n-tag v-if="hasDirty" type="warning" size="small">{{ dirtyCount }} 处待保存</n-tag>
            <n-button v-if="hasDirty" size="small" type="primary" :loading="saving" @click="saveOrders">
              保存排序
            </n-button>
            <n-button v-if="hasDirty" size="small" @click="cancelOrders">取消</n-button>
          </n-space>
        </n-space>

        <!-- 左右并排两层 -->
        <n-grid :cols="2" :x-gap="12" responsive="screen" item-responsive>
          <n-gi v-for="layer in layers" :key="layer.key">
            <div class="layer-card" :style="{ borderTop: `3px solid ${layer.color}` }">
              <div class="layer-head">
                <n-tag :bordered="false" :color="{ color: layer.color, textColor: '#fff' }" size="small">
                  {{ layer.label }}
                </n-tag>
                <n-text depth="3" style="font-size: 13px">
                  {{ layer.providers.length }} provider · 拖拽排序
                </n-text>
              </div>

              <!-- Level 2: provider 拖拽列表 -->
              <draggable
                v-model="layer.providers"
                item-key="provider"
                group="providers"
                :animation="180"
                handle=".drag-handle"
                class="provider-sortable"
                ghost-class="drag-ghost"
                @change="markDirty"
              >
                <template #item="{ element: prov, index: pi }">
                  <div class="provider-card" :data-idx="pi">
                    <div class="drag-handle">⠿</div>
                    <div class="provider-main">
                      <div class="provider-title">
                        <span class="provider-name">{{ prov.provider }}</span>
                        <n-tag v-if="prov.billing_source === 'token_plan'" size="small" :bordered="false">📦</n-tag>
                        <n-text depth="3" size="small">{{ prov.defaultModel || '' }}</n-text>
                      </div>

                      <!-- Level 3: key 拖拽列表 -->
                      <div class="key-lane">
                        <draggable
                          v-model="prov.keys"
                          item-key="name"
                          group="keys"
                          :animation="160"
                          handle=".key-handle"
                          class="key-sortable"
                          ghost-class="drag-ghost"
                          @change="markDirty"
                        >
                          <template #item="{ element: k, index: ki }">
                            <div class="key-chip" :data-idx="ki">
                              <span class="key-handle">⋮⋮</span>
                              <span class="key-name">{{ k.name }}</span>
                              <n-tag size="small" :bordered="false" :type="k.billing_source === 'token_plan' ? 'success' : 'info'">
                                {{ k.billing_source === 'token_plan' ? 'token_plan' : 'api' }}
                              </n-tag>
                            </div>
                          </template>
                        </draggable>
                      </div>
                    </div>
                  </div>
                </template>
              </draggable>
              <n-text v-if="layer.providers.length === 0" depth="3" style="font-size: 13px; padding: 12px 16px; display:block">
                (该层暂无 provider)
              </n-text>
            </div>
          </n-gi>
        </n-grid>
      </template>

      <template v-else>
        <n-space align="center" style="margin-bottom: 12px">
          <n-tag type="warning">显式列表</n-tag>
          <n-tag type="info">策略: {{ data.catch_all.Strategy }}</n-tag>
          <n-tag>{{ data.catch_all.Providers.length }} 个候选</n-tag>
        </n-space>
        <n-data-table :columns="columns" :data="data.catch_all.Providers" :bordered="false" :pagination="false" />
      </template>
    </n-card>

    <n-empty v-else-if="!loading" description="未配置 catch_all — 未知模型名将返回 503" />
  </n-spin>
</template>

<script setup lang="ts">
// 树状排序图(Routing 优先级编辑)
// Level 1: token_plan / api 两层(左右并排,由 key 的 billing_source 决定不可手改)
// Level 2: 层内 provider 顺序(可拖拽改写,默认按最早 key 加入时间)
// Level 3: provider 内 key 顺序(可拖拽改写,默认按加入时间、先加先用)
// 保存 → PUT /routing/order 落库 + 后端热生效。只排序,不展示 runtime 状态。
import { computed, onMounted, ref } from 'vue'
import draggable from 'vuedraggable'
import { NCard, NButton, NDataTable, NEmpty, NGrid, NGi, NSpace, NSpin, NTag, NText } from 'naive-ui'
import { api, type RoutingResp } from '../api/client'
import { useProvidersStore } from '../stores/providers'

const data = ref<RoutingResp | null>(null)
const loading = ref(true)
const saving = ref(false)

const store = useProvidersStore()

interface TreeNodeKey {
  name: string
  billing_source: string
}
interface TreeNodeProvider {
  provider: string
  defaultModel: string
  billing_source: string
  keys: TreeNodeKey[]
}
interface TreeLayer {
  key: string
  label: string
  color: string
  providers: TreeNodeProvider[]
}

const layers = ref<TreeLayer[]>([
  { key: 'token_plan', label: 'token_plan', color: '#18a058', providers: [] },
  { key: 'api', label: 'api 付费', color: '#2080f0', providers: [] },
])
const dirty = ref(false)
const dirtyCount = ref(0)

const hasDirty = computed(() => dirty.value)

function markDirty() {
  dirty.value = true
  // 统计与默认序不一致的 provider/key 便于提示(轻量:数当前层 provider 顺序与实际差异)
  dirtyCount.value = layers.value.reduce((acc, l) => acc + l.providers.length, 0)
}

function cancelOrders() {
  buildTree()
  dirty.value = false
  dirtyCount.value = 0
}

async function saveOrders() {
  saving.value = true
  try {
    for (const l of layers.value) {
      const provOrder = l.providers.map(p => p.provider)
      await api.routeOrder.put('provider', '', l.key, provOrder)
      for (const p of l.providers) {
        const keyOrder = p.keys.map(k => k.name)
        await api.routeOrder.put('key', p.provider, l.key, keyOrder)
      }
    }
    dirty.value = false
    dirtyCount.value = 0
  } finally {
    saving.value = false
  }
}

async function buildTree() {
  // 按 vendor 名聚合 provider(如 minimax / mimo),不渲染协议面注册名(minimax-anthropic 等)。
  // provider 下直接列它的 provider key(如 weige / key-1),按 key 的 billing_source 归层:
  //   token_plan: minimax(weige,key-1) + mimo(key-1)
  //   api:        deepseek(key-1) + mimo(key-2)
  await store.load()
  const byLayer: Record<string, Map<string, TreeNodeProvider>> = {
    token_plan: new Map(),
    api: new Map(),
  }
  for (const v of store.vendors) {
    const vendor = v.vendor
    try {
      const res = await api.providerKeys.list(vendor)
      const kws = (res.keys ?? []).filter(k => k.enabled !== false)
      for (const k of kws) {
        const bs = k.billing_source || 'api'
        if (!byLayer[bs]) byLayer[bs] = new Map()
        const node = byLayer[bs].get(vendor) || {
          provider: vendor,
          defaultModel: v.models?.[0] || '',
          billing_source: bs,
          keys: [],
        }
        node.keys.push({ name: k.name, billing_source: bs })
        byLayer[bs].set(vendor, node)
      }
    } catch {
      // provider 无 key —— 自动模式下自然不参与
    }
  }
  layers.value = layers.value.map(l => {
    const m = byLayer[l.key]
    l.providers = m ? Array.from(m.values()) : []
    return l
  })
  dirty.value = false
  dirtyCount.value = 0
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
.layer-card {
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  background: #fafafa;
  padding-bottom: 12px;
  min-height: 140px;
}
.layer-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  border-bottom: 1px dashed #e5e7eb;
}
.provider-sortable {
  padding: 12px;
}
.provider-card {
  display: flex;
  gap: 10px;
  background: #fff;
  border: 1px solid #ececec;
  border-radius: 10px;
  padding: 12px 14px;
  margin-bottom: 10px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.05);
  transition: box-shadow 0.15s;
}
.provider-card:hover {
  box-shadow: 0 4px 10px rgba(0, 0, 0, 0.12);
}
.drag-handle {
  cursor: grab;
  color: #bbb;
  user-select: none;
  padding-top: 2px;
  font-size: 18px;
}
.drag-handle:active {
  cursor: grabbing;
}
.provider-main {
  flex: 1;
}
.provider-title {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 16px;
}
.provider-name {
  font-weight: 600;
  font-size: 16px;
}
.key-lane {
  margin-top: 8px;
}
.key-sortable {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  padding: 4px 0;
}
.key-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: #f5f5f7;
  border-radius: 999px;
  padding: 5px 14px;
  font-size: 14px;
  cursor: default;
}
.key-handle {
  cursor: grab;
  color: #bbb;
  font-size: 12px;
  user-select: none;
}
.key-handle:active {
  cursor: grabbing;
}
.drag-ghost {
  opacity: 0.4;
  border: 1px dashed #aaa;
  background: #f0f0f0;
}
</style>
