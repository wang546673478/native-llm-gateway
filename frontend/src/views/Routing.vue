<template>
  <n-spin :show="loading">
    <!-- P-catch-all: 兜底路由卡片 — 任何 alias 表外的模型名都按它路由 -->
    <n-card
      v-if="data?.catch_all"
      title="*(任意未知模型)"
      style="margin-bottom: 12px"
    >
      <n-space align="center" style="margin-bottom: 12px">
        <n-tag type="warning">兜底路由 catch_all</n-tag>
        <!-- 空 providers = 自动模式:所有 provider 参与,无路由表 -->
        <template v-if="data.catch_all.Providers.length === 0">
          <n-tag type="success">自动模式</n-tag>
          <n-text depth="3" style="font-size: 12px">
            所有 enabled provider 参与(按请求路径选协议面),每个 provider 用其
            默认模型(default_model 或第一个声明),token_plan → api → free 计费。
            加 provider + key 即自动进链
          </n-text>
        </template>
        <template v-else>
          <n-tag type="info">策略: {{ data.catch_all.Strategy }}</n-tag>
          <n-tag>{{ data.catch_all.Providers.length }} 个候选</n-tag>
        </template>
      </n-space>
      <n-data-table
        v-if="data.catch_all.Providers.length > 0"
        :columns="columns"
        :data="data.catch_all.Providers"
        :bordered="false"
        :pagination="false"
      />
    </n-card>

    <n-card
      v-for="(rule, alias) in data?.aliases ?? {}"
      :key="alias"
      :title="alias"
      style="margin-bottom: 12px"
    >
      <n-space align="center" style="margin-bottom: 12px">
        <n-tag type="info">策略: {{ rule.Strategy }}</n-tag>
        <n-tag>{{ rule.Providers.length }} 个候选</n-tag>
      </n-space>
      <n-data-table :columns="columns" :data="rule.Providers" :bordered="false" :pagination="false" />
    </n-card>
  </n-spin>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NCard, NDataTable, NSpace, NSpin, NTag, NText } from 'naive-ui'
import { api, type RoutingResp } from '../api/client'

const data = ref<RoutingResp | null>(null)
const loading = ref(true)

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
  } finally {
    loading.value = false
  }
})
</script>
