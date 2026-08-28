<template>
  <div class="tsk" :aria-busy="true" aria-live="polite">
    <span class="sr-only">加载中</span>
    <!-- 表头一行略深,其后 rows 行等宽 —— 只求形状近似,不追求逐列对齐 -->
    <n-skeleton height="18px" width="24%" style="margin-bottom: 14px" :sharp="false" />
    <n-skeleton
      v-for="i in rows"
      :key="i"
      height="14px"
      :width="widths[i % widths.length]"
      style="margin-bottom: 12px"
      :sharp="false"
    />
  </div>
</template>

<script setup lang="ts">
import { NSkeleton } from 'naive-ui'

// 共享骨架:8 个表格页原本各自套一个整页 n-spin(内容全灰 + 转圈),
// 首屏改成骨架 —— 用户先看到版式,不是空白加转圈。
// 抽成组件而非每页复制:否则调一次行高要改 8 个文件。
withDefaults(defineProps<{ rows?: number }>(), { rows: 6 })

// 宽度错落,避免看起来像"整齐的假表格";纯展示,无语义
const widths = ['92%', '78%', '85%', '70%', '88%', '74%']
</script>

<style scoped>
.tsk {
  padding: 4px 0 8px;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
</style>
