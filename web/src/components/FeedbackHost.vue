<template>
  <teleport to="body">
    <div class="toast-host" aria-live="polite">
      <transition-group name="toast">
        <div
          v-for="t in toasts"
          :key="t.id"
          class="toast"
          :class="t.kind"
          title="点击关闭"
          @click="dismissToast(t.id)"
        >
          <span class="toast-dot"></span>
          <span class="toast-msg">{{ t.message }}</span>
        </div>
      </transition-group>
    </div>

    <div v-if="confirmState.open" class="modal-mask" @click.self="settleConfirm(false)">
      <div class="modal confirm" role="dialog" aria-modal="true">
        <h3>{{ confirmState.title }}</h3>
        <p class="confirm-msg">{{ confirmState.message }}</p>
        <div class="modal-foot">
          <button ref="cancelBtn" class="ghost" @click="settleConfirm(false)">{{ confirmState.cancelText }}</button>
          <button
            ref="okBtn"
            :class="confirmState.danger ? 'danger solid' : 'primary'"
            @click="settleConfirm(true)"
          >
            {{ confirmState.okText }}
          </button>
        </div>
      </div>
    </div>
  </teleport>
</template>

<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { confirmState, dismissToast, settleConfirm, toasts } from '../lib/feedback'

const okBtn = ref<HTMLButtonElement | null>(null)
const cancelBtn = ref<HTMLButtonElement | null>(null)
let lastFocus: HTMLElement | null = null

// 确认弹窗焦点管理：危险操作默认聚焦取消，关闭后归还焦点。
watch(
  () => confirmState.open,
  async open => {
    if (open) {
      lastFocus = (document.activeElement as HTMLElement | null) || null
      await nextTick()
      ;(confirmState.danger ? cancelBtn.value : okBtn.value)?.focus()
    } else {
      lastFocus?.focus?.()
      lastFocus = null
    }
  }
)

function onKey(e: KeyboardEvent) {
  if (!confirmState.open) return
  if (e.key === 'Escape') {
    e.preventDefault()
    settleConfirm(false)
  } else if (e.key === 'Tab') {
    e.preventDefault()
    const target = document.activeElement === cancelBtn.value ? okBtn.value : cancelBtn.value
    target?.focus()
  } else if (e.key === 'Enter' && !(e.target instanceof HTMLButtonElement)) {
    e.preventDefault()
    settleConfirm(true)
  }
}

onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>
