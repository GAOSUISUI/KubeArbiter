# 负载预测与容量规划（P2）

本文描述预测能力的口径：负载外推算法、资源耗尽预警、配额调优建议（rightsizing）与前端"预测与容量"页。实现位于 `internal/forecast`、`internal/decision/metrics`、`internal/aiops` 与 `internal/api`。

## 1. 数据流

```
kubelet stats/summary ──15s──► metrics.Store（节点 6h 历史）
                      └──────► PodUsageStore（per-pod EWMA 用量）
forecast API ──1min 桶 + OLS──► /api/v1/forecast/capacity
aiops.Detector（30s）──外推──► NODE_FORECAST_EXHAUST 事件 + 健康分扣分
/api/v1/rightsizing ◄── request vs per-pod EWMA 用量对比
```

- 预测是**纯函数**：输入历史样本，输出外推与置信度，不引入任何数学库，OLS 为手写一元线性回归。
- 所有输出**仅作建议**：不自动变更配额、不自动触发任务、不影响调度决策。

## 2. 负载预测算法（`internal/forecast`）

- 下采样：节点历史按 1 分钟桶取均值，剔除空桶后做普通最小二乘（OLS）拟合。
- 每个维度（CPU 毫核、内存字节）独立拟合 `DimFit`：斜率（每分钟）、末值、R²、标准误。
- 置信分档：

| 档位 | 条件 |
|---|---|
| `NONE` | 有效桶 < 10 或样本跨度 < 10 分钟（数据不足，显式降级） |
| `HIGH` | R² ≥ 0.8 |
| `MEDIUM` | R² ≥ 0.6 |
| `LOW` | 其余 |

  完全平稳的序列（方差为 0）R² 视为 1——它是完美可预测的，不按退化公式判 0。
- `TimeToFull`：仅当斜率 > 0 且当前占用占比 ≥ 60% 时计算，输出触达 allocatable 90% / 100% 的秒数；否则为 null（不给编造的 ETA）。
- 预测视野固定白名单：`15m`、`30m`、`1h`、`3h`，输出最多 24 个等间隔预测点与 ±标准误置信带。

## 3. per-pod 用量（`PodUsageStore`）

- 采集器在节点汇总时逐 Pod 逐容器累加用量，写入独立的 per-pod EWMA（与节点 EWMA 相同时间常数 300 秒）。
- 超过 15 分钟未上报的 Pod 条目在写入时惰性回收；`Snapshot()` 供 rightsizing 使用。
- 用量缺失时 rightsizing 输出 `UNKNOWN`，不猜测。

## 4. 资源耗尽预警（`NODE_FORECAST_EXHAUST`）

检测器每轮扫描把节点预测摘要注入规则：

- 触发：外推 ETA ≤ 30 分钟触达 allocatable 90%，且 R² ≥ 0.6、当前占比 ≥ 0.6。
- 严重度 WARN；事件 Facts 含 `time_to_full_90_seconds`、`r2`、`usage_ratio`，Inferences 带反证说明。
- 健康分：命中时该节点单独扣 5 分（组件码 `FORECAST_EXHAUST`），**不并入**通用 WARN 事件扣分，避免双重扣分。
- 事件枚举迁移：已有数据库执行 `db/migrations/0004_forecast_type.sql`（幂等）；新库由 `schema.sql` 直接包含。

## 5. API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/forecast/capacity?horizon=30m` | 按节点/集群的负载外推：历史（1 分钟桶）、预测点、置信带、耗尽时间估计 |
| GET | `/api/v1/rightsizing` | request vs per-pod EWMA 用量对比与调优建议 |

`forecast/capacity` 响应中：`degraded=true` 表示指标或诊断不可用；`confidence` 取各节点最差档；`cluster` 为节点样本按桶求和的聚合外推（集群 allocatable 取 Ready 且可调度节点之和）。数据不足时节点 `prediction`/`band` 为空数组，不给编造数字。

### rightsizing 口径

| 分类 | 条件 | 建议 |
|---|---|---|
| `OVER_PROVISIONED` | CPU 与内存利用率同时 < 30% | 按 usage × 1.3 收缩 request |
| `UNDER_PROVISIONED` | CPU 与内存利用率同时 > 95% | 按 request × 1.5 上调 |
| `UNKNOWN` | 缺 per-pod 用量 | 不给建议，等 EWMA 积累 |
| `OK` | 其余 | 利用率在合理区间 |

无 request（或 request ≤ 0）的 Pod 直接跳过；建议按用量/request 比值给出，绝不自动修改配额。

## 6. 前端

"预测与容量"页（`/forecast`，快捷键 `g` 然后 `c`）：

- 视野切换（15m/30m/1h/3h）；集群与单节点切换查看历史实线 + 预测虚线 + 置信带阴影。
- 节点预测明细表：置信度、当前占用、R²、斜率、90%/100% 耗尽时间。
- rightsizing 表按分类筛选；`NODE_FORECAST_EXHAUST` 事件在异常事件页正常渲染。
- 30 秒自动刷新；数据不足时显示降级横幅，图表留空不编造。

## 7. 限制与后续

- 历史仍在 API 进程内（6 小时，重启清空），预测因此最多只能覆盖这个窗口的跨度；**7 天 PG 历史持久化明确推迟**，当前用 `degraded`/`confidence=NONE` 显式表达不足。
- OLS 只捕获线性趋势，对周期性负载（昼夜波动）会给出保守外推；置信带反映拟合误差而非未来不确定性。
- rightsizing 基于 EWMA 平滑用量，对突发型负载建议值偏紧；上表阈值是代码常量（`RightsizingOverRatio` 等），便于解释与测试。
- 预测仅建议不自动执行；如需接入"自动收缩 request"类任务，必须走现有任务审批链路，本阶段未实现。
