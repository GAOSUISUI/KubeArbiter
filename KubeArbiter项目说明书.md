# KubeArbiter 项目说明书

**基于 Kubernetes 的多租户容量仲裁与 SLO 保障平台**

- 英文名：KubeArbiter — Multi-Tenant Capacity Arbitration and SLO Governance Platform for Kubernetes
- 版本：v2.0（替代 v1.0 的 KubePilot / KubeSentinel 方案）
- 文档性质：项目定义与实施基线，后续所有设计与编码不得超出本文档的范围红线

---

## 0. 一页速览

**一句话**：在一个多租户 Kubernetes 集群上，自研一套"资源该给谁、放到哪、要不要继续发布"的可解释决策系统，并用真实压测数据证明它比默认做法更好。

**三个技术支柱**（也是三个面试可展开的深度点）：

| 支柱 | 解决的问题 | 技术含量 |
|---|---|---|
| 容量仲裁（Fairness Allocator） | 多租户争抢资源时，谁该拿多少 | 加权最大最小公平 + DRF + 抢占代价模型 |
| 放置引擎（Placement Engine） | 同一个 Pod 放在哪个节点更好 | 多目标归一化评分 + 约束过滤 + 可复现仿真 |
| SLO 闭环（SLO Governor） | 服务变慢/变差时，系统如何自己决策 | 错误预算 + 多窗口燃尽率 + 容量模型 + 自动回滚 |

**闭环**：
资源与指标采集 → 租户资源画像 → 公平仲裁 → 放置决策 → 渐进式发布 → SLO 观测 → 偏差判定 → 受控动作 → 效果验证

**与旧方案的核心差异**：

| 维度 | 旧方案（KubePilot） | 新方案（KubeArbiter） |
|---|---|---|
| 重心 | 集群资源管理 + Operator + 调度插件 + 网络诊断 | 容量决策 + 公平仲裁 + SLO 闭环 |
| 底层程度 | 涉及 Scheduler Framework 插件、CNI、eBPF、内核命令 | 只到 Kubernetes API / Admission 层，不碰运行时与网络底层 |
| 调度落地方式 | 自研 Scheduler Plugin（高风险） | Admission Webhook 注入调度约束 + 离线仿真（低风险、可演示） |
| 难度来源 | 模块数量多 | 算法与实验方法严密度 |
| 可验证性 | 依赖大量人造故障 | 自带受控实验环境（演示应用 + 压测器），结论可复现 |

---

## 1. 项目基本信息

| 项 | 内容 |
|---|---|
| 项目名称 | KubeArbiter |
| 中文名称 | 基于 Kubernetes 的多租户容量仲裁与 SLO 保障平台 |
| 项目定位 | 面向平台工程（Platform Engineering）的决策型系统，不是 Kubernetes 管理面板 |
| 主要语言 | Go |
| 前端 | Vue 3 + TypeScript + ECharts |
| 关键依赖 | Kubernetes API、client-go、PostgreSQL、Prometheus、Helm SDK、ingress-nginx、Gin |
| 目标岗位 | 云原生平台研发、Kubernetes 平台研发、SRE / 稳定性工程、云平台后端、基础设施后端 |
| 开发周期 | 不设固定周数，以里程碑门禁（M0–M6）的退出标准为准 |
| 运行环境 | x86-64 物理机 + VMware 上的 3 节点 Kubernetes（1 控制面 + 2 工作节点） |
| 演示形态 | 可本地部署的完整系统 + 现场 8 分钟演示脚本 + 可复现实验数据 |

---

## 2. 问题定义：为什么这个项目值得做

不是"再造一个 Dashboard"，而是回答三个没人替你回答的问题。

**问题一：资源该给谁。**
多租户共享集群时，租户 A 的离线批处理和租户 B 的在线服务同时申请资源。默认 Kubernetes 只有 ResourceQuota（硬上限）和 PriorityClass（抢占），缺少"按权重公平分配 + 抢占代价最小化"的仲裁能力。结果是先到先得，谁先扩容谁占便宜。

**问题二：放到哪更好。**
默认调度器保证"放得下"，但不保证"放得好"：它不做租户公平性考量，不做容量趋势判断，也不给你解释"为什么选了这个节点"。运维只能看到结果，看不到依据。

**问题三：发布该不该继续。**
金丝雀发布普遍存在，但"看板盯人"仍是主流。缺少"以 SLO 错误预算为判据、自动决定推进或回滚"的闭环，回滚往往发生在故障已经扩散之后。

本项目的价值主张：**把这三个决策从"人凭经验"变成"系统给出可解释结论 + 数据可验证"**。

---

## 3. 范围红线：明确不做什么

这一节是硬约束。任何后续设计超出本节范围，必须先修改本文档并重新评审。

| 不做的事 | 原因 | 替代方案 |
|---|---|---|
| 开发 Scheduler Framework 插件、修改 kube-scheduler | 需要与 Kubernetes 源码版本强绑定编译，构建与调试成本极高，且属于调度器内核开发，偏离平台岗位定位 | 用 **Admission Webhook 注入 nodeAffinity** 实现真实的放置效果；用**离线仿真器**做策略对比 |
| CNI / Service 转发 / 网络底层诊断 | 属于网络方向，与本项目定位无关 | 只做**应用级**依赖诊断（下游超时、端点为空、DNS 解析失败的应用表现） |
| eBPF、内核参数、cgroup 调优、容器运行时改造 | 底层系统方向，单人不可控 | 全部不做，直接使用发行版默认配置 |
| 存储 CSI 开发、PV 调度细节 | 与核心命题无关 | 只用 emptyDir / hostPath 做演示 |
| 真实多集群纳管 | 单机环境无法提供有意义的第二集群 | 保留 Cluster 抽象与接口，只接入一个真实集群，并在文档中明确说明 |
| GPU 调度 | 本机无 GPU，模拟 GPU 缺乏说服力 | 不做；调度维度限定在 CPU / 内存 / 拓扑标签 |
| Istio 级流量治理 | 组件过重，会淹没自研价值 | 用 ingress-nginx 的 canary 权重实现 HTTP 灰度，并明确其能力边界 |

**判定标准**：凡是"需要读内核代码、改发行版组件、或依赖特定硬件"的内容，一律不做。

---

## 4. 总体架构

### 4.1 分层

| 层 | 组成 | 职责 |
|---|---|---|
| 展示层 | Vue 3 + TS + ECharts | 只做展示与交互，所有判断在后端完成 |
| 接入层 | Gin REST API、JWT 认证、RBAC、审计中间件 | 统一入口、权限、幂等、分页、审计 |
| 决策层（自研核心） | Capacity Profiler、Fairness Allocator、Placement Engine、SLO Governor、Scaling Planner、Release Orchestrator | 项目的全部技术含量集中在这一层 |
| 执行层 | Admission Webhook、Actuator、Task Engine、Recovery Controller | 把决策变成受控的集群写操作 |
| 数据层 | PostgreSQL、Prometheus、informer 只读视图 | 业务状态持久化、指标查询、资源视图 |
| 基础设施层 | 3 节点 Kubernetes、ingress-nginx、Helm、演示应用 | 提供运行环境 |

### 4.2 关键设计原则

1. **决策与执行分离**：决策层输出 `Decision` 对象（含依据），执行层只负责落地，二者通过任务记录关联，可回溯。
2. **决策必须可解释**：每个决策落库时保存完整的 `evidence`（各维度得分、被过滤节点及原因、指标快照哈希），支持"同一输入复现同一结论"。
3. **执行必须可回退**：所有写操作都有对应的回退动作与验证步骤，且默认需要人工审批（P0 阶段仅对测试工作负载允许自动执行）。
4. **不重复实现 Kubernetes 已有能力**：配额上限用 ResourceQuota，健康检查用 probe，滚动更新用 Deployment 策略，平台只补它缺的"仲裁、评分、判定"。

### 4.3 一次完整决策的数据流

```
Prometheus + K8s API
        │
        ├─► 资源视图（informer 只读缓存，5s 刷新）
        │        │
        │        ├─► 租户画像：配额、实际占用、历史峰值、增长斜率
        │        └─► 节点画像：可分配量、已分配量、实际用量、资源碎片
        │
        ▼
   Fairness Allocator ──► 租户可用额度（含超配额惩罚）
        │
        ▼
   Placement Engine ──► 候选节点 + 各维度得分 + 过滤原因 + 建议
        │
        ▼
   Task Engine ──► 审批 → Webhook/Actuator 落地 → 验证 → 记录
        │
        ▼
   SLO Governor ──► 错误预算余量 / 燃尽率 ──► 允许推进 or 触发回滚
```

---

## 5. 核心能力设计

### 5.1 资源视图与租户画像

**输入**：Node、Pod、Deployment、ResourceQuota、LimitRange、Prometheus 指标。
**输出**：

- 节点维度：`allocatable`、`Σrequest`、`Σlimit`、实际用量（5 分钟均值/峰值）、可放置余量。
- 租户维度：配额、已用、近 7 天峰值、增长斜率（线性回归）、超配额时长占比。
- 工作负载维度：request/usage 比值（识别"过度申请"）、重启次数、就绪率。

**关键约束**：`allocatable`、`request`、`usage`、`limit` 四个概念必须在数据模型和 UI 中严格区分，不得混用。调度判断只用前两者，`usage` 只用于软性评分。

### 5.2 放置引擎（核心）

**输入**：待放置工作负载（requests、replicas、约束、租户）、集群快照、策略配置。
**输出**：候选节点排序、每节点各维度得分、被淘汰节点及原因、最终建议与依据。

**阶段一：硬约束过滤**（任一不满足即淘汰，且必须记录原因码）

- 节点 Ready 且可调度（无 `node.kubernetes.io/unschedulable`）
- CPU / 内存 request 可容纳
- Taint / Toleration 匹配
- nodeSelector 与 required nodeAffinity 匹配
- 拓扑分布约束（基础版：同 zone/同 hostname 的最大分布）
- hostPort 冲突检测
- 租户配额余量足够

**阶段二：多维度评分**（全部归一化到 [0,1]，加权求和）

```
Score(n) = 100 × Σ(wᵢ × sᵢ(n)) / Σwᵢ

s_cpu     = clamp((Alloc_cpu(n) − Req_cpu(n) − demand_cpu) / Alloc_cpu(n), 0, 1)
s_mem     = 同上
s_balance = 1 − CV(u_cpu(n), u_mem(n))          // 放置后节点自身 CPU/内存占比的变异系数
s_pack    = 1 − (residual_cpu/Alloc_cpu + residual_mem/Alloc_mem)/2   // 装箱：剩余越少越好
s_load    = 1 − clamp(EWMA(usage_ratio, 5m), 0, 1)
s_fair    = 1 − clamp(tenant_alloc_after / tenant_fair_share, 0, 1)
s_frag    = 1 − 不可用碎片率   // 放置后剩余量小于待调度队列中最小 Pod 需求的部分
```

**阶段三：策略组合**（权重可配置，四种预设）

| 策略 | 权重特征 | 适用场景 |
|---|---|---|
| 均衡优先 Balanced | s_balance 高权重 | 长期运行、避免热点节点 |
| 装箱优先 BinPack | s_pack、s_frag 高权重 | 离线批处理、需要腾空节点 |
| 公平优先 FairShare | s_fair 高权重 | 多租户争抢、需要抑制大户 |
| 负载感知 LoadAware | s_load 高权重 | 在线服务、节点实际负载差异大 |

**阶段四：确定性输出**

- 得分相同按节点名排序，保证同一快照多次运行结果一致。
- 仿真器以 `snapshot_hash + policy_id + seed` 作为复现三元组。
- 指标年龄超过 60s 时 `s_load` 权重自动归零，决策标记 `degraded=true`，禁止静默使用过期数据。

**落地方式**：决策结果通过 Mutating Admission Webhook 给 Pod 注入 `nodeAffinity`（仅作用于带 `arbiter.io/managed=true` 标签的命名空间，`failurePolicy: Ignore`，注入失败不影响业务）。这样在不改调度器的前提下，真实地影响 Pod 落点，可以直接演示"建议节点 = 实际节点"。

### 5.3 多租户公平仲裁（核心）

**问题形式化**：给定 n 个租户，每个租户需求向量 `dᵢ = (cpu, mem)`、权重 `wᵢ`、下限 `minᵢ`、上限 `maxᵢ`，求分配向量 `xᵢ`，使加权主导份额最大化最小。

**实现的三种分配器**（用于对比实验）：

| 分配器 | 规则 | 预期表现 |
|---|---|---|
| FIFO | 按申请顺序满足 | 利用率高、公平性差 |
| 硬配额 | 不超过 ResourceQuota | 公平但资源闲置 |
| 加权 DRF | 主导份额 `shareᵢ = max_r(xᵢᵣ / Cᵣ) / wᵢ`，逐轮等步长提升最小份额租户 | 公平性与利用率兼顾 |

**输出指标**：Jain 公平指数 `J = (Σx)² / (n·Σx²)`、集群利用率、租户超配额时长、抢占次数。

**抢占（Preemption）**：只做"建议 + 审批执行"，不做自动驱逐。代价模型：

```
cost(pod) = 优先级权重 × 已运行时长收益 × 副本冗余系数
```

选择 `cost` 最小且能释放足够资源的 Pod 组合，输出"驱逐哪些、预计释放多少、影响哪些服务"。仅在测试工作负载上允许自动执行，且必须先做 `dry-run`。

### 5.4 SLO 与错误预算（核心）

**SLI 定义**（按服务配置）：

- 可用性：`成功请求数 / 总请求数`
- 延迟：`P99 ≤ 300ms 的请求占比`

**SLO**：例如 30 天窗口内可用性 99.5%、延迟达标率 95%。

**错误预算**：`Budget = 1 − SLO`，剩余预算 = 已消耗 / 总额。

**多窗口燃尽率**（按 SRE 实践实现）：

| 窗口组合 | 燃尽率阈值 | 动作 |
|---|---|---|
| 1h + 5m | > 14.4 | 告警 + 阻断发布 |
| 6h + 30m | > 6 | 告警 |
| 3d + 6h | > 1 | 记录工单 |

```
burn_rate(window) = 窗口内错误率 / (1 − SLO)
```

**难点**：长窗口需要预聚合，不能每次查询都扫全量指标。做法是按 1m 粒度预计算 `(total, error)` 到 PostgreSQL，UI 查询走预聚合表，原始指标只用于校验。

**与发布联动**：错误预算剩余低于 20% 时，发布中心自动降级为"只允许灰度、不允许全量推进"。

### 5.5 容量规划与扩缩容决策

**不做**：阈值触发的 HPA 复刻。
**要做**：基于实测延迟曲线的容量推算。

1. 压测得到服务在并发 `c = 1,2,4,8,16,32` 下的 P99 延迟曲线 `L(c)`。
2. 用 Little 定律 `并发 = 到达率 × 平均延迟` 换算目标并发。
3. 在曲线上插值求满足 `P99 ≤ target` 的最小并发，乘安全系数 1.2 得到建议副本数。
4. 输出建议时必须附带：预测 P99、当前实测 P99、模型误差（MAPE）、置信说明。

**诚实边界**：这是经验模型而非排队论精确解。文档与 UI 都要标注"基于实测曲线的插值估计"，并在实验章节报告预测误差。误差过大时系统必须输出"模型不可信"而不是给一个看似精确的数字。

### 5.6 渐进式发布与自动回滚

**状态机**：

```
Pending → Validating → Canary(5%) → Analyzing → Promote(25%) → Analyzing
        → Promote(50%) → Analyzing → Completed
任意 Analyzing 阶段 ──失败──► RollingBack → RolledBack
```

**流量切分**：ingress-nginx 的 `canary-weight` 注解，权重 5/25/50/100。
**能力边界**：只支持 HTTP 入口流量；非 HTTP 服务只能做副本比例灰度，且必须在 UI 上明确标注差异。

**自动判定**（每个阶段要求最小样本量，例如 200 请求或 3 分钟，先到者为准）：

| 判据 | 回滚条件 |
|---|---|
| 错误率 | 灰度组比基线高 2 倍且绝对值 > 1% |
| P99 延迟 | 灰度组比基线高 50% |
| Pod 就绪 | 新版本就绪率 < 90% 持续 2 分钟 |
| SLO 预算 | 本次发布消耗预算超过剩余预算的 50% |

**回滚**：Helm 回退到上一个 revision + 流量权重归零 + 验证 3 分钟 + 记录证据。整个链路有任务 ID、审批记录和执行日志。

### 5.7 应用级故障诊断与受控恢复

只做应用层，共 7 类：

| 故障 | 证据来源 | 恢复动作（需审批） |
|---|---|---|
| CrashLoopBackOff | Pod 状态、上次容器日志、退出码 | 回滚到上一版本 |
| ImagePullBackOff | Event、镜像地址 | 提示修正镜像（不自动改） |
| 就绪探针持续失败 | Probe 事件、端点列表 | 回滚 |
| OOMKilled | Pod 状态、内存曲线 | 建议提高 limit（不自动改） |
| 下游依赖超时 | 应用指标、服务端到端延迟分解 | 提示依赖方，不自动处理 |
| Service 端点为空 | EndpointSlice、Selector 匹配检查 | 提示 Selector 配置错误 |
| 配置错误导致启动失败 | 启动日志关键字、ConfigMap 版本 | 回滚 ConfigMap |

**输出结构**强制分两段：

```
Facts:      可直接观测的事实（时间、对象、事件、指标值）
Inferences: 推断（根因候选 1..n + 每条的证据 + 置信度 + 反证）
```

UI 必须把两段分开渲染。**禁止**只根据 Pod 状态直接断言根因。

### 5.8 任务与工作流引擎

- 状态机：`Pending → Queued → Running → Succeeded / Failed / Cancelled`，支持 `Retrying`。
- 持久化：PostgreSQL 表 + `FOR UPDATE SKIP LOCKED` 领取任务。
- 幂等：`idempotency_key` 唯一索引，重复提交返回同一任务。
- 超时：`context.WithTimeout` 贯穿，超时进入 `Failed(timeout)` 并可重试。
- 重试：指数退避 + 最大次数 + 错误分类（可重试 / 不可重试）。
- 审批：危险任务创建后处于 `PendingApproval`，审批通过才入队。
- 审计：创建、审批、开始、结束、失败原因全部落 `audit_logs`。

---

## 6. 数据模型

| 表 | 关键字段 | 说明 |
|---|---|---|
| tenants | id, name, weight, created_at | 租户 |
| quotas | tenant_id, resource, hard, fair_share | 配额与公平份额 |
| policies | id, name, weights(jsonb), strategy | 调度策略配置 |
| capacity_snapshots | id, ts, payload(jsonb), hash | 集群快照（用于复现） |
| placement_decisions | id, workload, tenant_id, policy_id, chosen_node, scores(jsonb), filtered(jsonb), evidence(jsonb), degraded, snapshot_hash | 决策留痕 |
| arbitration_plans | id, ts, allocations(jsonb), jain_index, preemption(jsonb) | 仲裁结果 |
| slo_definitions | id, service, sli_type, target, window | SLO 定义 |
| slo_windows | service, window_start, total, errors, latency_ok | 预聚合指标 |
| scaling_recommendations | id, service, predicted_p99, mape, replicas | 容量建议 |
| releases | id, app, from_rev, to_rev, state, started_at, finished_at | 发布记录 |
| release_steps | release_id, step, weight, verdict, evidence(jsonb) | 每个灰度阶段 |
| incidents | id, target, type, first_seen, resolved_at | 事件 |
| diagnosis_reports | incident_id, facts(jsonb), inferences(jsonb) | 诊断报告 |
| recovery_actions | incident_id, action, approved_by, result | 恢复动作 |
| tasks | id, type, status, params(jsonb), idempotency_key, retry_count, error | 任务 |
| task_logs | task_id, ts, level, message | 执行日志 |
| audit_logs | id, actor, action, object, before(jsonb), after(jsonb), ts | 审计 |
| users / roles / role_bindings | 标准 RBAC 三表 | 权限 |

约定：所有 `jsonb` 字段必须带 `schema_version`，避免结构演进时无法解析历史数据。

---

## 7. API 设计

统一约定：

- 响应结构 `{ "code": 0, "data": {...}, "trace_id": "..." }`，错误 `{ "code": 40001, "message": "...", "details": [...] }`。
- 列表接口统一 `?cursor=&limit=`，返回 `next_cursor`，不用 offset 分页。
- 所有写操作支持 `Idempotency-Key` 头。
- 认证 JWT，权限在路由级 + 对象级双重校验。

| 分组 | 接口 |
|---|---|
| 容量 | `GET /api/v1/capacity/overview`、`GET /api/v1/capacity/nodes`、`GET /api/v1/capacity/tenants` |
| 租户 | `GET/POST /api/v1/tenants`、`PUT /api/v1/tenants/{id}/quota` |
| 放置 | `POST /api/v1/placement/simulate`、`GET /api/v1/placement/decisions`、`GET /api/v1/placement/decisions/{id}`、`POST /api/v1/placement/apply` |
| 策略 | `GET/POST /api/v1/policies`、`PUT /api/v1/policies/{id}` |
| 仲裁 | `POST /api/v1/arbitration/plan`、`POST /api/v1/arbitration/preemption-preview` |
| SLO | `GET /api/v1/slo`、`GET /api/v1/slo/{service}`、`GET /api/v1/slo/{service}/budget` |
| 容量建议 | `GET /api/v1/scaling/recommendations`、`POST /api/v1/scaling/recommendations/{id}/apply` |
| 发布 | `GET/POST /api/v1/releases`、`POST /api/v1/releases/{id}/promote`、`POST /api/v1/releases/{id}/abort` |
| 诊断 | `GET /api/v1/incidents`、`GET /api/v1/incidents/{id}`、`POST /api/v1/diagnosis/run` |
| 任务 | `GET /api/v1/tasks`、`GET /api/v1/tasks/{id}`、`POST /api/v1/tasks/{id}/cancel`、`POST /api/v1/tasks/{id}/approve` |
| 审计 | `GET /api/v1/audit-logs`、`GET /api/v1/users`、`GET /api/v1/roles` |

必做：`openapi.yaml` 为唯一契约来源，后端路由与前端类型均由它生成或校验，禁止手写两份。

---

## 8. 前端设计

7 个页面，每页只回答一个问题：

| 页面 | 回答的问题 | 必须有 |
|---|---|---|
| 容量总览 | 集群还剩多少、谁在用、有没有风险 | 节点热力图、租户占比、SLO 总体状态 |
| 租户与公平性 | 分配合不合理 | 配额 vs 实际、Jain 指数趋势、超配额告警 |
| 放置决策台 | 这个负载放哪、为什么 | 输入表单、候选节点排序、每维度得分、被淘汰原因 |
| 策略管理 | 换策略会怎样 | 权重编辑、策略对比预览 |
| SLO 与错误预算 | 还能承受多少次故障 | 预算余量、燃尽率曲线、窗口明细 |
| 发布中心 | 这次发布继续还是回滚 | 灰度进度、基线 vs 灰度对比、判定证据 |
| 事件与任务 | 出了什么事、系统做了什么 | 诊断报告（事实/推断分开）、任务时间线、审计 |

硬性要求：页面上的每个数字必须来自真实接口；不允许前端硬编码演示数据；空状态必须明确显示"无数据"而不是 0。

---

## 9. 工程质量要求

**Go**

- 目录按职责分包，`internal/decision`、`internal/actuator`、`internal/store`、`internal/api` 边界清晰。
- 所有跨 goroutine 共享状态必须加锁或用 channel，`go test -race` 必须通过。
- 所有外部调用带 `context` 超时；禁止裸 `http.Get` / 无超时的 client-go 调用。
- 错误统一包装（`%w`），区分"可重试/不可重试/用户输入错误"。
- 结构化日志（slog），字段包含 `trace_id`、`task_id`、`decision_id`。
- 单元测试覆盖决策算法（表驱动，含边界：零容量、单节点、全部不满足、权重全零）。
- `gofmt`、`go vet`、`staticcheck` 必须干净。

**Kubernetes**

- 最小 RBAC：读全量，写只限 Deployment/StatefulSet 的 scale 与 patch，Secret 只读且不回显。
- Webhook 必须 `failurePolicy: Ignore` + `namespaceSelector` 限定 + 超时 3s。
- 所有写操作前先 `dry-run`，失败即中止。

**安全**

- 不对前端返回 Secret 明文；ConfigMap/Secret 只显示键名与哈希。
- 危险操作（回滚、驱逐、缩容）必须审批。
- 审计日志不可修改（只插入）。

---

## 10. 实验设计与验收标准

所有实验必须：同一环境、固定压测脚本、每组 ≥5 次重复、报告均值与 95% 置信区间、**同时报告失败案例**。禁止只展示最好的一次。

| 编号 | 实验 | 变量 | 指标 | 通过标准 |
|---|---|---|---|---|
| E1 | 放置策略对比 | 4 策略 × 3 负载类型（CPU 密集/内存密集/混合） | 碎片率、节点均衡度（CV）、放置成功率、决策耗时、SLO 达成率 | 至少一种策略在特定负载下显著优于默认；能解释劣势场景 |
| E2 | 租户公平性 | 5 租户、偏斜需求、FIFO vs 硬配额 vs DRF | Jain 指数、利用率、超配额时长、抢占次数 | DRF 的 Jain 指数显著高于 FIFO，且利用率不低于硬配额 10% |
| E3 | 仿真保真度 | 200 个待调度 Pod | 预测节点 vs 实际落点一致率 | 一致率 ≥ 90%，不一致的 case 逐条解释 |
| E4 | 容量模型 | 6 档并发压测 + 留出验证 | 预测 P99 误差（MAPE）、扩缩容后 SLO 恢复时间 | MAPE ≤ 25%，且系统能识别并标注低置信场景 |
| E5 | 渐进式发布 | 正常版本 / 故障版本各 20 次 | 发布成功率、异常检出时延、回滚成功率、影响请求数 | 故障版本 100% 被拦截，检出时延 ≤ 2 分钟 |
| E6 | 错误预算 | 持续注入 1% 错误 | 燃尽率告警时延、误报率 | 快窗口 10 分钟内告警，误报 ≤ 1 次/天 |
| E7 | 平台自身性能 | 1000 并发读接口 | API P99、决策计算耗时、指标入库延迟 | P99 ≤ 200ms，单次决策 ≤ 50ms |

**演示脚本（现场 8 分钟）**：容量总览 → 租户超配额 → 用放置决策台解释一次选择 → 切换策略看差异 → 发布一个故障版本 → 系统自动回滚并给出证据 → 查看诊断报告与审计记录。

---

## 11. 交付物

1. Git 仓库（含完整提交历史与 ADR）
2. Go 后端源码 + 单元测试 + 集成测试
3. 决策引擎（仲裁 / 放置 / SLO / 容量模型）源码与算法说明
4. Admission Webhook 与 Actuator 源码
5. Vue 前端源码
6. Helm Chart 与部署文档
7. 数据库设计文档、`openapi.yaml`
8. 架构图与关键时序图
9. **实验报告**：E1–E7 原始数据、脚本、结论、失败案例分析
10. 演示脚本与录屏
11. README（含"未实现内容及原因"一节）
12. 简历项目描述（3 行版 / 1 段版）

---

## 12. 里程碑与门禁

不设日期，只设退出标准。未达标准不得进入下一阶段。

| 里程碑 | 内容 | 退出标准 |
|---|---|---|
| M0 | 方案冻结 | 本文档评审通过；P0/P1/P2 确认；版本矩阵锁定 |
| M1 | 环境与骨架 | 3 节点集群可用；API 骨架可跑；informer 视图能正确反映 Pod 增删 |
| M2 | 放置引擎 | 4 策略可跑；仿真器结果可复现；E1、E3 有初版数据 |
| M3 | 公平仲裁 | DRF 实现并单测通过；E2 完成；抢占为"仅建议"模式 |
| M4 | SLO 与容量 | 错误预算计算正确（与手工核算一致）；E4、E6 完成 |
| M5 | 发布闭环 | 灰度 + 自动回滚 + 审批 + 审计全链路可演示；E5 完成 |
| M6 | 诊断与交付 | 7 类故障可检出；报告齐备；演示脚本彩排通过 |

**范围冻结规则**：M2 之后不再新增功能模块，只允许修复与打磨。新增需求一律进入 backlog。

---

## 13. 风险与降级方案

| 风险 | 概率 | 影响 | 降级方案 |
|---|---|---|---|
| 实验环境资源竞争导致数据抖动 | 高 | 中 | 压测与平台组件分节点部署；实验前清空测试 namespace；固定 CPU 预留 |
| ingress-nginx canary 能力不足 | 中 | 中 | 降级为"双 Service + 副本比例灰度"，并在文档标注差异 |
| 容量模型误差过大 | 中 | 中 | 明确输出"模型不可信"，改为纯阈值建议；把误差本身作为实验结论 |
| Webhook 影响业务 Pod | 中 | 高 | `failurePolicy: Ignore` + namespace 白名单 + 先 dry-run 观察一周 |
| DRF 实验规模小，结论有限 | 高 | 低 | 在报告中明确样本规模，只做趋势性结论，不做绝对化表述 |
| 前端 7 页拖慢进度 | 中 | 中 | 前 5 页为 P0，事件页与策略页可后置 |
| 单机故障导致环境全丢 | 中 | 高 | 每个里程碑结束做 VM 快照；数据与代码全部在 Git |

---

## 14. AI Agent 执行规约

1. 不得一次性生成全部代码，按模块推进：设计 → 数据模型 → 接口 → 编码 → 单测 → 集成 → 验收 → 文档。
2. 每个阶段必须报告：已完成内容、修改文件、核心代码解释、实际执行的测试命令、真实输出、遗留问题、下一步。
3. 禁止编造测试结果、性能数字、故障检出率；未运行的内容必须标注"未验证"。
4. 未获批准不得下载依赖、安装软件、修改集群或执行破坏性操作。
5. 修改环境前先检查当前状态；重要变更前创建 Git commit 或 VM 快照。
6. 测试产生的资源必须清理，但不得影响集群基础组件。

---

## 15. 难度来源说明（用于答辩与简历）

这不是一个 CRUD 管理面板，深度体现在五处：

1. **多目标优化建模**：把"放得好"形式化为可归一化、可加权、可复现的评分函数，并证明不同负载下最优策略不同。
2. **公平性与效率的权衡**：实现加权 DRF，用 Jain 指数与利用率两个相互冲突的指标做对比实验，而不是只讲概念。
3. **模型必须被验证**：容量预测给出误差区间和不可信判定，体现工程诚实性，这是大多数学生项目缺失的部分。
4. **闭环控制与安全边界**：决策 → 审批 → 执行 → 验证 → 回滚，每一环都有状态、幂等、超时与审计。
5. **实验方法论**：多次重复、置信区间、失败案例、留出集验证，保证结论站得住。

对应的面试考点：资源模型与 request/limit 语义、调度约束、公平分配算法、SLO/错误预算、canary 分析、幂等与状态机、并发安全、可观测性设计。

---

## 16. 与原方案的裁剪对照

| 原方案内容 | 处置 | 原因 |
|---|---|---|
| Informer 缓存同步机制（原第 4.3 节） | 保留为实现细节，不再作为卖点 | 属于工程实现，不构成差异化 |
| 14 类资源全量 CRUD | 裁剪为只读全量 + 写白名单 | 页面数量不等于技术深度 |
| 应用模板管理与依赖关系 | 裁剪为 Helm values 模板 | 依赖解析是包管理器问题，非本项目命题 |
| 灰度发布 | 保留并升级为"数据驱动判定 + 自动回滚" | 从功能升级为闭环 |
| 4 种调度策略 + 模拟器 | 保留并强化为可复现实验 | 本项目核心竞争力 |
| Scheduler Framework 插件 | 删除 | 高风险、偏底层，改用 Webhook 注入 |
| 两个 Operator | 裁剪为 0 个独立 Operator，控制逻辑内聚到 Release/Recovery Controller | 避免与 Helm、Deployment 职责冲突 |
| 网络诊断（连通性/DNS/TCP/丢包） | 删除 | 偏底层，且与命题无关 |
| eBPF | 删除 | 同上 |
| 多集群 | 删除，仅保留抽象 | 无法真实验证 |
| 8 类故障诊断 | 裁剪为 7 类应用级故障 | 聚焦可验证范围 |
| 30+ 组实验 | 收敛为 7 组高质量实验 | 质量优于数量 |
| 16 个前端页面 | 裁剪为 7 个 | 每页必须回答一个决策问题 |
| Loki | 删除 | 节省资源，日志按需拉取 |
| 新增：多租户公平仲裁 | 增加 | 补足"资源该给谁"这一缺口，且算法含量高 |
| 新增：SLO 错误预算与燃尽率 | 增加 | 补足"何时该停"这一缺口，是 SRE 核心能力 |
| 新增：容量预测模型与误差报告 | 增加 | 提供可验证的量化深度 |
| 新增：受控实验环境（演示应用 + 压测器） | 增加 | 解决"实验不可复现"这一根本问题 |

---

## 17. 最终验收标准

现场必须能完成以下 8 件事，缺一不可：

1. 展示集群容量与租户占用，指出一个超配额租户。
2. 对一个待部署负载执行放置模拟，解释每个候选节点的得分与被淘汰节点原因。
3. 切换调度策略，展示同一负载的推荐节点发生变化。
4. 执行一次真实部署，验证 Pod 落点与建议一致。
5. 发布一个故障版本，展示系统在灰度阶段自动拦截并回滚。
6. 展示 SLO 错误预算与燃尽率曲线，说明当前是否允许继续发布。
7. 查看一份诊断报告，确认"事实"与"推断"分离呈现。
8. 查看审计记录，确认从决策到执行的完整链路可追溯。

**最终目标**：交付一个能对"资源该给谁、放到哪、何时该停"给出可解释答案、并用真实数据证明其有效性的云原生决策平台。
