<template>
  <div class="login-container">
    <n-card class="login-card" :bordered="false">
      <div class="brand">
        <div class="brand-mark">⚡</div>
        <h1>LLM Gateway</h1>
        <p class="subtitle">管理员登录</p>
      </div>

      <n-alert
        v-if="disabled"
        type="warning"
        title="管理员认证功能未启用"
        :bordered="false"
      >
        请在 config.yaml 中设置 <code class="mono">admin_auth.enabled: true</code>
      </n-alert>

      <n-form
        v-else
        ref="formRef"
        :model="form"
        :show-label="true"
        @submit.prevent="handleLogin"
      >
        <n-form-item label="用户名" path="username">
          <n-input
            v-model:value="form.username"
            placeholder="请输入用户名"
            :disabled="loading"
            autocomplete="username"
            @keyup.enter="handleLogin"
          />
        </n-form-item>

        <n-form-item label="密码" path="password">
          <n-input
            v-model:value="form.password"
            type="password"
            show-password-on="click"
            placeholder="请输入密码"
            :disabled="loading"
            autocomplete="current-password"
            @keyup.enter="handleLogin"
          />
        </n-form-item>

        <n-alert
          v-if="error"
          type="error"
          :bordered="false"
          class="login-error"
        >
          {{ error }}
        </n-alert>

        <n-button
          type="primary"
          block
          size="large"
          attr-type="submit"
          :loading="loading"
          :disabled="loading"
          @click="handleLogin"
        >
          {{ loading ? '登录中…' : '登录' }}
        </n-button>
      </n-form>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { NAlert, NButton, NCard, NForm, NFormItem, NInput } from 'naive-ui'
import { useAuthStore } from '../stores/auth'
import { authApi } from '../api/client'

const router = useRouter()
const authStore = useAuthStore()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const error = ref('')
const disabled = ref(false)

// 错误分支与改造前逐条对应(feature_disabled / 后端 message / 401 / 429 / 兜底)—
// 这次只改表现层,鉴权行为不动。
async function handleLogin() {
  if (loading.value) return
  if (!form.username || !form.password) {
    error.value = '请输入用户名和密码'
    return
  }

  loading.value = true
  error.value = ''

  try {
    const resp = await authApi.login({
      username: form.username,
      password: form.password,
    })

    authStore.setToken(resp.token)

    // 获取用户信息
    const userInfo = await authApi.me()
    authStore.setUser({ username: userInfo.username, role: userInfo.role })

    router.push('/')
  } catch (err: any) {
    if (err.response?.data?.error?.type === 'feature_disabled') {
      disabled.value = true
      error.value = ''
    } else if (err.response?.data?.error?.message) {
      error.value = err.response.data.error.message
    } else if (err.response?.status === 401) {
      error.value = '用户名或密码错误'
    } else if (err.response?.status === 429) {
      error.value = '登录尝试次数过多，账户已被锁定'
    } else {
      error.value = '登录失败，请稍后重试'
    }
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
/* 背景由主色衍生(不再是与全站无关的孤立紫色)。
   两层 radial-gradient 叠在页面底色上 —— 暗色模式下 token 自动换值。 */
.login-container {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: var(--sp-4);
  background:
    radial-gradient(circle at 15% 20%, var(--c-primary-soft) 0%, transparent 45%),
    radial-gradient(circle at 85% 80%, var(--c-info-soft) 0%, transparent 45%),
    var(--s-page);
}

.login-card {
  width: 100%;
  max-width: 400px;
  border-radius: var(--r-lg);
  box-shadow: var(--sh-3);
}

.brand {
  text-align: center;
  margin-bottom: var(--sp-6);
}

.brand-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 48px;
  height: 48px;
  margin-bottom: var(--sp-3);
  border-radius: var(--r-md);
  background: var(--c-primary-soft);
  font-size: 24px;
  line-height: 1;
}

.brand h1 {
  margin: 0;
  font-size: 22px;
  font-weight: 650;
  letter-spacing: -0.02em;
  color: var(--t-1);
}

.subtitle {
  margin: var(--sp-1) 0 0;
  font-size: 14px;
  color: var(--t-3);
}

.login-error {
  margin-bottom: var(--sp-4);
}
</style>
