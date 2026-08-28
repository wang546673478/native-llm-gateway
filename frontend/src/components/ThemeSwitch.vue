<!-- 主题切换器 —— 单一职责:把 useTheme() 的三态暴露成一个分段控件。
     不自己持主题状态,不碰 DOM;换 UI 形态(下拉/按钮组/图标)只改这里。 -->
<template>
  <n-tooltip trigger="hover" :delay="400">
    <template #trigger>
      <n-radio-group
        :value="mode"
        size="small"
        @update:value="onChange"
      >
        <n-radio-button value="system" title="跟随系统">🖥</n-radio-button>
        <n-radio-button value="light" title="亮色">☀</n-radio-button>
        <n-radio-button value="dark" title="暗色">🌙</n-radio-button>
      </n-radio-group>
    </template>
    {{ tip }}
  </n-tooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { NRadioButton, NRadioGroup, NTooltip } from 'naive-ui'
import { useTheme, type ThemeMode } from '../composables/useTheme'

const { mode, isDark, setMode } = useTheme()

const tip = computed(() => {
  if (mode.value === 'system') {
    return `跟随系统（当前${isDark.value ? '暗色' : '亮色'}）`
  }
  return mode.value === 'dark' ? '暗色模式' : '亮色模式'
})

function onChange(v: string) {
  setMode(v as ThemeMode)
}
</script>
