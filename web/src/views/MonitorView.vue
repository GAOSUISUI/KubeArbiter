<template>
  <div class="stack">
    <div class="page-head">
      <h2>监控大盘</h2>
      <p class="muted small" style="margin: 0">
        集群、节点与租户的实时健康视图。指标历史保存在 API 进程内，默认保留 6 小时，重启后重新累积。
      </p>
    </div>

    <div class="toolbar">
      <span v-if="loading" class="small muted">刷新中…</span>
      <span v-else-if="updatedAt" class="small muted">更新于 {{ updatedAt.toLocaleTimeString() }}</span>
      <span v-if="overview?.degraded" class="chip warn">指标降级</span>
      <span style="flex: 1"></span>
      <label class="auto"><input v-model="autoRefresh" type="checkbox" /> 30s 自动刷新</label>
      <button class="ghost" :disabled="loading" @click="poll">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <p v-if="error" class="err">
      {{ error }}
      <button class="link" style="margin-left: 8px" @click="poll">重试</button>
    </p>

    <div v-if="overview?.degraded" class="panel banner">
      指标暂不完整或已过期，趋势与健康分可能偏低。最后样本：
      {{ overview.metrics_at ? new Date(overview.metrics_at).toLocaleString() : '无' }}。
    </div>

    <div v-if="loading && !overview" class="grid-3">
      <div v-for="i in 3" :key="'msk-' + i" class="stat">
        <div class="sk" style="width: 45%; margin-bottom: 12px"></div>
        <div class="sk" style="height: 26px; width: 60%"></div>
      </div>
    </div>

    <template v-else-if="overview">
      <div class="grid-3">
        <div class="stat">
          <div class="label">节点就绪</div>
          <div class="value">{{ overview.cluster.nodes_ready }} / {{ overview.cluster.nodes_total }}</div>
          <div class="sub">
            <span class="chip" :class="overview.cluster.nodes_ready === overview.cluster.nodes_total ? 'ok' : 'warn'">
              {{ overview.cluster.nodes_ready === overview.cluster.nodes_total ? '全部 Ready' : '存在异常节点' }}
            </span>
          </div>
        </div>
        <div class="stat">
          <div class="label">CPU 使用率</div>
          <div class="value">{{ ratio(overview.cluster.cpu_usage_milli, overview.cluster.cpu_alloc_milli) }}</div>
          <div class="sub">{{ cores(overview.cluster.cpu_usage_milli) }} / {{ cores(overview.cluster.cpu_alloc_milli) }} C</div>
        </div>
        <div class="stat">
          <div class="label">内存使用率</div>
          <div class="value">{{ ratio(overview.cluster.mem_usage_bytes, overview.cluster.mem_alloc_bytes) }}</div>
          <div class="sub">{{ gib(overview.cluster.mem_usage_bytes) }} / {{ gib(overview.cluster.mem_alloc_bytes) }} GiB</div>
        </div>
        <div class="stat">
          <div class="label">开放事件</div>
          <div class="value">{{ overview.incidents.open }}</div>
          <div class="sub">
            严重 {{ overview.incidents.critical }} · 告警 {{ overview.incidents.warn }} · 提示 {{ overview.incidents.info }}
          </div>
        </div>
        <div class="stat">
          <div class="label">集群健康分</div>
          <div class="value">{{ scoreText(overview.cluster.health.score) }}</div>
          <div class="sub">
            <span class="chip" :class="healthTone(overview.cluster.health.score)">
              {{ overview.cluster.health.components[0]?.message || '运行正常' }}
            </span>
          </div>
        </div>
        <div class="stat">
          <div class="label">指标年龄</div>
          <div class="value">{{ overview.metrics_age_seconds != null ? overview.metrics_age_seconds + 's' : '—' }}</div>
          <div class="sub">保留 {{ Math.round(overview.retention_seconds / 3600) }} 小时</div>
        </div>
      </div>

      <section class="panel">
        <h3>
          集群资源趋势
          <span style="flex: 1"></span>
          <span class="seg">
            <button v-for="w in windows" :key="w.value" :class="{ active: range === w.value }" @click="range = w.value">
              {{ w.label }}
            </button>
          </span>
          <span class="chip">{{ series?.step_seconds || 60 }}s / 点</span>
        </h3>
        <div ref="trendEl" class="chart"></div>
        <p class="hint">CPU 使用 EWMA 毫核，内存使用 MiB；数据来自节点 kubelet 汇总。</p>
      </section>

      <div class="grid-2">
        <section class="panel">
          <h3>
            健康分
            <span class="chip" :class="healthTone(overview.cluster.health.score)">
              集群 {{ scoreText(overview.cluster.health.score) }}
            </span>
          </h3>
          <div ref="healthEl" class="chart sm"></div>
        </section>
        <section class="panel">
          <h3>开放事件分布</h3>
          <div ref="incidentEl" class="chart sm"></div>
        </section>
      </div>

      <section class="panel">
        <h3>
          节点
          <span class="chip">{{ overview.nodes.length }} 个</span>
        </h3>
        <table>
          <thead>
            <tr>
              <th>节点</th>
              <th>状态</th>
              <th>健康分</th>
              <th>CPU 请求 / 实际</th>
              <th>内存请求 / 实际</th>
              <th>Pod</th>
              <th>条件</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in overview.nodes" :key="n.name">
              <td><strong>{{ n.name }}</strong></td>
              <td>
                <span class="chip" :class="n.ready ? 'ok' : 'bad'">{{ n.ready ? 'Ready' : 'NotReady' }}</span>
                <span v-if="n.unschedulable" class="chip warn" style="margin-left: 4px">Cordon</span>
              </td>
              <td>
                <span class="chip" :class="healthTone(n.health.score)">{{ scoreText(n.health.score) }}</span>
              </td>
              <td class="small">
                {{ cores(n.requested.cpu_milli) }} / {{ n.usage_known ? cores(n.usage.cpu_milli) : '—' }} C
                <div class="bar-track" style="margin-top: 4px">
                  <div class="bar-fill" :class="barTone(n.usage.cpu_milli, n.allocatable.cpu_milli)"
                       :style="{ width: n.usage_known ? ratioWidth(n.usage.cpu_milli, n.allocatable.cpu_milli) : '0%' }"></div>
                </div>
              </td>
              <td class="small">
                {{ gib(n.requested.mem_bytes) }} / {{ n.usage_known ? gib(n.usage.mem_bytes) : '—' }} GiB
                <div class="bar-track" style="margin-top: 4px">
                  <div class="bar-fill" :class="barTone(n.usage.mem_bytes, n.allocatable.mem_bytes)"
                       :style="{ width: n.usage_known ? ratioWidth(n.usage.mem_bytes, n.allocatable.mem_bytes) : '0%' }"></div>
                </div>
              </td>
              <td>{{ n.pod_count }}</td>
              <td>
                <span v-if="!n.conditions.memory_pressure && !n.conditions.disk_pressure && !n.conditions.pid_pressure" class="muted small">无压力</span>
                <span v-if="n.conditions.memory_pressure" class="chip warn">内存压力</span>
                <span v-if="n.conditions.disk_pressure" class="chip warn" style="margin-left: 4px">磁盘压力</span>
                <span v-if="n.conditions.pid_pressure" class="chip warn" style="margin-left: 4px">PID 压力</span>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-if="!overview.nodes.length" class="empty">暂无节点数据</p>
      </section>

      <section v-if="overview.tenants.length" class="panel">
        <h3>
          租户
          <span class="chip">{{ overview.tenants.length }} 个</span>
        </h3>
        <table>
          <thead>
            <tr>
              <th>租户</th>
              <th>配额使用率</th>
              <th>CPU 请求 / 配额</th>
              <th>内存请求 / 配额</th>
              <th>主导份额</th>
              <th>公平份额</th>
              <th>健康分</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in overview.tenants" :key="t.id">
              <td><strong>{{ t.name || t.id }}</strong></td>
              <td>
                <span class="chip" :class="t.quota_ratio >= 1 ? 'bad' : t.quota_ratio >= 0.9 ? 'warn' : 'ok'">
                  {{ pct(t.quota_ratio) }}
                </span>
              </td>
              <td class="small">{{ cores(t.requested.cpu_milli) }} / {{ cores(t.quota.cpu_milli) }} C</td>
              <td class="small">{{ gib(t.requested.mem_bytes) }} / {{ gib(t.quota.mem_bytes) }} GiB</td>
              <td>{{ pct(t.share) }}</td>
              <td>{{ pct(t.fair_share) }}</td>
              <td><span class="chip" :class="healthTone(t.health.score)">{{ scoreText(t.health.score) }}</span></td>
            </tr>
          </tbody>
        </table>
      </section>
    </template>

    <p v-else-if="!loading" class="empty">暂无监控数据，请确认 API 已连接集群。</p>
  </div>
</template>

<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  api,
  cores,
  gib,
  healthTone,
  pct,
  type HealthScore,
  type MonitorOverview,
  type MonitorSeries
} from '../api'
import { useChart } from '../lib/chart'

const AXIS = '#8b93a7'
const LINE = '#242a33'

const windows = [
  { label: '1h', value: '1h' },
  { label: '6h', value: '6h' },
  { label: '24h', value: '24h' }
]

const overview = ref<MonitorOverview | null>(null)
const series = ref<MonitorSeries | null>(null)
const range = ref('1h')
const loading = ref(false)
const error = ref('')
const autoRefresh = ref(true)
const updatedAt = ref<Date | null>(null)

const trendEl = ref<HTMLElement | null>(null)
const healthEl = ref<HTMLElement | null>(null)
const incidentEl = ref<HTMLElement | null>(null)
const trendChart = useChart(trendEl)
const healthChart = useChart(healthEl)
const incidentChart = useChart(incidentEl)

const ratioWidth = (a: number, b: number) => (b ? Math.min(100, Math.round((a / b) * 100)) + '%' : '0%')
const ratio = (a: number, b: number) => (b ? Math.round((a / b) * 100) + '%' : '—')
const ratioPercent = (a: number, b: number) => (b ? Math.min(100, Math.round((a / b) * 100)) : 0)
const scoreText = (score?: number | null) => (score == null ? '—' : score.toFixed(0))
const barTone = (a: number, b: number) => {
  const p = ratioPercent(a, b)
  return p >= 90 ? 'bad' : p >= 75 ? 'warn' : ''
}

async function poll() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const [ov, se] = await Promise.all([api.monitorOverview(), api.monitorSeries(range.value)])
    overview.value = ov
    series.value = se
    updatedAt.value = new Date()
    await nextTick()
    drawTrend()
    drawHealth()
    drawIncidents()
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

async function reloadSeries() {
  try {
    series.value = await api.monitorSeries(range.value)
    await nextTick()
    drawTrend()
  } catch (e: any) {
    error.value = e?.message || String(e)
  }
}

function drawTrend() {
  const points = series.value?.cluster || []
  const x = points.map(p => new Date(p.ts).toLocaleTimeString())
  const cpu = points.map(p => p.cpu_milli)
  const mem = points.map(p => Math.round(p.mem_bytes / 1024 / 1024))
  const zoom = points.length > 12
  trendChart.render({
    grid: { left: 56, right: 56, top: 30, bottom: zoom ? 44 : 28 },
    tooltip: { trigger: 'axis' },
    legend: { data: ['CPU (m)', '内存 (MiB)'], textStyle: { color: AXIS }, top: 0 },
    xAxis: {
      type: 'category',
      data: x,
      axisLabel: { color: AXIS, hideOverlap: true },
      axisLine: { lineStyle: { color: LINE } }
    },
    yAxis: [
      {
        type: 'value',
        name: 'm',
        nameTextStyle: { color: AXIS },
        axisLabel: { color: AXIS },
        splitLine: { lineStyle: { color: LINE } }
      },
      {
        type: 'value',
        name: 'MiB',
        nameTextStyle: { color: AXIS },
        axisLabel: { color: AXIS },
        splitLine: { show: false }
      }
    ],
    series: [
      {
        name: 'CPU (m)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: cpu,
        lineStyle: { color: '#e7a33e', width: 2 },
        areaStyle: { color: 'rgba(231,163,62,0.12)' }
      },
      {
        name: '内存 (MiB)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        yAxisIndex: 1,
        data: mem,
        lineStyle: { color: '#7aa2f7', width: 2 },
        areaStyle: { color: 'rgba(122,162,247,0.10)' }
      }
    ],
    ...(zoom
      ? {
          dataZoom: [
            { type: 'inside' },
            {
              type: 'slider',
              height: 12,
              bottom: 2,
              borderColor: LINE,
              fillerColor: 'rgba(122,162,247,0.15)',
              handleStyle: { color: AXIS },
              textStyle: { color: AXIS, fontSize: 9 }
            }
          ]
        }
      : {})
  })
}

function drawHealth() {
  const items: HealthScore[] = []
  if (overview.value) {
    items.push(overview.value.cluster.health)
    for (const node of overview.value.nodes) items.push(node.health)
  }
  const scored = items.filter(item => item.score != null)
  healthChart.render({
    grid: { left: 90, right: 36, top: 12, bottom: 24 },
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    xAxis: {
      type: 'value',
      max: 100,
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'category',
      data: scored.map(item => item.scope === 'CLUSTER' ? '集群' : item.name),
      axisLabel: { color: AXIS }
    },
    series: [
      {
        type: 'bar',
        barWidth: 14,
        data: scored.map(item => ({
          value: item.score,
          itemStyle: {
            color: (item.score || 0) >= 90 ? '#3fbf7f' : (item.score || 0) >= 70 ? '#e7a33e' : '#e2474a',
            borderRadius: [0, 4, 4, 0]
          }
        })),
        label: { show: true, position: 'right', color: AXIS, fontSize: 10 }
      }
    ]
  })
}

function drawIncidents() {
  const summary = overview.value?.incidents
  const data = summary && summary.open > 0
    ? [
        { name: '严重', value: summary.critical, itemStyle: { color: '#e2474a' } },
        { name: '告警', value: summary.warn, itemStyle: { color: '#e7a33e' } },
        { name: '提示', value: summary.info, itemStyle: { color: '#7aa2f7' } }
      ].filter(item => item.value > 0)
    : [{ name: '暂无开放事件', value: 1, itemStyle: { color: '#2a2f3a' } }]
  incidentChart.render({
    tooltip: { trigger: 'item', formatter: '{b}: {c} ({d}%)' },
    legend: { bottom: 0, textStyle: { color: AXIS } },
    series: [
      {
        type: 'pie',
        radius: ['42%', '68%'],
        center: ['50%', '45%'],
        label: { color: AXIS, fontSize: 10 },
        data
      }
    ]
  })
}

watch(range, reloadSeries)
watch(autoRefresh, armTimer)

let timer: number | undefined
function armTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
  if (autoRefresh.value) timer = window.setInterval(poll, 30000)
}

onMounted(async () => {
  await poll()
  armTimer()
})
onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.banner {
  border-color: rgba(231, 163, 62, 0.5);
  color: #e7a33e;
  font-size: 12px;
}
</style>
