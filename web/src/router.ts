import { createRouter, createWebHashHistory } from 'vue-router'

// 用 hash 模式，静态托管（任意子路径）都能直接刷新。
// 视图按路由懒加载：首屏只下载模拟工作台，其余页面在首次进入时才拉取。
export default createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/simulator' },
    { path: '/simulator', component: () => import('./views/SimulatorView.vue'), meta: { title: '模拟工作台' } },
    { path: '/fairness', component: () => import('./views/FairnessView.vue'), meta: { title: '公平性与历史' } },
    { path: '/policies', component: () => import('./views/PoliciesView.vue'), meta: { title: '策略管理' } },
    { path: '/nodes', component: () => import('./views/NodesView.vue'), meta: { title: '节点与容量' } },
    { path: '/tasks', component: () => import('./views/TasksView.vue'), meta: { title: '任务与审计' } }
  ]
})
