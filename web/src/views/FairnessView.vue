<template>
  <div class="stack">
    <div class="page-head">
      <h2>公平性与历史</h2>
      <p class="muted small" style="margin: 0">
        租户配额与实际占用对比、Jain 公平指数，以及任意两条决策的并排对比。
      </p>
    </div>

    <div class="grid-3">
      <div class="stat">
        <div class="label">Jain 公平指数</div>
        <div class="value">{{ overview?.jain_index != null ? overview.jain_index.toFixed(3) : '—' }}</div>
        <div class="sub">1.0 表示主导份额完全相同</div>
      </div>
      <div class="stat">
        <div class="label">集群可分配</div>
        <div class="value">{{ overview ? cores(overview.nodes.allocatable.cpu_milli) + ' C' : '—' }}</div>
        <div class="sub">{{ overview ? gib(overview.nodes.allocatable.mem_bytes) + ' GiB 内存' : '' }}</div>
      </div>
      <div class="stat">
        <div class="label">已请求</div>
        <div class="value">{{ overview ? cores(overview.nodes.requested.cpu_milli) + ' C' : '—' }}</div>
        <div class="sub">{{ overview ? gib(overview.nodes.requested.mem_bytes) + ' GiB 内存' : '' }}</div>
      </div>
    </div>

    <div class="grid-2">
      <section class="panel">
        <h3>配额占比（CPU）</h3>
        <div ref="donutEl" class="chart"></div>
      </section>
      <section class="panel">
        <h3>租户占用 vs 配额</h3>
        <div ref="barEl" class="chart"></div>
      </section>
    </div>

    <div class="grid-2">
      <section class="panel">
        <h3>配额推演</h3>
        <p class="muted small" style="margin: 0 0 12px">
          本地近似推演：按容量比例线性叠加主导份额，不写入任何数据。
        </p>
        <div class="field-row">
          <label>
            租户
            <select v-model="simTenant">
              <option v-for="t in tenants" :key="t.tenant_id" :value="t.tenant_id">{{ t.name }}</option>
            </select>
          </label>
          <label>追加 CPU (m)<input v-model.number="extraCpu" type="number" min="0" step="50" /></label>
          <label>追加内存 (MiB)<input v-model.number="extraMem" type="number" min="0" step="64" /></label>
        </div>
        <div v-if="simTenantInfo" class="kv">
          <span class="k">当前主导份额</span><span>{{ pct(simTenantInfo.jain_share) }}</span>
          <span class="k">公平份额</span><span>{{ pct(simTenantInfo.fair_share) }}</span>
          <span class="k">推演后份额</span>
          <span :class="projection.exceedsFair ? 'warn-text' : ''">{{ pct(projection.share) }}</span>
          <span class="k">Jain 指数</span>
          <span>{{ projection.jain.toFixed(3) }}（当前 {{ overview?.jain_index?.toFixed(3) ?? '—' }}）</span>
          <span class="k">配额余量</span>
          <span>
            CPU {{ simTenantInfo.hard.cpu_milli ? cores(simTenantInfo.hard.cpu_milli - simTenantInfo.requested.cpu_milli) + ' C' : '未设置' }} ·
            内存 {{ simTenantInfo.hard.mem_bytes ? gib(simTenantInfo.hard.mem_bytes - simTenantInfo.requested.mem_bytes) + ' GiB' : '未设置' }}
          </span>
        </div>
        <div v-if="projection.exceedsFair" class="err" style="margin-top: 10px">
          推演结果超过该租户的公平份额，若真实调度发生会被公平性维度惩罚。
        </div>
      </section>

      <section class="panel">
        <h3>决策对比</h3>
        <p class="muted small" style="margin: 0 0 10px">在下方历史里点「A」「B」各选一条，这里并排对比。</p>
        <div class="field-row">
          <label>
            决策 A
            <select v-model="pickA">
              <option value="">未选择</option>
              <option v-for="d in decisions" :key="d.id" :value="d.id">{{ decisionLabel(d) }}</option>
            </select>
          </label>
          <label>
            决策 B
            <select v-model="pickB">
              <option value="">未选择</option>
              <option v-for="d in decisions" :key="d.id" :value="d.id">{{ decisionLabel(d) }}</option>
            </select>
          </label>
        </div>
        <div v-if="diffRows.length" class="scroll-y">
          <div class="diff-row" style="border-bottom: 1px solid var(--line); color: var(--muted)">
            <span class="k">字段</span><span class="v">A</span><span class="v">B</span>
          </div>
          <div
            v-for="row in diffRows"
            :key="row.label"
            class="diff-row"
            :class="{ changed: row.changed }"
          >
            <span class="k">{{ row.label }}</span>
            <span class="v">{{ row.a }}</span>
            <span class="v">{{ row.b }}</span>
          </div>
        </div>
        <div v-else class="empty">选择两条决策后显示差异</div>
      </section>
    </div>

    <section class="panel">
      <h3>
        决策历史
        <span class="chip">{{ decisions.length }} 条</span>
        <button class="link" @click="reloadDecisions">刷新</button>
      </h3>
      <table>
        <thead>
          <tr>
            <th>时间</th>
            <th>Workload</th>
            <th>选中节点</th>
            <th>状态</th>
            <th>指标</th>
            <th class="num">耗时</th>
            <th>对比</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="d in decisions" :key="d.id">
            <td class="muted">{{ new Date(d.created_at).toLocaleString() }}</td>
            <td>{{ d.workload ? d.workload.namespace + '/' + d.workload.name : '—' }}</td>
            <td>{{ d.chosen_node || '—' }}</td>
            <td>
              <span class="chip" :class="d.degraded ? 'warn' : 'ok'">
                {{ d.status }}{{ d.degraded ? ' · 退化' : '' }}
              </span>
            </td>
            <td>
              <span class="chip" :class="d.evidence?.metrics_known ? 'ok' : 'warn'">
                {{ d.evidence?.metrics_known ? '已知' : '缺失' }}
              </span>
            </td>
            <td class="num muted">{{ d.evidence?.elapsed_ms ?? '—' }} ms</td>
            <td>
              <button class="link" @click="pickA = d.id">A</button>
              <button class="link" @click="pickB = d.id">B</button>
            </td>
          </tr>
        </tbody>
      </table>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  api,
  cores,
  gib,
  pct,
  shortID,
  type Decision,
  type Overview,
  type TenantItem
} from '../api'
import { useChart } from '../lib/chart'

const overview = ref<Overview | null>(null)
const tenants = ref<TenantItem[]>([])
const decisions = ref<Decision[]>([])

const simTenant = ref('')
const extraCpu = ref(500)
const extraMem = ref(256)
const pickA = ref('')
const pickB = ref('')

const donutEl = ref<HTMLElement | null>(null)
const barEl = ref<HTMLElement | null>(null)
const donut = useChart(donutEl)
const bar = useChart(barEl)

const simTenantInfo = computed(() => tenants.value.find(t => t.tenant_id === simTenant.value) || null)

// jain_share 是租户在 CPU/内存两维上的最大占用比例；追加量按容量比例线性折算成份额增量。
const projection = computed(() => {
  const t = simTenantInfo.value
  const total = overview.value?.nodes.allocatable
  if (!t || !total) return { share: 0, jain: 0, exceedsFair: false }
  const dCpu = total.cpu_milli ? extraCpu.value / total.cpu_milli : 0
  const dMem = total.mem_bytes ? (extraMem.value * 1024 * 1024) / total.mem_bytes : 0
  const delta = Math.max(dCpu, dMem)
  const share = t.jain_share + delta
  const shares = tenants.value.map(x => (x.tenant_id === t.tenant_id ? share : x.jain_share))
  const sum = shares.reduce((s, v) => s + v, 0)
  const sq = shares.reduce((s, v) => s + v * v, 0)
  const jain = sq > 0 ? (sum * sum) / (shares.length * sq) : 0
  return { share, jain, exceedsFair: t.fair_share > 0 && share > t.fair_share }
})

function decisionLabel(d: Decision) {
  const t = new Date(d.created_at).toLocaleTimeString()
  return `${t} · ${d.chosen_node || '无'} · ${shortID(d.policy_id)}`
}

const diffRows = computed(() => {
  const a = decisions.value.find(d => d.id === pickA.value)
  const b = decisions.value.find(d => d.id === pickB.value)
  if (!a || !b) return []
  const rows: { label: string; a: string; b: string; changed: boolean }[] = []
  const push = (label: string, va: any, vb: any) => {
    const sa = va == null ? '—' : String(va)
    const sb = vb == null ? '—' : String(vb)
    rows.push({ label, a: sa, b: sb, changed: sa !== sb })
  }
  push('状态', a.status, b.status)
  push('选中节点', a.chosen_node, b.chosen_node)
  push('退化', a.degraded ? '是' : '否', b.degraded ? '是' : '否')
  push('指标已知', a.evidence?.metrics_known ? '是' : '否', b.evidence?.metrics_known ? '是' : '否')
  push('耗时', (a.evidence?.elapsed_ms ?? '—') + ' ms', (b.evidence?.elapsed_ms ?? '—') + ' ms')
  push('快照', shortID(a.snapshot_hash), shortID(b.snapshot_hash))
  push('策略', shortID(a.policy_id), shortID(b.policy_id))
  const wa = a.evidence?.weights || {}
  const wb = b.evidence?.weights || {}
  for (const k of ['s_load', 's_fair', 's_balance', 's_pack']) {
    push(k, Number(wa[k] ?? 0).toFixed(2), Number(wb[k] ?? 0).toFixed(2))
  }
  const topA = [...(a.scores || [])].sort((x, y) => y.score - x.score)[0]
  const topB = [...(b.scores || [])].sort((x, y) => y.score - x.score)[0]
  push(
    '最高分',
    topA ? `${topA.node} ${topA.score.toFixed(1)}` : '—',
    topB ? `${topB.node} ${topB.score.toFixed(1)}` : '—'
  )
  return rows
})

function draw() {
  if (tenants.value.length) {
    const cpuData = tenants.value.map(t => ({
      name: t.name,
      value: t.hard.cpu_milli || t.requested.cpu_milli
    }))
    donut.render({
      tooltip: { trigger: 'item', formatter: ({ name, value, percent }: any) => `${name}：${(value / 1000).toFixed(2)} C（${percent}%）` },
      legend: { bottom: 0, textStyle: { color: '#8b93a7' } },
      series: [
        {
          type: 'pie',
          radius: ['46%', '68%'],
          center: ['50%', '44%'],
          itemStyle: { borderColor: '#171b22', borderWidth: 2 },
          label: { color: '#8b93a7', formatter: '{b}' },
          data: cpuData,
          color: ['#7aa2f7', '#27c281', '#e7a33e', '#b48ead', '#e2474a']
        }
      ]
    })

    bar.render({
      tooltip: { trigger: 'axis' },
      legend: { data: ['已请求', '配额'], textStyle: { color: '#8b93a7' }, top: 0 },
      grid: { left: 56, right: 20, top: 34, bottom: 30 },
      xAxis: {
        type: 'category',
        data: tenants.value.map(t => t.name),
        axisLabel: { color: '#8b93a7', interval: 0, rotate: tenants.value.length > 4 ? 25 : 0 },
        axisLine: { lineStyle: { color: '#242a33' } }
      },
      yAxis: {
        type: 'value',
        name: 'C',
        nameTextStyle: { color: '#8b93a7' },
        axisLabel: { color: '#8b93a7' },
        splitLine: { lineStyle: { color: '#242a33' } }
      },
      series: [
        {
          name: '已请求',
          type: 'bar',
          data: tenants.value.map(t => +(t.requested.cpu_milli / 1000).toFixed(2)),
          itemStyle: { color: '#7aa2f7', borderRadius: [3, 3, 0, 0] }
        },
        {
          name: '配额',
          type: 'bar',
          data: tenants.value.map(t => +(t.hard.cpu_milli / 1000).toFixed(2)),
          itemStyle: { color: '#3c4453', borderRadius: [3, 3, 0, 0] }
        }
      ]
    })
  }
}

async function reloadDecisions() {
  decisions.value = (await api.decisions(30)).items || []
}

onMounted(async () => {
  try {
    overview.value = await api.overview()
  } catch {
    overview.value = null
  }
  try {
    tenants.value = (await api.tenants()).items || []
    simTenant.value = tenants.value[0]?.tenant_id || ''
  } catch {
    tenants.value = []
  }
  try {
    await reloadDecisions()
  } catch {
    decisions.value = []
  }
  draw()
})
</script>
