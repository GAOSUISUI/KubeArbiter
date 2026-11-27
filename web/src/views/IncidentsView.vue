<template>
  <div class="stack">
    <div class="page-head">
      <h2>异常事件</h2>
      <p class="muted small" style="margin: 0">
        检测器每 30 秒扫描一次节点、Pod 与租户配额；条件消失后连续两次未命中会自动恢复。事件只读，不会触发任何集群变更。
      </p>
    </div>

    <div class="toolbar">
      <select v-model="filters.status">
        <option value="">全部状态</option>
        <option value="OPEN">未解决</option>
        <option value="RESOLVED">已恢复</option>
      </select>
      <select v-model="filters.type">
        <option value="">全部类型</option>
        <option v-for="(label, key) in INCIDENT_LABELS" :key="key" :value="key">{{ label }}</option>
      </select>
      <select v-model="filters.severity">
        <option value="">全部严重度</option>
        <option value="CRITICAL">严重</option>
        <option value="WARN">告警</option>
        <option value="INFO">提示</option>
      </select>
      <label class="auto"><input v-model="autoRefresh" type="checkbox" /> 30s 自动刷新</label>
      <span v-if="loading" class="small muted">刷新中…</span>
      <span v-else-if="updatedAt" class="small muted">更新于 {{ updatedAt.toLocaleTimeString() }}</span>
      <span style="flex: 1"></span>
      <button class="ghost" :disabled="loading" @click="poll">{{ loading ? '刷新中…' : '刷新' }}</button>
    </div>

    <p v-if="error" class="err">
      {{ error }}
      <button class="link" style="margin-left: 8px" @click="poll">重试</button>
    </p>

    <section class="panel">
      <h3>
        事件列表
        <span class="chip">{{ items.length }} 条</span>
      </h3>
      <table>
        <thead>
          <tr>
            <th>严重度</th>
            <th>类型</th>
            <th>范围</th>
            <th>对象</th>
            <th>状态</th>
            <th>首次发现</th>
            <th>恢复时间</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="inc in items"
            :key="inc.id"
            :class="{ active: selected?.id === inc.id }"
            style="cursor: pointer"
            @click="openDetail(inc.id)"
          >
            <td><span class="chip" :class="severityTone(inc.severity)">{{ severityLabel(inc.severity) }}</span></td>
            <td>{{ INCIDENT_LABELS[inc.type] || inc.type }}</td>
            <td>{{ scopeLabel(inc.namespace) }}</td>
            <td><strong>{{ inc.target }}</strong></td>
            <td>
              <span class="chip" :class="inc.status === 'OPEN' ? 'bad' : 'ok'">
                {{ inc.status === 'OPEN' ? '未解决' : '已恢复' }}
              </span>
            </td>
            <td class="small">{{ fmtTime(inc.first_seen) }}</td>
            <td class="small">{{ fmtTime(inc.resolved_at) }}</td>
          </tr>
        </tbody>
      </table>
      <p v-if="!items.length && !loading" class="empty">当前筛选条件下没有事件</p>
    </section>

    <section v-if="selected" class="panel">
      <h3>
        {{ INCIDENT_LABELS[selected.type] || selected.type }}
        <span class="chip" :class="severityTone(selected.severity)">{{ severityLabel(selected.severity) }}</span>
        <span class="chip" :class="selected.status === 'OPEN' ? 'bad' : 'ok'">
          {{ selected.status === 'OPEN' ? '未解决' : '已恢复' }}
        </span>
        <span style="flex: 1"></span>
        <button
          v-if="selected.status === 'OPEN'"
          class="ghost"
          :disabled="diagnosing"
          @click="rerunDiagnosis"
        >
          {{ diagnosing ? '诊断中…' : '重新诊断' }}
        </button>
      </h3>

      <div class="grid-2">
        <div class="kv">
          <span class="k">事件 ID</span><span class="mono small">{{ selected.id }}</span>
          <span class="k">范围 / 对象</span><span>{{ scopeLabel(selected.namespace) }} / {{ selected.target }}</span>
          <span class="k">首次发现</span><span>{{ fmtTime(selected.first_seen) }}</span>
          <span class="k">恢复时间</span><span>{{ fmtTime(selected.resolved_at) }}</span>
          <span class="k">诊断时间</span><span>{{ fmtTime(selected.diagnosis?.created_at) }}</span>
        </div>
        <div>
          <h4 class="section-title">诊断推断</h4>
          <div v-if="selected.diagnosis?.inferences?.length" class="stack-sm">
            <div v-for="(inf, i) in selected.diagnosis.inferences" :key="'inf-' + i" class="inference">
              <div style="display: flex; align-items: center; gap: 8px">
                <strong>{{ inf.root_cause }}</strong>
                <span class="chip" :class="confidenceTone(inf.confidence)">{{ inf.confidence }}</span>
              </div>
              <ul v-if="inf.evidence?.length" class="small">
                <li v-for="(ev, j) in inf.evidence" :key="'ev-' + j">{{ ev }}</li>
              </ul>
              <p v-if="inf.counter_evidence?.length" class="small muted" style="margin: 4px 0 0">
                反证：{{ inf.counter_evidence.join('；') }}
              </p>
            </div>
          </div>
          <p v-else class="muted small">暂无诊断推断</p>
        </div>
      </div>

      <h4 class="section-title">观测事实</h4>
      <table>
        <thead>
          <tr>
            <th>阶段</th>
            <th>时间</th>
            <th>对象</th>
            <th>说明</th>
            <th>数值</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(fact, i) in selected.diagnosis?.facts || []" :key="'fact-' + i">
            <td><span class="chip">{{ fact.stage }}</span></td>
            <td class="small">{{ fmtTime(fact.at) }}</td>
            <td class="small">{{ fact.object || '—' }}</td>
            <td>{{ fact.summary }}</td>
            <td class="mono small">{{ fact.values ? jsonText(fact.values) : '—' }}</td>
          </tr>
        </tbody>
      </table>
      <p v-if="!selected.diagnosis?.facts?.length" class="empty">暂无事实记录</p>

      <h4 class="section-title">恢复动作</h4>
      <p v-if="!selected.recovery_actions?.length" class="muted small">
        检测器不执行恢复动作；需要变更时请通过模拟工作台提交任务并走审批。
      </p>
      <table v-else>
        <thead>
          <tr><th>动作</th><th>任务</th><th>审批人</th><th>结果</th><th>时间</th></tr>
        </thead>
        <tbody>
          <tr v-for="action in selected.recovery_actions" :key="action.id">
            <td>{{ action.action }}</td>
            <td class="mono small">{{ action.task_id || '—' }}</td>
            <td>{{ action.approved_by || '—' }}</td>
            <td>{{ action.result }}</td>
            <td class="small">{{ fmtTime(action.created_at) }}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <p v-else-if="!loading && items.length" class="muted small">点击任意事件查看诊断详情。</p>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  api,
  fmtTime,
  INCIDENT_LABELS,
  jsonText,
  severityTone,
  type Incident,
  type IncidentDetail
} from '../api'
import { toastBad, toastOk } from '../lib/feedback'

const filters = reactive({ status: 'OPEN', type: '', severity: '' })
const items = ref<Incident[]>([])
const selected = ref<IncidentDetail | null>(null)
const loading = ref(false)
const diagnosing = ref(false)
const error = ref('')
const updatedAt = ref<Date | null>(null)
const autoRefresh = ref(true)

const severityLabel = (severity: string) =>
  severity === 'CRITICAL' ? '严重' : severity === 'WARN' ? '告警' : '提示'
const confidenceTone = (confidence: string) =>
  confidence === 'HIGH' ? 'ok' : confidence === 'MEDIUM' ? 'warn' : ''
const scopeLabel = (namespace: string) => {
  switch (namespace) {
    case '__node__':
      return '节点'
    case '__cluster__':
      return '集群'
    case '__tenant__':
      return '租户'
    default:
      return namespace || '—'
  }
}

async function poll() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const page = await api.incidents({
      status: filters.status,
      type: filters.type,
      severity: filters.severity,
      limit: 100
    })
    items.value = page.items || []
    updatedAt.value = new Date()
    if (selected.value && !items.value.some(item => item.id === selected.value?.id)) {
      selected.value = null
    }
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

async function openDetail(id: string) {
  try {
    selected.value = await api.incident(id)
  } catch (e: any) {
    toastBad(e?.message || String(e))
  }
}

async function rerunDiagnosis() {
  if (!selected.value) return
  diagnosing.value = true
  try {
    await api.runDiagnosis({ incident_id: selected.value.id })
    toastOk('诊断已更新')
    await openDetail(selected.value.id)
  } catch (e: any) {
    toastBad(e?.message || String(e))
  } finally {
    diagnosing.value = false
  }
}

watch(filters, poll)

let timer: number | undefined
function armTimer() {
  if (timer) clearInterval(timer)
  timer = undefined
  if (autoRefresh.value) timer = window.setInterval(poll, 30000)
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

<style scoped>
tr.active td {
  background: rgba(122, 162, 247, 0.08);
}
.section-title {
  margin: 18px 0 8px;
  font-size: 13px;
  color: #c9d1e3;
}
.inference {
  border: 1px solid #242a33;
  border-radius: 8px;
  padding: 10px 12px;
}
.inference ul {
  margin: 6px 0 0;
  padding-left: 18px;
}
.stack-sm {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}
</style>
