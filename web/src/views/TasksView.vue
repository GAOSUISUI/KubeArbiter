<template>
  <div class="stack">
    <div class="page-head">
      <h2>任务与审计</h2>
      <p class="muted small" style="margin: 0">
        决策落地后会生成受控任务，需人工审批后才进入执行队列。当前身份：<strong>{{ actor || 'anonymous' }}</strong>
        （可在左下角切换，审批人不能是任务创建人，admin 例外）。
      </p>
    </div>

    <div class="grid-3">
      <div class="stat">
        <div class="label">本页待审批</div>
        <div class="value">{{ pendingApproval.length }}</div>
        <div class="sub">需要人工确认后才能执行</div>
      </div>
      <div class="stat">
        <div class="label">本页执行中</div>
        <div class="value">{{ activeTasks.length }}</div>
        <div class="sub">QUEUED / RUNNING / RETRYING</div>
      </div>
      <div class="stat">
        <div class="label">本页成功率</div>
        <div class="value">{{ successRate }}</div>
        <div class="sub">已完成任务中 SUCCEEDED 占比</div>
      </div>
    </div>

    <section class="panel">
      <h3>
        任务列表
        <span class="chip">{{ tasks.length }} 条</span>
        <button class="link" @click="loadFirst">刷新</button>
        <span style="flex: 1"></span>
        <span v-if="autoRefresh" class="small muted" style="display: inline-flex; align-items: center; gap: 6px">
          <span class="dot-live"></span>更新于 {{ lastLoadedText }}
        </span>
        <label class="auto"><input v-model="autoRefresh" type="checkbox" /> 5s 自动刷新</label>
      </h3>

      <div class="field-row">
        <label>
          状态
          <select v-model="filterStatus" @change="applyFilters">
            <option value="">全部</option>
            <option v-for="s in STATUSES" :key="s" :value="s">{{ s }}</option>
          </select>
        </label>
        <label>
          类型
          <select v-model="filterType" @change="applyFilters">
            <option value="">全部</option>
            <option v-for="t in TYPES" :key="t" :value="t">{{ t }}</option>
          </select>
        </label>
        <label>
          每页
          <select v-model.number="limit" @change="applyFilters">
            <option :value="20">20</option>
            <option :value="50">50</option>
            <option :value="100">100</option>
          </select>
        </label>
      </div>

      <div v-if="selectedIds.length" class="batch-bar">
        <span>已选 <strong>{{ selectedIds.length }}</strong> 条</span>
        <button class="link" :disabled="acting || !batchApproveCount" @click="batchApprove">
          批量审批（{{ batchApproveCount }}）
        </button>
        <button class="link" style="color: var(--bad)" :disabled="acting || !batchCancelCount" @click="batchCancel">
          批量取消（{{ batchCancelCount }}）
        </button>
        <span style="flex: 1"></span>
        <button class="link" @click="clearSelection">清除选择</button>
      </div>

      <div v-if="error && !backendMissing" class="err">
        {{ error }}
        <button class="link" style="margin-left: 8px" @click="loadFirst">重试</button>
      </div>
      <div v-if="backendMissing" class="err">
        后端未提供任务/审计接口（HTTP 404），当前运行的可能仍是旧版本。重新构建并部署 arbiter-api 后刷新本页。
      </div>
      <table>
        <thead>
          <tr>
            <th style="width: 30px">
              <input
                type="checkbox"
                :checked="allSelected"
                :indeterminate.prop="someSelected"
                title="全选本页"
                @change="toggleAll"
              />
            </th>
            <th>创建时间</th>
            <th>类型</th>
            <th>状态</th>
            <th>创建人</th>
            <th class="num">重试</th>
            <th class="num">耗时</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody v-if="loading && !tasks.length">
          <tr v-for="i in 5" :key="'sk-' + i">
            <td><div class="sk" style="width: 14px; height: 14px"></div></td>
            <td><div class="sk" style="width: 110px"></div></td>
            <td><div class="sk" style="width: 90px"></div></td>
            <td><div class="sk" style="width: 70px"></div></td>
            <td><div class="sk" style="width: 50px"></div></td>
            <td><div class="sk" style="width: 28px; margin-left: auto"></div></td>
            <td><div class="sk" style="width: 40px; margin-left: auto"></div></td>
            <td><div class="sk" style="width: 70px"></div></td>
          </tr>
        </tbody>
        <tbody v-else>
          <tr
            v-for="t in tasks"
            :key="t.id"
            class="clickable"
            :class="{ selected: t.id === selectedId }"
            @click="selectTask(t.id)"
          >
            <td @click.stop>
              <input type="checkbox" :checked="isSelected(t.id)" @change="toggleSelect(t.id)" />
            </td>
            <td class="muted">{{ new Date(t.created_at).toLocaleString() }}</td>
            <td><span class="chip info">{{ t.type }}</span></td>
            <td><span class="chip" :class="statusTone(t.status)">{{ t.status }}</span></td>
            <td>{{ t.created_by }}</td>
            <td class="num muted">{{ t.retry_count }}/{{ t.max_retries }}</td>
            <td class="num muted">{{ fmtDuration(t.started_at, t.finished_at) }}</td>
            <td>
              <button v-if="t.status === 'PENDING_APPROVAL'" class="link" @click.stop="approve(t)">审批</button>
              <button v-if="cancellable(t.status)" class="link" style="color: var(--bad)" @click.stop="cancel(t)">取消</button>
              <button class="link" @click.stop="selectTask(t.id)">详情</button>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-if="!tasks.length && !loading" class="empty">没有符合条件的任务</div>
      <div v-if="nextCursor" style="margin-top: 12px; text-align: center">
        <button class="ghost" :disabled="loading" @click="loadMore">
          {{ loading ? '加载中…' : '加载更多' }}
        </button>
      </div>
    </section>

    <section v-if="selectedId" class="panel">
      <h3>
        任务详情
        <span class="tag">{{ shortID(selectedId) }}</span>
        <button class="link" @click="copyId">复制 ID</button>
        <span v-if="detail" class="chip" :class="statusTone(detail.status)">{{ detail.status }}</span>
        <span style="flex: 1"></span>
        <button v-if="detail?.status === 'PENDING_APPROVAL'" class="primary" :disabled="acting" @click="approve(detail)">
          审批通过
        </button>
        <button v-if="detail && cancellable(detail.status)" class="danger" :disabled="acting" @click="cancel(detail)">
          取消任务
        </button>
      </h3>

      <p v-if="detailLoading" class="muted small">加载中…</p>
      <p v-if="detailErr" class="err">{{ detailErr }}</p>
      <p v-if="actionErr" class="err">{{ actionErr }}</p>
      <p v-if="actionMsg" class="hint">{{ actionMsg }}</p>

      <template v-if="detail">
        <div class="toolbar" style="margin-bottom: 4px">
          <span class="small muted">时间线</span>
          <span class="seg">
            <button :class="{ active: timelineFilter === 'all' }" @click="timelineFilter = 'all'">全部</button>
            <button :class="{ active: timelineFilter === 'done' }" @click="timelineFilter = 'done'">已完成</button>
            <button :class="{ active: timelineFilter === 'pending' }" @click="timelineFilter = 'pending'">待处理</button>
          </span>
          <span v-if="!filteredTimeline.length" class="small muted">没有符合的事件</span>
        </div>
        <div class="timeline">
          <div v-for="s in filteredTimeline" :key="s.label" class="tl-step" :class="{ done: s.done }">
            <div class="tl-dot"></div>
            <div class="tl-body">
              <div class="tl-label">{{ s.label }}</div>
              <div class="small muted">{{ s.text }}</div>
            </div>
          </div>
        </div>

        <div class="grid-2" style="margin-top: 14px">
          <div>
            <div class="kv">
              <span class="k">类型</span><span>{{ detail.type }}</span>
              <span class="k">创建人</span><span>{{ detail.created_by }}</span>
              <span class="k">审批人</span><span>{{ detail.approved_by || '—' }}</span>
              <span class="k">创建时间</span><span>{{ fmtTime(detail.created_at) }}</span>
              <span class="k">开始时间</span><span>{{ fmtTime(detail.started_at) }}</span>
              <span class="k">结束时间</span><span>{{ fmtTime(detail.finished_at) }}</span>
              <span class="k">重试</span><span>{{ detail.retry_count }} / {{ detail.max_retries }}</span>
              <span class="k">错误码</span><span>{{ detail.error_code ?? '—' }}</span>
              <span class="k">错误</span><span :class="detail.error ? 'warn-text' : ''">{{ detail.error || '—' }}</span>
              <template v-if="decisionId">
                <span class="k">来源决策</span>
                <span>
                  <router-link class="link" :to="`/fairness?decision=${decisionId}`">{{ shortID(decisionId) }}</router-link>
                </span>
              </template>
            </div>
            <h3 style="margin-top: 14px">任务参数</h3>
            <pre class="json">{{ jsonText(detail.params) }}</pre>
          </div>

          <div>
            <h3>
              执行日志 <span class="chip">{{ detail.logs?.length || 0 }}</span>
              <button class="link" :disabled="!detail.logs?.length" @click="copyLogs">复制</button>
              <button class="link" :disabled="!detail.logs?.length" @click="downloadLogs">下载</button>
              <span style="flex: 1"></span>
              <label class="auto">
                <input v-model="logAutoScroll" type="checkbox" @change="persistLogScroll" /> 自动滚动
              </label>
            </h3>
            <div ref="logEl" class="log-list">
              <div v-for="(l, i) in detail.logs" :key="i" class="log-item">
                <span class="log-level" :class="l.level">{{ l.level }}</span>
                <span class="log-msg">{{ l.message }}</span>
                <span class="log-ts">{{ new Date(l.ts).toLocaleTimeString() }}</span>
              </div>
              <div v-if="!detail.logs?.length" class="empty" style="padding: 20px 0">暂无日志</div>
            </div>
          </div>
        </div>
      </template>
    </section>

    <section class="panel">
      <h3>
        审计日志
        <span class="chip">{{ audits.length }} 条</span>
        <button class="link" @click="loadAudits">刷新</button>
        <button class="link" :disabled="!audits.length" @click="exportAudits">导出 CSV</button>
      </h3>
      <div class="field-row">
        <label>对象类型<input v-model="auditType" placeholder="task / decision / policy" /></label>
        <label>对象 ID<input v-model="auditObject" placeholder="留空表示不过滤" /></label>
      </div>
      <p class="hint" style="margin-top: 0">在任务列表点开某条任务时，会自动按该任务 ID 过滤审计；清空对象 ID 可查看全局。</p>
      <p v-if="auditErr" class="err">{{ auditErr }}</p>
      <table>
        <thead>
          <tr>
            <th>时间</th>
            <th>操作人</th>
            <th>动作</th>
            <th>对象</th>
            <th>Trace</th>
            <th>变更</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="a in audits" :key="a.id">
            <td class="muted">{{ new Date(a.ts).toLocaleString() }}</td>
            <td>{{ a.actor }}</td>
            <td><span class="chip info">{{ a.action }}</span></td>
            <td>
              {{ a.object_type }}
              <span class="tag">{{ shortID(a.object_id) }}</span>
            </td>
            <td class="muted"><span class="tag">{{ shortID(a.trace_id) }}</span></td>
            <td>
              <details v-if="a.before || a.after">
                <summary class="small link">查看</summary>
                <pre class="json">{{ jsonText({ before: a.before, after: a.after }) }}</pre>
              </details>
              <span v-else class="muted small">—</span>
            </td>
          </tr>
        </tbody>
      </table>
      <div v-if="!audits.length" class="empty">暂无审计记录</div>
      <div v-if="auditCursor" style="margin-top: 12px; text-align: center">
        <button class="ghost" :disabled="auditLoading" @click="loadMoreAudits">
          {{ auditLoading ? '加载中…' : '加载更多' }}
        </button>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  actorName,
  api,
  fmtDuration,
  fmtTime,
  jsonText,
  shortID,
  statusTone,
  type AuditLog,
  type Task,
  type TaskDetail
} from '../api'
import { confirmAction, copyText, toastBad, toastOk, toastWarn } from '../lib/feedback'

const STATUSES = [
  'PENDING',
  'PENDING_APPROVAL',
  'QUEUED',
  'RUNNING',
  'RETRYING',
  'SUCCEEDED',
  'FAILED',
  'CANCELLED'
]
const TYPES = ['PLACEMENT_APPLY', 'PREEMPT', 'SCALE', 'RELEASE_PROMOTE', 'RELEASE_ABORT', 'RECOVERY', 'CONFIG_ROLLBACK']
const CANCELLABLE = ['PENDING', 'PENDING_APPROVAL', 'QUEUED']

const actor = ref(actorName())
const route = useRoute()
const router = useRouter()
const tasks = ref<Task[]>([])
const nextCursor = ref<string | null>(null)
const loading = ref(false)
const error = ref('')
const errorStatus = ref(0)
const filterStatus = ref('')
const filterType = ref('')
const limit = ref(20)
const autoRefresh = ref(localStorage.getItem('ka_tasks_autorefresh') === '1')
const lastLoadedAt = ref<Date | null>(null)
const selectedIds = ref<string[]>([])

const selectedId = ref('')
const detail = ref<TaskDetail | null>(null)
const detailLoading = ref(false)
const detailErr = ref('')
const actionMsg = ref('')
const actionErr = ref('')
const acting = ref(false)
const timelineFilter = ref<'all' | 'done' | 'pending'>('all')
const logEl = ref<HTMLElement | null>(null)
const logAutoScroll = ref(localStorage.getItem('ka_task_log_autoscroll') !== '0')
// 仅用于自动刷新时检测“进入终态”，首次加载不提示。
const statusMap = new Map<string, string>()

const audits = ref<AuditLog[]>([])
const auditCursor = ref<string | null>(null)
const auditLoading = ref(false)
const auditErr = ref('')
const auditStatus = ref(0)
const auditType = ref('task')
const auditObject = ref('')

const backendMissing = computed(() => errorStatus.value === 404 || auditStatus.value === 404)
const decisionId = computed(() => {
  const v = detail.value?.params?.decision_id
  return typeof v === 'string' ? v : ''
})

const pendingApproval = computed(() => tasks.value.filter(t => t.status === 'PENDING_APPROVAL'))
const activeTasks = computed(() =>
  tasks.value.filter(t => ['QUEUED', 'RUNNING', 'RETRYING'].includes(String(t.status)))
)
const successRate = computed(() => {
  const done = tasks.value.filter(t => ['SUCCEEDED', 'FAILED', 'CANCELLED'].includes(String(t.status)))
  if (!done.length) return '—'
  const ok = done.filter(t => t.status === 'SUCCEEDED').length
  return ((ok / done.length) * 100).toFixed(0) + '%'
})

const lastLoadedText = computed(() => (lastLoadedAt.value ? lastLoadedAt.value.toLocaleTimeString() : '—'))
const isSelected = (id: string) => selectedIds.value.includes(id)
const allSelected = computed(() => tasks.value.length > 0 && tasks.value.every(t => isSelected(t.id)))
const someSelected = computed(() => selectedIds.value.length > 0 && !allSelected.value)

function toggleSelect(id: string) {
  if (isSelected(id)) selectedIds.value = selectedIds.value.filter(x => x !== id)
  else selectedIds.value = [...selectedIds.value, id]
}

function toggleAll() {
  selectedIds.value = allSelected.value ? [] : tasks.value.map(t => t.id)
}

function clearSelection() {
  selectedIds.value = []
}

const batchApproveCount = computed(
  () => tasks.value.filter(t => isSelected(t.id) && t.status === 'PENDING_APPROVAL').length
)
const batchCancelCount = computed(
  () => tasks.value.filter(t => isSelected(t.id) && cancellable(String(t.status))).length
)

const timeline = computed(() => {
  const d = detail.value
  if (!d) return []
  return [
    { label: '创建', done: true, text: `${d.created_by} · ${new Date(d.created_at).toLocaleString()}` },
    {
      label: '审批',
      done: !!d.approved_by,
      text: d.approved_by ? `${d.approved_by} 已审批` : d.status === 'PENDING_APPROVAL' ? '等待审批' : '—'
    },
    {
      label: '开始执行',
      done: !!d.started_at,
      text: d.started_at ? new Date(d.started_at).toLocaleString() : '—'
    },
    {
      label: '结束',
      done: !!d.finished_at,
      text: d.finished_at ? `${new Date(d.finished_at).toLocaleString()} · ${d.status}` : '—'
    }
  ]
})

const cancellable = (status: string) => CANCELLABLE.includes(status)

const filteredTimeline = computed(() =>
  timeline.value.filter(s =>
    timelineFilter.value === 'all' ? true : timelineFilter.value === 'done' ? s.done : !s.done
  )
)

const TERMINAL = ['SUCCEEDED', 'FAILED', 'CANCELLED']

function noticeTerminal(prev: Map<string, string>, next: Task[]) {
  for (const t of next) {
    const before = prev.get(t.id)
    if (!before || before === t.status) continue
    if (!TERMINAL.includes(String(t.status))) continue
    const label = `任务 ${shortID(t.id)}（${t.type}）`
    if (t.status === 'SUCCEEDED') toastOk(`${label} 已完成`)
    else if (t.status === 'FAILED') toastBad(`${label} 失败${t.error ? '：' + t.error : ''}`)
    else toastWarn(`${label} 已取消`)
  }
}

function rememberStatuses(list: Task[]) {
  list.forEach(t => statusMap.set(t.id, String(t.status)))
}

function persistLogScroll() {
  localStorage.setItem('ka_task_log_autoscroll', logAutoScroll.value ? '1' : '0')
}

async function scrollLogsToEnd() {
  if (!logAutoScroll.value) return
  await nextTick()
  const el = logEl.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(() => [detail.value?.id, detail.value?.logs?.length], scrollLogsToEnd)

async function loadFirst() {
  loading.value = true
  error.value = ''
  errorStatus.value = 0
  try {
    const page = await api.tasks({
      status: filterStatus.value || undefined,
      type: filterType.value || undefined,
      limit: limit.value
    })
    tasks.value = page.items || []
    nextCursor.value = page.next_cursor || null
    selectedIds.value = selectedIds.value.filter(id => tasks.value.some(t => t.id === id))
    lastLoadedAt.value = new Date()
    noticeTerminal(statusMap, tasks.value)
    rememberStatuses(tasks.value)
  } catch (e: any) {
    error.value = e?.message || String(e)
    errorStatus.value = e?.status || 0
  } finally {
    loading.value = false
  }
}

function applyFilters() {
  router.replace({
    query: {
      ...route.query,
      status: filterStatus.value || undefined,
      type: filterType.value || undefined,
      limit: limit.value === 20 ? undefined : limit.value,
      task: undefined
    }
  })
  selectedId.value = ''
  detail.value = null
  auditObject.value = ''
  clearSelection()
  loadFirst()
  loadAudits()
}

async function loadMore() {
  if (!nextCursor.value) return
  loading.value = true
  try {
    const page = await api.tasks({
      status: filterStatus.value || undefined,
      type: filterType.value || undefined,
      limit: limit.value,
      cursor: nextCursor.value
    })
    tasks.value = [...tasks.value, ...(page.items || [])]
    nextCursor.value = page.next_cursor || null
    rememberStatuses(page.items || [])
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loading.value = false
  }
}

async function selectTask(id: string, syncUrl = true) {
  selectedId.value = id
  if (syncUrl) router.replace({ query: { ...route.query, task: id } })
  detailLoading.value = true
  detailErr.value = ''
  actionMsg.value = ''
  actionErr.value = ''
  auditObject.value = id
  auditType.value = 'task'
  try {
    detail.value = await api.task(id)
  } catch (e: any) {
    detail.value = null
    detailErr.value = e?.message || String(e)
  } finally {
    detailLoading.value = false
  }
  await loadAudits()
}

async function approve(t: Task) {
  const who = actor.value || 'anonymous'
  const ok = await confirmAction({
    title: '审批任务',
    message: `以「${who}」身份审批任务 ${shortID(t.id)}？`,
    okText: '审批通过'
  })
  if (!ok) return
  acting.value = true
  actionErr.value = ''
  actionMsg.value = ''
  try {
    const updated = await api.approveTask(t.id)
    actionMsg.value = `任务已审批，当前状态 ${updated.status}`
    toastOk(`任务 ${shortID(t.id)} 已审批，状态 ${updated.status}`)
    await loadFirst()
    await selectTask(t.id)
  } catch (e: any) {
    actionErr.value = e?.message || String(e)
    toastBad(`审批失败：${actionErr.value}`)
  } finally {
    acting.value = false
  }
}

async function cancel(t: Task) {
  const who = actor.value || 'anonymous'
  const ok = await confirmAction({
    title: '取消任务',
    message: `以「${who}」身份取消任务 ${shortID(t.id)}？\n取消后不可恢复。`,
    okText: '取消任务',
    danger: true
  })
  if (!ok) return
  acting.value = true
  actionErr.value = ''
  actionMsg.value = ''
  try {
    const updated = await api.cancelTask(t.id)
    actionMsg.value = `任务已取消，当前状态 ${updated.status}`
    toastOk(`任务 ${shortID(t.id)} 已取消`)
    await loadFirst()
    await selectTask(t.id)
  } catch (e: any) {
    actionErr.value = e?.message || String(e)
    toastBad(`取消失败：${actionErr.value}`)
  } finally {
    acting.value = false
  }
}

async function batchApprove() {
  const ids = tasks.value.filter(t => isSelected(t.id) && t.status === 'PENDING_APPROVAL').map(t => t.id)
  if (!ids.length) return
  const who = actor.value || 'anonymous'
  const ok = await confirmAction({
    title: '批量审批',
    message: `以「${who}」身份审批 ${ids.length} 个任务？`,
    okText: `审批 ${ids.length} 个`
  })
  if (!ok) return
  acting.value = true
  let done = 0
  const fails: string[] = []
  for (const id of ids) {
    try {
      await api.approveTask(id)
      done++
    } catch (e: any) {
      fails.push(`${shortID(id)}：${e?.message || e}`)
    }
  }
  acting.value = false
  clearSelection()
  await loadFirst()
  if (selectedId.value) await refreshDetail()
  if (fails.length) toastWarn(`成功 ${done} 个，失败 ${fails.length} 个 · ${fails[0]}`)
  else toastOk(`已审批 ${done} 个任务`)
}

async function batchCancel() {
  const ids = tasks.value.filter(t => isSelected(t.id) && cancellable(String(t.status))).map(t => t.id)
  if (!ids.length) return
  const who = actor.value || 'anonymous'
  const ok = await confirmAction({
    title: '批量取消',
    message: `以「${who}」身份取消 ${ids.length} 个任务？\n取消后不可恢复。`,
    okText: `取消 ${ids.length} 个`,
    danger: true
  })
  if (!ok) return
  acting.value = true
  let done = 0
  const fails: string[] = []
  for (const id of ids) {
    try {
      await api.cancelTask(id)
      done++
    } catch (e: any) {
      fails.push(`${shortID(id)}：${e?.message || e}`)
    }
  }
  acting.value = false
  clearSelection()
  await loadFirst()
  if (selectedId.value) await refreshDetail()
  if (fails.length) toastWarn(`成功 ${done} 个，失败 ${fails.length} 个 · ${fails[0]}`)
  else toastOk(`已取消 ${done} 个任务`)
}

async function copyId() {
  if (!detail.value) return
  const ok = await copyText(detail.value.id)
  if (ok) toastOk('任务 ID 已复制')
  else toastBad('复制失败，请手动选择文本')
}

function logText() {
  return (detail.value?.logs || [])
    .map(l => `${new Date(l.ts).toISOString()} [${l.level}] ${l.message}`)
    .join('\n')
}

async function copyLogs() {
  const text = logText()
  if (!text) return
  const ok = await copyText(text)
  if (ok) toastOk(`已复制 ${detail.value?.logs?.length || 0} 条日志`)
  else toastBad('复制失败，请手动选择文本')
}

function downloadLogs() {
  const text = logText()
  if (!text) return
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `task-${shortID(detail.value?.id)}-logs.txt`
  a.click()
  URL.revokeObjectURL(url)
  toastOk('任务日志已下载')
}

function csvCell(v: unknown) {
  const s = v == null ? '' : typeof v === 'string' ? v : jsonText(v, 0)
  return `"${s.replace(/"/g, '""')}"`
}

function exportAudits() {
  if (!audits.value.length) return
  const header = ['时间', '操作人', '动作', '对象类型', '对象ID', 'Trace ID', '变更']
  const lines = audits.value.map(a =>
    [
      new Date(a.ts).toLocaleString(),
      a.actor,
      a.action,
      a.object_type,
      a.object_id || '',
      a.trace_id || '',
      a.before || a.after ? jsonText({ before: a.before, after: a.after }, 0) : ''
    ]
      .map(csvCell)
      .join(',')
  )
  const csv = '\uFEFF' + [header.map(csvCell).join(','), ...lines].join('\r\n')
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `audit-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.csv`
  a.click()
  URL.revokeObjectURL(url)
  toastOk(`已导出 ${audits.value.length} 条审计记录`)
}

async function loadAudits() {
  auditLoading.value = true
  auditErr.value = ''
  auditStatus.value = 0
  try {
    const page = await api.auditLogs({
      object_type: auditType.value || undefined,
      object_id: auditObject.value || undefined,
      limit: 20
    })
    audits.value = page.items || []
    auditCursor.value = page.next_cursor || null
  } catch (e: any) {
    auditErr.value = e?.message || String(e)
    auditStatus.value = e?.status || 0
  } finally {
    auditLoading.value = false
  }
}

async function loadMoreAudits() {
  if (!auditCursor.value) return
  auditLoading.value = true
  try {
    const page = await api.auditLogs({
      object_type: auditType.value || undefined,
      object_id: auditObject.value || undefined,
      limit: 20,
      cursor: auditCursor.value
    })
    audits.value = [...audits.value, ...(page.items || [])]
    auditCursor.value = page.next_cursor || null
  } catch (e: any) {
    auditErr.value = e?.message || String(e)
  } finally {
    auditLoading.value = false
  }
}

async function refreshDetail() {
  if (!selectedId.value) return
  try {
    detail.value = await api.task(selectedId.value)
  } catch {
    /* 保留下一次轮询 */
  }
}

let timer: number | undefined
watch(autoRefresh, enabled => {
  localStorage.setItem('ka_tasks_autorefresh', enabled ? '1' : '0')
  if (timer) clearInterval(timer)
  timer = undefined
  if (enabled)
    timer = window.setInterval(() => {
      if (loading.value) return
      loadFirst()
      refreshDetail()
    }, 5000)
})

watch(
  () => route.query.task,
  value => {
    const id = typeof value === 'string' ? value : ''
    if (id && id !== selectedId.value) selectTask(id, false)
  }
)

onMounted(async () => {
  actor.value = actorName()
  const q = route.query
  if (typeof q.status === 'string') filterStatus.value = q.status
  if (typeof q.type === 'string') filterType.value = q.type
  const lim = Number(q.limit)
  if ([20, 50, 100].includes(lim)) limit.value = lim
  await loadFirst()
  await loadAudits()
  const task = typeof q.task === 'string' ? q.task : ''
  if (task) await selectTask(task, false)
})
onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>
