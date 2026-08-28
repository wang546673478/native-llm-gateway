<!-- 统计卡片 —— 单一职责:展示一个「图标 + 标签 + 数值」的指标。
     不取数、不格式化(调用方传格式化好的字符串),换视觉只改这里。
     tone 决定强调色语义(primary/info/warning/error),与 tokens.css 对齐。 -->
<template>
  <div class="stat-card" :class="`tone-${tone}`">
    <div class="stat-head">
      <span class="stat-icon">{{ icon }}</span>
      <span class="stat-label">{{ label }}</span>
    </div>
    <div class="stat-value">{{ value }}</div>
    <div v-if="hint" class="stat-hint">{{ hint }}</div>
  </div>
</template>

<script setup lang="ts">
withDefaults(
  defineProps<{
    label: string
    value: string
    icon?: string
    /** 强调色语义;error 用于「错误数 > 0」这类需要警示的指标 */
    tone?: 'primary' | 'info' | 'warning' | 'error'
    hint?: string
  }>(),
  { icon: '', tone: 'primary', hint: '' },
)
</script>

<style scoped>
.stat-card {
  position: relative;
  padding: var(--sp-5) var(--sp-5) var(--sp-4);
  background: var(--s-card);
  border: 1px solid var(--b-1);
  border-radius: var(--r-md);
  box-shadow: var(--sh-1);
  overflow: hidden;
  transition: box-shadow var(--tr-fast), transform var(--tr-fast);
}

.stat-card:hover {
  box-shadow: var(--sh-2);
  transform: translateY(-1px);
}

/* 顶部渐变条:用强调色做视觉锚点,比整卡染色更克制 */
.stat-card::before {
  content: '';
  position: absolute;
  inset: 0 0 auto 0;
  height: 3px;
  background: linear-gradient(90deg, var(--accent), transparent);
}

.tone-primary { --accent: var(--c-primary); --accent-soft: var(--c-primary-soft); }
.tone-info    { --accent: var(--c-info);    --accent-soft: var(--c-info-soft);    }
.tone-warning { --accent: var(--c-warning); --accent-soft: var(--c-warning-soft); }
.tone-error   { --accent: var(--c-error);   --accent-soft: var(--c-error-soft);   }

.stat-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-bottom: var(--sp-3);
}

.stat-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  border-radius: var(--r-sm);
  background: var(--accent-soft);
  font-size: 14px;
  line-height: 1;
  flex-shrink: 0;
}

.stat-label {
  font-size: 13px;
  color: var(--t-3);
  font-weight: 500;
}

.stat-value {
  font-size: 30px;
  font-weight: 650;
  line-height: 1.15;
  color: var(--accent);
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.02em;
  word-break: break-all;
}

.stat-hint {
  margin-top: var(--sp-2);
  font-size: 12px;
  color: var(--t-3);
}
</style>
