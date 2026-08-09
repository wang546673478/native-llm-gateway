// 后端 API 客户端
import axios from 'axios'

const client = axios.create({
  baseURL: '/api/v1',
  timeout: 10_000,
})

// P-provider-vendor: /providers 按 vendor 聚合 — 一个厂商多个注册名(协议面)
export interface ProviderNameInfo {
  name: string
  protocol: string
}

export interface KeyPoolStatus {
  provider_name: string
  total_keys: number
  active_keys: number
  cooling_keys: number
  disabled_keys: number
}

export interface CircuitBreakerInfo {
  name: string
  state: string
  failures_in_window: number
}

export interface VendorInfo {
  vendor: string
  names: ProviderNameInfo[]
  models: string[]
  key_pool?: KeyPoolStatus | null
  circuit_breaker?: CircuitBreakerInfo | null
}

export interface ProvidersResponse {
  vendors: VendorInfo[]
  count: number
}

// /providers/registered — 比 /providers 轻量,只含 name/protocol/loaded/models
// AccessLogs.vue 用它做 Provider/Model 下拉(spec §0)
export interface RegisteredProvider {
  name: string
  protocol: string
  loaded: boolean
  models: string[]
}

export interface RegisteredProvidersResp {
  providers: RegisteredProvider[]
  count: number
}

export interface AliasInfo {
  Alias: string
  Strategy: string
  Providers: Array<{ Name: string; Model: string; Priority: number; Weight: number }>
}

export interface RoutingResp {
  aliases: Record<string, AliasInfo>
  count: number
  // P-catch-all: 兜底路由 — 未知模型名按此规则路由(可能为 null = 未配置)
  catch_all?: AliasInfo | null
}

export interface GatewayKeyInfo {
  name: string
  allowed_models: string[]
  rpm: number
  tpm: number
  // 以下字段 /keys list 实际也返回(见 backend auth/keys_handler.go)——
  // 之前 GatewayKeyInfo 太窄,view 用 KeyView 强转;这里补全为唯一真实契约
  key: string
  providers: string[]
  provider_key_ids: number[]
  default_model?: string
  enabled: boolean
  id?: number
  created_at?: string
}

export interface KeysResp {
  keys: GatewayKeyInfo[]
  count: number
}

// ProviderKeyView provider API-key 视图 —— 单一类型源。
// 之前 Keys.vue / ProviderKeys.vue 各定义一份(Keys.vue 的还是旧版缺字段子集),
// 后端改字段只改这处理应、两端 UI 同步。须与 backend ProviderKeyView 对齐。
export interface ProviderKeyView {
  id: number
  provider_name: string
  name: string
  key_masked: string
  enabled: boolean
  // 运行时状态 — "ACTIVE" / "COOLING" / "QUOTA_EXCEEDED"(P-no-disabled:无 DISABLED)
  status: string
  // 计费来源 — "token_plan" / "api" / "free"
  billing_source: string
  created_at: string
  updated_at: string
  remaining: number
  last_polled_at: string | null
  // 数值类型 — "percent" / "currency" / ""(空按 currency)
  quota_kind: 'percent' | 'currency' | ''
  // 该 key 允许的协议面(逗号分隔,空 = 全部)
  protocols: string
  // per-key 熔断状态
  circuit_open: boolean
  circuit_state: string
}

export interface AccessLog {
  id: number
  trace_id: string
  created_at: string
  gateway_key_id: string
  gateway_key_name: string
  method: string
  path: string
  client_ip: string
  user_agent: string
  requested_model: string
  final_model: string
  provider_name: string
  // P-key: 实际发请求的上游 key(成功 = 最终成功 key;失败 = 最后尝试的 key)
  provider_key_id: string
  provider_key_name: string
  protocol: string
  is_stream: boolean
  status_code: number
  error_type: string
  latency_ms: number
  req_body_path: string
  req_body_size: number
  resp_body_path: string
  resp_body_size: number
}

export interface AccessLogListResp {
  records: AccessLog[]
  total: number
  limit: number
  offset: number
}

export interface AccessLogDetailResp {
  metadata: AccessLog
  req_body: string
  resp_body: string
  req_body_trunc: boolean
  resp_body_trunc: boolean
}

export interface AccessLogStatsResp {
  total_24h: number
  errors_24h: number
  active_keys: number
}

// AggregateResult P65: 通用聚合列(独立类型,只含聚合指标)
//   - dashboard.total 用此类型(不含 provider/model)
//   - 之前误用 AggregateRow 表达 total,本次拆分清楚
export interface AggregateResult {
  total_requests: number
  total_input_tokens: number
  total_output_tokens: number
  total_tokens: number
  total_cost: number
  avg_latency_ms: number
  error_count: number
}

// AggregateRow P65: 一行聚合(只按 Model 维度,去 provider_name)
//   - 之前 GROUP BY (provider_name, model_id),卡片按 provider 分类
//   - 现在 GROUP BY model_id,卡片按 model 分类
//   - Provider 信息走单独的 modelProviders 端点按需查
export interface AggregateRow {
  model_id: string
  total_requests: number
  total_input_tokens: number
  total_output_tokens: number
  total_tokens: number
  total_cost: number
  avg_latency_ms: number
  error_count: number
}

export interface DashboardResp {
  window: string
  // P65: total 是独立 AggregateResult 类型(只含聚合列)
  total: AggregateResult
  // P65: 重命名 by_provider_model → by_model
  by_model: AggregateRow[]
  // P47: 按 billing_source 聚合(token_plan / api / free)
  by_billing_source: Array<{
    billing_source: string
    total_requests: number
    total_input_tokens: number
    total_output_tokens: number
    total_tokens: number
    total_cost: number
    avg_latency_ms: number
    error_count: number
  }>
  providers_count: number
  keypools: Array<{
    provider_name: string
    total_keys: number
    active_keys: number
    cooling_keys: number
    disabled_keys: number
    // P-quota-balance: 上游 quota polling 聚合 — spec §6.2 dashboard
    // "Pool 列表里每行显示 QuotaKnownSum"
    quota_polled_keys: number
    quota_known_sum: number
    // P-quota-display: 池级 dominant kind — 全部 percent → "percent",否则 "currency"
    // (percent 池不可汇总,前端显示 —)
    quota_kind: 'percent' | 'currency' | ''
  }>
}

// ModelProviderRow P65: 给定 model,列出调用过的 provider + 请求数
export interface ModelProviderRow {
  provider_name: string
  request_count: number
}

export const api = {
  providers: () => client.get<ProvidersResponse>('/providers').then(r => r.data),
  providersRegistered: () =>
    client.get<RegisteredProvidersResp>('/providers/registered').then(r => r.data),
  // GET /providers/:name — 单注册名详情(无前端消费方,保留类型对齐后端)
  provider: (name: string) =>
    client
      .get<{
        name: string
        protocol: string
        models: string[]
        key_pool?: KeyPoolStatus | null
        circuit_breaker?: CircuitBreakerInfo | null
      }>(`/providers/${name}`)
      .then(r => r.data),
  routing: () => client.get<RoutingResp>('/routing').then(r => r.data),
  // P-route-order: Level 2/3 priority 改写(GET 读改写,PUT 整体替换 → 热生效)
  routeOrder: {
    get: (scope: 'provider' | 'key', provider?: string) =>
      client
        .get<{ scope: string; provider: string; order: string[] }>('/routing/order', {
          params: { scope, provider: provider ?? '' },
        })
        .then(r => r.data),
    put: (scope: 'provider' | 'key', provider: string, billingSource: string, order: string[]) =>
      client
        .put<{ ok: boolean; scope: string; provider: string; order: string[] }>('/routing/order', {
          scope,
          provider,
          billing_source: billingSource,
          order,
        })
        .then(r => r.data),
  },
  keys: {
    list: () => client.get<KeysResp>('/keys').then(r => r.data),
    // 以下 CRUD 从 Keys.vue 的 raw axios 收编到 client.ts(单一 endpoint 源)
    create: (body: { name: string; key?: string; enabled?: boolean; allowed_models?: string[]; rpm?: number; tpm?: number }) =>
      client.post<{ key?: string }>('/keys', body).then(r => r.data),
    update: (name: string, body: Record<string, unknown>) =>
      client.put(`/keys/${encodeURIComponent(name)}`, body).then(r => r.data),
    delete: (name: string) => client.delete(`/keys/${encodeURIComponent(name)}`).then(r => r.data),
    // GatewayKeyInfo 目前缺 key 明文;create 返回的 key 由调用方处理
  },
  // P30: provider api-keys CRUD(从 ProviderKeys.vue / Keys.vue 的 raw axios 收编)
  providerKeys: {
    list: (providerName: string) =>
      client
        .get<{ keys: ProviderKeyView[]; count: number; provider: string }>(
          `/providers/${encodeURIComponent(providerName)}/api-keys`,
        )
        .then(r => r.data),
    create: (providerName: string, body: { name?: string; key: string; enabled?: boolean; billing_source?: string; protocols?: string }) =>
      client
        .post<ProviderKeyView>(`/providers/${encodeURIComponent(providerName)}/api-keys`, body)
        .then(r => r.data),
    delete: (providerName: string, id: number | string) =>
      client
        .delete(`/providers/${encodeURIComponent(providerName)}/api-keys/${id}`)
        .then(r => r.data),
  },
  dashboard: () => client.get<DashboardResp>('/dashboard').then(r => r.data),
  aggregateUsage: (params?: { start?: string; end?: string }) =>
    client.get<{ rows: AggregateRow[]; count: number }>('/usage/aggregate', { params }).then(r => r.data),
  // P66: usage 返回 total/limit/offset,支持后端分页
  usage: (params?: { start?: string; end?: string; limit?: number; offset?: number }) =>
    client.get<{ records: any[]; total: number; limit: number; offset: number }>('/usage', { params }).then(r => r.data),
  // P65: 给定 model,查 provider 分布
  modelProviders: (modelId: string, params?: { start?: string; end?: string }) =>
    client.get<{ model_id: string; providers: ModelProviderRow[]; count: number }>(
      `/usage/by_model/${encodeURIComponent(modelId)}/providers`,
      { params },
    ).then(r => r.data),
  accessLogs: {
    list: (params?: Record<string, string | number>) =>
      client.get<AccessLogListResp>('/access-logs', { params }).then(r => r.data),
    detail: (id: number) =>
      client.get<AccessLogDetailResp>(`/access-logs/${id}/detail`).then(r => r.data),
    stats: () =>
      client.get<AccessLogStatsResp>('/access-logs/stats').then(r => r.data),
    // export URL —— 单一 endpoint 源(AccessLogs.vue 原 window.open('/api/v1/access-logs/export?...'))
    exportUrl: (params?: Record<string, string | number>) => {
      const qs = new URLSearchParams()
      if (params) {
        for (const [k, v] of Object.entries(params)) {
          if (v !== undefined && v !== null && v !== '') qs.set(k, String(v))
        }
      }
      const q = qs.toString()
      return q ? `/access-logs/export?${q}` : '/access-logs/export'
    },
  },
  // P-quota-balance: 后端 quota runtime config(目前只含 warn_threshold_pct)
  // ProviderKeys.vue 用它做余额颜色阈值,避免硬编码。
  quotaConfig: () => client.get<{ warn_threshold_pct: number }>('/config/quota').then(r => r.data),
}
