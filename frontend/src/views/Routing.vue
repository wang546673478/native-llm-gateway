<template>
  <n-spin :show="loading">
    <!-- P-catch-all 自动模式:唯一的"路由规则" — 无路由表,所有 provider 自动参与 -->
    <n-card v-if="data?.catch_all" title="兜底路由 *(任意未知模型)" style="margin-bottom: 12px">
      <template v-if="(data.catch_all.Providers ?? []).length === 0">
        <n-space align="center" style="margin-bottom: 12px">
          <n-tag type="success">自动模式</n-tag>
          <n-text depth="3" style="font-size: 12px">
            无路由表:客户端发任何 alias 表外且无 provider 声明的 model 名,所有
            enabled provider 自动参与(按请求路径选协议面),每个 provider 用其默认
            模型(default_model 或第一个声明),token_plan → api → free 计费。
            加 provider + key 即自动进链。
          </n-text>
        </n-space>
        <n-data-table
          :columns="providerColumns"
          :data="autoProviders"
          :bordered="false"
          :pagination="false"
        />
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
import { onMounted, ref } from 'vue'
import { NCard, NDataTable, NEmpty, NSpace, NSpin, NTag, NText } from 'naive-ui'
import { api, type RoutingResp } from '../api/client'

const data = ref<RoutingResp | null>(null)
const loading = ref(true)

// 自动模式下展示「参与 provider → 默认模型」,从 /providers 拉取实际参与方
const autoProviders = ref<Array<{ provider: string; protocol: string; defaultModel: string }>>([])

const providerColumns = [
  { title: 'Provider(自动参与)', key: 'provider' },
  { title: '协议面', key: 'protocol' },
  { title: '默认模型', key: 'defaultModel' },
]

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
      // 自动模式:拉 provider 列表,展示参与方与默认模型(第一个声明 = 默认)
      const prov = await api.providers()
      autoProviders.value = prov.vendors.map(v => ({
        provider: v.vendor,
        protocol: v.names?.[0]?.protocol ?? '',
        defaultModel: v.models?.[0] ?? '',
      }))
    }
  } finally {
    loading.value = false
  }
})
</script>
