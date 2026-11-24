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
        <h3>
          配额占比
          <span style="flex: 1"></span>
          <span class="seg">
            <button :class="{ active: metric === 'cpu' }" @click="metric = 'cpu'">CPU</button>
            <button :class="{ active: metric === 'mem' }" @click="metric = 'mem'">内存</button>
          </span>
        </h3>
        <div ref="donutEl" class="chart"></div>
        <p class="hint">点击扇区可在下方推演中选中该租户。</p>
      </section>
      <section class="panel">
        <h3>租户占用 vs 配额 · {{ metricLabel }}</h3>
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
        <p class="muted small" style="margin: 0 0 10px">
          在下方历史里点「A」「B」各选一条，或用「详情」单独查看。雷达图对比两条决策榜首节点的维度得分。
        </p>
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
        <div v-show="diffRows.length" ref="compareEl" class="chart sm" style="margin-top: 8px"></div>
        <div v-if="!diffRows.length" class="empty">选择两条决策后显示差异</div>
      </section>
    </div>

    <section class="panel">
      <h3>
        决策分布与耗时
        <span class="chip">{{ decisions.length }} 条</span>
      </h3>
      <div class="grid-2">
        <div>
          <div class="small muted" style="margin-bottom: 6px">选中节点分布</div>
          <div ref="distEl" class="chart sm"></div>
        </div>
        <div>
          <div class="small muted" style="margin-bottom: 6px">决策耗时（点击圆点查看详情）</div>
          <div ref="timeEl" class="chart sm"></div>
        </div>
      </div>
    </section>

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
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="d in decisions" :key="d.id" class="clickable" @click="openDecision(d.id)">
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
              <button class="link" @click.stop="openDecision(d.id)">详情</button>
              <button class="link" @click.stop="pickA = d.id">A</button>
              <button class="link" @click.stop="pickB = d.id">B</button>
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <!-- 决策详情 -->
    <div v-show="detailOpen" class="modal-mask" @click.self="closeDetail">
      <div class="modal wide">
        <h3>
          决策详情
          <span v-if="detail" class="chip">{{ shortID(detail.id) }}</span>
          <span v-if="detail" class="chip" :class="detail.status === 'OK' ? 'ok' : 'bad'">{{ detail.status }}</span>
        </h3>
        <p v-if="detailLoading" class="muted small">加载中…</p>
        <p v-if="detailErr" class="err">{{ detailErr }}</p>

        <template v-if="detail">
          <div class="chips-row">
            <span class="chip info">选中：{{ detail.chosen_node || '无' }}</span>
            <span class="chip" :class="detail.degraded ? 'warn' : 'ok'">{{ detail.degraded ? '退化' : '指标正常' }}</span>
            <span class="chip">耗时 {{ detail.evidence?.elapsed_ms ?? '—' }} ms</span>
            <span class="chip" :class="detail.evidence?.metrics_known ? 'ok' : 'warn'">
              metrics {{ detail.evidence?.metrics_known ? '已知' : '缺失' }}
            </span>
            <span class="chip">snapshot {{ shortID(detail.snapshot_hash) }}</span>
            <span class="chip">policy {{ shortID(detail.policy_id) }}</span>
          </div>

          <div class="grid-2">
            <div ref="detailRankEl" class="chart sm"></div>
            <div ref="detailRadarEl" class="chart sm"></div>
          </div>

          <div class="kv" style="margin-top: 10px">
            <span class="k">Workload</span>
            <span>{{ detail.workload ? detail.workload.namespace + '/' + detail.workload.name : '—' }}</span>
            <span class="k">租户</span><span>{{ detail.tenant_id || '未指定' }}</span>
            <span class="k">创建时间</span><span>{{ new Date(detail.created_at).toLocaleString() }}</span>
          </div>

          <h3 style="margin-top: 16px">节点评分</h3>
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
                <tr v-for="s in sortScores(detail)" :key="s.node">
                  <td>
                    <span class="rank-badge" :class="{ first: s.node === detail.chosen_node }">{{ s.rank }}</span>
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

          <template v-if="detail.filtered?.length">
            <h3 style="margin-top: 16px">被过滤节点</h3>
            <div class="chips-row">
              <span v-for="f in detail.filtered" :key="f.node" class="chip bad">{{ f.node }} · {{ f.reason }}</span>
            </div>
          </template>

          <div v-if="detail.warnings?.length" class="chips-row" style="margin-top: 10px">
            <span v-for="w in detail.warnings" :key="w" class="chip warn">{{ w }}</span>
          </div>
        </template>

        <div class="modal-foot">
          <button class="ghost" @click="closeDetail">关闭</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  api,
  cores,
  gib,
  pct,
  shortID,
  WEIGHT_KEYS,
  type Decision,
  type Overview,
  type TenantItem,
  type WeightKey
} from '../api'
import { useChart } from '../lib/chart'

const SHORT: Record<WeightKey, string> = {
  s_cpu: 'CPU',
  s_mem: 'MEM',
  s_balance: 'BAL',
  s_pack: 'PACK',
  s_load: 'LOAD',
  s_fair: 'FAIR',
  s_frag: 'FRAG'
}

const overview = ref<Overview | null>(null)
const route = useRoute()
const router = useRouter()
const tenants = ref<TenantItem[]>([])
const decisions = ref<Decision[]>([])
const metric = ref<'cpu' | 'mem'>('cpu')

const simTenant = ref('')
const extraCpu = ref(500)
const extraMem = ref(256)
const pickA = ref('')
const pickB = ref('')

const detailOpen = ref(false)
const detailLoading = ref(false)
const detailErr = ref('')
const detail = ref<Decision | null>(null)
const pendingId = ref('')

const donutEl = ref<HTMLElement | null>(null)
const barEl = ref<HTMLElement | null>(null)
const distEl = ref<HTMLElement | null>(null)
const timeEl = ref<HTMLElement | null>(null)
const compareEl = ref<HTMLElement | null>(null)
const detailRankEl = ref<HTMLElement | null>(null)
const detailRadarEl = ref<HTMLElement | null>(null)

const donut = useChart(donutEl)
const bar = useChart(barEl)
const dist = useChart(distEl)
const timeline = useChart(timeEl)
const compare = useChart(compareEl)
const detailRank = useChart(detailRankEl)
const detailRadar = useChart(detailRadarEl)

const PALETTE = ['#7aa2f7', '#27c281', '#e7a33e', '#b48ead', '#e2474a', '#5fb3b3', '#9ece6a', '#f7768e']

const metricLabel = computed(() => (metric.value === 'cpu' ? 'CPU' : '内存'))
const simTenantInfo = computed(() => tenants.value.find(t => t.tenant_id === simTenant.value) || null)

function tenantValue(t: TenantItem, kind: 'requested' | 'hard') {
  return metric.value === 'cpu' ? t[kind].cpu_milli : t[kind].mem_bytes
}

function displayValue(v: number) {
  return metric.value === 'cpu' ? cores(v) + ' C' : gib(v) + ' GiB'
}

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

const topNode = (d: Decision) => [...(d.scores || [])].sort((x, y) => y.score - x.score)[0]
const sortScores = (d: Decision) => [...(d.scores || [])].sort((a, b) => b.score - a.score)

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
  const topA = topNode(a)
  const topB = topNode(b)
  push(
    '最高分',
    topA ? `${topA.node} ${topA.score.toFixed(1)}` : '—',
    topB ? `${topB.node} ${topB.score.toFixed(1)}` : '—'
  )
  return rows
})

const AXIS = '#8b93a7'
const LINE = '#242a33'

function drawDonut() {
  if (!tenants.value.length) return
  const data = tenants.value.map((t, i) => ({
    name: t.name,
    value: tenantValue(t, 'hard') || tenantValue(t, 'requested'),
    itemStyle: { color: PALETTE[i % PALETTE.length] }
  }))
  donut.onClick((p: any) => {
    const t = tenants.value.find(x => x.name === p?.name)
    if (t) simTenant.value = t.tenant_id
  })
  donut.render({
    tooltip: {
      trigger: 'item',
      formatter: ({ name, value, percent }: any) =>
        `${name}：${displayValue(value)}（${percent}%）`
    },
    legend: { bottom: 0, textStyle: { color: AXIS } },
    series: [
      {
        type: 'pie',
        radius: ['46%', '68%'],
        center: ['50%', '44%'],
        itemStyle: { borderColor: '#171b22', borderWidth: 2 },
        label: { color: AXIS, formatter: '{b}' },
        data
      }
    ]
  })
}

function drawBar() {
  if (!tenants.value.length) return
  bar.render({
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: any) => displayValue(Number(v) * (metric.value === 'cpu' ? 1000 : 1024 * 1024))
    },
    legend: { data: ['已请求', '配额'], textStyle: { color: AXIS }, top: 0 },
    grid: { left: 56, right: 20, top: 34, bottom: 30 },
    xAxis: {
      type: 'category',
      data: tenants.value.map(t => t.name),
      axisLabel: { color: AXIS, interval: 0, rotate: tenants.value.length > 4 ? 25 : 0 },
      axisLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'value',
      name: metric.value === 'cpu' ? 'C' : 'GiB',
      nameTextStyle: { color: AXIS },
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        name: '已请求',
        type: 'bar',
        data: tenants.value.map(t =>
          metric.value === 'cpu'
            ? +(tenantValue(t, 'requested') / 1000).toFixed(2)
            : +(tenantValue(t, 'requested') / 1024 / 1024 / 1024).toFixed(2)
        ),
        itemStyle: { color: '#7aa2f7', borderRadius: [3, 3, 0, 0] }
      },
      {
        name: '配额',
        type: 'bar',
        data: tenants.value.map(t =>
          metric.value === 'cpu'
            ? +(tenantValue(t, 'hard') / 1000).toFixed(2)
            : +(tenantValue(t, 'hard') / 1024 / 1024 / 1024).toFixed(2)
        ),
        itemStyle: { color: '#3c4453', borderRadius: [3, 3, 0, 0] }
      }
    ]
  })
}

function drawDist() {
  const counts: Record<string, number> = {}
  decisions.value.forEach(d => {
    const k = d.chosen_node || '（无）'
    counts[k] = (counts[k] || 0) + 1
  })
  const entries = Object.entries(counts).sort((a, b) => b[1] - a[1])
  if (!entries.length) {
    dist.render({ title: { text: '暂无决策', left: 'center', top: 'middle', textStyle: { color: AXIS, fontSize: 12 } } })
    return
  }
  dist.render({
    grid: { left: 88, right: 30, top: 10, bottom: 20 },
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    xAxis: {
      type: 'value',
      minInterval: 1,
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'category',
      data: entries.map(e => e[0]),
      inverse: true,
      axisLabel: { color: AXIS },
      axisLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'bar',
        barWidth: 14,
        data: entries.map(e => e[1]),
        itemStyle: { color: '#7aa2f7', borderRadius: [0, 3, 3, 0] },
        label: { show: true, position: 'right', color: '#e6e8ec', fontSize: 11 }
      }
    ]
  })
}

function drawTimeline() {
  const pts = decisions.value
    .filter(d => d.evidence?.elapsed_ms != null)
    .map(d => ({
      id: d.id,
      value: [new Date(d.created_at).getTime(), Number(d.evidence.elapsed_ms)],
      node: d.chosen_node || '—',
      degraded: !!d.degraded,
      status: d.status
    }))
  if (!pts.length) {
    timeline.render({
      title: { text: '暂无耗时数据', left: 'center', top: 'middle', textStyle: { color: AXIS, fontSize: 12 } }
    })
    return
  }
  timeline.onClick((p: any) => {
    if (p?.data?.id) openDecision(p.data.id)
  })
  const zoom = pts.length > 12
  timeline.render({
    grid: { left: 52, right: 24, top: 16, bottom: zoom ? 40 : 26 },
    ...(zoom
      ? {
          dataZoom: [
            { type: 'inside' as const },
            {
              type: 'slider' as const,
              height: 12,
              bottom: 2,
              borderColor: LINE,
              fillerColor: 'rgba(122,162,247,0.15)',
              handleStyle: { color: AXIS },
              textStyle: { color: AXIS, fontSize: 9 }
            }
          ]
        }
      : {}),
    tooltip: {
      formatter: (p: any) => {
        const d = p.data
        return `${new Date(d.value[0]).toLocaleString()}<br/>节点：${d.node}<br/>耗时：${d.value[1]} ms<br/>状态：${d.status}`
      }
    },
    xAxis: {
      type: 'time',
      axisLabel: { color: AXIS, hideOverlap: true },
      axisLine: { lineStyle: { color: LINE } },
      splitLine: { show: false }
    },
    yAxis: {
      type: 'value',
      name: 'ms',
      nameTextStyle: { color: AXIS },
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'scatter',
        symbolSize: 11,
        data: pts.map(p => ({
          ...p,
          itemStyle: { color: p.degraded ? '#e7a33e' : '#27c281' }
        }))
      }
    ]
  })
}

function radarOption(a: Decision, b?: Decision) {
  const ta = topNode(a)
  const tb = b ? topNode(b) : null
  return {
    tooltip: { trigger: 'item' },
    legend: { bottom: 0, textStyle: { color: AXIS } },
    radar: {
      center: ['50%', '46%'],
      radius: '62%',
      indicator: WEIGHT_KEYS.map(k => ({ name: SHORT[k], max: 1 })),
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
            name: 'A · ' + (ta?.node || '—'),
            value: WEIGHT_KEYS.map(k => +(ta?.breakdown?.[k] ?? 0).toFixed(3)),
            lineStyle: { color: '#7aa2f7', width: 2 },
            areaStyle: { color: 'rgba(122,162,247,0.14)' },
            itemStyle: { color: '#7aa2f7' }
          },
          ...(tb
            ? [
                {
                  name: 'B · ' + tb.node,
                  value: WEIGHT_KEYS.map(k => +(tb.breakdown?.[k] ?? 0).toFixed(3)),
                  lineStyle: { color: '#e7a33e', width: 2 },
                  areaStyle: { color: 'rgba(231,163,62,0.12)' },
                  itemStyle: { color: '#e7a33e' }
                }
              ]
            : [])
        ]
      }
    ]
  }
}

function drawCompare() {
  const a = decisions.value.find(d => d.id === pickA.value)
  const b = decisions.value.find(d => d.id === pickB.value)
  if (!a || !b) return
  compare.render(radarOption(a, b) as any)
}

function drawDetail() {
  const d = detail.value
  if (!d) return
  const rows = sortScores(d)
  detailRank.render({
    grid: { left: 80, right: 46, top: 8, bottom: 20 },
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
        barWidth: 14,
        data: rows.map(r => ({
          value: +r.score.toFixed(2),
          itemStyle: { color: r.node === d.chosen_node ? '#27c281' : '#7aa2f7', borderRadius: [0, 3, 3, 0] }
        })),
        label: { show: true, position: 'right', color: '#e6e8ec', fontSize: 10 }
      }
    ]
  })
  const top = topNode(d)
  detailRadar.render({
    tooltip: { trigger: 'item' },
    radar: {
      center: ['50%', '50%'],
      radius: '62%',
      indicator: WEIGHT_KEYS.map(k => ({ name: SHORT[k], max: 1 })),
      axisName: { color: AXIS, fontSize: 10 },
      splitLine: { lineStyle: { color: LINE } },
      splitArea: { areaStyle: { color: ['rgba(255,255,255,0.02)', 'transparent'] } },
      axisLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'radar',
        data: [
          {
            name: top?.node || '—',
            value: WEIGHT_KEYS.map(k => +(top?.breakdown?.[k] ?? 0).toFixed(3)),
            lineStyle: { color: '#27c281', width: 2 },
            areaStyle: { color: 'rgba(39,194,129,0.15)' },
            itemStyle: { color: '#27c281' }
          }
        ]
      }
    ]
  })
}

async function openDecision(id: string, syncUrl = true) {
  detailOpen.value = true
  pendingId.value = id
  if (syncUrl) router.replace({ query: { ...route.query, decision: id } })
  detailLoading.value = true
  detailErr.value = ''
  try {
    detail.value = await api.decision(id)
    await nextTick()
    drawDetail()
  } catch (e: any) {
    detailErr.value = e?.message || String(e)
  } finally {
    detailLoading.value = false
  }
}

function closeDetail() {
  detailOpen.value = false
  if (typeof route.query.decision === 'string') {
    const q = { ...route.query }
    delete q.decision
    router.replace({ query: q })
  }
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape' && detailOpen.value) closeDetail()
}

async function reloadDecisions() {
  decisions.value = (await api.decisions(30)).items || []
  await nextTick()
  drawDist()
  drawTimeline()
  drawCompare()
}

watch(metric, () => {
  drawDonut()
  drawBar()
})

watch([pickA, pickB], async () => {
  await nextTick()
  drawCompare()
})

watch(
  () => route.query.decision,
  value => {
    const id = typeof value === 'string' ? value : ''
    if (!id) {
      detailOpen.value = false
      return
    }
    if (id !== detail.value?.id && id !== pendingId.value) openDecision(id, false)
  }
)

onMounted(async () => {
  window.addEventListener('keydown', onKeydown)
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
  const q = route.query.decision
  if (typeof q === 'string' && q) await openDecision(q, false)
  drawDonut()
  drawBar()
  drawDist()
  drawTimeline()
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>
