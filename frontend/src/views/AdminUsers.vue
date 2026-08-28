<template>
  <div>
    <n-card>
      <template #header>
        <n-space align="center" justify="space-between" style="width: 100%">
          <span>管理员用户</span>
          <n-button type="primary" size="small" @click="showCreateDialog = true">
            + 新建用户
          </n-button>
        </n-space>
      </template>

      <n-alert
        v-if="error"
        type="error"
        closable
        :bordered="false"
        style="margin-bottom: 16px"
        @close="error = ''"
      >
        {{ error }}
      </n-alert>

      <table-skeleton v-if="firstLoad" :rows="4" />
      <n-data-table
        v-else
        :columns="columns"
        :data="users"
        :bordered="false"
        :pagination="false"
        :row-class-name="rowClassName"
      />
    </n-card>

    <!-- 创建用户 -->
    <n-modal
      v-model:show="showCreateDialog"
      preset="card"
      title="新建管理员用户"
      style="max-width: 460px"
      :bordered="false"
    >
      <n-form :model="newUser">
        <n-form-item label="用户名">
          <n-input v-model:value="newUser.username" placeholder="登录用户名" />
        </n-form-item>
        <n-form-item label="密码">
          <n-input
            v-model:value="newUser.password"
            type="password"
            show-password-on="click"
            placeholder="登录密码"
          />
        </n-form-item>
        <n-form-item label="角色">
          <n-select v-model:value="newUser.role" :options="roleOptions" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showCreateDialog = false">取消</n-button>
          <n-button type="primary" :loading="submitting" @click="createUser">
            创建
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 重置密码 -->
    <n-modal
      v-model:show="showResetDialog"
      preset="card"
      :title="`重置密码：${currentUser?.username ?? ''}`"
      style="max-width: 460px"
      :bordered="false"
    >
      <n-form>
        <n-form-item label="新密码">
          <n-input
            v-model:value="newPassword"
            type="password"
            show-password-on="click"
            placeholder="输入新密码"
          />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showResetDialog = false">取消</n-button>
          <n-button type="primary" :loading="submitting" @click="confirmResetPassword">
            确认
          </n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { h, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NCard, NDataTable, NForm, NFormItem, NInput, NModal,
  NSelect, NSpace, NTag, useDialog, useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import { adminUsersApi, type AdminUserInfo } from '../api/client'
import { fmtDateTime } from '../utils/time'
import { useFirstLoad } from '../composables/useFirstLoad'
import TableSkeleton from '../components/TableSkeleton.vue'

const message = useMessage()
const dialog = useDialog()

const users = ref<AdminUserInfo[]>([])
const loading = ref(true)
const { firstLoad } = useFirstLoad(loading)
const submitting = ref(false)
const error = ref('')

const showCreateDialog = ref(false)
const showResetDialog = ref(false)
const currentUser = ref<AdminUserInfo | null>(null)
const newPassword = ref('')

const newUser = reactive({ username: '', password: '', role: 'admin' })

const roleOptions = [
  { label: '管理员', value: 'admin' },
  { label: '超级管理员', value: 'root' },
]

// 锁定行标红色条 —— 与 ProviderKeys/Keys 同一套 row-* 约定
function rowClassName(row: AdminUserInfo): string {
  return row.locked ? 'row-error' : 'row-ok'
}

const columns: DataTableColumns<AdminUserInfo> = [
  { title: 'ID', key: 'id', width: 70 },
  {
    title: '用户名',
    key: 'username',
    render: row => h('span', { class: 'mono' }, row.username),
  },
  {
    title: '角色',
    key: 'role',
    render: row =>
      h(
        NTag,
        { type: row.role === 'root' ? 'warning' : 'default', size: 'small', bordered: false },
        () => (row.role === 'root' ? '超级管理员' : '管理员'),
      ),
  },
  {
    title: '状态',
    key: 'locked',
    render: row =>
      h(
        NTag,
        { type: row.locked ? 'error' : 'success', size: 'small', bordered: false },
        () => (row.locked ? '已锁定' : '正常'),
      ),
  },
  { title: '登录失败次数', key: 'login_attempts', render: row => row.login_attempts || 0 },
  { title: '创建时间', key: 'created_at', render: row => fmtDateTime(row.created_at) },
  {
    title: '操作',
    key: 'actions',
    render: row =>
      h(NSpace, { size: 8 }, () => [
        row.locked
          ? h(
              NButton,
              { size: 'tiny', secondary: true, type: 'success', onClick: () => unlockUser(row) },
              () => '解锁',
            )
          : null,
        h(
          NButton,
          { size: 'tiny', secondary: true, type: 'warning', onClick: () => resetPassword(row) },
          () => '重置密码',
        ),
        h(
          NButton,
          {
            size: 'tiny',
            secondary: true,
            type: 'error',
            disabled: row.role === 'root',
            onClick: () => deleteUser(row),
          },
          () => '删除',
        ),
      ]),
  },
]

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
  if (!newUser.username || !newUser.password) {
    message.warning('用户名和密码不能为空')
    return
  }
  submitting.value = true
  try {
    await adminUsersApi.create({ ...newUser })
    showCreateDialog.value = false
    newUser.username = ''
    newUser.password = ''
    newUser.role = 'admin'
    message.success('用户已创建')
    await loadUsers()
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '创建用户失败'
  } finally {
    submitting.value = false
  }
}

function resetPassword(user: AdminUserInfo) {
  currentUser.value = user
  newPassword.value = ''
  showResetDialog.value = true
}

async function confirmResetPassword() {
  if (!currentUser.value?.id) return
  if (!newPassword.value) {
    message.warning('请输入新密码')
    return
  }
  submitting.value = true
  try {
    await adminUsersApi.resetPassword(currentUser.value.id, { new_password: newPassword.value })
    showResetDialog.value = false
    currentUser.value = null
    newPassword.value = ''
    message.success('密码重置成功')
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '重置密码失败'
  } finally {
    submitting.value = false
  }
}

async function unlockUser(user: AdminUserInfo) {
  if (!user.id) return
  try {
    await adminUsersApi.update(user.id, { locked: false })
    message.success(`已解锁 ${user.username}`)
    await loadUsers()
  } catch (err: any) {
    error.value = err.response?.data?.error?.message || '解锁用户失败'
  }
}

// 原生 confirm/alert 换 naive-ui dialog —— 原生弹窗不受主题控制,暗色下突兀
function deleteUser(user: AdminUserInfo) {
  if (!user.id) return
  if (user.role === 'root') {
    message.error('无法删除超级管理员')
    return
  }
  dialog.warning({
    title: '删除用户',
    content: `确定要删除用户 ${user.username} 吗？此操作不可撤销。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await adminUsersApi.delete(user.id!)
        message.success('用户已删除')
        await loadUsers()
      } catch (err: any) {
        error.value = err.response?.data?.error?.message || '删除用户失败'
      }
    },
  })
}

onMounted(loadUsers)
</script>
