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

    <div class="grid-3">
      <div
        v-for="n in nodes"
        :key="n.name"
        class="node-card"
        :class="{ active: n.name === selected }"
        @click="selected = n.name"
      >
        <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 8px">
          <strong>{{ n.name }}</strong>
          <span class="chip" :class="n.ready ? 'ok' : 'bad'">{{ n.ready ? 'Ready' : 'NotReady' }}</span>
          <span style="flex: 1"></span>
          <span class="muted small">{{ n.pod_count }} pods</span>
        </div>
        <div class="small muted">CPU 请求 / 可分配</div>
        <div style="display: flex; align-items: center; gap: 8px; margin: 4px 0 10px">
          <div class="bar-track" style="flex: 1">
            <div class="bar-fill" :style="{ width: ratio(n.requested.cpu_milli, n.allocatable.cpu_milli) }"></div>
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

    <section v-if="current" class="panel">
      <h3>
        {{ current.name }} 详情
        <span style="flex: 1"></span>
        <span class="chip">采样 {{ samples.length }} 点</span>
      </h3>
      <div class="grid-2">
        <div>
          <div class="kv">
            <span class="k">可分配 CPU</span><span>{{ cores(current.allocatable.cpu_milli) }} C</span>
            <span class="k">可分配内存</span><span>{{ gib(current.allocatable.mem_bytes) }} GiB</span>
            <span class="k">已请求 CPU</span><span>{{ cores(current.requested.cpu_milli) }} C</span>
            <span class="k">已请求内存</span><span>{{ gib(current.requested.mem_bytes) }} GiB</span>
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

    <p v-if="error" class="err">{{ error }}</p>
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

const nodes = ref<NodeItem[]>([])
const metricsAt = ref<string | null>(null)
const selected = ref('')
const error = ref('')
const history = ref<Record<string, Sample[]>>({})

const trendEl = ref<HTMLElement | null>(null)
const { render } = useChart(trendEl)

const totalUsage = computed(() =>
  nodes.value
    .filter(n => n.usage_known)
    .reduce(
      (a, n) => ({ cpu_milli: a.cpu_milli + n.usage.cpu_milli, mem_bytes: a.mem_bytes + n.usage.mem_bytes }),
      { cpu_milli: 0, mem_bytes: 0 }
    )
)
const metricsAge = computed(() => ageSeconds(metricsAt.value))
const current = computed(() => nodes.value.find(n => n.name === selected.value) || null)
const samples = computed(() => (selected.value ? history.value[selected.value] || [] : []))

function ratio(a: number, b: number) {
  if (!b) return '0%'
  return Math.min(100, Math.round((a / b) * 100)) + '%'
}

async function poll() {
  try {
    const resp = await api.nodes()
    nodes.value = resp.nodes || []
    metricsAt.value = resp.metrics_at || null
    if (!selected.value && nodes.value.length) selected.value = nodes.value[0].name

    const now = Date.now()
    for (const n of nodes.value) {
      if (!n.usage_known) continue
      const arr = history.value[n.name] || (history.value[n.name] = [])
      arr.push({ t: now, cpu: n.usage.cpu_milli, mem: n.usage.mem_bytes })
      if (arr.length > 60) arr.shift()
    }
    await nextTick()
    draw()
  } catch (e: any) {
    error.value = e?.message || String(e)
  }
}

function draw() {
  const list = samples.value
  if (!current.value || !list.length) return
  const fmt = (t: number) => new Date(t).toLocaleTimeString()
  render({
    grid: { left: 52, right: 52, top: 30, bottom: 28 },
    tooltip: { trigger: 'axis' },
    legend: { data: ['CPU (m)', '内存 (MiB)'], textStyle: { color: '#8b93a7' }, top: 0 },
    xAxis: {
      type: 'category',
      data: list.map(s => fmt(s.t)),
      axisLabel: { color: '#8b93a7', hideOverlap: true },
      axisLine: { lineStyle: { color: '#242a33' } }
    },
    yAxis: [
      {
        type: 'value',
        name: 'm',
        nameTextStyle: { color: '#8b93a7' },
        axisLabel: { color: '#8b93a7' },
        splitLine: { lineStyle: { color: '#242a33' } }
      },
      {
        type: 'value',
        name: 'MiB',
        nameTextStyle: { color: '#8b93a7' },
        axisLabel: { color: '#8b93a7' },
        splitLine: { show: false }
      }
    ],
    series: [
      {
        name: 'CPU (m)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: list.map(s => s.cpu),
        lineStyle: { color: '#e7a33e', width: 2 },
        areaStyle: { color: 'rgba(231,163,62,0.12)' }
      },
      {
        name: '内存 (MiB)',
        type: 'line',
        smooth: true,
        showSymbol: false,
        yAxisIndex: 1,
        data: list.map(s => Math.round(s.mem / 1024 / 1024)),
        lineStyle: { color: '#7aa2f7', width: 2 }
      }
    ]
  })
}

watch(selected, () => nextTick().then(draw))

let timer: number | undefined
onMounted(async () => {
  await poll()
  timer = window.setInterval(poll, 5000)
})
onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>
