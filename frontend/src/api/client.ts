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

export interface AggregateRow {
  provider_name: string
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
  total: AggregateRow
  by_provider_model: AggregateRow[]
  providers_count: number
  keypools: Array<{
    provider_name: string
    total_keys: number
    active_keys: number
    cooling_keys: number
    disabled_keys: number
  }>
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
}
