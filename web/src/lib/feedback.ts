// 全局反馈：轻量 Toast + Promise 化确认弹窗，避免各页面各自 window.confirm。
import { reactive } from 'vue'

export type FeedbackKind = 'ok' | 'warn' | 'bad' | 'info'

export interface ToastItem {
  id: number
  kind: FeedbackKind
  message: string
}

export const toasts = reactive<ToastItem[]>([])

let seq = 0
const timers = new Map<number, number>()

export function dismissToast(id: number) {
  const i = toasts.findIndex(t => t.id === id)
  if (i >= 0) toasts.splice(i, 1)
  const timer = timers.get(id)
  if (timer) window.clearTimeout(timer)
  timers.delete(id)
}

export function toast(message: string, kind: FeedbackKind = 'info', ttl = 3200) {
  const id = ++seq
  toasts.push({ id, kind, message })
  if (toasts.length > 5) dismissToast(toasts[0].id)
  timers.set(
    id,
    window.setTimeout(() => dismissToast(id), ttl)
  )
}

export const toastOk = (message: string) => toast(message, 'ok')
export const toastWarn = (message: string) => toast(message, 'warn', 4000)
export const toastBad = (message: string) => toast(message, 'bad', 5000)
export const toastInfo = (message: string) => toast(message, 'info')

// ---------- 确认弹窗 ----------

export interface ConfirmOptions {
  title?: string
  message: string
  okText?: string
  cancelText?: string
  danger?: boolean
}

export const confirmState = reactive({
  open: false,
  title: '请确认',
  message: '',
  okText: '确定',
  cancelText: '取消',
  danger: false
})

let pendingResolve: ((ok: boolean) => void) | null = null

/** 用法：if (!(await confirmAction({ message: '...' }))) return */
export function confirmAction(opts: ConfirmOptions): Promise<boolean> {
  pendingResolve?.(false)
  confirmState.title = opts.title || '请确认'
  confirmState.message = opts.message
  confirmState.okText = opts.okText || '确定'
  confirmState.cancelText = opts.cancelText || '取消'
  confirmState.danger = !!opts.danger
  confirmState.open = true
  return new Promise(resolve => {
    pendingResolve = resolve
  })
}

export function settleConfirm(ok: boolean) {
  if (!confirmState.open) return
  confirmState.open = false
  const resolve = pendingResolve
  pendingResolve = null
  resolve?.(ok)
}

// ---------- 剪贴板 ----------

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* 回退到 execCommand */
  }
  try {
    const el = document.createElement('textarea')
    el.value = text
    el.style.position = 'fixed'
    el.style.opacity = '0'
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(el)
    return ok
  } catch {
    return false
  }
}
