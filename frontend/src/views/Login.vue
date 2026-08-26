<template>
  <div class="login-container">
    <div class="login-card">
      <h1>LLM Gateway</h1>
      <h2>管理员登录</h2>

      <div v-if="disabled" class="feature-disabled">
        <p>⚠️ 管理员认证功能未启用</p>
        <p class="hint">请在 config.yaml 中设置 admin_auth.enabled: true</p>
      </div>

      <form v-else @submit.prevent="handleLogin" class="login-form">
        <div class="form-group">
          <label for="username">用户名</label>
          <input
            id="username"
            v-model="username"
            type="text"
            autocomplete="username"
            required
            :disabled="loading"
          />
        </div>

        <div class="form-group">
          <label for="password">密码</label>
          <input
            id="password"
            v-model="password"
            type="password"
            autocomplete="current-password"
            required
            :disabled="loading"
          />
        </div>

        <div v-if="error" class="error-message">
          {{ error }}
        </div>

        <button type="submit" :disabled="loading" class="login-button">
          {{ loading ? '登录中...' : '登录' }}
        </button>
      </form>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import { authApi } from '../api/client'

const router = useRouter()
const authStore = useAuthStore()

const username = ref('')
const password = ref('')
const loading = ref(false)
const error = ref('')
const disabled = ref(false)

async function handleLogin() {
  if (!username.value || !password.value) {
    error.value = '请输入用户名和密码'
    return
  }

  loading.value = true
  error.value = ''

  try {
    const resp = await authApi.login({
      username: username.value,
      password: password.value,
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
.login-container {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
}

.login-card {
  background: white;
  padding: 2.5rem;
  border-radius: 12px;
  box-shadow: 0 10px 40px rgba(0, 0, 0, 0.2);
  width: 100%;
  max-width: 400px;
}

h1 {
  margin: 0 0 0.5rem 0;
  font-size: 1.8rem;
  color: #333;
  text-align: center;
}

h2 {
  margin: 0 0 2rem 0;
  font-size: 1.2rem;
  font-weight: 400;
  color: #666;
  text-align: center;
}

.feature-disabled {
  padding: 1.5rem;
  background: #fff3cd;
  border: 1px solid #ffc107;
  border-radius: 8px;
  text-align: center;
}

.feature-disabled p {
  margin: 0.5rem 0;
  color: #856404;
}

.feature-disabled .hint {
  font-size: 0.9rem;
  color: #666;
}

.login-form {
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}

.form-group {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

label {
  font-size: 0.9rem;
  font-weight: 500;
  color: #555;
}

input {
  padding: 0.75rem;
  border: 1px solid #ddd;
  border-radius: 6px;
  font-size: 1rem;
  transition: border-color 0.2s;
}

input:focus {
  outline: none;
  border-color: #667eea;
}

input:disabled {
  background: #f5f5f5;
  cursor: not-allowed;
}

.error-message {
  padding: 0.75rem;
  background: #fee;
  border: 1px solid #fcc;
  border-radius: 6px;
  color: #c33;
  font-size: 0.9rem;
}

.login-button {
  padding: 0.875rem;
  background: #667eea;
  color: white;
  border: none;
  border-radius: 6px;
  font-size: 1rem;
  font-weight: 500;
  cursor: pointer;
  transition: background 0.2s;
}

.login-button:hover:not(:disabled) {
  background: #5568d3;
}

.login-button:disabled {
  background: #ccc;
  cursor: not-allowed;
}
</style>
