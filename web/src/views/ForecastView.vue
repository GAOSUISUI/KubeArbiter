<template>
  <div class="stack">
    <div class="page-head">
      <h2>预测与容量</h2>
      <p class="muted small" style="margin: 0">
        基于近期指标的线性外推：预测点、置信带与耗尽时间估计，外加 request 与实际用量的配额调优建议。
        所有输出仅作建议，不会自动执行任何变更。
      </p>
    </div>

    <div class="toolbar">
      <span v-if="loading" class="small muted">刷新中…</span>
      <span v-else-if="updatedAt" class="small muted">更新于 {{ updatedAt.toLocaleTimeString() }}</span>
      <span v-if="forecast?.degraded" class="chip warn">预测降级</span>
      <span style="flex: 1"></span>
      <label class="auto"><input v-model="autoRefresh" type="checkbox" /> 30s 自动刷新</label>
      <button class="ghost" :disabled="loading" @click="poll">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <p v-if="error" class="err">
      {{ error }}
      <button class="link" style="margin-left: 8px" @click="poll">重试</button>
    </p>

    <div v-if="loading && !forecast" class="grid-3">
      <div v-for="i in 3" :key="'msk-' + i" class="stat">
        <div class="sk" style="width: 45%; margin-bottom: 12px"></div>
        <div class="sk" style="height: 26px; width: 60%"></div>
      </div>
    </div>

    <template v-else-if="forecast">
      <div v-if="forecast.degraded || forecast.confidence === 'NONE'" class="panel banner">
        指标数据不足（保留窗口内样本过少），无法给出可靠预测。采集器持续运行约 10 分钟后即可形成初步拟合。
      </div>

      <div class="grid-3">
        <div class="stat">
          <div class="label">整体置信度</div>
          <div class="value">
            <span class="chip" :class="confTone(forecast.confidence)">{{ confLabel(forecast.confidence) }}</span>
          </div>
          <div class="sub">样本跨度 {{ fmtSpan(forecast.sample_span_seconds) }}</div>
        </div>
        <div class="stat">
          <div class="label">耗尽预警（90% 容量）</div>
          <div class="value">{{ exhausted90.length }}</div>
          <div class="sub">
            <span v-if="exhausted90.length" class="chip bad">
              {{ exhausted90.map(n => n.name).join('、') }}
            </span>
            <span v-else class="muted">未来 {{ forecast.horizon }} 内无节点触达 90% 容量</span>
          </div>
        </div>
        <div class="stat">
          <div class="label">配额调优建议</div>
          <div class="value">{{ rightsizing ? rightsizing.over_provisioned + rightsizing.under_provisioned : 0 }}</div>
          <div class="sub">
            超配 {{ rightsizing?.over_provisioned ?? 0 }} · 低配 {{ rightsizing?.under_provisioned ?? 0 }} ·
            缺用量 {{ rightsizing?.unknown ?? 0 }}
          </div>
        </div>
      </div>

      <section class="panel">
        <h3>
          {{ chartTarget === 'cluster' ? '集群' : chartTarget }} 负载外推
          <span style="flex: 1"></span>
          <span class="seg">
            <button v-for="h in horizons" :key="h.value" :class="{ active: horizon === h.value }" @click="horizon = h.value">
              {{ h.label }}
            </button>
          </span>
        </h3>
        <div class="toolbar" style="margin-bottom: 8px">
          <span class="small muted">查看对象：</span>
          <span class="seg">
            <button :class="{ active: chartTarget === 'cluster' }" @click="chartTarget = 'cluster'">集群</button>
            <button v-for="n in forecast.nodes" :key="n.name" :class="{ active: chartTarget === n.name }" @click="chartTarget = n.name">
              {{ n.name }}
            </button>
          </span>
        </div>
        <div ref="trendEl" class="chart"></div>
        <p class="hint">实线为历史（1 分钟桶），虚线为线性外推，阴影为 ±标准误置信带；仅展示当前对象的两维指标。</p>
      </section>

      <section class="panel">
        <h3>
          节点预测明细
          <span class="chip">{{ forecast.nodes.length }} 个</span>
        </h3>
        <table>
          <thead>
            <tr>
              <th>节点</th>
              <th>置信度</th>
              <th>当前占用</th>
              <th>R²</th>
              <th>斜率 (CPU / 内存)</th>
              <th>90% 耗尽</th>
              <th>100% 耗尽</th>
              <th>样本跨度</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in forecast.nodes" :key="n.name">
              <td><strong>{{ n.name }}</strong></td>
              <td><span class="chip" :class="confTone(n.confidence)">{{ confLabel(n.confidence) }}</span></td>
              <td>{{ pct(n.usage_ratio) }}</td>
              <td>{{ n.r2.toFixed(2) }}</td>
              <td class="small">
                {{ slopeText(n.slope_cpu_per_min, '毫核/分') }} · {{ slopeText(n.slope_mem_per_min / 1024 / 1024, 'MiB/分') }}
              </td>
              <td>
                <span v-if="n.time_to_full_90_seconds != null" class="chip" :class="ttfTone(n.time_to_full_90_seconds)">
                  {{ fmtTtf(n.time_to_full_90_seconds) }}
                </span>
                <span v-else class="muted">—</span>
              </td>
              <td>
                <span v-if="n.time_to_full_100_seconds != null">{{ fmtTtf(n.time_to_full_100_seconds) }}</span>
                <span v-else class="muted">—</span>
              </td>
              <td class="small">{{ fmtSpan(n.sample_span_seconds) }}</td>
            </tr>
          </tbody>
        </table>
        <p v-if="!forecast.nodes.length" class="empty">暂无节点样本，等待采集器积累数据。</p>
      </section>

      <section v-if="rightsizing" class="panel">
        <h3>
          配额调优建议（request vs 实际用量）
          <span style="flex: 1"></span>
          <select v-model="rsFilter">
            <option value="ALL">全部 ({{ rightsizing.items.length }})</option>
            <option value="OVER_PROVISIONED">超配 ({{ rightsizing.over_provisioned }})</option>
            <option value="UNDER_PROVISIONED">低配 ({{ rightsizing.under_provisioned }})</option>
            <option value="UNKNOWN">缺用量 ({{ rightsizing.unknown }})</option>
          </select>
        </h3>
        <table>
          <thead>
            <tr>
              <th>Pod</th>
              <th>节点</th>
              <th>request (CPU / 内存)</th>
              <th>实际用量</th>
              <th>CPU / 内存利用率</th>
              <th>分类</th>
              <th>建议</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="it in filteredItems" :key="it.namespace + '/' + it.name">
              <td>
                <strong>{{ it.namespace }}/{{ it.name }}</strong>
                <div v-if="it.tenant" class="small muted">{{ it.tenant }}</div>
              </td>
              <td class="small">{{ it.node }}</td>
              <td class="small">{{ it.requests.cpu_milli }} 毫核 · {{ gib(it.requests.mem_bytes) }} GiB</td>
              <td class="small">
                <template v-if="it.usage">{{ it.usage.cpu_milli.toFixed(0) }} 毫核 · {{ gib(it.usage.mem_bytes) }} GiB</template>
                <span v-else class="muted">无数据</span>
              </td>
              <td class="small">
                <template v-if="it.cpu_ratio != null">{{ pct(it.cpu_ratio) }} / {{ pct(it.mem_ratio || 0) }}</template>
                <span v-else class="muted">—</span>
              </td>
              <td>
                <span class="chip" :class="rsTone(it.classification)">{{ rsLabel(it.classification) }}</span>
              </td>
              <td class="small">
                <template v-if="it.recommendation">
                  {{ it.recommendation.cpu_milli }} 毫核 · {{ gib(it.recommendation.mem_bytes) }} GiB
                </template>
                {{ it.recommend_note }}
              </td>
            </tr>
          </tbody>
        </table>
        <p v-if="!filteredItems.length" class="empty">该分类下暂无条目。</p>
        <p class="hint">超配：两维利用率均 &lt;30%，建议按用量 ×1.3 收缩；低配：两维均 &gt;95%，建议按 request ×1.5 上调。仅建议，不自动变更。</p>
      </section>
    </template>

    <p v-else-if="!loading" class="empty">暂无预测数据，请确认 API 已连接集群。</p>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, gib, pct, type CapacityForecast, type NodeForecast, type RightsizingReport } from '../api'
import { useChart } from '../lib/chart'

const AXIS = '#8b93a7'
const LINE = '#242a33'

const horizons = [
  { label: '15m', value: '15m' },
  { label: '30m', value: '30m' },
  { label: '1h', value: '1h' },
  { label: '3h', value: '3h' }
]

const forecast = ref<CapacityForecast | null>(null)
const rightsizing = ref<RightsizingReport | null>(null)
const horizon = ref('30m')
const chartTarget = ref('cluster')
const rsFilter = ref('ALL')
const loading = ref(false)
const error = ref('')
const autoRefresh = ref(true)
const updatedAt = ref<Date | null>(null)

const trendEl = ref<HTMLElement | null>(null)
const trendChart = useChart(trendEl)

const exhausted90 = computed(() =>
  (forecast.value?.nodes || []).filter(n => n.time_to_full_90_seconds != null)
)
const chartNode = computed<NodeForecast | null>(() => {
  const f = forecast.value
  if (!f) return null
  if (chartTarget.value === 'cluster') return f.cluster
  return f.nodes.find(n => n.name === chartTarget.value) || null
})
const filteredItems = computed(() => {
  const items = rightsizing.value?.items || []
  if (rsFilter.value === 'ALL') return items
  return items.filter(it => it.classification === rsFilter.value)
})

const confLabel = (c: string) =>
  c === 'HIGH' ? '高' : c === 'MEDIUM' ? '中' : c === 'LOW' ? '低' : '数据不足'
const confTone = (c: string) =>
  c === 'HIGH' ? 'ok' : c === 'MEDIUM' ? 'info' : c === 'LOW' ? 'warn' : 'warn'
const ttfTone = (sec: number) => (sec <= 900 ? 'bad' : sec <= 1800 ? 'warn' : '')
const rsLabel = (c: string) =>
  c === 'OVER_PROVISIONED' ? '超配' : c === 'UNDER_PROVISIONED' ? '低配' : c === 'UNKNOWN' ? '缺用量' : '合理'
const rsTone = (c: string) =>
  c === 'OVER_PROVISIONED' ? 'warn' : c === 'UNDER_PROVISIONED' ? 'bad' : c === 'UNKNOWN' ? '' : 'ok'
const slopeText = (v: number, unit: string) =>
  v === 0 ? '0' : (v > 0 ? '+' : '') + v.toFixed(1) + ' ' + unit
const fmtSpan = (sec: number) => {
  if (!sec) return '—'
  if (sec < 3600) return Math.round(sec / 60) + ' 分钟'
  return (sec / 3600).toFixed(1) + ' 小时'
}
const fmtTtf = (sec: number) => {
  if (sec < 60) return sec.toFixed(0) + 's'
  if (sec < 3600) return Math.round(sec / 60) + ' 分钟'
  return (sec / 3600).toFixed(1) + ' 小时'
}

async function poll() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const [fc, rs] = await Promise.all([api.forecastCapacity(horizon.value), api.rightsizing()])
    forecast.value = fc
    rightsizing.value = rs
    if (chartTarget.value !== 'cluster' && !fc.nodes.some(n => n.name === chartTarget.value)) {
      chartTarget.value = 'cluster'
    }
    updatedAt.value = new Date()
    await nextTick()
    drawTrend()
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

/** 历史实线 + 预测虚线（首点接历史末点保证连续）+ 置信带（stack 透明线 + 阴影）。 */
function drawTrend() {
  const node = chartNode.value
  if (!node || !node.history.length) {
    trendChart.render({
      grid: { left: 56, right: 56, top: 30, bottom: 28 },
      xAxis: { type: 'category', data: [], axisLabel: { color: AXIS } },
      yAxis: [{ type: 'value' }],
      series: []
    })
    return
  }

  const hist = node.history
  const pred = node.prediction || []
  const x = [
    ...hist.map(p => new Date(p.ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })),
    ...pred.map(p => new Date(p.ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }))
  ]
  const pad = (arr: (number | null)[], total: number) => arr.concat(new Array(total - arr.length).fill(null))

  const cpuHist = pad(hist.map(p => Math.round(p.cpu_milli)), x.length)
  const memHist = pad(hist.map(p => Math.round(p.mem_bytes / 1024 / 1024)), x.length)
  // 预测线从历史末点接出，视觉上连续。
  const predStart = hist.length - 1
  const cpuPred = new Array(predStart).fill(null)
    .concat(hist.length ? [hist[hist.length - 1].cpu_milli] : [])
    .concat(pred.map(p => Math.round(p.cpu_milli)))
  const memPred = new Array(predStart).fill(null)
    .concat(hist.length ? [Math.round(hist[hist.length - 1].mem_bytes / 1024 / 1024)] : [])
    .concat(pred.map(p => Math.round(p.mem_bytes / 1024 / 1024)))

  const series: any[] = [
    {
      name: 'CPU 历史 (m)',
      type: 'line',
      showSymbol: false,
      data: cpuHist,
      lineStyle: { color: '#e7a33e', width: 2 },
      itemStyle: { color: '#e7a33e' }
    },
    {
      name: '内存历史 (MiB)',
      type: 'line',
      yAxisIndex: 1,
      showSymbol: false,
      data: memHist,
      lineStyle: { color: '#7aa2f7', width: 2 },
      itemStyle: { color: '#7aa2f7' }
    }
  ]

  if (pred.length) {
    series.push(
      {
        name: 'CPU 预测 (m)',
        type: 'line',
        showSymbol: false,
        data: cpuPred,
        lineStyle: { color: '#e7a33e', width: 2, type: 'dashed' },
        itemStyle: { color: '#e7a33e' }
      },
      {
        name: '内存预测 (MiB)',
        type: 'line',
        yAxisIndex: 1,
        showSymbol: false,
        data: memPred,
        lineStyle: { color: '#7aa2f7', width: 2, type: 'dashed' },
        itemStyle: { color: '#7aa2f7' }
      }
    )

    // 置信带：stack 两段透明线，上段带浅色面积形成阴影带。
    const bandCpuLower = new Array(predStart).fill(null)
      .concat(pred.map(p => Math.max(0, Math.round(p.cpu_milli))))
    const bandCpuRange = new Array(predStart).fill(null)
      .concat(node.band.map((b, i) =>
        Math.max(0, Math.round(b.cpu_upper)) - Math.max(0, Math.round(b.cpu_lower))))
    const bandMemLower = new Array(predStart).fill(null)
      .concat(pred.map(p => Math.max(0, Math.round(p.mem_bytes / 1024 / 1024))))
    const bandMemRange = new Array(predStart).fill(null)
      .concat(node.band.map(b =>
        Math.max(0, Math.round(b.mem_upper / 1024 / 1024)) - Math.max(0, Math.round(b.mem_lower / 1024 / 1024))))
    series.push(
      {
        name: 'cpu-band-base',
        type: 'line',
        stack: 'cpu-band',
        showSymbol: false,
        data: bandCpuLower,
        lineStyle: { opacity: 0 },
        areaStyle: { opacity: 0 },
        silent: true
      },
      {
        name: 'CPU 置信带',
        type: 'line',
        stack: 'cpu-band',
        showSymbol: false,
        data: bandCpuRange,
        lineStyle: { opacity: 0 },
        areaStyle: { color: 'rgba(231,163,62,0.15)' },
        silent: true
      },
      {
        name: 'mem-band-base',
        type: 'line',
        yAxisIndex: 1,
        stack: 'mem-band',
        showSymbol: false,
        data: bandMemLower,
        lineStyle: { opacity: 0 },
        areaStyle: { opacity: 0 },
        silent: true
      },
      {
        name: '内存置信带',
        type: 'line',
        yAxisIndex: 1,
        stack: 'mem-band',
        showSymbol: false,
        data: bandMemRange,
        lineStyle: { opacity: 0 },
        areaStyle: { color: 'rgba(122,162,247,0.12)' },
        silent: true
      }
    )
  }

  trendChart.render({
    grid: { left: 56, right: 56, top: 34, bottom: 44 },
    tooltip: { trigger: 'axis' },
    legend: { data: ['CPU 历史 (m)', 'CPU 预测 (m)', '内存历史 (MiB)', '内存预测 (MiB)'], textStyle: { color: AXIS }, top: 0 },
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
    ],
    series
  })
}

watch(horizon, poll)
watch(autoRefresh, armTimer)
watch(chartTarget, () => nextTick(drawTrend))

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
