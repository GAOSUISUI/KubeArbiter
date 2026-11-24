import * as echarts from 'echarts'
import type { EChartsOption } from 'echarts'
import { onBeforeUnmount, onMounted, type Ref } from 'vue'

// 统一图表工具箱：右上角导出 PNG，避免每个页面重复声明。
const TOOLBOX: EChartsOption['toolbox'] = {
  right: 4,
  top: 0,
  itemSize: 13,
  itemGap: 8,
  iconStyle: { borderColor: '#8b93a7' },
  feature: {
    saveAsImage: {
      title: '导出 PNG',
      backgroundColor: 'transparent',
      pixelRatio: 2
    }
  }
}

/** 把 ECharts 实例的生命周期挂到组件上：自动 init / resize / dispose。 */
export function useChart(el: Ref<HTMLElement | null>) {
  let chart: echarts.ECharts | null = null
  let clickHandler: ((params: any) => void) | null = null

  const bindClick = () => {
    if (!chart) return
    chart.off('click')
    chart.on('click', (params: any) => clickHandler?.(params))
  }

  const render = (option: EChartsOption) => {
    if (!el.value) return
    if (!chart) {
      chart = echarts.init(el.value)
      bindClick()
    }
    // 页面显式配置了 toolbox 时以其为准。
    const merged = (option as any).toolbox ? option : { ...option, toolbox: TOOLBOX }
    chart.setOption(merged, true)
  }

  /** 同一个实例只保留一个点击回调；不传参数时解除绑定。 */
  const onClick = (handler: ((params: any) => void) | null) => {
    clickHandler = handler
    bindClick()
  }

  const resize = () => chart?.resize()

  onMounted(() => window.addEventListener('resize', resize))
  onBeforeUnmount(() => {
    window.removeEventListener('resize', resize)
    chart?.dispose()
    chart = null
  })

  return { render, resize, onClick }
}
