<template>
  <teleport to="body">
    <div v-if="paletteState.open" class="modal-mask" @click.self="closePalette">
      <div class="palette" role="dialog" aria-modal="true" aria-label="命令面板" @keydown="onKeydown">
        <input
          ref="inputEl"
          v-model="query"
          class="palette-input"
          placeholder="搜索页面、任务，或输入身份名切换…"
          spellcheck="false"
          @input="cursor = 0"
        />
        <div ref="listEl" class="palette-list">
          <button
            v-for="(it, i) in items"
            :key="it.key"
            type="button"
            class="palette-item"
            :class="{ active: i === cursor }"
            @mouseenter="cursor = i"
            @click="run(it)"
          >
            <span class="palette-kind">{{ it.kind }}</span>
            <span class="palette-label">{{ it.label }}</span>
            <span v-if="it.hint" class="palette-hint">{{ it.hint }}</span>
          </button>
          <div v-if="tasksLoading && !items.length" class="empty" style="padding: 18px 0">加载任务中…</div>
          <div v-else-if="!items.length" class="empty" style="padding: 18px 0">没有匹配项</div>
        </div>
        <div class="palette-foot">
          <span><kbd>↑</kbd><kbd>↓</kbd> 选择</span>
          <span><kbd>Enter</kbd> 执行</span>
          <span><kbd>Esc</kbd> 关闭</span>
        </div>
      </div>
    </div>

    <div v-if="paletteState.helpOpen" class="modal-mask" @click.self="closeHelp">
      <div class="modal help-modal" role="dialog" aria-modal="true" aria-label="快捷键帮助" @keydown="onHelpKey">
        <h3>键盘快捷键</h3>
        <div class="help-grid">
          <div v-for="s in SHORTCUT_LIST" :key="s.label" class="help-row">
            <span class="help-keys">
              <kbd v-for="k in s.keys" :key="k">{{ k }}</kbd>
            </span>
            <span class="help-text">{{ s.label }}</span>
          </div>
        </div>
        <p class="hint">输入框内不会触发单键快捷键；命令面板中可直接搜索任务 ID、节点或页面。</p>
        <div class="modal-foot">
          <button ref="closeHelpBtn" class="ghost" @click="closeHelp">关闭</button>
        </div>
      </div>
    </div>
  </teleport>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, shortID, type Task } from '../api'
import { closeHelp, closePalette, openHelp, paletteState, togglePalette } from '../lib/palette'
import { toastOk, toastWarn } from '../lib/feedback'

interface Item {
  key: string
  kind: string
  label: string
  hint?: string
  run: () => void
}

const NAV = [
  { path: '/simulator', label: '模拟工作台', kw: 'simulator 放置 模拟 placement' },
  { path: '/fairness', label: '公平性与历史', kw: 'fairness 公平 历史 decision' },
  { path: '/policies', label: '策略管理', kw: 'policy 策略 权重' },
  { path: '/nodes', label: '节点与容量', kw: 'node 节点 容量 集群' },
  { path: '/tasks', label: '任务与审计', kw: 'task 任务 审计 audit' }
]

const SHORTCUT_LIST = [
  { label: '打开命令面板（搜索页面 / 任务 / 身份）', keys: ['Ctrl/⌘', 'K'] },
  { label: '打开本快捷键帮助', keys: ['?'] },
  { label: '跳转到对应页面（先按 g）', keys: ['g', 's/f/p/n/t'] },
  { label: '模拟页立即运行一次', keys: ['Ctrl/⌘', 'Enter'] },
  { label: '关闭弹窗或命令面板', keys: ['Esc'] }
]

const router = useRouter()
const query = ref('')
const cursor = ref(0)
const inputEl = ref<HTMLInputElement | null>(null)
const listEl = ref<HTMLElement | null>(null)
const closeHelpBtn = ref<HTMLButtonElement | null>(null)

const tasks = ref<Task[]>([])
const tasksLoading = ref(false)
const actorHistory = ref<string[]>(readActors())

let lastFocus: HTMLElement | null = null

function readActors(): string[] {
  try {
    const raw = JSON.parse(localStorage.getItem('ka_actor_history') || '[]')
    return Array.isArray(raw) ? raw.filter((x: unknown): x is string => typeof x === 'string').slice(0, 5) : []
  } catch {
    return []
  }
}

const text = computed(() => query.value.trim())
const q = computed(() => text.value.toLowerCase())
const hit = (s: string) => !q.value || s.toLowerCase().includes(q.value)

const items = computed<Item[]>(() => {
  const out: Item[] = []

  for (const n of NAV) {
    if (hit(`${n.label} ${n.path} ${n.kw}`)) {
      out.push({
        key: 'nav:' + n.path,
        kind: '页面',
        label: n.label,
        hint: n.path,
        run: () => router.push(n.path)
      })
    }
  }

  const matchedTasks = tasks.value
    .filter(t => hit(`${t.id} ${t.type} ${t.status} ${t.created_by}`))
    .slice(0, 6)
  for (const t of matchedTasks) {
    out.push({
      key: 'task:' + t.id,
      kind: '任务',
      label: `${shortID(t.id)} · ${t.type}`,
      hint: `${t.status} · ${t.created_by}`,
      run: () => router.push({ path: '/tasks', query: { task: t.id } })
    })
  }

  if (text.value) {
    out.push({
      key: 'actor:switch',
      kind: '身份',
      label: `切换身份为「${text.value}」`,
      hint: '写入 X-Actor',
      run: () => switchActor(text.value)
    })
  } else {
    for (const a of actorHistory.value.slice(0, 3)) {
      out.push({
        key: 'actor:' + a,
        kind: '身份',
        label: `切换为 ${a}`,
        hint: '最近使用',
        run: () => switchActor(a)
      })
    }
    if (localStorage.getItem('ka_actor')) {
      out.push({
        key: 'actor:clear',
        kind: '身份',
        label: '清除当前身份（anonymous）',
        run: () => clearActor()
      })
    }
  }

  if (hit('快捷键 帮助 shortcuts help')) {
    out.push({
      key: 'action:help',
      kind: '帮助',
      label: '查看键盘快捷键',
      hint: '?',
      run: () => openHelp()
    })
  }

  return out
})

function switchActor(name: string) {
  localStorage.setItem('ka_actor', name)
  const list = [name, ...readActors().filter(x => x !== name)].slice(0, 5)
  localStorage.setItem('ka_actor_history', JSON.stringify(list))
  actorHistory.value = list
  window.dispatchEvent(new CustomEvent('ka:actor', { detail: name }))
  toastOk(`身份已切换为 ${name}`)
}

function clearActor() {
  localStorage.removeItem('ka_actor')
  window.dispatchEvent(new CustomEvent('ka:actor', { detail: '' }))
  toastWarn('已清除身份，后续请求按 anonymous 处理')
}

async function loadTasks() {
  if (tasksLoading.value) return
  tasksLoading.value = true
  try {
    tasks.value = (await api.tasks({ limit: 100 })).items || []
  } catch {
    tasks.value = []
  } finally {
    tasksLoading.value = false
  }
}

function run(it: Item) {
  closePalette()
  it.run()
}

function move(step: number) {
  const n = items.value.length
  if (!n) return
  cursor.value = (cursor.value + step + n) % n
  nextTick(() => {
    const el = listEl.value?.querySelector('.palette-item.active')
    el?.scrollIntoView({ block: 'nearest' })
  })
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    move(1)
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    move(-1)
  } else if (e.key === 'Enter') {
    e.preventDefault()
    const it = items.value[cursor.value]
    if (it) run(it)
  } else if (e.key === 'Escape') {
    e.preventDefault()
    closePalette()
  }
}

function onHelpKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.preventDefault()
    closeHelp()
  }
}

function typingInField(e: KeyboardEvent) {
  const t = e.target as HTMLElement | null
  return !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)
}

function onGlobalKey(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    togglePalette()
    return
  }
  if (e.key === '?' && !typingInField(e) && !paletteState.open && !paletteState.helpOpen) {
    e.preventDefault()
    openHelp()
    return
  }
  if (e.key === 'Escape') {
    if (paletteState.open) {
      e.preventDefault()
      closePalette()
    } else if (paletteState.helpOpen) {
      e.preventDefault()
      closeHelp()
    }
  }
}

watch(
  () => paletteState.open,
  async open => {
    if (open) {
      lastFocus = (document.activeElement as HTMLElement | null) || null
      query.value = ''
      cursor.value = 0
      loadTasks()
      await nextTick()
      inputEl.value?.focus()
    } else {
      lastFocus?.focus?.()
    }
  }
)

watch(
  () => paletteState.helpOpen,
  async open => {
    if (open) {
      lastFocus = (document.activeElement as HTMLElement | null) || null
      await nextTick()
      closeHelpBtn.value?.focus()
    } else {
      lastFocus?.focus?.()
    }
  }
)

watch(items, list => {
  if (cursor.value >= list.length) cursor.value = Math.max(0, list.length - 1)
})

onMounted(() => window.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onGlobalKey))
</script>
