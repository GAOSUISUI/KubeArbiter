<template>
  <div class="stack">
    <div class="page-head">
      <h2>SLO 治理</h2>
      <p class="muted small" style="margin: 0">
        平台内置采集 arbiter-api 自身的可用性 SLI；其他服务的分钟桶需要由外部预聚合写入。数据不足时显示"数据积累中"，不会用 0 值代替。
      </p>
    </div>

    <div class="toolbar">
      <span v-if="loading" class="small muted">加载中…</span>
      <span v-else-if="updatedAt" class="small muted">更新于 {{ updatedAt.toLocaleTimeString() }}</span>
      <span style="flex: 1"></span>
      <button class="ghost" :disabled="loading" @click="reload">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <p v-if="error" class="err">
      {{ error }}
      <button class="link" style="margin-left: 8px" @click="reload">重试</button>
    </p>

    <section class="panel">
      <h3>
        SLO 定义
        <span class="chip">{{ list.length }} 条</span>
      </h3>
      <table>
        <thead>
          <tr>
            <th>服务</th>
            <th>SLI</th>
            <th>目标</th>
            <th>当前达成</th>
            <th>请求数</th>
            <th>预算余量</th>
            <th>发布放行</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="item in list"
            :key="item.service + item.sli_type"
            :class="{ active: selectedKey === item.service + item.sli_type }"
            style="cursor: pointer"
            @click="select(item)"
          >
            <td><strong>{{ item.service }}</strong></td>
            <td>{{ item.sli_type === 'LATENCY' ? '延迟' : '可用性' }}</td>
            <td>{{ targetText(item.target) }}</td>
            <td>
              <span v-if="item.total_requests === 0" class="muted small">无请求</span>
              <span v-else>{{ pct(item.current) }}</span>
            </td>
            <td>{{ item.total_requests }}</td>
            <td>{{ pct(item.budget_remaining) }}</td>
            <td>
              <span class="chip" :class="item.publishing_allowed ? 'ok' : 'warn'">
                {{ item.publishing_allowed ? '允许' : '阻断' }}
              </span>
            </td>
            <td>
              <span v-if="item.degraded" class="chip warn">数据积累中</span>
              <span v-else class="chip ok">正常</span>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="!list.length && !loading" class="empty">暂无 SLO 定义，可在下方创建。</p>
    </section>

    <section v-if="detail" class="panel">
      <h3>
        {{ detail.service }} · {{ detail.sli_type === 'LATENCY' ? '延迟' : '可用性' }}
        <span class="chip">{{ targetText(detail.target) }}</span>
        <span v-if="detail.degraded || budget?.degraded" class="chip warn">数据积累中</span>
        <span style="flex: 1"></span>
        <button class="ghost" @click="startEdit(detail)">编辑</button>
        <button class="ghost danger" @click="remove(detail)">删除</button>
      </h3>

      <div v-if="budget" class="grid-3">
        <div class="stat">
          <div class="label">当前达成（{{ budget.window }}）</div>
          <div class="value">{{ budget.total_requests === 0 ? '无请求' : pct(budget.total_requests === 0 ? 0 : 1 - budget.budget_consumed) }}</div>
          <div class="sub">{{ budget.total_requests }} 个请求</div>
        </div>
        <div class="stat">
          <div class="label">预算余量</div>
          <div class="value">{{ pct(budget.budget_remaining) }}</div>
          <div class="sub">允许失败 {{ pct(budget.budget_total) }} · 已用 {{ pct(budget.budget_consumed) }}</div>
        </div>
        <div class="stat">
          <div class="label">数据完整度</div>
          <div class="value">{{ pct(budget.completeness) }}</div>
          <div class="sub">
            <span class="chip" :class="budget.degraded ? 'warn' : 'ok'">
              {{ budget.degraded ? '数据积累中' : '完整' }}
            </span>
          </div>
        </div>
      </div>

      <div v-if="budget" class="kv" style="margin-top: 12px">
        <span class="k">治理动作</span><span><span class="chip" :class="actionTone(budget.action)">{{ actionLabel(budget.action) }}</span></span>
        <span class="k">触发原因</span><span class="small">{{ budget.reasons?.length ? budget.reasons.join('、') : '无' }}</span>
        <span class="k">延迟阈值</span><span>{{ detail.latency_threshold_ms ? detail.latency_threshold_ms + ' ms' : '—' }}</span>
      </div>

      <h4 class="section-title">燃尽率窗口</h4>
      <table>
        <thead>
          <tr>
            <th>长窗口</th>
            <th>短窗口</th>
            <th>阈值</th>
            <th>燃尽率</th>
            <th>触发</th>
            <th>完整度</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="w in detail.windows" :key="w.long_window + w.short_window">
            <td>{{ w.long_window }}</td>
            <td>{{ w.short_window }}</td>
            <td>{{ w.threshold }}</td>
            <td>{{ w.burn_rate.toFixed(2) }}</td>
            <td>
              <span class="chip" :class="w.firing ? 'bad' : 'ok'">{{ w.firing ? 'Firing' : '正常' }}</span>
            </td>
            <td>{{ pct(w.completeness) }}</td>
          </tr>
        </tbody>
      </table>
      <p v-if="!detail.windows.length" class="empty">暂无燃尽率窗口</p>
    </section>

    <section class="panel">
      <h3>
        {{ editingExisting ? '编辑 SLO' : '创建 SLO' }}
        <span style="flex: 1"></span>
        <button v-if="editingExisting" class="ghost" @click="resetForm">取消编辑</button>
      </h3>
      <div class="form-grid">
        <label>
          <span class="small muted">服务名</span>
          <input v-model="form.service" :disabled="editingExisting" placeholder="例如 arbiter-api" />
        </label>
        <label>
          <span class="small muted">SLI 类型</span>
          <select v-model="form.sli_type" :disabled="editingExisting">
            <option value="AVAILABILITY">可用性</option>
            <option value="LATENCY">延迟</option>
          </select>
        </label>
        <label>
          <span class="small muted">目标（%）</span>
          <input v-model.number="form.target_percent" type="number" min="0" max="100" step="0.1" />
        </label>
        <label>
          <span class="small muted">统计窗口</span>
          <select v-model.number="form.window_seconds" :disabled="editingExisting">
            <option :value="3600">1h</option>
            <option :value="21600">6h</option>
            <option :value="259200">3d</option>
            <option :value="2592000">30d</option>
          </select>
        </label>
        <label v-if="form.sli_type === 'LATENCY'">
          <span class="small muted">延迟阈值（ms）</span>
          <input v-model.number="form.latency_threshold_ms" type="number" min="1" />
        </label>
      </div>
      <div style="display: flex; gap: 8px; margin-top: 12px">
        <button class="primary" :disabled="saving" @click="submit">
          {{ saving ? '保存中…' : editingExisting ? '保存修改' : '创建' }}
        </button>
        <button class="ghost" :disabled="saving" @click="resetForm">重置</button>
      </div>
      <p class="hint">
        arbiter-api 的可用性 SLO 由后端自动创建并采集；其他服务的 slo_windows 需要外部写入，否则会显示"数据积累中"。
      </p>
    </section>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  api,
  pct,
  type SLOBudget,
  type SLODetail,
  type SLODefinitionInput,
  type SLOStatus
} from '../api'
import { confirmAction, toastBad, toastOk } from '../lib/feedback'

const list = ref<SLOStatus[]>([])
const detail = ref<SLODetail | null>(null)
const budget = ref<SLOBudget | null>(null)
const selectedKey = ref('')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const updatedAt = ref<Date | null>(null)
const editingExisting = ref(false)

const form = reactive({
  service: '',
  sli_type: 'AVAILABILITY',
  target_percent: 99.5,
  window_seconds: 3600,
  latency_threshold_ms: 200
})

const targetText = (target: number) => (target * 100).toFixed(2).replace(/\.?0+$/, '') + '%'

const actionLabel = (action: string) => {
  switch (action) {
    case 'BLOCK_FULL_ROLLOUT':
      return '阻断全量'
    case 'ROLLBACK':
      return '建议回滚'
    case 'ALERT':
      return '告警'
    case 'TICKET':
      return '建议工单'
    default:
      return '无需动作'
  }
}
const actionTone = (action: string) => {
  switch (action) {
    case 'BLOCK_FULL_ROLLOUT':
    case 'ROLLBACK':
      return 'bad'
    case 'ALERT':
    case 'TICKET':
      return 'warn'
    default:
      return 'ok'
  }
}

async function reload() {
  loading.value = true
  error.value = ''
  try {
    list.value = await api.sloList()
    updatedAt.value = new Date()
    if (!list.value.length) {
      detail.value = null
      budget.value = null
      selectedKey.value = ''
      return
    }
    const current =
      list.value.find(item => item.service + item.sli_type === selectedKey.value) || list.value[0]
    await select(current)
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

async function select(item: SLOStatus) {
  selectedKey.value = item.service + item.sli_type
  try {
    const [d, b] = await Promise.all([api.sloDetail(item.service), api.sloBudget(item.service)])
    detail.value = d
    budget.value = b
  } catch (e: any) {
    toastBad(e?.message || String(e))
  }
}

function startEdit(d: SLODetail) {
  editingExisting.value = true
  form.service = d.service
  form.sli_type = d.sli_type
  form.target_percent = Number((d.target * 100).toFixed(2))
  form.window_seconds = windowSeconds(budget.value?.window || '1h')
  form.latency_threshold_ms = d.latency_threshold_ms || 200
}

function resetForm() {
  editingExisting.value = false
  form.service = ''
  form.sli_type = 'AVAILABILITY'
  form.target_percent = 99.5
  form.window_seconds = 3600
  form.latency_threshold_ms = 200
}

async function submit() {
  if (!form.service.trim()) {
    toastBad('服务名不能为空')
    return
  }
  if (form.target_percent <= 0 || form.target_percent >= 100) {
    toastBad('目标必须在 0 到 100 之间')
    return
  }
  saving.value = true
  const body: SLODefinitionInput = {
    service: form.service.trim(),
    sli_type: form.sli_type,
    target: form.target_percent / 100,
    window_seconds: form.window_seconds,
    latency_threshold_ms:
      form.sli_type === 'LATENCY' ? Number(form.latency_threshold_ms) || null : null
  }
  try {
    if (editingExisting.value) {
      await api.updateSLO(body.service, body.sli_type, body)
      toastOk('SLO 已更新')
    } else {
      await api.createSLO(body)
      toastOk('SLO 已创建')
    }
    selectedKey.value = body.service + body.sli_type
    resetForm()
    await reload()
  } catch (e: any) {
    toastBad(e?.message || String(e))
  } finally {
    saving.value = false
  }
}

async function remove(d: SLODetail) {
  const ok = await confirmAction({
    title: '删除 SLO',
    message: `确认删除 ${d.service} 的 ${d.sli_type} SLO？历史分钟桶不会被删除。`,
    okText: '删除',
    danger: true
  })
  if (!ok) return
  try {
    await api.deleteSLO(d.service, d.sli_type)
    toastOk('SLO 已删除')
    if (selectedKey.value === d.service + d.sli_type) selectedKey.value = ''
    await reload()
  } catch (e: any) {
    toastBad(e?.message || String(e))
  }
}

function windowSeconds(window: string) {
  const map: Record<string, number> = {
    '1h': 3600,
    '6h': 21600,
    '3d': 259200,
    '30d': 2592000
  }
  return map[window] || 3600
}

onMounted(reload)
</script>

<style scoped>
tr.active td {
  background: rgba(122, 162, 247, 0.08);
}
.section-title {
  margin: 18px 0 8px;
  font-size: 13px;
  color: #c9d1e3;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
}
.form-grid label {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
</style>
