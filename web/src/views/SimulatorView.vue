<template>
  <div>
    <div class="page-head">
      <h2>放置模拟工作台</h2>
      <p class="muted small" style="margin: 0">
        把 workload 放进当前集群快照，拖动权重观察决策如何变化。每次模拟都会落库为一条决策记录。
      </p>
    </div>

    <div class="grid-2">
      <!-- 输入 -->
      <section class="panel">
        <h3>Workload</h3>
        <div class="tpl-row">
          <button v-for="t in TEMPLATES" :key="t.key" type="button" class="tpl" @click="applyTemplate(t)">
            <strong>{{ t.label }}</strong>
            <span class="small muted">{{ t.desc }}</span>
          </button>
        </div>
        <div class="field-row">
          <label>名称<input v-model="workload.name" /></label>
          <label>命名空间<input v-model="workload.namespace" /></label>
        </div>
        <div class="field-row">
          <label>副本数<input v-model.number="workload.replicas" type="number" min="1" /></label>
          <label>CPU (m)<input v-model.number="workload.cpu" type="number" min="0" step="50" /></label>
          <label>内存 (MiB)<input v-model.number="workload.mem" type="number" min="0" step="64" /></label>
        </div>
        <div class="field-row">
          <label>
            租户
            <select v-model="tenantId">
              <option value="">（不指定）</option>
              <option v-for="t in tenants" :key="t.tenant_id" :value="t.tenant_id">{{ t.name }}</option>
            </select>
          </label>
          <label>
            策略
            <select v-model="policyId">
              <option v-for="p in policies" :key="p.id" :value="p.id">
                {{ p.name }} · {{ p.strategy }} · {{ p.fairness_mode === 'WEIGHTED_SHARE' ? '基线份额' : 'DRF' }}
              </option>
            </select>
          </label>
        </div>

        <h3 style="margin-top: 20px">
          权重
          <span class="chip" :class="custom ? 'warn' : 'info'">{{ custom ? '自定义' : '策略预设' }}</span>
          <button class="link" @click="applyPreset">恢复预设</button>
        </h3>
        <div class="tpl-row" style="margin: 0 0 10px">
          <button
            v-for="p in WEIGHT_PRESETS"
            :key="p.key"
            type="button"
            class="tpl"
            :title="p.desc"
            @click="applyWeightPreset(p)"
          >
            <strong>{{ p.label }}</strong>
          </button>
        </div>
        <div v-for="k in WEIGHT_KEYS" :key="k" class="slider">
          <span class="k">{{ WEIGHT_LABELS[k] }}</span>
          <input
            v-model.number="weightsPct[k]"
            type="range"
            min="0"
            max="100"
            step="1"
            @input="custom = true"
          />
          <span class="v">{{ weightsPct[k] }}%</span>
        </div>
        <div class="hint">
          合计 {{ sumPct }}%<span v-if="sumPct !== 100">，发送前按比例归一化到 100%</span>
        </div>

        <div class="actions">
          <button class="primary" :disabled="loading || !policyId" @click="run">
            {{ loading ? '模拟中…' : '运行模拟' }}
          </button>
          <label class="auto"><input v-model="auto" type="checkbox" /> 参数变化自动模拟</label>
        </div>
        <p class="hint">快捷键 <kbd>Ctrl</kbd>+<kbd>Enter</kbd>（macOS 为 <kbd>⌘</kbd>+<kbd>Enter</kbd>）立即运行模拟。</p>
        <p v-if="error" class="err">{{ error }}</p>

        <div style="margin-top: 16px; border-top: 1px solid var(--line); padding-top: 14px">
          <div class="small muted" style="margin-bottom: 6px">把当前权重另存为策略</div>
          <div style="display: flex; gap: 8px">
            <input v-model="saveName" placeholder="新策略名称" />
            <button :disabled="!saveName.trim() || !selectedPolicy" @click="saveAsPolicy">保存</button>
          </div>
          <p v-if="saveMsg" class="hint">{{ saveMsg }}</p>
        </div>
      </section>

      <!-- 结果 -->
      <section class="panel">
        <h3>
          决策结果
          <span v-if="result" class="chip" :class="result.status === 'OK' ? 'ok' : 'bad'">{{ result.status }}</span>
          <span v-if="loading" class="chip info">模拟中…</span>
          <span style="flex: 1"></span>
          <span v-if="result" class="seg">
            <button :class="{ active: view === 'rank' }" @click="setView('rank')">排名</button>
            <button :class="{ active: view === 'contrib' }" @click="setView('contrib')">贡献</button>
            <button :class="{ active: view === 'radar' }" @click="setView('radar')">权重</button>
          </span>
        </h3>

        <div v-if="!result" class="empty">运行一次模拟后在这里查看排名与评分拆解</div>

        <template v-else>
          <div class="chips-row">
            <span class="chip info">选中：{{ result.chosen_node || '无' }}</span>
            <span class="chip" :class="result.degraded ? 'warn' : 'ok'">
              {{ result.degraded ? '退化（指标缺失/过期）' : '指标正常' }}
            </span>
            <span class="chip">耗时 {{ result.evidence?.elapsed_ms ?? '—' }} ms</span>
            <span class="chip">节点 {{ result.evidence?.node_count ?? '—' }}</span>
            <span class="chip" :class="result.evidence?.metrics_known ? 'ok' : 'warn'">
              metrics {{ result.evidence?.metrics_known ? '已知' : '缺失' }}
            </span>
            <span class="chip">snapshot {{ shortID(result.snapshot_hash) }}</span>
          </div>
          <div v-if="result.warnings?.length" class="chips-row">
            <span v-for="w in result.warnings" :key="w" class="chip warn">{{ w }}</span>
          </div>

          <div v-if="history.length > 1" class="history-row" style="margin-bottom: 12px">
            <span class="small muted" style="align-self: center">最近决策</span>
            <button
              v-for="d in history"
              :key="d.id"
              type="button"
              class="history-chip"
              :class="{ active: d.id === result.id }"
              :title="`${d.workload?.name || '—'} · ${new Date(d.created_at).toLocaleString()}`"
              @click="loadHistory(d)"
            >
              {{ shortID(d.id) }} · {{ d.chosen_node || '无' }}
            </button>
          </div>

          <div v-if="history.length > 1" class="batch-compare">
            <div class="small muted" style="margin-bottom: 6px; display: flex; align-items: center; gap: 8px">
              <span>批量对比</span>
              <span class="chip">{{ compareIds.length }} 已选</span>
              <span style="flex: 1"></span>
              <button class="link" :disabled="!compareIds.length" @click="compareIds = []">清空选择</button>
            </div>
            <div class="history-row" style="margin-bottom: 8px">
              <label
                v-for="d in history"
                :key="'cmp-' + d.id"
                class="history-chip"
                :class="{ active: compareIds.includes(d.id) }"
              >
                <input type="checkbox" :checked="compareIds.includes(d.id)" @change="toggleCompare(d.id)" />
                {{ shortID(d.id) }}
              </label>
            </div>
            <div v-if="compareDecisions.length >= 2" class="scroll-y">
              <table>
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>Workload</th>
                    <th>选中节点</th>
                    <th class="num">榜首分</th>
                    <th class="num">过滤</th>
                    <th class="num">耗时</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="d in compareDecisions" :key="'row-' + d.id">
                    <td class="muted">{{ new Date(d.created_at).toLocaleTimeString() }}</td>
                    <td>{{ d.workload ? d.workload.namespace + '/' + d.workload.name : '—' }}</td>
                    <td>{{ d.chosen_node || '无' }}</td>
                    <td class="num">{{ topScore(d)?.score.toFixed(2) ?? '—' }}</td>
                    <td class="num muted">{{ d.filtered?.length || 0 }}</td>
                    <td class="num muted">{{ d.evidence?.elapsed_ms ?? '—' }} ms</td>
                    <td><span class="chip" :class="d.status === 'OK' ? 'ok' : 'bad'">{{ d.status }}</span></td>
                  </tr>
                </tbody>
              </table>
            </div>
            <p v-else class="hint" style="margin: 0">勾选至少两条决策进行批量对比</p>
          </div>

          <div ref="chartEl" class="chart"></div>

          <div class="apply-bar">
            <button
              class="primary"
              :disabled="applyState.loading || !!applyState.taskId || result.status !== 'OK' || !result.chosen_node"
              @click="applyDecision"
            >
              {{ applyState.loading ? '提交中…' : applyState.taskId ? '已提交' : '提交执行' }}
            </button>
            <span class="hint" style="margin: 0">把这条决策转成受控任务，进入待审批队列</span>
            <span v-if="applyState.msg" class="chip ok">{{ applyState.msg }}</span>
            <router-link v-if="applyState.taskId" to="/tasks" class="link">去任务页审批 →</router-link>
          </div>
          <p v-if="applyState.err" class="err">{{ applyState.err }}</p>

          <template v-if="baseline && comparison.length">
            <div class="apply-bar" style="margin-top: 12px">
              <span class="small muted">对比基线</span>
              <select v-model="baselineId" class="baseline-select" @change="baselinePinned = true">
                <option v-for="d in history" :key="d.id" :value="d.id">
                  {{ shortID(d.id) }} · {{ d.chosen_node || '无' }} · {{ new Date(d.created_at).toLocaleTimeString() }}
                </option>
              </select>
              <span class="chip" :class="baselineChanged ? 'warn' : 'ok'">
                {{ baselineChanged ? `${baseline.chosen_node} → ${result.chosen_node}` : '选中节点未变' }}
              </span>
              <span class="chip">过滤 {{ baseline.filtered?.length || 0 }} → {{ result.filtered?.length || 0 }}</span>
              <span style="flex: 1"></span>
              <button class="link" @click="clearBaseline">清除基线</button>
            </div>
            <div class="scroll-y" style="max-height: 240px">
              <table>
                <thead>
                  <tr>
                    <th>节点</th>
                    <th class="num">基线分</th>
                    <th class="num">本次分</th>
                    <th class="num">Δ</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="r in comparison" :key="r.node">
                    <td>
                      {{ r.node }}
                      <span v-if="r.node === result.chosen_node" class="chip ok">本次选中</span>
                      <span v-else-if="r.node === baseline.chosen_node" class="tag">基线选中</span>
                    </td>
                    <td class="num muted">{{ r.base != null ? r.base.toFixed(2) : '—' }}</td>
                    <td class="num">{{ r.cur != null ? r.cur.toFixed(2) : '—' }}</td>
                    <td
                      class="num cmp-delta"
                      :class="r.base != null && r.cur != null ? (r.delta > 0.005 ? 'up' : r.delta < -0.005 ? 'down' : '') : ''"
                    >
                      {{ r.base != null && r.cur != null ? (r.delta > 0 ? '+' : '') + r.delta.toFixed(2) : '—' }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>

          <template v-if="selectedScore">
            <h3 style="margin-top: 18px">
              节点明细 · {{ selectedScore.node }}
              <span class="chip" :class="selectedScore.node === result.chosen_node ? 'ok' : 'info'">
                排名 {{ selectedScore.rank }}
              </span>
              <span class="chip">综合 {{ selectedScore.score.toFixed(2) }}</span>
            </h3>
            <div class="contrib-list">
              <div v-for="c in contributions" :key="c.key" class="contrib-row">
                <span class="k">{{ c.label }}</span>
                <div class="bar-track">
                  <div class="bar-fill" :style="{ width: c.pct + '%', background: c.color }"></div>
                </div>
                <span class="v">{{ c.contribution.toFixed(1) }}</span>
                <span class="small muted w">{{ (c.weight * 100).toFixed(0) }}% × {{ c.score.toFixed(2) }}</span>
              </div>
            </div>
            <p class="hint">贡献 = 权重 × 维度得分，累加后与综合分（0–100）近似对应。点击图表或表格可切换节点。</p>
          </template>

          <h3 style="margin-top: 18px">评分拆解（0–1）</h3>
          <div class="scroll-y">
            <table>
              <thead>
                <tr>
                  <th>节点</th>
                  <th v-for="k in WEIGHT_KEYS" :key="k" class="num">{{ SHORT[k] }}</th>
                  <th class="num">综合</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="s in sortedScores"
                  :key="s.node"
                  class="clickable"
                  :class="{ selected: s.node === selectedNode }"
                  @click="selectedNode = s.node"
                >
                  <td>
                    <span class="rank-badge" :class="{ first: s.node === result.chosen_node }">{{ s.rank }}</span>
                    {{ s.node }}
                  </td>
                  <td v-for="k in WEIGHT_KEYS" :key="k" class="num muted">
                    {{ (s.breakdown?.[k] ?? 0).toFixed(3) }}
                  </td>
                  <td class="num"><strong>{{ s.score.toFixed(2) }}</strong></td>
                </tr>
              </tbody>
            </table>
          </div>

          <template v-if="result.filtered?.length">
            <h3 style="margin-top: 18px">被过滤节点</h3>
            <div class="chips-row">
              <span v-for="f in result.filtered" :key="f.node" class="chip bad">
                {{ f.node }} · {{ f.reason }}
              </span>
            </div>
          </template>
        </template>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  api,
  WEIGHT_KEYS,
  WEIGHT_LABELS,
  shortID,
  type Decision,
  type NodeScore,
  type Policy,
  type TenantItem,
  type WeightKey,
  type Weights
} from '../api'
import { useChart } from '../lib/chart'
import { toastBad, toastInfo, toastOk } from '../lib/feedback'

const SHORT: Record<WeightKey, string> = {
  s_cpu: 'CPU',
  s_mem: 'MEM',
  s_balance: 'BAL',
  s_pack: 'PACK',
  s_load: 'LOAD',
  s_fair: 'FAIR',
  s_frag: 'FRAG'
}

const DIM_COLORS: Record<WeightKey, string> = {
  s_cpu: '#7aa2f7',
  s_mem: '#27c281',
  s_balance: '#e7a33e',
  s_pack: '#b48ead',
  s_load: '#5fb3b3',
  s_fair: '#e2474a',
  s_frag: '#6b7a90'
}

interface SavedSimState {
  workload?: { name?: string; namespace?: string; replicas?: number; cpu?: number; mem?: number }
  weightsPct?: Partial<Record<WeightKey, number>>
  tenantId?: string
  policyId?: string
  custom?: boolean
  auto?: boolean
  view?: 'rank' | 'contrib' | 'radar'
  history?: Decision[]
  baselineId?: string
  compareIds?: string[]
}

function readSimState(): SavedSimState {
  try {
    return JSON.parse(localStorage.getItem('ka_sim_state') || '{}')
  } catch {
    return {}
  }
}

const DEFAULT_WEIGHTS: Record<WeightKey, number> = {
  s_cpu: 18,
  s_mem: 18,
  s_balance: 25,
  s_pack: 10,
  s_load: 15,
  s_fair: 10,
  s_frag: 4
}

const saved = readSimState()
const policies = ref<Policy[]>([])
const tenants = ref<TenantItem[]>([])
const policyId = ref(saved.policyId || '')
const tenantId = ref(saved.tenantId ?? '')
const tenantSaved = typeof saved.tenantId === 'string'
const loading = ref(false)
const auto = ref(saved.auto ?? true)
const error = ref('')
const result = ref<Decision | null>(null)
const view = ref<'rank' | 'contrib' | 'radar'>(saved.view || 'rank')
const selectedNode = ref('')

const workload = reactive({
  name: saved.workload?.name ?? 'demo-api',
  namespace: saved.workload?.namespace ?? 'team-a',
  replicas: saved.workload?.replicas ?? 1,
  cpu: saved.workload?.cpu ?? 500,
  mem: saved.workload?.mem ?? 256
})
const weightsPct = reactive<Record<WeightKey, number>>({ ...DEFAULT_WEIGHTS })
WEIGHT_KEYS.forEach(k => {
  const v = saved.weightsPct?.[k]
  if (typeof v === 'number' && v >= 0) weightsPct[k] = Math.round(v)
})
const custom = ref(saved.custom ?? false)
const saveName = ref('')
const saveMsg = ref('')
const applyState = reactive({ loading: false, msg: '', err: '', taskId: '' })

// 最近模拟结果与批量对比选择持久化到 localStorage，刷新后仍可回看。
const history = ref<Decision[]>(
  Array.isArray(saved.history) ? saved.history.filter(d => d && d.id).slice(0, 8) : []
)
const baselineId = ref(saved.baselineId || '')
const baselinePinned = ref(false)
const compareIds = ref<string[]>(Array.isArray(saved.compareIds) ? saved.compareIds : [])

if (history.value.length) {
  result.value = history.value[0]
  selectedNode.value =
    result.value.chosen_node || [...(result.value.scores || [])].sort((a, b) => b.score - a.score)[0]?.node || ''
}
compareIds.value = compareIds.value.filter(id => history.value.some(d => d.id === id))

let persistTimer: number | undefined

function persist() {
  const state = {
    workload: { ...workload },
    weightsPct: { ...weightsPct },
    tenantId: tenantId.value,
    policyId: policyId.value,
    custom: custom.value,
    auto: auto.value,
    view: view.value,
    history: history.value.slice(0, 8),
    baselineId: baselineId.value,
    compareIds: compareIds.value
  }
  try {
    localStorage.setItem('ka_sim_state', JSON.stringify(state))
  } catch {
    /* 忽略存储配额错误 */
  }
}

function schedulePersist() {
  if (persistTimer) clearTimeout(persistTimer)
  persistTimer = window.setTimeout(persist, 300)
}

interface Template {
  key: string
  label: string
  desc: string
  name: string
  namespace: string
  replicas: number
  cpu: number
  mem: number
}

const TEMPLATES: Template[] = [
  { key: 'web', label: 'Web 服务', desc: '3 副本 · 500m / 512Mi', name: 'web-api', namespace: 'team-a', replicas: 3, cpu: 500, mem: 512 },
  { key: 'batch', label: '批处理任务', desc: '1 副本 · 2C / 4Gi', name: 'batch-job', namespace: 'team-b', replicas: 1, cpu: 2000, mem: 4096 },
  { key: 'cache', label: '缓存节点', desc: '1 副本 · 1C / 1Gi', name: 'redis-cache', namespace: 'team-a', replicas: 1, cpu: 1000, mem: 1024 },
  { key: 'sidecar', label: '小 Sidecar', desc: '2 副本 · 100m / 64Mi', name: 'sidecar', namespace: 'team-c', replicas: 2, cpu: 100, mem: 64 }
]

interface WeightPreset {
  key: string
  label: string
  desc: string
  w: Partial<Record<WeightKey, number>>
}

const WEIGHT_PRESETS: WeightPreset[] = [
  {
    key: 'balance',
    label: '均衡优先',
    desc: '提高均衡维度，避免节点间倾斜',
    w: { s_cpu: 15, s_mem: 15, s_balance: 45, s_pack: 5, s_load: 10, s_fair: 5, s_frag: 5 }
  },
  {
    key: 'pack',
    label: '装箱优先',
    desc: '提高装箱与碎片维度，减少碎片',
    w: { s_cpu: 10, s_mem: 10, s_balance: 5, s_pack: 45, s_load: 10, s_fair: 5, s_frag: 15 }
  },
  {
    key: 'load',
    label: '负载感知',
    desc: '按实际用量负载均衡',
    w: { s_cpu: 10, s_mem: 10, s_balance: 10, s_pack: 5, s_load: 45, s_fair: 10, s_frag: 10 }
  },
  {
    key: 'fair',
    label: '公平优先',
    desc: '强化租户公平性惩罚',
    w: { s_cpu: 10, s_mem: 10, s_balance: 10, s_pack: 5, s_load: 5, s_fair: 50, s_frag: 10 }
  }
]

const baseline = computed(() => history.value.find(d => d.id === baselineId.value) || null)
const baselineChanged = computed(
  () => !!result.value && !!baseline.value && baseline.value.id !== result.value.id && baseline.value.chosen_node !== result.value.chosen_node
)

const compareDecisions = computed(() =>
  history.value
    .filter(d => compareIds.value.includes(d.id))
    .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
)

const topScore = (d: Decision) => [...(d.scores || [])].sort((a, b) => b.score - a.score)[0]

function toggleCompare(id: string) {
  compareIds.value = compareIds.value.includes(id)
    ? compareIds.value.filter(x => x !== id)
    : [...compareIds.value, id]
}

const comparison = computed(() => {
  const cur = result.value
  const base = baseline.value
  if (!cur || !base || cur.id === base.id) return []
  const rows = new Map<string, { node: string; base?: number; cur?: number }>()
  for (const s of base.scores || []) rows.set(s.node, { node: s.node, base: s.score })
  for (const s of cur.scores || []) {
    const row = rows.get(s.node) || { node: s.node }
    row.cur = s.score
    rows.set(s.node, row)
  }
  return [...rows.values()]
    .map(r => ({ ...r, delta: (r.cur ?? 0) - (r.base ?? 0) }))
    .sort((a, b) => (b.cur ?? 0) - (a.cur ?? 0))
})

const sumPct = computed(() => WEIGHT_KEYS.reduce((s, k) => s + weightsPct[k], 0))
const selectedPolicy = computed(() => policies.value.find(p => p.id === policyId.value))
const sortedScores = computed(() =>
  result.value ? [...result.value.scores].sort((a, b) => b.score - a.score) : []
)
const selectedScore = computed(() => {
  if (!result.value) return null
  const list = result.value.scores || []
  return (
    list.find(s => s.node === selectedNode.value) ||
    list.find(s => s.node === result.value?.chosen_node) ||
    sortedScores.value[0] ||
    null
  )
})

// 决策实际使用的权重优先取 evidence，缺失时回退到策略预设或本地滑杆。
const usedWeights = computed<Weights>(() => {
  const out = {} as Weights
  const ev = result.value?.evidence?.weights
  if (ev && typeof ev === 'object') {
    WEIGHT_KEYS.forEach(k => (out[k] = Number(ev[k] ?? 0)))
    return out
  }
  if (custom.value) return normalized()
  const pw = selectedPolicy.value?.weights
  WEIGHT_KEYS.forEach(k => (out[k] = pw?.[k] ?? 0))
  return out
})

const preset = computed<Weights>(() => {
  const out = {} as Weights
  const pw = selectedPolicy.value?.weights
  WEIGHT_KEYS.forEach(k => (out[k] = pw?.[k] ?? 0))
  return out
})

const contributions = computed(() => {
  const s = selectedScore.value
  if (!s) return []
  const w = usedWeights.value
  return WEIGHT_KEYS.map(k => {
    const score = Number(s.breakdown?.[k] ?? 0)
    return {
      key: k,
      label: WEIGHT_LABELS[k],
      color: DIM_COLORS[k],
      weight: w[k] ?? 0,
      score,
      contribution: (w[k] ?? 0) * score * 100,
      pct: Math.min(100, score * 100)
    }
  }).sort((a, b) => b.contribution - a.contribution)
})

const chartEl = ref<HTMLElement | null>(null)
const { render, onClick } = useChart(chartEl)
onClick((p: any) => {
  if (p?.name) selectedNode.value = String(p.name)
})

function applyPreset() {
  const p = selectedPolicy.value
  if (!p) return
  WEIGHT_KEYS.forEach(k => {
    weightsPct[k] = Math.round((p.weights?.[k] ?? 0) * 100)
  })
  custom.value = false
  schedulePersist()
}

function applyWeightPreset(p: WeightPreset) {
  WEIGHT_KEYS.forEach(k => {
    weightsPct[k] = p.w[k] ?? 0
  })
  custom.value = true
  toastInfo(`已应用权重预设：${p.label}`)
  schedulePersist()
}

function normalized(): Weights {
  const sum = sumPct.value || 1
  const out = {} as Weights
  WEIGHT_KEYS.forEach(k => {
    out[k] = weightsPct[k] / sum
  })
  return out
}

async function run() {
  if (!policyId.value) return
  loading.value = true
  error.value = ''
  const prevId = result.value?.id || ''
  try {
    const next = await api.simulate({
      workload: {
        name: workload.name,
        namespace: workload.namespace,
        replicas: workload.replicas,
        requests: { cpu_milli: workload.cpu, mem_bytes: workload.mem * 1024 * 1024 }
      },
      tenant_id: tenantId.value || undefined,
      policy_id: policyId.value,
      weights: custom.value ? normalized() : undefined
    })
    result.value = next
    applyState.msg = ''
    applyState.err = ''
    applyState.taskId = ''
    selectedNode.value = next.chosen_node || sortedScores.value[0]?.node || ''
    history.value = [next, ...history.value.filter(d => d.id !== next.id)].slice(0, 8)
    compareIds.value = compareIds.value.filter(id => history.value.some(d => d.id === id))
    if (!baselinePinned.value) {
      baselineId.value =
        (prevId && history.value.some(d => d.id === prevId) ? prevId : history.value.find(d => d.id !== next.id)?.id) || ''
    }
    schedulePersist()
    await nextTick()
    draw()
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

function applyTemplate(t: Template) {
  workload.name = t.name
  workload.namespace = t.namespace
  workload.replicas = t.replicas
  workload.cpu = t.cpu
  workload.mem = t.mem
  schedulePersist()
  toastInfo(`已应用模板：${t.label}`)
}

function loadHistory(d: Decision) {
  result.value = d
  selectedNode.value = d.chosen_node || d.scores?.[0]?.node || ''
  applyState.msg = ''
  applyState.err = ''
  applyState.taskId = ''
  error.value = ''
  schedulePersist()
  nextTick().then(draw)
  toastInfo(`已载入决策 ${shortID(d.id)}`)
}

function clearBaseline() {
  baselinePinned.value = false
  baselineId.value = ''
  schedulePersist()
}

function setView(v: 'rank' | 'contrib' | 'radar') {
  view.value = v
  schedulePersist()
  nextTick().then(draw)
}

const AXIS = '#8b93a7'
const LINE = '#242a33'

function draw() {
  const d = result.value
  if (!d) return
  if (view.value === 'radar') return drawRadar()
  const rows = [...d.scores].sort((a, b) => b.score - a.score)
  if (view.value === 'contrib') return drawContrib(rows)
  render({
    grid: { left: 88, right: 52, top: 12, bottom: 26 },
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    xAxis: {
      type: 'value',
      max: 100,
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'category',
      data: rows.map(r => r.node),
      inverse: true,
      axisLabel: { color: AXIS },
      axisLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'bar',
        barWidth: 18,
        data: rows.map(r => ({
          value: +r.score.toFixed(2),
          name: r.node,
          itemStyle: {
            color: r.node === d.chosen_node ? '#27c281' : '#7aa2f7',
            borderRadius: [0, 4, 4, 0]
          }
        })),
        label: {
          show: true,
          position: 'right',
          color: '#e6e8ec',
          formatter: (p: any) => Number(p.value).toFixed(1)
        }
      }
    ]
  })
}

function drawContrib(rows: NodeScore[]) {
  const w = usedWeights.value
  const labels = Object.fromEntries(
    contributions.value.map(c => [c.key, c.label + ' ' + (c.weight * 100).toFixed(0) + '%'])
  ) as Record<WeightKey, string>
  render({
    grid: { left: 88, right: 24, top: 10, bottom: 48 },
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      valueFormatter: (v: any) => Number(v).toFixed(1)
    },
    legend: {
      type: 'scroll',
      bottom: 0,
      textStyle: { color: AXIS, fontSize: 10 },
      itemWidth: 10,
      itemHeight: 8
    },
    xAxis: {
      type: 'value',
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'category',
      data: rows.map(r => r.node),
      inverse: true,
      axisLabel: { color: AXIS },
      axisLine: { lineStyle: { color: LINE } }
    },
    series: WEIGHT_KEYS.map(k => ({
      name: labels[k],
      type: 'bar' as const,
      stack: 'total',
      barWidth: 16,
      itemStyle: { color: DIM_COLORS[k] },
      emphasis: { focus: 'series' as const },
      data: rows.map(r => +(((w[k] ?? 0) * Number(r.breakdown?.[k] ?? 0) * 100).toFixed(2)))
    }))
  })
}

function drawRadar() {
  const current = WEIGHT_KEYS.map(k => Math.round(weightsPct[k]))
  const base = WEIGHT_KEYS.map(k => Math.round((preset.value[k] ?? 0) * 100))
  const max = Math.max(30, ...current, ...base) + 5
  render({
    tooltip: { trigger: 'item' },
    legend: { bottom: 0, textStyle: { color: AXIS } },
    radar: {
      center: ['50%', '48%'],
      radius: '62%',
      indicator: WEIGHT_KEYS.map(k => ({ name: WEIGHT_LABELS[k], max })),
      axisName: { color: AXIS, fontSize: 11 },
      splitLine: { lineStyle: { color: LINE } },
      splitArea: { areaStyle: { color: ['rgba(255,255,255,0.02)', 'transparent'] } },
      axisLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'radar',
        data: [
          {
            name: '当前权重',
            value: current,
            lineStyle: { color: '#7aa2f7', width: 2 },
            areaStyle: { color: 'rgba(122,162,247,0.18)' },
            itemStyle: { color: '#7aa2f7' }
          },
          {
            name: '策略预设',
            value: base,
            lineStyle: { color: '#8b93a7', width: 1.5, type: 'dashed' },
            areaStyle: { color: 'rgba(139,147,167,0.06)' },
            itemStyle: { color: '#8b93a7' }
          }
        ]
      }
    ]
  })
}

async function applyDecision() {
  const d = result.value
  if (!d) return
  applyState.loading = true
  applyState.msg = ''
  applyState.err = ''
  applyState.taskId = ''
  try {
    const t = await api.applyPlacement(d.id)
    applyState.taskId = t.id
    applyState.msg = `已创建任务 · ${t.status}`
    toastOk(`已提交执行，任务 ${shortID(t.id)} 待审批`)
  } catch (e: any) {
    applyState.err = e?.message || String(e)
    toastBad(`提交失败：${applyState.err}`)
  } finally {
    applyState.loading = false
  }
}

async function loadPolicies(select?: string) {
  policies.value = await api.listPolicies()
  const wanted = select || policyId.value
  if (wanted && policies.value.some(p => p.id === wanted)) policyId.value = wanted
  else policyId.value = (policies.value.find(p => p.is_default) || policies.value[0])?.id || ''
}

async function saveAsPolicy() {
  if (!selectedPolicy.value) return
  try {
    const created = await api.createPolicy({
      name: saveName.value.trim(),
      strategy: selectedPolicy.value.strategy,
      weights: normalized(),
      fairness_mode: selectedPolicy.value.fairness_mode || 'DRF',
      is_default: false
    })
    saveMsg.value = `已保存：${created.name}`
    saveName.value = ''
    await loadPolicies(created.id)
    schedulePersist()
    toastOk(`策略已保存：${created.name}`)
  } catch (e: any) {
    saveMsg.value = '保存失败：' + (e?.message || e)
    toastBad(saveMsg.value)
  }
}

let timer: number | undefined
let initializing = true
watch(
  [workload, weightsPct, policyId, tenantId],
  () => {
    saveMsg.value = ''
    schedulePersist()
    if (view.value === 'radar') drawRadar()
    if (initializing || !auto.value) return
    clearTimeout(timer)
    timer = window.setTimeout(run, 450)
  },
  { deep: true }
)

watch([custom, auto, view, baselineId], schedulePersist)

function onHotkey(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault()
    if (!loading.value && policyId.value) run()
  }
}

onMounted(async () => {
  window.addEventListener('keydown', onHotkey)
  try {
    await loadPolicies()
    const ts = (await api.tenants()).items || []
    tenants.value = ts
    if (!tenantSaved) tenantId.value = ts[0]?.tenant_id || ''
    else if (tenantId.value && !ts.some(t => t.tenant_id === tenantId.value)) {
      tenantId.value = ts[0]?.tenant_id || ''
    }
    if (!saved.weightsPct) applyPreset()

    if (history.value.length) {
      result.value = history.value[0]
      selectedNode.value = result.value?.chosen_node || sortedScores.value[0]?.node || ''
      await nextTick()
      draw()
    } else {
      await run()
    }
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    initializing = false
    persist()
  }
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onHotkey)
  if (timer) clearTimeout(timer)
  if (persistTimer) clearTimeout(persistTimer)
  persist()
})
</script>
