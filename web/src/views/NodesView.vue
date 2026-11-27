<template>
  <div class="stack">
    <div class="page-head">
      <h2>节点与容量</h2>
      <p class="muted small" style="margin: 0">
        节点级 allocatable / requested / 实际用量。趋势曲线由本页每 5 秒采样累积（仅当前会话）。
      </p>
    </div>

    <div class="grid-3">
      <div class="stat">
        <div class="label">节点数</div>
        <div class="value">{{ nodes.length }}</div>
        <div class="sub">{{ nodes.filter(n => n.ready).length }} 个 Ready</div>
      </div>
      <div class="stat">
        <div class="label">集群实际用量</div>
        <div class="value">{{ cores(totalUsage.cpu_milli) }} C</div>
        <div class="sub">{{ gib(totalUsage.mem_bytes) }} GiB 内存</div>
      </div>
      <div class="stat">
        <div class="label">指标新鲜度</div>
        <div class="value">{{ metricsAge != null ? metricsAge.toFixed(0) + 's' : '—' }}</div>
        <div class="sub">{{ metricsAt ? new Date(metricsAt).toLocaleTimeString() : '无指标' }}</div>
      </div>
    </div>

    <div class="toolbar">
      <label class="auto"><input v-model="readyOnly" type="checkbox" /> 只看 Ready</label>
      <label class="auto"><input v-model="sortPressure" type="checkbox" /> 按请求压力排序</label>
      <span class="muted small">点击卡片或气泡可切换下方趋势</span>
      <span style="flex: 1"></span>
      <span v-if="loading" class="small muted">刷新中…</span>
      <span v-else-if="updatedAt" class="small muted">更新于 {{ updatedAt.toLocaleTimeString() }}</span>
      <span v-if="autoRefresh" class="small muted" style="display: inline-flex; align-items: center; gap: 6px">
        <span class="dot-live"></span>5s
      </span>
      <label class="auto"><input v-model="autoRefresh" type="checkbox" /> 自动刷新</label>
      <button class="ghost" :disabled="loading" @click="poll">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <div v-if="loading && !nodes.length" class="grid-3">
      <div v-for="i in 3" :key="'nsk-' + i" class="node-card">
        <div class="sk" style="width: 55%; margin-bottom: 12px"></div>
        <div class="sk" style="height: 8px; margin-bottom: 10px"></div>
        <div class="sk" style="width: 72%"></div>
      </div>
    </div>

    <div v-else class="grid-3">
      <div
        v-for="n in visibleNodes"
        :key="n.name"
        class="node-card"
        :class="{ active: n.name === selected }"
        @click="selected = n.name"
      >
        <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 8px">
          <strong>{{ n.name }}</strong>
          <span class="chip" :class="n.ready ? 'ok' : 'bad'">{{ n.ready ? 'Ready' : 'NotReady' }}</span>
          <span v-if="n.allocatable.gpu > 0" class="chip info">GPU {{ n.allocatable.gpu }} 卡</span>
          <span style="flex: 1"></span>
          <span class="muted small">{{ n.pod_count }} pods</span>
        </div>
        <div class="small muted">CPU 请求 / 可分配</div>
        <div style="display: flex; align-items: center; gap: 8px; margin: 4px 0 10px">
          <div class="bar-track" style="flex: 1">
            <div
              class="bar-fill"
              :class="pressureClass(n.requested.cpu_milli, n.allocatable.cpu_milli)"
              :style="{ width: ratio(n.requested.cpu_milli, n.allocatable.cpu_milli) }"
            ></div>
          </div>
          <span class="small">{{ ratio(n.requested.cpu_milli, n.allocatable.cpu_milli) }}</span>
        </div>
        <div class="small muted">实际用量（EWMA）</div>
        <div style="display: flex; align-items: center; gap: 8px; margin-top: 4px">
          <div class="bar-track" style="flex: 1">
            <div
              class="bar-fill warn"
              :style="{ width: n.usage_known ? ratio(n.usage.cpu_milli, n.allocatable.cpu_milli) : '0%' }"
            ></div>
          </div>
          <span class="small">{{ n.usage_known ? cores(n.usage.cpu_milli) + ' C' : '无数据' }}</span>
        </div>
      </div>
    </div>
    <p v-if="!visibleNodes.length && !loading" class="empty">当前过滤条件下没有节点</p>

    <section class="panel">
      <h3>
        节点压力分布
        <span class="chip">气泡大小 = Pod 数</span>
      </h3>
      <div ref="scatterEl" class="chart"></div>
      <p class="hint">横轴为 CPU 请求率，纵轴为内存请求率；颜色按较高一侧的压力分级。</p>
    </section>

    <section v-if="current" class="panel">
      <h3>
        {{ current.name }} 详情
        <span style="flex: 1"></span>
        <span class="seg">
          <button :class="{ active: trendMode === 'both' }" @click="trendMode = 'both'">双轴</button>
          <button :class="{ active: trendMode === 'cpu' }" @click="trendMode = 'cpu'">CPU</button>
          <button :class="{ active: trendMode === 'mem' }" @click="trendMode = 'mem'">内存</button>
        </span>
        <span class="chip">采样 {{ samples.length }} 点</span>
      </h3>
      <div class="grid-2">
        <div>
          <div class="kv">
            <span class="k">可分配 CPU</span><span>{{ cores(current.allocatable.cpu_milli) }} C</span>
            <span class="k">可分配内存</span><span>{{ gib(current.allocatable.mem_bytes) }} GiB</span>
            <span v-if="current.allocatable.gpu > 0" class="k">可分配 GPU</span>
            <span v-if="current.allocatable.gpu > 0">{{ current.allocatable.gpu }} 卡</span>
            <span class="k">已请求 CPU</span><span>{{ cores(current.requested.cpu_milli) }} C</span>
            <span class="k">已请求内存</span><span>{{ gib(current.requested.mem_bytes) }} GiB</span>
            <span class="k">请求率</span>
            <span>
              CPU {{ ratio(current.requested.cpu_milli, current.allocatable.cpu_milli) }} ·
              内存 {{ ratio(current.requested.mem_bytes, current.allocatable.mem_bytes) }}
            </span>
            <span class="k">实际 CPU</span><span>{{ current.usage_known ? cores(current.usage.cpu_milli) + ' C' : '无数据' }}</span>
            <span class="k">实际内存</span><span>{{ current.usage_known ? gib(current.usage.mem_bytes) + ' GiB' : '无数据' }}</span>
            <span class="k">Pod 数</span><span>{{ current.pod_count }}</span>
            <template v-for="(v, k) in current.topology" :key="k">
              <span class="k">{{ k }}</span><span>{{ v }}</span>
            </template>
          </div>
        </div>
        <div ref="trendEl" class="chart sm"></div>
      </div>
    </section>

    <p v-if="error" class="err">
      {{ error }}
      <button class="link" style="margin-left: 8px" @click="poll">重试</button>
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, ageSeconds, cores, gib, type NodeItem } from '../api'
import { useChart } from '../lib/chart'

interface Sample {
  t: number
  cpu: number
  mem: number
}

interface UiState {
  readyOnly?: boolean
  sortPressure?: boolean
  trendMode?: 'both' | 'cpu' | 'mem'
  autoRefresh?: boolean
  selected?: string
}

function readUi(): UiState {
  try {
    return JSON.parse(localStorage.getItem('ka_nodes_ui') || '{}')
  } catch {
    return {}
  }
}

const ui = readUi()
const nodes = ref<NodeItem[]>([])
const metricsAt = ref<string | null>(null)
const selected = ref(ui.selected || '')
const error = ref('')
const history = ref<Record<string, Sample[]>>({})
const readyOnly = ref(ui.readyOnly ?? false)
const sortPressure = ref(ui.sortPressure ?? true)
const trendMode = ref<'both' | 'cpu' | 'mem'>(ui.trendMode || 'both')
const loading = ref(false)
const autoRefresh = ref(ui.autoRefresh ?? true)
const updatedAt = ref<Date | null>(null)

watch([readyOnly, sortPressure, trendMode, autoRefresh, selected], () => {
  localStorage.setItem(
    'ka_nodes_ui',
    JSON.stringify({
      readyOnly: readyOnly.value,
      sortPressure: sortPressure.value,
      trendMode: trendMode.value,
      autoRefresh: autoRefresh.value,
      selected: selected.value
    })
  )
})

const trendEl = ref<HTMLElement | null>(null)
const scatterEl = ref<HTMLElement | null>(null)
const { render } = useChart(trendEl)
const scatter = useChart(scatterEl)
scatter.onClick((p: any) => {
  if (p?.name) selected.value = String(p.name)
})

const totalUsage = computed(() =>
  nodes.value
    .filter(n => n.usage_known)
    .reduce(
      (a, n) => ({ cpu_milli: a.cpu_milli + n.usage.cpu_milli, mem_bytes: a.mem_bytes + n.usage.mem_bytes, gpu: a.gpu + n.usage.gpu }),
      { cpu_milli: 0, mem_bytes: 0, gpu: 0 }
    )
)
const metricsAge = computed(() => ageSeconds(metricsAt.value))
const current = computed(() => nodes.value.find(n => n.name === selected.value) || null)
const samples = computed(() => (selected.value ? history.value[selected.value] || [] : []))

const ratioPct = (a: number, b: number) => (b ? Math.min(100, Math.round((a / b) * 100)) : 0)

const visibleNodes = computed(() => {
  const list = nodes.value.filter(n => !readyOnly.value || n.ready)
  if (!sortPressure.value) return list
  return [...list].sort(
    (a, b) =>
      Math.max(ratioPct(b.requested.cpu_milli, b.allocatable.cpu_milli), ratioPct(b.requested.mem_bytes, b.allocatable.mem_bytes)) -
      Math.max(ratioPct(a.requested.cpu_milli, a.allocatable.cpu_milli), ratioPct(a.requested.mem_bytes, a.allocatable.mem_bytes))
  )
})

function ratio(a: number, b: number) {
  return ratioPct(a, b) + '%'
}

function pressureClass(a: number, b: number) {
  const p = ratioPct(a, b)
  if (p >= 90) return 'bad'
  if (p >= 75) return 'warn'
  return ''
}

const AXIS = '#8b93a7'
const LINE = '#242a33'

async function poll() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const resp = await api.nodes()
    nodes.value = resp.nodes || []
    metricsAt.value = resp.metrics_at || null
    updatedAt.value = new Date()
    if (!selected.value || !nodes.value.some(n => n.name === selected.value)) {
      selected.value = nodes.value[0]?.name || ''
    }

    const now = Date.now()
    for (const n of nodes.value) {
      if (!n.usage_known) continue
      const arr = history.value[n.name] || (history.value[n.name] = [])
      arr.push({ t: now, cpu: n.usage.cpu_milli, mem: n.usage.mem_bytes })
      if (arr.length > 60) arr.shift()
    }
    await nextTick()
    draw()
    drawScatter()
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

function draw() {
  const list = samples.value
  if (!current.value || !list.length) return
  const fmt = (t: number) => new Date(t).toLocaleTimeString()
  const timeAxis = {
    type: 'category' as const,
    data: list.map(s => fmt(s.t)),
    axisLabel: { color: AXIS, hideOverlap: true },
    axisLine: { lineStyle: { color: LINE } }
  }
  const cpuSeries = {
    name: 'CPU (m)',
    type: 'line' as const,
    smooth: true,
    showSymbol: false,
    data: list.map(s => s.cpu),
    lineStyle: { color: '#e7a33e', width: 2 },
    areaStyle: { color: 'rgba(231,163,62,0.12)' }
  }
  const memSeries = {
    name: '内存 (MiB)',
    type: 'line' as const,
    smooth: true,
    showSymbol: false,
    data: list.map(s => Math.round(s.mem / 1024 / 1024)),
    lineStyle: { color: '#7aa2f7', width: 2 }
  }

  // 采样点较多时开启缩放，默认展示全量，可拖拽或滚轮聚焦区间。
  const zoom = list.length > 12
  const zoomOpt = zoom
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
    : {}

  if (trendMode.value === 'both') {
    render({
      grid: { left: 52, right: 52, top: 30, bottom: zoom ? 42 : 28 },
      tooltip: { trigger: 'axis' },
      legend: { data: ['CPU (m)', '内存 (MiB)'], textStyle: { color: AXIS }, top: 0 },
      xAxis: timeAxis,
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
      series: [cpuSeries, { ...memSeries, yAxisIndex: 1 }],
      ...zoomOpt
    })
    return
  }

  const cpu = trendMode.value === 'cpu'
  const series = cpu ? cpuSeries : { ...memSeries, areaStyle: { color: 'rgba(122,162,247,0.12)' } }
  render({
    grid: { left: 56, right: 24, top: 30, bottom: zoom ? 42 : 28 },
    tooltip: { trigger: 'axis' },
    legend: { data: [series.name], textStyle: { color: AXIS }, top: 0 },
    xAxis: timeAxis,
    yAxis: {
      type: 'value',
      name: cpu ? 'm' : 'MiB',
      nameTextStyle: { color: AXIS },
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    series: [series],
    ...zoomOpt
  })
}

function drawScatter() {
  if (!nodes.value.length) return
  scatter.render({
    grid: { left: 60, right: 28, top: 30, bottom: 34 },
    tooltip: {
      formatter: (p: any) => {
        const d = p.data
        return `${d.name}<br/>CPU 请求率：${d.value[0]}%<br/>内存请求率：${d.value[1]}%<br/>Pod：${d.pods}<br/>${d.ready ? 'Ready' : 'NotReady'}`
      }
    },
    xAxis: {
      type: 'value',
      name: 'CPU 请求率 %',
      max: 100,
      nameTextStyle: { color: AXIS },
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    yAxis: {
      type: 'value',
      name: '内存请求率 %',
      max: 100,
      nameTextStyle: { color: AXIS },
      axisLabel: { color: AXIS },
      splitLine: { lineStyle: { color: LINE } }
    },
    series: [
      {
        type: 'scatter',
        symbolSize: (val: any, p: any) => Math.max(12, Math.min(46, 10 + (p?.data?.pods || 0) * 2)),
        data: nodes.value.map(n => {
          const cp = ratioPct(n.requested.cpu_milli, n.allocatable.cpu_milli)
          const mp = ratioPct(n.requested.mem_bytes, n.allocatable.mem_bytes)
          const peak = Math.max(cp, mp)
          return {
            name: n.name,
            value: [cp, mp],
            pods: n.pod_count,
            ready: n.ready,
            itemStyle: {
              color: !n.ready ? '#6b7a90' : peak >= 90 ? '#e2474a' : peak >= 75 ? '#e7a33e' : '#7aa2f7',
              opacity: 0.85,
              borderColor: '#0f1115',
              borderWidth: 1
            }
          }
        }),
        label: {
          show: true,
          position: 'top',
          color: AXIS,
          fontSize: 10,
          formatter: (p: any) => p.data.name
        }
      }
    ]
  })
}

watch(selected, () => nextTick().then(draw))
watch(trendMode, () => nextTick().then(draw))

let timer: number | undefined

function armTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
  if (autoRefresh.value) timer = window.setInterval(poll, 5000)
}

watch(autoRefresh, armTimer)

onMounted(async () => {
  await poll()
  armTimer()
})
onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>
