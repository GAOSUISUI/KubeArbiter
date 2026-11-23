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
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <span class="title">{{ $route.meta.title }}</span>
        <span class="spacer" />
        <span class="chip" :class="health.cls">{{ health.text }}</span>
        <button class="ghost" @click="checkHealth">刷新</button>
      </header>
      <div class="content">
        <router-view />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from './api'

const nav = [
  { path: '/simulator', label: '模拟工作台' },
  { path: '/fairness', label: '公平性与历史' },
  { path: '/policies', label: '策略管理' },
  { path: '/nodes', label: '节点与容量' }
]

const apiBase = ref(localStorage.getItem('ka_api_base') || '')
const baseDraft = ref(apiBase.value)
const editingBase = ref(false)
const health = ref({ cls: 'warn', text: '检查中…' })

function saveBase() {
  const v = baseDraft.value.trim().replace(/\/$/, '')
  if (v) localStorage.setItem('ka_api_base', v)
  else localStorage.removeItem('ka_api_base')
  apiBase.value = v
  editingBase.value = false
  checkHealth()
}

function resetBase() {
  localStorage.removeItem('ka_api_base')
  apiBase.value = ''
  baseDraft.value = ''
  editingBase.value = false
  checkHealth()
}

async function checkHealth() {
  try {
    const r = await api.health()
    health.value = r?.status === 'ok' ? { cls: 'ok', text: 'API 在线' } : { cls: 'warn', text: 'API 异常' }
  } catch {
    health.value = { cls: 'bad', text: 'API 不可达' }
  }
}

onMounted(checkHealth)
</script>
