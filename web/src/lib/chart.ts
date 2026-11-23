import * as echarts from 'echarts'
import type { EChartsOption } from 'echarts'
import { onBeforeUnmount, onMounted, type Ref } from 'vue'

/** 把 ECharts 实例的生命周期挂到组件上：自动 init / resize / dispose。 */
export function useChart(el: Ref<HTMLElement | null>) {
  let chart: echarts.ECharts | null = null

  const render = (option: EChartsOption) => {
    if (!el.value) return
    if (!chart) chart = echarts.init(el.value)
    chart.setOption(option, true)
  }

  const resize = () => chart?.resize()

  onMounted(() => window.addEventListener('resize', resize))
  onBeforeUnmount(() => {
    window.removeEventListener('resize', resize)
    chart?.dispose()
    chart = null
  })

  return { render, resize }
}
