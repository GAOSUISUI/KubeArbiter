// 命令面板与快捷键帮助的全局开关，供 App 外壳与面板组件共享。
import { reactive } from 'vue'

export const paletteState = reactive({
  open: false,
  helpOpen: false
})

export function openPalette() {
  paletteState.helpOpen = false
  paletteState.open = true
}

export function closePalette() {
  paletteState.open = false
}

export function togglePalette() {
  if (paletteState.open) closePalette()
  else openPalette()
}

export function openHelp() {
  paletteState.open = false
  paletteState.helpOpen = true
}

export function closeHelp() {
  paletteState.helpOpen = false
}
