<template>
  <div class="admin-users-page">
    <div class="header">
      <h1>管理员用户</h1>
      <button @click="showCreateDialog = true" class="create-button">+ 新建用户</button>
    </div>

    <div v-if="error" class="error-banner">{{ error }}</div>

    <div v-if="loading" class="loading">加载中...</div>

    <table v-else-if="users.length > 0" class="users-table">
      <thead>
        <tr>
          <th>ID</th>
          <th>用户名</th>
          <th>角色</th>
          <th>状态</th>
          <th>登录失败次数</th>
          <th>创建时间</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="user in users" :key="user.id">
          <td>{{ user.id }}</td>
          <td>{{ user.username }}</td>
          <td>
            <span :class="['role-badge', user.role]">
              {{ user.role === 'root' ? '超级管理员' : '管理员' }}
            </span>
          </td>
          <td>
            <span :class="['status-badge', user.locked ? 'locked' : 'active']">
              {{ user.locked ? '已锁定' : '正常' }}
            </span>
          </td>
          <td>{{ user.login_attempts || 0 }}</td>
          <td>{{ formatDate(user.created_at) }}</td>
          <td class="actions">
            <button
              v-if="user.locked"
              @click="unlockUser(user)"
              class="action-button unlock"
              title="解锁"
            >
              解锁
            </button>
            <button
              @click="resetPassword(user)"
              class="action-button reset"
              title="重置密码"
            >
              重置密码
            </button>
            <button
              @click="deleteUser(user)"
              class="action-button delete"
              title="删除"
              :disabled="user.role === 'root'"
            >
              删除
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <div v-else class="empty">暂无用户</div>

    <!-- 创建用户对话框 -->
    <div v-if="showCreateDialog" class="dialog-overlay" @click.self="showCreateDialog = false">
      <div class="dialog">
        <h2>新建管理员用户</h2>
        <form @submit.prevent="createUser">
          <div class="form-group">
            <label>用户名</label>
            <input v-model="newUser.username" required />
          </div>
          <div class="form-group">
            <label>密码</label>
            <input v-model="newUser.password" type="password" required />
          </div>
          <div class="form-group">
            <label>角色</label>
            <select v-model="newUser.role" required>
              <option value="admin">管理员</option>
              <option value="root">超级管理员</option>
            </select>
          </div>
          <div class="dialog-actions">
            <button type="button" @click="showCreateDialog = false">取消</button>
            <button type="submit" class="primary">创建</button>
          </div>
        </form>
      </div>
    </div>

    <!-- 重置密码对话框 -->
    <div v-if="showResetDialog" class="dialog-overlay" @click.self="showResetDialog = false">
      <div class="dialog">
        <h2>重置密码: {{ currentUser?.username }}</h2>
        <form @submit.prevent="confirmResetPassword">
          <div class="form-group">
            <label>新密码</label>
            <input v-model="newPassword" type="password" required />
          </div>
          <div class="dialog-actions">
            <button type="button" @click="showResetDialog = false">取消</button>
            <button type="submit" class="primary">确认</button>
          </div>
        </form>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { adminUsersApi, type AdminUserInfo } from '../api/client'

const users = ref<AdminUserInfo[]>([])
const loading = ref(false)
const error = ref('')

const showCreateDialog = ref(false)
const showResetDialog = ref(false)
const currentUser = ref<AdminUserInfo | null>(null)
const newPassword = ref('')

const newUser = ref({
  username: '',
  password: '',
  role: 'admin',
})

async function loadUsers() {
  loading.value = true
  error.value = ''
  try {
    const resp = await adminUsersApi.list()
    users.value = resp.users || []
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '加载用户列表失败'
  } finally {
    loading.value = false
  }
}

async function createUser() {
  try {
    await adminUsersApi.create(newUser.value)
    showCreateDialog.value = false
    newUser.value = { username: '', password: '', role: 'admin' }
    await loadUsers()
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '创建用户失败'
  }
}

function resetPassword(user: AdminUserInfo) {
  currentUser.value = user
  newPassword.value = ''
  showResetDialog.value = true
}

async function confirmResetPassword() {
  if (!currentUser.value?.id) return
  try {
    await adminUsersApi.resetPassword(currentUser.value.id, { new_password: newPassword.value })
    showResetDialog.value = false
    currentUser.value = null
    newPassword.value = ''
    alert('密码重置成功')
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '重置密码失败'
  }
}

async function unlockUser(user: AdminUserInfo) {
  if (!user.id) return
  try {
    await adminUsersApi.update(user.id, { locked: false })
    await loadUsers()
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '解锁用户失败'
  }
}

async function deleteUser(user: AdminUserInfo) {
  if (!user.id) return
  if (user.role === 'root') {
    alert('无法删除超级管理员')
    return
  }
  if (!confirm(`确定要删除用户 ${user.username} 吗？`)) return

  try {
    await adminUsersApi.delete(user.id)
    await loadUsers()
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '删除用户失败'
  }
}

function formatDate(date?: string) {
  if (!date) return '-'
  return new Date(date).toLocaleString('zh-CN')
}

onMounted(() => {
  loadUsers()
})
</script>

<style scoped>
.admin-users-page {
  padding: 2rem;
  max-width: 1200px;
  margin: 0 auto;
}

.header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 2rem;
}

h1 {
  margin: 0;
  font-size: 1.8rem;
}

.create-button {
  padding: 0.75rem 1.5rem;
  background: #667eea;
  color: white;
  border: none;
  border-radius: 6px;
  cursor: pointer;
  font-size: 1rem;
}

.create-button:hover {
  background: #5568d3;
}

.error-banner {
  padding: 1rem;
  background: #fee;
  border: 1px solid #fcc;
  border-radius: 6px;
  color: #c33;
  margin-bottom: 1rem;
}

.loading, .empty {
  text-align: center;
  padding: 2rem;
  color: #666;
}

.users-table {
  width: 100%;
  border-collapse: collapse;
  background: white;
  border-radius: 8px;
  overflow: hidden;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.1);
}

.users-table th,
.users-table td {
  padding: 1rem;
  text-align: left;
  border-bottom: 1px solid #eee;
}

.users-table th {
  background: #f8f9fa;
  font-weight: 600;
  color: #555;
}

.role-badge,
.status-badge {
  padding: 0.25rem 0.75rem;
  border-radius: 4px;
  font-size: 0.875rem;
  font-weight: 500;
}

.role-badge.root {
  background: #ffeaa7;
  color: #d63031;
}

.role-badge.admin {
  background: #dfe6e9;
  color: #2d3436;
}

.status-badge.active {
  background: #d4edda;
  color: #155724;
}

.status-badge.locked {
  background: #f8d7da;
  color: #721c24;
}

.actions {
  display: flex;
  gap: 0.5rem;
}

.action-button {
  padding: 0.4rem 0.8rem;
  border: none;
  border-radius: 4px;
  cursor: pointer;
  font-size: 0.875rem;
}

.action-button.unlock {
  background: #d4edda;
  color: #155724;
}

.action-button.reset {
  background: #fff3cd;
  color: #856404;
}

.action-button.delete {
  background: #f8d7da;
  color: #721c24;
}

.action-button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.dialog-overlay {
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 1000;
}

.dialog {
  background: white;
  padding: 2rem;
  border-radius: 8px;
  width: 90%;
  max-width: 500px;
}

.dialog h2 {
  margin: 0 0 1.5rem 0;
}

.form-group {
  margin-bottom: 1rem;
}

.form-group label {
  display: block;
  margin-bottom: 0.5rem;
  font-weight: 500;
}

.form-group input,
.form-group select {
  width: 100%;
  padding: 0.75rem;
  border: 1px solid #ddd;
  border-radius: 4px;
  font-size: 1rem;
}

.dialog-actions {
  display: flex;
  justify-content: flex-end;
  gap: 0.5rem;
  margin-top: 1.5rem;
}

.dialog-actions button {
  padding: 0.75rem 1.5rem;
  border: 1px solid #ddd;
  border-radius: 6px;
  cursor: pointer;
  background: white;
}

.dialog-actions button.primary {
  background: #667eea;
  color: white;
  border: none;
}
</style>
