# AI 任务调度与资源管理（P3）

本文描述 AI 任务调度能力的口径：GPU 扩展资源建模、gang 成组放置、GPU 租户配额与执行侧边界。实现位于 `pkg/schedcore`、`internal/decision/capacity`、`internal/api` 与 `internal/actuator`。

## 1. 架构与数据流

```
Node.Status.Allocatable["nvidia.com/gpu"] ──informer──► capacity.NodeView（Resources.GPU）
Pod requests（逐容器累加/逐维取 max） ─────────────────► PodView.Requests.GPU
模拟请求 gang 参数 ──► schedcore.PlaceGang ──► Decision.Assignments + Evidence
租户配额 quotas.resource='gpu' ──► filterNode TENANT_QUOTA_EXCEEDED
执行闸门 ──► 多节点 assignments 一律 GANG_NOT_EXECUTABLE 拒绝（P4 再支持批量注入）
```

安全模型不变：所有输出仅为建议与决策记录，落地执行必须走"创建任务 → 人工审批 → dry-run → 提交"链路。

## 2. GPU 资源建模

- `schedcore.Resources` 增加 `GPU int64`（整卡数，对应 extended resource `nvidia.com/gpu`）；`Add/Sub/Mul/FitsIn/IsZero` 逐维机械扩展，`Workload.Demand()` 与租户配额检查自动生效。
- 节点画像从 `Node.Status.Allocatable` / `Capacity` 读取 `nvidia.com/gpu`（`capacity.GPUResourceName`）；Pod request 按经典语义逐维取 max(Σ 业务容器, max(init 容器))。
- 无卡集群所有节点 GPU=0，行为与 P2 之前完全一致（GPU 维不参与主导份额、打包与过滤）。

### DRF 口径

`dominantShare = max_r(x_r / C_r)` 增加 GPU 维：**GPU 容量为 0 的集群该维不参与**（避免除零），即 GPU 公平维只在有卡的集群激活。DRF 水位法（`drfAllocate`）与 FIFO/HARD_QUOTA 分配器的取量、水位上界同步贯通 GPU 维。

### 打分口径

**不加第八权重维**：`Weights` 七维枚举牵动校验、四组预设、前端滑杆与 openapi 全链兼容。GPU 的"放不放得下"由硬过滤解决；评分上 GPU 余量并入 `s_pack`（容量 > 0 时按三维平均，无卡集群口径不变）。

### 硬过滤

`filterNode` 余量检查按 **CPU → GPU → 内存** 的固定顺序报因，新原因码 `INSUFFICIENT_GPU`（evidence 含 remaining_gpu/request_gpu）。租户配额 evidence 增加 `quota_gpu` / `after_gpu`。

## 3. 快照哈希语义变化

GPU 随 Nodes 的 Allocatable/Requested 参与快照哈希（`hash.go` 逻辑未改）。集群打了 GPU 资源后哈希变化是正确语义（集群状态确实变了）；0-GPU 集群哈希不变。决策 evidence 的 `schema_version` 自 P3 起为 **2**。

## 4. Gang 成组放置

- 复用 `POST /api/v1/placement/simulate`，请求加可选 `gang.min_available`（`1 <= min_available <= replicas`，缺省 = replicas 即全有全无）。
- 算法（`pkg/schedcore/gang.go` 的 `PlaceGang`）：
  1. 复用 `filterNode` 硬过滤（单副本可行性，含 GPU 余量与租户配额）；
  2. 复用 `scoreNode` 七维评分，按总分降序、同分按节点名升序；
  3. 贪心装箱：逐节点取 `floor(余量/单副本)` 的最小维度上限（节点内可放多副本），直到放满 replicas 或候选耗尽；
  4. 总量 ≥ min_available → `OK`，输出 `Assignments[]`（节点 → 副本数），`ChosenNode` = 副本数最多的节点（并列取名字最小者）；否则整体 `NO_CANDIDATE`，Evidence 记 `total_feasible` / `min_available`，**绝不输出"放一半"的部分方案**。
- GPU 碎片是 AI 任务的典型瓶颈：单节点各剩 1 卡放不下 2 卡副本时，gang 会如实判失败——这正是全有全无语义要暴露的问题。
- 决策落库仍是一条记录；assignments 同时写入 `Decision.Assignments` 与 evidence 明细。

## 5. GPU 租户配额

- `quotas.resource` 枚举增加 `'gpu'`（0005 迁移，幂等；既有租户补 hard=0 的 gpu 行，0 = 不限，与缺行语义一致）。
- 配额检查复用 `filterNode` 的 TENANT_QUOTA_EXCEEDED 路径：`Allocated + Demand` 超过 quota 的任一维即拒绝。
- 不新建 ai_jobs 概念："队列"复用 taskengine 状态机（PENDING_APPROVAL → QUEUED，审批人 ≠ 创建人）。

## 6. 执行侧边界（重要）

- 当前执行器（`actuator.KubeExecutor`）只支持**单节点** nodeAffinity 注入。gang 决策若分布到多个节点：
  - `/placement/apply` 直接业务拒绝 `GANG_NOT_EXECUTABLE`；
  - 执行器参数层再做一次防御性检查（含 >1 项 assignments 的参数拒绝执行）。
- 多节点批量注入（按 assignments 拆分任务/批量 patch）留待后续阶段（P4）。

## 7. 前端

- 模拟工作台：GPU 卡/副本输入、gang 开关 + 最少可调度副本、结果区显示节点分布 chips（`节点 × N 副本`）、gang 参数与不可行证据；新增"AI 训练（gang）"模板。
- 节点与容量页：节点卡片显示 GPU 卡数（allocatable=0 不显示），详情面板显示可分配 GPU。

## 8. 验证方式（无真实 GPU 环境）

kubeadm VM 集群可通过 patch 节点 status 打上假的 GPU 资源，走通 informer → 快照 → 模拟的全链路：

```powershell
kubectl patch node ka-w2 --subresource=status --type=merge `
  -p '{"status":{"capacity":{"nvidia.com/gpu":4},"allocatable":{"nvidia.com/gpu":4}}}'
```

然后按 03-user-guide.md 的验证清单跑普通/Gang/配额三类模拟。撤销时把两个值改回 0（或删除键）。

## 9. 限制

- GPU 只建模"整卡数"，不支持 MIG/分时共享/拓扑（NVLink 域）感知；单副本多卡 via `nvidia.com/gpu` 语义已覆盖，跨卡池调度未覆盖。
- gang 是模拟与决策语义；真实的多节点落地需要 P4 的批量注入。
- quota 的 gpu hard=0 表示不限，不表示"0 卡可用"；需要限制为 0 卡的场景用极小负值不可行，当前用缺行表达不限、无"显式禁止"语义。
- 假 GPU（patch status）不代表真实设备插件行为：真实环境里 unschedulable 的 GPU Pod 会滞留 Pending，本系统只在模拟层表达可行性。
