# 决策算法与契约规范（M0 冻结）

- 状态：**待评审**
- 作用：`pkg/schedcore` 的实现依据，也是单元测试与实验报告的判定依据
- 原则：本文档的公式、枚举、原因码是**唯一来源**；`db/schema.sql` 与 `api/openapi.yaml` 必须与之对齐
- 与说明书冲突时以说明书为准

---

## 1. 符号与单位

| 符号 | 含义 | 单位 |
|---|---|---|
| `r` | 资源维度，本期 `r ∈ {cpu, mem}` | — |
| `A_r(n)` | 节点 n 在该资源上的 `allocatable` | cpu: millicore；mem: byte |
| `R_r(n)` | 节点 n 上已调度 Pod 的 `Σrequest` | 同上 |
| `U_r(n)` | 节点 n 的实际用量（5 分钟均值） | 同上 |
| `d_r` | 待放置工作负载的 `request`（单副本） | 同上 |
| `C_r` | 集群可仲裁容量 `ΣA_r(n) − reserved_r` | 同上 |
| `x_ir` | 租户 i 在该资源上被分配的量 | 同上 |
| `w_i` | 租户权重，`w_i > 0` | 无量纲 |
| `u_r(n)` | 使用率 `U_r(n) / A_r(n)` | [0,1+] |

**四条硬规矩**

1. 调度判定只用 `A_r` 与 `R_r`，**不得**用 `U_r` 做硬过滤。
2. `limit` 不参与放置计算，只用于超卖风险提示。
3. 内部一律用整数（millicore / byte）运算，禁止浮点累加资源量。
4. 所有比率在**输出**时保留 4 位小数，在**比较**时用原始 float64，平局阈值 `1e-9`。

---

## 2. 放置引擎

### 2.1 输入

```
PlacementRequest {
  workload: {name, namespace, kind, replicas, requests{cpu,mem}, nodeSelector,
             requiredAffinity, tolerations, topologySpread, hostPorts[]}
  tenant_id
  policy_id
  snapshot_hash        // 由服务端生成，客户端不得传入
}
```

### 2.2 阶段一：硬过滤与原因码

按顺序执行，命中即淘汰，**必须**记录原因码与被比较的数值：

| 顺序 | 规则 | 原因码 | 证据字段 |
|---|---|---|---|
| 1 | Node `Ready` | `NODE_NOT_READY` | condition 状态、最后转换时间 |
| 2 | 无 `node.kubernetes.io/unschedulable` | `NODE_UNSCHEDULABLE` | taint 键值 |
| 3 | `A_cpu(n) − R_cpu(n) ≥ d_cpu` | `INSUFFICIENT_CPU` | 剩余 millicore vs 需求 |
| 4 | `A_mem(n) − R_mem(n) ≥ d_mem` | `INSUFFICIENT_MEMORY` | 剩余 byte vs 需求 |
| 5 | Taint 均被 Toleration 覆盖（`NoSchedule` / `NoExecute`） | `TAINT_NOT_TOLERATED` | 未覆盖的 taint |
| 6 | `nodeSelector` 全匹配 | `NODE_SELECTOR_MISMATCH` | 不匹配的 key |
| 7 | required nodeAffinity 匹配 | `AFFINITY_MISMATCH` | 不匹配的表达式 |
| 8 | 拓扑分布约束（同 zone / 同 hostname 的 `maxSkew`） | `TOPOLOGY_VIOLATION` | 当前计数、skew |
| 9 | 无 hostPort 冲突 | `HOSTPORT_CONFLICT` | 冲突端口与占用者 |
| 10 | 租户配额余量足够（含本次副本数） | `TENANT_QUOTA_EXCEEDED` | 余量 vs 需求 |

过滤结果为空 → 返回 `status = NO_CANDIDATE`，并返回**每个**节点的淘汰原因，禁止只返回"无可用节点"。

### 2.3 阶段二：七维评分

所有维度先归一化到 `[0,1]`，再加权：

```
Score(n) = 100 × Σ_r (w_r × s_r(n)) / Σ_r w_r
```

其中权重向量 `w` 由策略给出（见 2.4），`Σw = 1`。

**定义**（`clamp(v, lo, hi)` 为截断；`A = A_r(n)`，`R = R_r(n)`）

| 维度 | 公式 | 语义 |
|---|---|---|
| `s_cpu` | `clamp((A_cpu − R_cpu − d_cpu) / A_cpu, 0, 1)` | 放置后 CPU 余量比例 |
| `s_mem` | `clamp((A_mem − R_mem − d_mem) / A_mem, 0, 1)` | 放置后内存余量比例 |
| `s_balance` | `1 − clamp(CV(u_cpu', u_mem'), 0, 1)`，`u'` 为放置后的使用率 | 节点自身 CPU/内存是否偏科 |
| `s_pack` | `1 − clamp(0.5 × (res_cpu/A_cpu + res_mem/A_mem), 0, 1)`，`res = A − R − d` | 装箱：剩余越少得分越高 |
| `s_load` | `1 − clamp(max(ewma_cpu, ewma_mem), 0, 1)` | 实际负载（非申请量） |
| `s_fair` | `1 − clamp(share_after / fair_share, 0, 1)` | 抑制大户继续膨胀 |
| `s_frag` | `1 − clamp(0.5 × (unusable_cpu/A_cpu + unusable_mem/A_mem), 0, 1)` | 剩余量是否小到无法再用 |

**补充定义**

- `CV(u1, u2)`：两元素变异系数，闭式 `|u1 − u2| / (u1 + u2)`；当 `u1 + u2 = 0` 时 `CV = 0`。
- `ewma_r`：对节点使用率序列做指数加权平均，采样 15s，时间常数 `τ = 300s`，`α = 1 − exp(−Δt/τ)`；取 CPU 与内存中较大者，因为瓶颈资源才决定能否再接负载。
- `share_after`：放置该工作负载后，租户主导份额 `max_r((x_ir + d_ir) / C_r)`；`fair_share` 取自 `quotas.fair_share`（占集群的比例）。`fair_share ≤ 0` 时 `s_fair = 0` 并在 evidence 中记 `FAIR_SHARE_MISSING`。
- `unusable_r`：放置后剩余量小于待调度队列中最小 Pod 需求时，该剩余量全部计为不可用碎片；否则计 0。

### 2.4 策略预设权重（`Σw = 1`）

| 维度 | Balanced | BinPack | FairShare | LoadAware |
|---|---|---|---|---|
| `s_cpu` | 0.18 | 0.10 | 0.10 | 0.10 |
| `s_mem` | 0.18 | 0.10 | 0.10 | 0.10 |
| `s_balance` | 0.25 | 0.05 | 0.08 | 0.12 |
| `s_pack` | 0.10 | 0.35 | 0.05 | 0.06 |
| `s_load` | 0.15 | 0.05 | 0.05 | 0.50 |
| `s_fair` | 0.10 | 0.05 | 0.52 | 0.07 |
| `s_frag` | 0.04 | 0.30 | 0.10 | 0.05 |

权重存于 `policies.weights`（jsonb），四组预设为种子数据。校验：所有维度权重 `≥ 0`、`Σ = 1 ± 1e-6`，否则拒绝保存。

### 2.5 阶段三：确定性输出

1. 选 `Score` 最大者；平局按节点名**字节序升序**取第一个。
2. 输出 `rank`、`score`、`breakdown{维度: 得分}`、`filtered[{node, reason, evidence}]`。
3. `snapshot_hash = sha256(canonical_json(snapshot))`，canonical 规则：
   - 键按字典序排列；数组按主键排序（节点名 / pod uid）；
   - 浮点统一格式化为 6 位小数；时间统一 RFC3339 UTC；
   - 参与哈希的字段固定为 `nodes`、`tenants`、`pending`、`policy_id`、`metrics_ts`、`reserved`。
4. 复现三元组 `(snapshot_hash, policy_id, seed)`；本期算法内无随机性，`seed` 保留给未来抢占组合搜索。

### 2.6 降级规则

| 条件 | 行为 |
|---|---|
| 指标年龄 > 60s | `s_load` 权重置 0，其余权重按比例归一化，`degraded = true` |
| 节点指标缺失 | 该节点 `s_load = 0`，`degraded = true`，evidence 记 `METRICS_MISSING` |
| 所有节点指标缺失 | 拒绝输出建议，返回错误码 `50301`，`message = METRICS_UNAVAILABLE` |
| 快照内节点数为 0 | 返回 `NO_CANDIDATE`，原因码 `NO_NODE_IN_SNAPSHOT` |

---

## 3. 多租户公平仲裁

### 3.1 三种分配器

| 分配器 | 规则 |
|---|---|
| `FIFO` | 按租户申请时间顺序满足需求，直到容量耗尽 |
| `HARD_QUOTA` | `x_ir = min(d_ir, hard_ir, C_r)`，超出即拒绝，不回收 |
| `DRF` | 见 3.2 |

### 3.2 加权 DRF（两阶段）

**阶段一：保底**

对每个租户分配 `min_ir`（同时不超过 `d_ir` 与 `max_ir`）。容量不足以覆盖全部保底时，按容量比例缩减，返回 `infeasible = true` 与各租户缺口；此时不进入阶段二。

**阶段二：加权主导份额最大化最小（水位法）**

设份额水位 `λ ≥ 0`，租户 `i` 在资源 `r` 上的取量为

```
x_ir(λ) = clamp( floor(λ · w_i · C_r), min_ir, min(d_ir, max_ir) )
```

总分配量对 `λ` 单调不减，因此满足 `Σ_i x_ir(λ) ≤ C_r` 的最大 `λ` 即对应公平解。用二分法求该 `λ`：

```
low ← 0
high ← 2 × max_{i,r} target_ir / (w_i · C_r)
repeat 200:
    mid ← (low + high) / 2
    if Σ_i x_i(mid) ≤ C: low ← mid  else  high ← mid
x_i ← x_i(low)
```

- 水位法等价于渐进填充（progressive filling），结果与租户遍历顺序无关。
- 取整使用整数向下取整（`floor`）而非四舍五入，保证分配总量不会因取整而超出容量。
- 同一 `λ` 下各租户的加权主导份额相同，即达到加权最大最小公平。
- 固定 200 次迭代使结果对同一输入完全确定，无需随机种子；`n ≤ 50` 时耗时远低于 §8 的 10ms 预算。

**为什么不用"逐轮整段推进"**

早期实现按"每轮选加权份额最小的租户、把它剩余需求一次性给满"推进。该做法在首轮就把整段剩余容量交给第一个被选中的租户，后续租户只能拿残渣，等价于 FIFO——3 个等权租户需求 90000m 时得到 90000/0/0，公平性完全失效。水位法消除了这条退化路径，也不再需要 `blocked` 集合与迭代上限保护。

**实现与本文档的对应**：`pkg/schedcore/fairness.go` 的 `drfAllocate` 即上述水位法；`Arbitrate` 负责阶段一/阶段二串联、`infeasible` 与缺口输出。

### 3.3 公平性度量

- 主导份额 `s_i = max_r(x_ir / C_r)`（**不加权**，权重只作用于分配过程，不作用于度量，避免自己证明自己）。
- Jain 指数 `J = (Σ s_i)² / (n · Σ s_i²)`；全为 0 时 `J = 1` 且 `n_effective = 0`，报告中必须标注"该值无意义"。
- 同时报告单资源 Jain（cpu / mem 各一份），供实验 E2 对比。

### 3.4 抢占代价模型（仅建议，不自动驱逐）

```
cost(pod) = priority_weight × runtime_credit × redundancy_factor
```

| 因子 | 定义 | 取值约束 |
|---|---|---|
| `priority_weight` | 优先级归一化值，`lowest = 0.1`，`highest = 1.0` | (0, 1] |
| `runtime_credit` | `1 / (1 + uptime_hours / 24)`，运行越久越不该被驱逐 | (0, 1] |
| `redundancy_factor` | `1 / ready_replicas` | (0, 1] |

- 单副本服务（`ready_replicas = 1`）**不可抢占**，直接排除，不计入候选。
- 组合选择：按 `cost` 升序累加，直到释放量覆盖缺口；同一 Deployment 最多驱逐 `ceil(replicas × 0.34)` 个副本。
- 输出必须包含：被驱逐 Pod 列表、每个 Pod 的 cost 分解、预计释放资源、受影响服务、`dry_run` 结果。

---

## 4. SLO 与错误预算

### 4.1 SLI

| 类型 | 定义 |
|---|---|
| `AVAILABILITY` | `good_requests / total_requests`（非 5xx 且非超时） |
| `LATENCY` | `count(latency ≤ threshold_ms) / total_requests` |

### 4.2 预算与燃尽率

```
budget_total(w)      = 1 − SLO_target
budget_consumed(w)   = bad_requests(w) / total_requests(w)
budget_remaining(w)  = 1 − budget_consumed(w) / budget_total(w)
burn_rate(w)         = error_rate(w) / (1 − SLO_target)
```

预算余量 `< 0` 视为已透支，UI 显示为负值而非 0（不得掩盖）。

### 4.3 多窗口告警（长窗与短窗**同时**超阈值才触发）

| 长窗 | 短窗 | 阈值 | 动作 |
|---|---|---|---|
| 1h | 5m | 14.4 | 告警 + 阻断发布推进 |
| 6h | 30m | 6 | 告警 |
| 3d | 6h | 1 | 记录工单 |

### 4.4 预聚合

- 每 1 分钟写入一行 `slo_windows(service, window_start, total, errors, latency_ok)`。
- 查询任意窗口 = 对区间求和，禁止扫描原始指标。
- 数据完整度 `completeness = 有数据的分钟数 / 应有分钟数`；`< 0.95` 时该窗口标记 `degraded`，**不得**据此触发自动回滚。
- 原始指标只用于校验（E6 中抽样比对，误差 > 1% 需排查）。

### 4.5 与发布联动

`budget_remaining < 0.20` → 发布中心禁止 `Promote` 到 100%，只允许灰度与回滚。

---

## 5. 容量模型

### 5.1 测量

单副本条件下，并发 `c ∈ {1,2,4,8,16,32}` 各压测 ≥ 60s（或 ≥ 500 请求），取稳定段 P99，得到 `(c_k, L_k)`。

### 5.2 曲线校验

- 若存在 `L_{k+1} < L_k` 且降幅 > 5%，判定曲线不可用 → `INVALID_CURVE`，不输出建议。
- 测量点少于 4 个 → `INSUFFICIENT_POINTS`。

### 5.3 求解

在 `log2(c)` 空间做线性插值，求满足 `L(c) ≤ target_p99` 的最大 `c*`：

```
c* = 2^( log2(c_k) + (T − L_k) / (L_{k+1} − L_k) × (log2 c_{k+1} − log2 c_k) )
```

（区间满足 `L_k ≤ T < L_{k+1}`；`T = L_k` 时取 `c_k`，`T = L_{k+1}` 时取 `c_{k+1}`。）

- `T < L_1` → `c* = 1`，`confidence = HIGH`，同时标注"目标低于测量下限"。
- `T ≥ L_6` → 外推，`c*` 取最后一个测量点值并标 `EXTRAPOLATION`，`confidence` 最高只能 `LOW`。

### 5.4 副本数与输出

```
replicas_suggested = max(1, ceil(1.2 × C_peak / c*))
```

`C_peak` 为目标高峰并发（由 Little 定律换算：`并发 = 到达率 QPS × 平均延迟`）。

输出必须同时包含：`predicted_p99`、`current_p99`、`mape`、`replicas_current`、`replicas_suggested`、`confidence`、`note`。

### 5.5 误差与置信

留出验证：在未参与拟合的并发档位实测，`MAPE = mean(|pred − actual| / actual) × 100%`（排除 `actual = 0`）。

| MAPE / 情形 | confidence | 是否给出建议 |
|---|---|---|
| ≤ 15% | HIGH | 是 |
| 15%–25% | MEDIUM | 是，附误差区间 |
| 25%–50% | LOW | 是，顶部显示"低置信" |
| > 50% 或 `INVALID_CURVE` | UNUSABLE | **否**，只输出"模型不可信"与原因 |

---

## 6. 渐进式发布判定

### 6.1 状态机

```
PENDING → VALIDATING → CANARY(5) → ANALYZING → PROMOTING(25) → ANALYZING
        → PROMOTING(50) → ANALYZING → PROMOTING(100) → COMPLETED
任意 ANALYZING ──ROLLBACK──► ROLLING_BACK → ROLLED_BACK
任意阶段 ──错误──► FAILED
```

状态与 `releases.state` 枚举严格一致（见第 7 节）。

### 6.2 判定时机

- 满足 `samples ≥ 200` **或** `elapsed ≥ 180s` 时进入判定。
- 若此时 `samples < 50` → `INSUFFICIENT_DATA`，继续等待，最长 600s；超时仍未达标 → `HOLD` 并告警，**不自动推进**。
- 基线与灰度使用**等长窗口**，且时间对齐。

### 6.3 判定规则

| 判据 | 回滚条件 |
|---|---|
| 错误率 | `canary_error_rate > 2 × baseline_error_rate` 且 `canary_error_rate > 1%` |
| P99 延迟 | `canary_p99 > 1.5 × baseline_p99` |
| 就绪率 | 新版本 `ready/desired < 0.90` 持续 120s |
| 预算 | 本阶段消耗预算 > 剩余预算的 50% |

任一条命中即 `ROLLBACK`；全部未命中且样本充足 → `PROCEED`。

### 6.4 回滚动作（顺序固定，任一步失败则中止并 `FAILED`）

1. 灰度权重归零（`canary-weight: 0`）
2. Helm 回退到 `from_rev`
3. 等待 3 分钟并验证错误率回落至基线 ±20%
4. 写 `release_steps` 证据 + `audit_logs`

---

## 7. 枚举与错误码（唯一来源）

### 7.1 枚举

| 枚举 | 取值 |
|---|---|
| `policy.strategy` | `BALANCED` \| `BIN_PACK` \| `FAIR_SHARE` \| `LOAD_AWARE` |
| `allocator` | `FIFO` \| `HARD_QUOTA` \| `DRF` |
| `sli_type` | `AVAILABILITY` \| `LATENCY` |
| `release.state` | `PENDING` \| `VALIDATING` \| `CANARY` \| `ANALYZING` \| `PROMOTING` \| `COMPLETED` \| `ROLLING_BACK` \| `ROLLED_BACK` \| `FAILED` |
| `release_step.verdict` | `PROCEED` \| `ROLLBACK` \| `HOLD` \| `INSUFFICIENT_DATA` |
| `task.status` | `PENDING` \| `PENDING_APPROVAL` \| `QUEUED` \| `RUNNING` \| `RETRYING` \| `SUCCEEDED` \| `FAILED` \| `CANCELLED` |
| `task.type` | `PLACEMENT_APPLY` \| `PREEMPT` \| `SCALE` \| `RELEASE_PROMOTE` \| `RELEASE_ABORT` \| `RECOVERY` \| `CONFIG_ROLLBACK` |
| `incident.type` | `CRASH_LOOP` \| `IMAGE_PULL_BACKOFF` \| `READINESS_FAIL` \| `OOM_KILLED` \| `DOWNSTREAM_TIMEOUT` \| `EMPTY_ENDPOINTS` \| `CONFIG_ERROR` |
| `diagnosis.confidence` | `HIGH` \| `MEDIUM` \| `LOW` |
| `capacity.confidence` | `HIGH` \| `MEDIUM` \| `LOW` \| `UNUSABLE` |
| `placement.status` | `OK` \| `NO_CANDIDATE` |
| `severity` | `INFO` \| `WARN` \| `CRITICAL` |

### 7.2 放置原因码

`NODE_NOT_READY`、`NODE_UNSCHEDULABLE`、`INSUFFICIENT_CPU`、`INSUFFICIENT_MEMORY`、`TAINT_NOT_TOLERATED`、`NODE_SELECTOR_MISMATCH`、`AFFINITY_MISMATCH`、`TOPOLOGY_VIOLATION`、`HOSTPORT_CONFLICT`、`TENANT_QUOTA_EXCEEDED`、`NO_NODE_IN_SNAPSHOT`

### 7.3 降级 / 警告码

`METRICS_STALE`、`METRICS_MISSING`、`FAIR_SHARE_MISSING`、`EXTRAPOLATION`、`INVALID_CURVE`、`INSUFFICIENT_POINTS`、`BUDGET_LOW`、`SLO_DATA_DEGRADED`、`SINGLE_REPLICA_EXCLUDED`

### 7.4 错误码（HTTP 响应 `code`）

| code | HTTP | 含义 |
|---|---|---|
| 0 | 200 | 成功 |
| 40001 | 400 | 参数校验失败（`details[]` 指明字段） |
| 40101 | 401 | 未认证 / token 失效 |
| 40301 | 403 | 无权限 |
| 40401 | 404 | 资源不存在 |
| 40901 | 409 | 幂等键冲突且请求体不一致 |
| 40902 | 409 | 状态机非法跃迁 |
| 40903 | 409 | 资源已被占用（如并发审批） |
| 42201 | 422 | 业务规则拒绝（如权重和不为 1、预算不足禁止推进） |
| 42901 | 429 | 限流 |
| 50001 | 500 | 内部错误 |
| 50301 | 503 | 依赖不可用（K8s API / Prometheus / PG） |

---

## 8. 复杂度与性能预算

| 操作 | 规模上界 | 目标耗时 |
|---|---|---|
| 单次放置评分 | 50 节点 × 7 维 | ≤ 5 ms |
| 快照构建（内存，已缓存） | 2000 Pod | ≤ 20 ms |
| DRF 仲裁 | 50 租户 × 2 资源 | ≤ 10 ms |
| 单次决策端到端（含落库） | — | ≤ 50 ms（E7 判定） |
| API 读接口 P99 | 1000 并发 | ≤ 200 ms（E7 判定） |

超过预算需给出 profiling 证据，不得用"机器慢"作为解释。

---

## 9. 单元测试强制用例（表驱动）

| 场景 | 期望 |
|---|---|
| 零剩余容量 | 全部 `INSUFFICIENT_*`，`NO_CANDIDATE` |
| 单节点集群 | 该节点即结果，不因无比较对象报错 |
| 全部节点被 taint 排斥 | 返回全部 `TAINT_NOT_TOLERATED` |
| 权重和为 0.99 / 1.01 | 保存被拒，返回 `42201` |
| 两节点得分完全相同 | 取名字字节序靠前者 |
| 指标年龄 61s | `degraded=true`，`s_load` 权重为 0 |
| `fair_share = 0` | `s_fair = 0` + `FAIR_SHARE_MISSING` |
| DRF 容量按 CPU 先耗尽 | 总分配量不超容量，维度间互不挤占，结果确定可复现 |
| DRF 所有租户 min 之和超容量 | `infeasible=true`，返回缺口 |
| 容量曲线非单调 | `INVALID_CURVE`，不输出副本建议 |
| MAPE = 60% | `UNUSABLE`，不出数字 |
| 发布阶段样本 30 且超时 | `HOLD`，不推进 |
| 同一 `Idempotency-Key` 重复提交 | 返回同一 task id，`code = 0` |
| 同一幂等键不同请求体 | `40901` |
