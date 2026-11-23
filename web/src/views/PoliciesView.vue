<template>
  <div class="stack">
    <div class="page-head">
      <h2>策略管理</h2>
      <p class="muted small" style="margin: 0">
        策略 = 一组 7 维权重。权重总和必须为 100%，保存前会自动归一化。
      </p>
    </div>

    <section class="panel">
      <h3>
        策略列表
        <span class="chip">{{ policies.length }} 条</span>
        <span style="flex: 1"></span>
        <button class="primary" @click="openCreate">新建策略</button>
      </h3>
      <table>
        <thead>
          <tr>
            <th>名称</th>
            <th>策略</th>
            <th>权重分布</th>
            <th>默认</th>
            <th style="width: 130px">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in policies" :key="p.id">
            <td>
              {{ p.name }}
              <div class="tag">{{ shortID(p.id) }}</div>
            </td>
            <td><span class="chip info">{{ p.strategy }}</span></td>
            <td>
              <div style="display: flex; gap: 3px; align-items: center">
                <div
                  v-for="k in WEIGHT_KEYS"
                  :key="k"
                  class="weight-block"
                  :style="{ width: (p.weights[k] * 100) + '%', background: colorOf(k) }"
                  :title="WEIGHT_LABELS[k] + ' ' + (p.weights[k] * 100).toFixed(0) + '%'"
                ></div>
              </div>
              <div class="small muted" style="margin-top: 4px">
                LOAD {{ (p.weights.s_load * 100).toFixed(0) }}% · FAIR {{ (p.weights.s_fair * 100).toFixed(0) }}% ·
                BAL {{ (p.weights.s_balance * 100).toFixed(0) }}%
              </div>
            </td>
            <td><span v-if="p.is_default" class="chip ok">默认</span></td>
            <td>
              <button class="link" @click="openEdit(p)">编辑</button>
              <button class="link" style="color: var(--bad)" @click="remove(p)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="error" class="err">{{ error }}</p>
    </section>

    <div v-if="editing" class="modal-mask" @click.self="editing = false">
      <div class="modal">
        <h3>{{ form.id ? '编辑策略' : '新建策略' }}</h3>
        <div class="field-row">
          <label>名称<input v-model="form.name" placeholder="例如 load-aware-v2" /></label>
          <label>
            策略类型
            <select v-model="form.strategy">
              <option v-for="s in strategies" :key="s" :value="s">{{ s }}</option>
            </select>
          </label>
        </div>
        <label class="auto" style="margin-bottom: 10px">
          <input v-model="form.is_default" type="checkbox" /> 设为默认策略
        </label>

        <div v-for="k in WEIGHT_KEYS" :key="k" class="slider">
          <span class="k">{{ WEIGHT_LABELS[k] }}</span>
          <input v-model.number="form.pct[k]" type="range" min="0" max="100" step="1" />
          <span class="v">{{ form.pct[k] }}%</span>
        </div>
        <div class="hint">合计 {{ sumPct }}%<span v-if="sumPct !== 100">，保存时归一化</span></div>
        <p v-if="formError" class="err">{{ formError }}</p>

        <div class="modal-foot">
          <button class="ghost" @click="editing = false">取消</button>
          <button class="primary" :disabled="saving" @click="save">{{ saving ? '保存中…' : '保存' }}</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, WEIGHT_KEYS, WEIGHT_LABELS, shortID, type Policy, type WeightKey } from '../api'

const strategies = ['BALANCED', 'BIN_PACK', 'FAIR_SHARE', 'LOAD_AWARE']
const policies = ref<Policy[]>([])
const error = ref('')
const formError = ref('')
const editing = ref(false)
const saving = ref(false)

const form = reactive({
  id: '',
  name: '',
  strategy: 'BALANCED',
  is_default: false,
  pct: {
    s_cpu: 18,
    s_mem: 18,
    s_balance: 25,
    s_pack: 10,
    s_load: 15,
    s_fair: 10,
    s_frag: 4
  } as Record<WeightKey, number>
})

const sumPct = computed(() => WEIGHT_KEYS.reduce((s, k) => s + form.pct[k], 0))

const COLORS: Record<WeightKey, string> = {
  s_cpu: '#7aa2f7',
  s_mem: '#27c281',
  s_balance: '#e7a33e',
  s_pack: '#b48ead',
  s_load: '#5fb3b3',
  s_fair: '#e2474a',
  s_frag: '#6b7a90'
}
const colorOf = (k: WeightKey) => COLORS[k]

async function load() {
  try {
    policies.value = await api.listPolicies()
  } catch (e: any) {
    error.value = e?.message || String(e)
  }
}

function openCreate() {
  form.id = ''
  form.name = ''
  form.strategy = 'BALANCED'
  form.is_default = false
  // 以 BALANCED 预设作为新建起点，避免全 0 权重无法保存。
  const base: Record<WeightKey, number> = {
    s_cpu: 18,
    s_mem: 18,
    s_balance: 25,
    s_pack: 10,
    s_load: 15,
    s_fair: 10,
    s_frag: 4
  }
  WEIGHT_KEYS.forEach(k => (form.pct[k] = base[k]))
  formError.value = ''
  editing.value = true
}

function openEdit(p: Policy) {
  form.id = p.id
  form.name = p.name
  form.strategy = p.strategy
  form.is_default = p.is_default
  WEIGHT_KEYS.forEach(k => (form.pct[k] = Math.round((p.weights[k] ?? 0) * 100)))
  formError.value = ''
  editing.value = true
}

function normalized() {
  const sum = sumPct.value || 1
  const w: any = {}
  WEIGHT_KEYS.forEach(k => (w[k] = form.pct[k] / sum))
  return w
}

async function save() {
  if (!form.name.trim()) {
    formError.value = '名称不能为空'
    return
  }
  if (sumPct.value === 0) {
    formError.value = '权重不能全为 0'
    return
  }
  saving.value = true
  formError.value = ''
  try {
    const payload = {
      name: form.name.trim(),
      strategy: form.strategy,
      weights: normalized(),
      is_default: form.is_default
    }
    if (form.id) await api.updatePolicy({ id: form.id, ...payload })
    else await api.createPolicy(payload)
    editing.value = false
    await load()
  } catch (e: any) {
    formError.value = e?.message || String(e)
  } finally {
    saving.value = false
  }
}

async function remove(p: Policy) {
  if (!window.confirm(`删除策略「${p.name}」？`)) return
  try {
    await api.deletePolicy(p.id)
    await load()
  } catch (e: any) {
    error.value = e?.message || String(e)
  }
}

onMounted(load)
</script>
