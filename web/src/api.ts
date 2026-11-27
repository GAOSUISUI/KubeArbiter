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
  // DRF：节点级 DRF 试算；WEIGHTED_SHARE：M5 基线算法（用于 A/B 对比）。
  fairness_mode: string
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

export type TaskStatus =
  | 'PENDING'
  | 'PENDING_APPROVAL'
  | 'QUEUED'
  | 'RUNNING'
  | 'RETRYING'
  | 'SUCCEEDED'
  | 'FAILED'
  | 'CANCELLED'

export type TaskType =
  | 'PLACEMENT_APPLY'
  | 'PREEMPT'
  | 'SCALE'
  | 'RELEASE_PROMOTE'
  | 'RELEASE_ABORT'
  | 'RECOVERY'
  | 'CONFIG_ROLLBACK'

export interface Task {
  id: string
  type: TaskType | string
  status: TaskStatus | string
  params: Record<string, any>
  retry_count: number
  max_retries: number
  error: string | null
  error_code: number | null
  created_by: string
  approved_by: string | null
  created_at: string
  started_at: string | null
  finished_at: string | null
}

export interface TaskLog {
  ts: string
  level: string
  message: string
}

export interface TaskDetail extends Task {
  logs: TaskLog[]
}

export interface TaskPage {
  items: Task[]
  next_cursor?: string | null
}

export interface AuditLog {
  id: number
  actor: string
  action: string
  object_type: string
  object_id: string | null
  before: Record<string, any> | null
  after: Record<string, any> | null
  trace_id: string | null
  ts: string
}

export interface AuditPage {
  items: AuditLog[]
  next_cursor?: string | null
}

export interface TaskQuery {
  status?: string
  type?: string
  limit?: number
  cursor?: string
}

export interface AuditQuery {
  object_type?: string
  object_id?: string
  limit?: number
  cursor?: string
}

// ---------- AIOps ----------

export interface HealthComponent {
  code: string
  message: string
  delta: number
}

export interface HealthScore {
  scope: 'CLUSTER' | 'NODE' | 'TENANT' | string
  name: string
  score: number | null
  components: HealthComponent[]
}

export interface Incident {
  id: string
  target: string
  namespace: string
  type: string
  severity: 'INFO' | 'WARN' | 'CRITICAL' | string
  status: string
  first_seen: string
  resolved_at: string | null
}

export interface IncidentFact {
  stage: string
  at: string
  object?: string
  summary: string
  values?: Record<string, any>
}

export interface IncidentInference {
  root_cause: string
  confidence: string
  evidence: string[]
  counter_evidence?: string[]
}

export interface IncidentDiagnosis {
  id: string
  incident_id: string
  facts: IncidentFact[]
  inferences: IncidentInference[]
  created_at: string
}

export interface RecoveryAction {
  id: string
  action: string
  task_id: string | null
  approved_by: string | null
  result: string
  created_at: string
}

export interface IncidentDetail extends Incident {
  diagnosis: IncidentDiagnosis | null
  recovery_actions: RecoveryAction[]
}

export interface IncidentPage {
  items: Incident[]
  next_cursor?: string | null
}

export interface IncidentQuery {
  status?: string
  type?: string
  severity?: string
  namespace?: string
  target?: string
  limit?: number
  cursor?: string
}

export interface MonitorPoint {
  ts: string
  cpu_milli: number
  mem_bytes: number
}

export interface MonitorNodeSeries {
  name: string
  points: MonitorPoint[]
}

export interface MonitorSeries {
  window: string
  step_seconds: number
  retention_seconds: number
  degraded: boolean
  nodes: MonitorNodeSeries[]
  cluster: MonitorPoint[]
}

export interface MonitorNode {
  name: string
  ready: boolean
  unschedulable: boolean
  conditions: Record<string, boolean>
  allocatable: Resources
  requested: Resources
  usage: Resources
  usage_known: boolean
  pod_count: number
  health: HealthScore
}

export interface MonitorTenant {
  id: string
  name: string
  quota: Resources
  requested: Resources
  quota_ratio: number
  share: number
  fair_share: number
  health: HealthScore
}

export interface MonitorOverview {
  taken_at: string
  degraded: boolean
  metrics_at: string | null
  metrics_age_seconds: number | null
  retention_seconds: number
  cluster: {
    nodes_total: number
    nodes_ready: number
    cpu_alloc_milli: number
    cpu_requested_milli: number
    cpu_usage_milli: number
    mem_alloc_bytes: number
    mem_requested_bytes: number
    mem_usage_bytes: number
    pods: number
    health: HealthScore
  }
  nodes: MonitorNode[]
  tenants: MonitorTenant[]
  incidents: { open: number; critical: number; warn: number; info: number }
}

// ---------- SLO ----------

export interface SLODefinitionInput {
  service: string
  sli_type: string
  target: number
  window_seconds: number
  latency_threshold_ms?: number | null
}

export interface SLOStatus {
  service: string
  sli_type: string
  target: number
  current: number
  budget_remaining: number
  total_requests: number
  publishing_allowed: boolean
  degraded: boolean
}

export interface BurnRateWindow {
  long_window: string
  short_window: string
  threshold: number
  burn_rate: number
  firing: boolean
  completeness: number
}

export interface SLODetail {
  service: string
  sli_type: string
  target: number
  latency_threshold_ms?: number | null
  windows: BurnRateWindow[]
  degraded: boolean
}

export interface SLOBudget {
  service: string
  window: string
  budget_total: number
  budget_consumed: number
  budget_remaining: number
  total_requests: number
  publishing_allowed: boolean
  degraded: boolean
  completeness: number
  action: string
  reasons?: string[]
}

const BASE = () => localStorage.getItem('ka_api_base') || ''

/** 当前调用方身份（X-Actor）；为空时后端按 anonymous 处理。 */
export const actorName = () => localStorage.getItem('ka_actor') || ''

function requestHeaders(extra?: HeadersInit, idempotent = false): Record<string, string> {
  const h: Record<string, string> = { 'Content-Type': 'application/json' }
  const actor = actorName()
  if (actor) h['X-Actor'] = actor
  if (idempotent) h['X-Idempotency-Key'] = newIdempotencyKey()
  return { ...h, ...((extra as Record<string, string>) || {}) }
}

function newIdempotencyKey() {
  const c = globalThis.crypto as Crypto | undefined
  if (c && typeof c.randomUUID === 'function') return c.randomUUID()
  return `web-${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function query(params: Record<string, string | number | undefined | null>) {
  const q = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined && v !== null && v !== '') q.set(k, String(v))
  })
  const s = q.toString()
  return s ? `?${s}` : ''
}

type CallError = Error & { status?: number; code?: number }

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const method = (init?.method || 'GET').toUpperCase()
  const retriable = method === 'GET'

  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(BASE() + path, {
        ...init,
        headers: requestHeaders(init?.headers)
      })
      if (res.status === 204) return undefined as T
      const body = await res.json().catch(() => null)
      if (!res.ok || (body && body.code !== 0)) {
        const msg = body?.message || body?.error?.message || `HTTP ${res.status}`
        const err = new Error(msg) as CallError
        err.status = res.status
        if (body && typeof body.code === 'number') err.code = body.code
        throw err
      }
      return (body ? body.data : undefined) as T
    } catch (e) {
      const err = e as CallError
      // GET 幂等：网络错误或 5xx 时最多自动重试一次，避免瞬时抖动直接抛给页面。
      const transient = !err.status || err.status >= 500
      if (retriable && attempt === 0 && transient) {
        await new Promise(r => setTimeout(r, 350))
        continue
      }
      if (!err.status) err.status = 0
      throw err
    }
  }
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
  decision: (id: string) => call<Decision>(`/api/v1/placement/decisions/${id}`),

  // 把一条已落库决策转成受控任务（进入待审批）。
  applyPlacement: (decisionId: string) =>
    call<Task>('/api/v1/placement/apply', {
      method: 'POST',
      headers: requestHeaders(undefined, true),
      body: JSON.stringify({ decision_id: decisionId, allow_auto_execute: false })
    }),

  tasks: (q: TaskQuery = {}) =>
    call<TaskPage>(
      '/api/v1/tasks' + query({ status: q.status, type: q.type, limit: q.limit, cursor: q.cursor })
    ),
  task: (id: string) => call<TaskDetail>(`/api/v1/tasks/${encodeURIComponent(id)}`),
  approveTask: (id: string) =>
    call<Task>(`/api/v1/tasks/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      headers: requestHeaders(undefined, true),
      body: '{}'
    }),
  cancelTask: (id: string) =>
    call<Task>(`/api/v1/tasks/${encodeURIComponent(id)}/cancel`, {
      method: 'POST',
      headers: requestHeaders(undefined, true),
      body: '{}'
    }),

  auditLogs: (q: AuditQuery = {}) =>
    call<AuditPage>(
      '/api/v1/audit-logs' +
        query({
          object_type: q.object_type,
          object_id: q.object_id,
          limit: q.limit,
          cursor: q.cursor
        })
    ),

  // ---------- AIOps ----------
  incidents: (q: IncidentQuery = {}) =>
    call<IncidentPage>(
      '/api/v1/incidents' +
        query({
          status: q.status,
          type: q.type,
          severity: q.severity,
          namespace: q.namespace,
          target: q.target,
          limit: q.limit,
          cursor: q.cursor
        })
    ),
  incident: (id: string) => call<IncidentDetail>(`/api/v1/incidents/${encodeURIComponent(id)}`),
  runDiagnosis: (body: { incident_id?: string; target?: string; namespace?: string }) =>
    call<IncidentDiagnosis>('/api/v1/diagnosis/run', {
      method: 'POST',
      headers: requestHeaders(undefined, true),
      body: JSON.stringify(body)
    }),
  monitorOverview: () => call<MonitorOverview>('/api/v1/monitor/overview'),
  monitorSeries: (window = '1h') =>
    call<MonitorSeries>('/api/v1/monitor/series' + query({ window })),

  // ---------- SLO ----------
  sloList: () => call<SLOStatus[]>('/api/v1/slo'),
  sloDetail: (service: string) =>
    call<SLODetail>(`/api/v1/slo/${encodeURIComponent(service)}`),
  sloBudget: (service: string, window?: string) =>
    call<SLOBudget>(
      `/api/v1/slo/${encodeURIComponent(service)}/budget` + (window ? query({ window }) : '')
    ),
  createSLO: (def: SLODefinitionInput) =>
    call<SLODefinitionInput>('/api/v1/slo', {
      method: 'POST',
      headers: requestHeaders(undefined, true),
      body: JSON.stringify(def)
    }),
  updateSLO: (service: string, sliType: string, def: SLODefinitionInput) =>
    call<SLODefinitionInput>(`/api/v1/slo/${encodeURIComponent(service)}/${encodeURIComponent(sliType)}`, {
      method: 'PUT',
      headers: requestHeaders(undefined, true),
      body: JSON.stringify(def)
    }),
  deleteSLO: (service: string, sliType: string) =>
    call<void>(`/api/v1/slo/${encodeURIComponent(service)}/${encodeURIComponent(sliType)}`, {
      method: 'DELETE'
    })
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

/** 状态对应的 chip 色系，和页面现有 chip 类名保持一致。 */
export function statusTone(status: string) {
  switch (status) {
    case 'SUCCEEDED':
    case 'OK':
      return 'ok'
    case 'FAILED':
    case 'CANCELLED':
    case 'ERROR':
      return 'bad'
    case 'PENDING_APPROVAL':
    case 'RETRYING':
      return 'warn'
    case 'RUNNING':
    case 'QUEUED':
      return 'info'
    default:
      return ''
  }
}

export const INCIDENT_LABELS: Record<string, string> = {
  CRASH_LOOP: '容器反复重启',
  IMAGE_PULL_BACKOFF: '镜像拉取失败',
  READINESS_FAIL: '就绪检查失败',
  OOM_KILLED: '内存溢出',
  DOWNSTREAM_TIMEOUT: '下游超时',
  EMPTY_ENDPOINTS: '无可用端点',
  CONFIG_ERROR: '配置异常',
  NODE_NOT_READY: '节点未就绪',
  NODE_PRESSURE: '节点资源压力',
  NODE_HIGH_USAGE: '节点高负载',
  METRICS_STALE: '指标过期',
  POD_PENDING_LONG: '调度等待过久',
  POD_READINESS_FAIL: 'Pod 未就绪',
  QUOTA_NEAR: '配额临界'
}

export function severityTone(severity: string) {
  switch (severity) {
    case 'CRITICAL':
      return 'bad'
    case 'WARN':
      return 'warn'
    case 'INFO':
      return 'info'
    default:
      return ''
  }
}

export function healthTone(score?: number | null) {
  if (score == null) return ''
  if (score >= 90) return 'ok'
  if (score >= 70) return 'warn'
  return 'bad'
}

export function fmtTime(iso?: string | null) {
  return iso ? new Date(iso).toLocaleString() : '—'
}

/** 任务耗时：有 started_at 才算，未结束按当前时间滚动。 */
export function fmtDuration(from?: string | null, to?: string | null) {
  if (!from) return '—'
  const end = to ? new Date(to).getTime() : Date.now()
  const sec = Math.max(0, (end - new Date(from).getTime()) / 1000)
  if (sec < 60) return sec.toFixed(1) + 's'
  if (sec < 3600) return (sec / 60).toFixed(1) + 'min'
  return (sec / 3600).toFixed(1) + 'h'
}

export function jsonText(v: unknown, indent = 2) {
  try {
    return JSON.stringify(v, null, indent)
  } catch {
    return String(v)
  }
}
