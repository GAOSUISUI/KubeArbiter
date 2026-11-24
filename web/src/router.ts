import { createRouter, createWebHashHistory } from 'vue-router'
import SimulatorView from './views/SimulatorView.vue'
import FairnessView from './views/FairnessView.vue'
import PoliciesView from './views/PoliciesView.vue'
import NodesView from './views/NodesView.vue'
import TasksView from './views/TasksView.vue'

// 用 hash 模式，静态托管（任意子路径）都能直接刷新。
export default createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/simulator' },
    { path: '/simulator', component: SimulatorView, meta: { title: '模拟工作台' } },
    { path: '/fairness', component: FairnessView, meta: { title: '公平性与历史' } },
    { path: '/policies', component: PoliciesView, meta: { title: '策略管理' } },
    { path: '/nodes', component: NodesView, meta: { title: '节点与容量' } },
    { path: '/tasks', component: TasksView, meta: { title: '任务与审计' } }
  ]
})
