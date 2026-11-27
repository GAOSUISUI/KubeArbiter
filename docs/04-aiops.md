# AIOps 底座：检测规则、健康分与 SLO

本文描述第一阶段智能运维能力的口径：指标历史、故障检测、事件生命周期、健康分与内置 SLI 采集。实现位于 `internal/aiops`、`internal/observe` 与 `internal/decision/capacity/diagnostics.go`。

## 1. 数据流

```
kubelet stats/summary ──15s──► metrics.Store（EWMA + 6h 环形历史）
informer(Node/Pod) ──► capacity.Diagnostics ──► aiops.Detector（30s）
                                                  ├─► incidents
                                                  ├─► diagnosis_reports
                                                  └─► 条件消失后自动恢复
HTTP /api/v1/* ──► observe.Recorder ──1min──► slo_windows ──► SLO 页面
```

- 检测器只读节点与 Pod，不执行任何恢复动作。
- 决策快照 `capacity.Snapshot` 不增加字段，避免影响快照哈希与历史决策留档；运维字段走独立的 `Diagnostics()`。

## 2. 指标历史

- 采集周期 15 秒，每节点保留 6 小时原始样本（约 1440 点，环形缓冲上限 2048）。
- 历史保存在 API 进程内，**重启即清空**，页面会通过 `retention_seconds` 与 `degraded` 显式说明。
- EWMA 时间常数 300 秒，节点高负载规则使用 EWMA，避免瞬时毛刺触发。
- 查询接口按窗口聚合：`1h`/`6h` 为 60 秒一个点，`24h` 为 300 秒一个点。

## 3. 检测规则

作用域写入 `incidents.namespace`，用于区分没有真实命名空间的资源：

| 作用域 | namespace | target |
|---|---|---|
| 节点 | `__node__` | 节点名 |
| 集群 | `__cluster__` | `cluster` |
| 租户 | `__tenant__` | 租户 ID |
| 工作负载 | 真实命名空间 | Pod 名 |

| 事件类型 | 触发条件 | 严重度 |
|---|---|---|
| `NODE_NOT_READY` | Node Ready=False | CRITICAL |
| `NODE_PRESSURE` | Memory/Disk/PIDPressure 任一为 True | WARN |
| `NODE_HIGH_USAGE` | EWMA CPU 或内存 / allocatable ≥ 0.9 | WARN |
| `METRICS_STALE` | 最新样本距今超过 90 秒 | WARN |
| `CRASH_LOOP` | Waiting=CrashLoopBackOff，或未就绪且重启次数 ≥ 3 | WARN；重启 ≥ 10 为 CRITICAL |
| `IMAGE_PULL_BACKOFF` | ErrImagePull / ImagePullBackOff / InvalidImageName | WARN |
| `OOM_KILLED` | 最近一次终止为 OOMKilled，且未就绪或发生在 30 分钟内 | CRITICAL |
| `POD_PENDING_LONG` | 未调度且创建超过 5 分钟 | WARN |
| `POD_READINESS_FAIL` | Running 且 Ready=False 超过 3 分钟 | WARN |
| `QUOTA_NEAR` | 租户 requested / quota ≥ 0.9 | WARN；≥ 1 为 CRITICAL |

### 生命周期

- 去重键：`type|namespace|target`，同一条件不会重复建事件。
- 严重度只升不降：WARN 事件在重启次数增加等情况下升级为 CRITICAL。
- 自动恢复：连续 2 次扫描（约 60 秒）未再命中，状态改为 `RESOLVED` 并写入 `resolved_at`。
- 事件创建时生成一次诊断；详情页可对未解决事件"重新诊断"。
- 诊断分为 `facts`（只可观测事实）与 `inferences`（根因假设，带置信度、证据与反证），不把推断写成事实。

## 4. 健康分

分数范围 0–100，返回 `components` 说明每一项扣分；数据不足时 `score=null`，不填 0。

**节点**

```
100
− 60（未就绪）
− 10 × 压力项，最多 −20
− 10（CPU 或内存 EWMA ≥ 90%）
− 15（指标缺失）
− 10（指标过期）
− 10 × CRITICAL 事件，最多 −20
− 5 × WARN 事件，最多 −10
```

**集群**：按节点 allocatable CPU 加权平均，再扣：

```
− 15 × CRITICAL 事件，最多 −30
− 5 × WARN 事件，最多 −15
− 10（指标过期）
```

**租户**

```
100
− 45（配额已用尽 ≥ 100%）
− 25（配额 ≥ 90%）
− 10（配额 ≥ 80%）
− 15（CRITICAL 事件）
− 5（WARN 事件）
− 10（主导份额 > 2 × 公平份额）
```

## 5. 内置 SLI 与 SLO

- `observe.Recorder` 只统计 `/api/v1/` 请求：`total` 为全部请求，`errors` 为 5xx；4xx 属于调用方错误，不计入可用性。
- 每 60 秒把上一分钟写入 `slo_windows`；没有请求的分钟写 `total=0` 的空桶，维持 `completeness`。
- 启动时默认确保 `arbiter-api` 的 AVAILABILITY 定义存在（目标 99.5%，窗口 1h），不覆盖已有定义；`ARBITER_SELF_SLO=off` 可关闭。
- SLO 页面在 `total_requests=0` 时显示"无请求"；`degraded` 或 `completeness` 不足时显示"数据积累中"，不会把 0 请求当成 0% 达成。
- 其他服务若要使用 SLO 页面，需要外部把分钟桶写入 `slo_windows`；当前没有通用指标写入通道。

## 6. API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/incidents` | 事件列表；支持 `status/type/severity/namespace/target/limit/cursor` |
| GET | `/api/v1/incidents/:id` | 事件详情与最新诊断 |
| POST | `/api/v1/diagnosis/run` | 对未解决事件重新诊断 |
| GET | `/api/v1/monitor/overview` | 集群/节点/租户概览与健康分 |
| GET | `/api/v1/monitor/series?window=1h` | 节点与集群短期指标序列 |
| GET | `/api/v1/slo` | SLO 列表（含 `total_requests`） |
| GET | `/api/v1/slo/:service/budget` | 预算、燃尽率与完整度 |

## 7. 配置与限制

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ARBITER_DETECT_INTERVAL` | `30s` | 检测扫描周期 |
| `ARBITER_SELF_SLO` | 开启 | 设为 `off` 关闭平台自身 SLO 定义与采集 |

- 指标历史与检测器的"连续未命中"状态都在进程内，重启后重新累积。
- 检测器按单副本设计；多副本会重复扫描。事件写入 PG 时依赖事件枚举迁移 `0003`。
- 阈值当前为代码常量，便于解释与测试；后续可做成策略配置。
- 检测器不读取 Kubernetes Events，也不做日志/链路分析；OOM、镜像拉取等判断来自 Pod 状态字段。
