<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">
        <h1>KubeArbiter</h1>
        <span>多租户容量仲裁控制台</span>
      </div>
      <router-link
        v-for="item in nav"
        :key="item.path"
        :to="item.path"
        class="nav-item"
        :class="{ active: $route.path === item.path }"
      >
        {{ item.label }}
      </router-link>
      <div class="sidebar-foot">
        <div class="small">
          身份：<span class="actor-name">{{ currentActor || 'anonymous' }}</span>
        </div>
        <div class="actor-row">
          <input v-model="actorDraft" placeholder="X-Actor，例如 alice" @keyup.enter="saveActor" />
          <button class="ghost" @click="saveActor">切换</button>
        </div>
        <div class="small muted" style="margin-bottom: 8px">审批人不能是任务创建人（admin 例外）</div>
        <div class="small">API：{{ apiBase || 'Vite 代理 → :8080' }}</div>
        <button class="link" style="padding-left: 0" @click="editingBase = !editingBase">
          {{ editingBase ? '收起' : '修改' }}
        </button>
        <div v-if="editingBase" style="margin-top: 8px">
          <input v-model="baseDraft" placeholder="http://127.0.0.1:8080" />
          <div style="display: flex; gap: 8px; margin-top: 8px">
            <button @click="saveBase">保存</button>
            <button class="ghost" @click="resetBase">用代理</button>
          </div>
        </div>
        <div class="shortcut-hint">
          <kbd>Ctrl</kbd>+<kbd>K</kbd> 命令面板 · <kbd>?</kbd> 快捷键<br />
          <kbd>g</kbd>+<kbd>s</kbd> 模拟 · <kbd>g</kbd>+<kbd>f</kbd> 公平性 · <kbd>g</kbd>+<kbd>p</kbd>
          策略 · <kbd>g</kbd>+<kbd>n</kbd> 节点 · <kbd>g</kbd>+<kbd>t</kbd> 任务
        </div>
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <span class="title">{{ $route.meta.title }}</span>
        <span class="spacer" />
        <button class="cmd-trigger" title="打开命令面板" @click="openPalette">
          <span>搜索 / 命令</span>
          <kbd>Ctrl K</kbd>
        </button>
        <span class="chip" :class="health.cls" :title="healthAt ? '最近检查：' + healthAt.toLocaleTimeString() : ''">
          {{ health.text }}
        </span>
        <button class="ghost" @click="checkHealth">刷新</button>
      </header>
      <div class="content">
        <router-view />
      </div>
    </div>

    <FeedbackHost />
    <CommandPalette />
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from './api'
import CommandPalette from './components/CommandPalette.vue'
import FeedbackHost from './components/FeedbackHost.vue'
import { toastOk, toastWarn } from './lib/feedback'
import { openPalette } from './lib/palette'

const nav = [
  { path: '/simulator', label: '模拟工作台' },
  { path: '/fairness', label: '公平性与历史' },
  { path: '/policies', label: '策略管理' },
  { path: '/nodes', label: '节点与容量' },
  { path: '/tasks', label: '任务与审计' }
]

const router = useRouter()
const apiBase = ref(localStorage.getItem('ka_api_base') || '')
const baseDraft = ref(apiBase.value)
const editingBase = ref(false)
const actorDraft = ref(localStorage.getItem('ka_actor') || '')
const currentActor = ref(localStorage.getItem('ka_actor') || '')
const health = ref({ cls: 'warn', text: '检查中…' })
const healthAt = ref<Date | null>(null)

function saveActor() {
  const v = actorDraft.value.trim()
  if (v) {
    localStorage.setItem('ka_actor', v)
    toastOk(`身份已切换为 ${v}`)
  } else {
    localStorage.removeItem('ka_actor')
    toastWarn('已清除身份，后续请求按 anonymous 处理')
  }
  actorDraft.value = v
  currentActor.value = v
}

function saveBase() {
  const v = baseDraft.value.trim().replace(/\/$/, '')
  if (v) localStorage.setItem('ka_api_base', v)
  else localStorage.removeItem('ka_api_base')
  apiBase.value = v
  editingBase.value = false
  toastOk(v ? `API 地址已保存：${v}` : '已恢复使用 Vite 代理')
  checkHealth()
}

function resetBase() {
  localStorage.removeItem('ka_api_base')
  apiBase.value = ''
  baseDraft.value = ''
  editingBase.value = false
  toastOk('已恢复使用 Vite 代理')
  checkHealth()
}

async function checkHealth() {
  try {
    const r = await api.health()
    health.value = r?.status === 'ok' ? { cls: 'ok', text: 'API 在线' } : { cls: 'warn', text: 'API 异常' }
  } catch {
    health.value = { cls: 'bad', text: 'API 不可达' }
  }
  healthAt.value = new Date()
}

// 命令面板切换身份后同步侧边栏显示。
function onActorEvent(e: Event) {
  const v = (e as CustomEvent<string>).detail || ''
  currentActor.value = v
  actorDraft.value = v
}

// g + s/f/p/n/t 快速跳转；输入框内不拦截。
const SHORTCUTS: Record<string, string> = {
  s: '/simulator',
  f: '/fairness',
  p: '/policies',
  n: '/nodes',
  t: '/tasks'
}
let gAt = 0

function typingInField(e: KeyboardEvent) {
  const t = e.target as HTMLElement | null
  return !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)
}

function onKeydown(e: KeyboardEvent) {
  if (typingInField(e) || e.metaKey || e.ctrlKey || e.altKey) return
  const k = e.key.toLowerCase()
  if (k === 'g') {
    gAt = Date.now()
    return
  }
  if (Date.now() - gAt < 1200 && SHORTCUTS[k]) {
    e.preventDefault()
    gAt = 0
    router.push(SHORTCUTS[k])
  }
}

let healthTimer: number | undefined
onMounted(() => {
  checkHealth()
  window.addEventListener('keydown', onKeydown)
  window.addEventListener('ka:actor', onActorEvent)
  healthTimer = window.setInterval(checkHealth, 20000)
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  window.removeEventListener('ka:actor', onActorEvent)
  if (healthTimer) clearInterval(healthTimer)
})
</script>
