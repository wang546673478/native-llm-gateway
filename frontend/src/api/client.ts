// 后端 API 客户端
import axios from 'axios'

const client = axios.create({
  baseURL: '/api/v1',
  timeout: 10_000,
})

export interface ProviderInfo {
  name: string
  protocol: string
  models: string[]
  loaded?: boolean
  key_pool?: {
    provider_name: string
    total_keys: number
    active_keys: number
    cooling_keys: number
    disabled_keys: number
  }
  circuit_breaker?: {
    name: string
    state: string
    failures_in_window: number
  }
}

export interface ProvidersResp {
  providers: ProviderInfo[]
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
}

export interface GatewayKeyInfo {
  name: string
  allowed_models: string[]
  rpm: number
  tpm: number
}

export interface KeysResp {
  keys: GatewayKeyInfo[]
  count: number
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
  }>
}

// ModelProviderRow P65: 给定 model,列出调用过的 provider + 请求数
export interface ModelProviderRow {
  provider_name: string
  request_count: number
}

export const api = {
  providers: () => client.get<ProvidersResp>('/providers').then(r => r.data),
  provider: (name: string) => client.get<ProviderInfo>(`/providers/${name}`).then(r => r.data),
  routing: () => client.get<RoutingResp>('/routing').then(r => r.data),
  keys: () => client.get<KeysResp>('/keys').then(r => r.data),
  dashboard: () => client.get<DashboardResp>('/dashboard').then(r => r.data),
  aggregateUsage: (params?: { start?: string; end?: string }) =>
    client.get<{ rows: AggregateRow[]; count: number }>('/usage/aggregate', { params }).then(r => r.data),
  usage: (params?: { start?: string; end?: string; limit?: number }) =>
    client.get<{ records: any[]; count: number }>('/usage', { params }).then(r => r.data),
  // P65: 给定 model,查 provider 分布
  modelProviders: (modelId: string, params?: { start?: string; end?: string }) =>
    client.get<{ model_id: string; providers: ModelProviderRow[]; count: number }>(
      `/usage/by_model/${encodeURIComponent(modelId)}/providers`,
      { params },
    ).then(r => r.data),
}
