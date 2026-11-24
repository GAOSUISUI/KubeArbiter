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

          <div ref="chartEl" class="chart"></div>

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
                <tr v-for="s in sortedScores" :key="s.node">
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
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import {
  api,
  WEIGHT_KEYS,
  WEIGHT_LABELS,
  shortID,
  type Decision,
  type Policy,
  type TenantItem,
  type WeightKey,
  type Weights
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

const policies = ref<Policy[]>([])
const tenants = ref<TenantItem[]>([])
const policyId = ref('')
const tenantId = ref('')
const loading = ref(false)
const auto = ref(true)
const error = ref('')
const result = ref<Decision | null>(null)

const workload = reactive({ name: 'demo-api', namespace: 'team-a', replicas: 1, cpu: 500, mem: 256 })
const weightsPct = reactive<Record<WeightKey, number>>({
  s_cpu: 18,
  s_mem: 18,
  s_balance: 25,
  s_pack: 10,
  s_load: 15,
  s_fair: 10,
  s_frag: 4
})
const custom = ref(false)
const saveName = ref('')
const saveMsg = ref('')

const sumPct = computed(() => WEIGHT_KEYS.reduce((s, k) => s + weightsPct[k], 0))
const selectedPolicy = computed(() => policies.value.find(p => p.id === policyId.value))
const sortedScores = computed(() =>
  result.value ? [...result.value.scores].sort((a, b) => b.score - a.score) : []
)

const chartEl = ref<HTMLElement | null>(null)
const { render } = useChart(chartEl)

function applyPreset() {
  const p = selectedPolicy.value
  if (!p) return
  WEIGHT_KEYS.forEach(k => {
    weightsPct[k] = Math.round((p.weights?.[k] ?? 0) * 100)
  })
  custom.value = false
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
  try {
    result.value = await api.simulate({
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
    await nextTick()
    draw()
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

function draw() {
  const d = result.value
  if (!d) return
  const rows = [...d.scores].sort((a, b) => b.score - a.score)
  render({
    grid: { left: 88, right: 52, top: 12, bottom: 26 },
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    xAxis: {
      type: 'value',
      max: 100,
      axisLabel: { color: '#8b93a7' },
      splitLine: { lineStyle: { color: '#242a33' } }
    },
    yAxis: {
      type: 'category',
      data: rows.map(r => r.node),
      inverse: true,
      axisLabel: { color: '#8b93a7' },
      axisLine: { lineStyle: { color: '#242a33' } }
    },
    series: [
      {
        type: 'bar',
        barWidth: 18,
        data: rows.map(r => ({
          value: +r.score.toFixed(2),
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

async function loadPolicies(select?: string) {
  policies.value = await api.listPolicies()
  if (select) policyId.value = select
  else if (!policyId.value) {
    policyId.value = (policies.value.find(p => p.is_default) || policies.value[0])?.id || ''
  }
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
  } catch (e: any) {
    saveMsg.value = '保存失败：' + (e?.message || e)
  }
}

let timer: number | undefined
watch(
  [workload, weightsPct, policyId, tenantId],
  () => {
    saveMsg.value = ''
    if (!auto.value) return
    clearTimeout(timer)
    timer = window.setTimeout(run, 450)
  },
  { deep: true }
)

onMounted(async () => {
  try {
    await loadPolicies()
    tenants.value = (await api.tenants()).items || []
    tenantId.value = tenants.value[0]?.tenant_id || ''
    applyPreset()
    await run()
  } catch (e: any) {
    error.value = e?.message || String(e)
  }
})
</script>
