// 与 arbiter-api 契约对应的最小类型集合（只声明前端用到的字段）。

export interface Resources {
  cpu_milli: number
  mem_bytes: number
}

export type WeightKey = 's_cpu' | 's_mem' | 's_balance' | 's_pack' | 's_load' | 's_fair' | 's_frag'

export type Weights = Record<WeightKey, number>

export interface Policy {
  id: string
  name: string
  strategy: string
  weights: Weights
  is_default: boolean
}

export interface NodeScore {
  node: string
  rank: number
  score: number
  breakdown: Record<string, number>
}

export interface FilteredNode {
  node: string
  reason: string
  evidence?: Record<string, unknown>
}

export interface Decision {
  id: string
  status: string
  workload?: {
    name: string
    namespace: string
    replicas: number
    requests: Resources
  }
  tenant_id?: string | null
  policy_id: string
  chosen_node?: string | null
  snapshot_hash: string
  scores: NodeScore[]
  filtered: FilteredNode[]
  degraded: boolean
  warnings?: string[]
  evidence?: Record<string, any>
  created_at: string
}

export interface TenantItem {
  tenant_id: string
  name: string
  requested: Resources
  hard: Resources
  fair_share: number
  jain_share: number
}

export interface NodeItem {
  name: string
  ready: boolean
  allocatable: Resources
  requested: Resources
  usage: Resources
  usage_known: boolean
  pod_count: number
  topology: Record<string, string>
}

export interface NodeListResp {
  as_of: string
  metrics_at?: string | null
  nodes: NodeItem[]
}

export interface Overview {
  as_of: string
  nodes: { count: number; allocatable: Resources; requested: Resources }
  tenant_count: number
  jain_index?: number | null
  slo_summary: any
}

export interface SimulateInput {
  workload: {
    name: string
    namespace: string
    replicas: number
    requests: Resources
  }
  tenant_id?: string
  policy_id: string
  weights?: Weights
}

const BASE = () => localStorage.getItem('ka_api_base') || ''

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE() + path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) }
  })
  if (res.status === 204) return undefined as T
  const body = await res.json().catch(() => null)
  if (!res.ok || (body && body.code !== 0)) {
    const msg = body?.message || body?.error?.message || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return (body ? body.data : undefined) as T
}

export const api = {
  health: () => fetch(BASE() + '/healthz').then(r => r.json()),

  listPolicies: () => call<Policy[]>('/api/v1/policies'),
  createPolicy: (p: Omit<Policy, 'id'>) =>
    call<Policy>('/api/v1/policies', { method: 'POST', body: JSON.stringify(p) }),
  updatePolicy: (p: Policy) =>
    call<Policy>(`/api/v1/policies/${p.id}`, { method: 'PUT', body: JSON.stringify(p) }),
  deletePolicy: (id: string) => call<void>(`/api/v1/policies/${id}`, { method: 'DELETE' }),

  simulate: (input: SimulateInput) =>
    call<Decision>('/api/v1/placement/simulate', { method: 'POST', body: JSON.stringify(input) }),

  overview: () => call<Overview>('/api/v1/capacity/overview'),
  nodes: () => call<NodeListResp>('/api/v1/capacity/nodes'),
  tenants: () => call<{ items: TenantItem[] }>('/api/v1/capacity/tenants'),
  decisions: (limit = 20) =>
    call<{ items: Decision[]; next_cursor?: string | null }>(
      `/api/v1/placement/decisions?limit=${limit}`
    ),
  decision: (id: string) => call<Decision>(`/api/v1/placement/decisions/${id}`)
}

// ---------- 展示用格式化 ----------

export const WEIGHT_LABELS: Record<WeightKey, string> = {
  s_cpu: 'CPU 余量',
  s_mem: '内存余量',
  s_balance: '均衡',
  s_pack: '装箱',
  s_load: '实际负载',
  s_fair: '公平性',
  s_frag: '碎片'
}

export const WEIGHT_KEYS: WeightKey[] = [
  's_cpu',
  's_mem',
  's_balance',
  's_pack',
  's_load',
  's_fair',
  's_frag'
]

export const cores = (milli: number) => (milli / 1000).toFixed(2)
export const gib = (bytes: number) => (bytes / 1024 / 1024 / 1024).toFixed(2)
export const mib = (bytes: number) => Math.round(bytes / 1024 / 1024)
export const pct = (v: number) => (v * 100).toFixed(0) + '%'

export function shortID(id?: string | null) {
  return id ? id.slice(0, 8) : '—'
}

export function ageSeconds(iso?: string | null) {
  if (!iso) return null
  return Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
}
