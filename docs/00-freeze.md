# KubeArbiter 方案冻结包（M0）

- 状态：**待评审**（未通过评审前不得进入 M1 编码）
- 冻结对象：范围、模块边界、依赖白名单、契约来源、版本策略、安全护栏
- 上游依据：`KubeArbiter项目说明书.md` v2.0
- 冲突处理：本文件与说明书冲突时以说明书为准，并回改本文件；不得由代码"暗中"扩大范围

---

## 1. 冻结内容与不冻结内容

| 项目 | 是否冻结 | 说明 |
|---|---|---|
| 功能范围（做什么 / 不做什么） | 冻结 | 见第 3 节与说明书第 3 节 |
| 模块边界与依赖方向 | 冻结 | 见第 5 节，`schedcore` 禁止 import client-go |
| 对外契约（HTTP / DB / 错误码） | 冻结 | 见 `api/openapi.yaml`、`db/schema.sql`、`docs/01-algorithm-spec.md` 第 10 节 |
| 调度算法公式与权重预设 | 冻结 | 见 `docs/01-algorithm-spec.md`，改动需走 ADR |
| 具体依赖的补丁版本 | 不冻结 | 按第 4 节策略在 M1 安装后回填实际版本 |
| 前端视觉细节 | 不冻结 | 只冻结"7 个页面 / 每页回答一个问题"的结构 |
| 实验数值结论 | 不冻结 | 必须实测得出，禁止预先填写 |

---

## 2. 运行环境基线（实测，仅只读核查，未安装任何组件）

| 项 | 实测结果 | 影响 |
|---|---|---|
| CPU | i7-12700K，12 核 20 线程 | 可支撑 3 节点集群 + 压测器 |
| 内存 | 63.7 GB | 控制面 10G、工作节点各 18G 后仍有富余 |
| 磁盘 | Disk0 = 希捷 ST4000NM000B 4TB SATA **HDD**（D:，3726GB）；Disk1 = 联想 2TB **NVMe 4.0**（C: 500GB / 可用 358.5GB，E: 1407GB / 可用约 1.3TB） | 三台 VM 放到 E（与 C 同属一块 NVMe，空间充足且不占用系统盘）；**禁止放 D**（机械盘，etcd fsync 抖动会直接污染实验数据） |
| 虚拟化 | HypervisorPresent=True（Hyper-V 已启用） | 与 VMware 共存会带来额外调度开销，需用户决定是否关闭 |
| VMware Workstation | 25.0.1 已安装 | 作为 3 节点集群载体 |
| 已装工具 | VS Code（E:\VSCode）、Git 2.56.1、Go 1.27.4（`C:\Program Files\Go`）、mingw-w64 / gcc 16.2.0（WinLibs，winget 安装） | Docker / kubectl / helm **均未安装**，M1 需用户批准后安装；gcc 用于 `CGO_ENABLED=1 go test -race` |
| 集群 | 不存在 | 无任何 Kubernetes 资源，无遗留状态 |

> 说明：硬件与磁盘探测为只读结果；软件安装均经用户逐项批准（Git、Go、mingw-w64）。未创建任何虚拟机，未修改集群。若当前 shell 的 PATH 尚未刷新，运行竞态检测需显式前置 gcc 目录：
>
> ```powershell
> $env:PATH = 'C:\Users\Gracie\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin;' + $env:PATH
> $env:CGO_ENABLED = '1'; go test -race ./pkg/schedcore/
> ```

---

## 3. 范围红线（复述，具备否决权）

**不做**：Scheduler Framework 插件与 kube-scheduler 改造、CNI / Service 转发 / 网络底层诊断、eBPF、内核参数与 cgroup 调优、容器运行时改造、CSI 开发、真实多集群纳管、GPU 调度、Istio。

**判定标准**：任何需求若需要读内核代码、改发行版组件、或依赖特定硬件，直接进入 backlog，不进入本期。

**落地方式（唯一允许的调度影响手段）**：Mutating Admission Webhook 向 Pod 注入 `nodeAffinity`。

| 约束项 | 取值 | 理由 |
|---|---|---|
| `failurePolicy` | `Ignore` | 注入失败不得影响业务 Pod 创建 |
| `namespaceSelector` | 仅 `arbiter.io/managed=true` | 默认不接管任何命名空间 |
| `timeoutSeconds` | `3` | 超时即放行 |
| `sideEffects` | `None` | 允许 dry-run |
| 注入内容 | 只写 `spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`，且仅当 Pod 无同名约束 | 不覆盖用户已有约束 |

---

## 4. 版本策略（M0 只锁策略，M1 回填实际版本）

| 组件 | 版本策略 | 锁定时机 | 校验方式 |
|---|---|---|---|
| Go | 官方当前稳定 minor，最低不低于 1.23；只允许该 minor 的最新 patch | 已回填 `go version` = **go1.27.4 windows/amd64** | `go.mod` 的 `go` 指令与 CI 一致 |
| Kubernetes | kubeadm 官方源当前稳定 minor；三节点同版本 | M1 建集群时回填 `kubectl version` | 节点版本一致 |
| client-go | 与集群 minor 偏差不超过 ±1 | M1 | `go list -m k8s.io/client-go` |
| PostgreSQL | 16.x | M1 用容器启动并记录镜像 digest | `select version()` |
| ingress-nginx | 与集群兼容的当前稳定版 | M2 前 | `helm list -n ingress-nginx` |
| Helm | 3.x 稳定版 | M1 | `helm version` |
| 前端 | Node LTS + Vue 3 + TS + ECharts 5 | M5 前 | `package.json` 锁版本 + lockfile |
| 压测器 | k6 或 wrk（择一，M2 定） | M2 | 脚本入库，版本写入实验报告 |

**规则**：所有版本在 `deploy/versions.lock.md` 中回填"实际安装版本 + 安装日期 + 校验命令输出"，不得凭记忆填写。

---

## 5. 仓库布局与模块边界

```
kubearbiter/
├── cmd/
│   ├── arbiter-api/          # Gin HTTP 服务入口
│   ├── arbiter-worker/       # 任务执行器（领取 tasks 表任务）
│   └── arbiter-webhook/      # Admission Webhook 入口
├── internal/
│   ├── decision/
│   │   ├── capacity/         # 资源视图、节点画像、租户画像
│   │   ├── placement/        # 过滤 → 评分 → 策略选择
│   │   ├── fairness/         # FIFO / 硬配额 / 加权 DRF / 抢占代价
│   │   ├── slo/              # 错误预算、多窗口燃尽率
│   │   ├── scaling/          # 容量曲线、Little 定律、MAPE
│   │   └── release/          # 灰度状态机与判定
│   ├── actuator/             # 集群写操作（dry-run 前置 + 回退动作）
│   ├── webhook/              # 注入逻辑（只调用 schedcore 与只读缓存）
│   ├── taskengine/           # 状态机、幂等、重试、审批
│   ├── store/                # PostgreSQL 访问（sqlc 或手写 DAO，M1 定）
│   ├── api/                  # handler、DTO、中间件（认证/审计/幂等）
│   └── observe/              # Prometheus 客户端、slog 配置、trace_id
├── pkg/
│   └── schedcore/            # 纯函数决策核：只允许 import 标准库
├── db/                       # schema.sql、迁移脚本、种子数据
├── api/                      # openapi.yaml（唯一契约来源）
├── web/                      # Vue 3 + TS 前端
├── deploy/helm/kubearbiter/  # Chart（含 webhook 证书与 RBAC）
├── docs/                     # 本冻结包、ADR、实验报告
└── test/{e2e,load}/          # 端到端脚本与压测脚本
```

**依赖方向（硬规则，违反即打回）**

| 规则 | 检测方式 |
|---|---|
| `pkg/schedcore` 只 import 标准库与 `pkg/schedcore/*` | `go list -deps ./pkg/schedcore \| grep -v '^internal/'` 断言无第三方模块；另加单测断言 |
| `internal/decision/*` 不得 import `internal/actuator`、`internal/api` | 同上，用 `go list` 断言 |
| `internal/actuator` 不得写业务表，只能写任务日志与审计 | code review + 单测 |
| `web/` 不出现硬编码业务数字 | review 时 grep 常量数组 |

**为什么这样切**：决策核是纯函数（输入快照 → 输出决策），因此可以离线跑仿真、可以表驱动单测、可以在没有集群的机器上验证。这是本项目可复现性的技术基础。

---

## 6. 依赖白名单（M2 后冻结，新增需 ADR）

| 依赖 | 用途 | 引入阶段 |
|---|---|---|
| `github.com/gin-gonic/gin` | HTTP 框架 | M1 |
| `k8s.io/client-go` | 只读 informer + 受控写操作 + dry-run | M1 |
| `k8s.io/api`、`k8s.io/apimachinery` | 类型定义（版本随 client-go） | M1 |
| `github.com/jackc/pgx/v5` | PostgreSQL 驱动与连接池 | M1 |
| `github.com/prometheus/client_golang` | 指标查询（Prometheus HTTP API）与自身指标暴露 | M1 |
| `github.com/golang-jwt/jwt/v5` | JWT 签发与校验 | M1 |
| `helm.sh/helm/v3` | 发布回滚（仅 SDK 的 release 操作） | M5 |
| `github.com/google/uuid` | 主键与幂等键 | M1 |
| `stretchr/testify`（仅测试） | 断言 | M1 |
| `Python 3.13`（离线分析工具，非服务运行时依赖） | E1–E7 原始样本的统计与置信区间（`scripts/analysis/`，仅标准库） | M1 |
| `k6`（外部二进制，非 Go 依赖） | 压测 | M2 |

**禁止引入**：任何 ORM（gorm/ent）、任何 Scheduler Framework 相关包、Istio SDK、eBPF 库、Loki 客户端、消息队列（Kafka/RabbitMQ/Redis 亦不引入，任务队列用 PG）。

---

## 7. 功能优先级（M0 需用户确认的三级划分）

| 级别 | 内容 | 判定 |
|---|---|---|
| **P0** | 资源视图与租户画像（只读全量）；放置引擎 4 策略 + 仿真器 + Webhook 注入；公平仲裁 3 分配器 + 抢占建议；SLO 错误预算 + 多窗口燃尽率 + 预聚合；任务引擎（幂等 / 审批 / 审计）；前端 5 页（容量总览、租户与公平性、放置决策台、SLO、发布中心） | 缺任一则项目不成立 |
| **P1** | 渐进式发布 + 自动回滚；容量规划模型与误差报告；诊断 7 类；E1–E7 实验与报告；前端事件与任务页 | M5 / M6 退出标准要求 |
| **P2** | 前端策略管理页；非 HTTP 副本比例灰度；ConfigMap 回滚；多集群抽象接口 | 可延后，M2 后只进 backlog |

**范围冻结规则**：M2 之后不再新增功能模块，只做修复与打磨。

---

## 8. 安全护栏（不可协商）

| 编号 | 护栏 | 落地方式 |
|---|---|---|
| G1 | Webhook 默认不接管任何命名空间 | `namespaceSelector` + `failurePolicy: Ignore` + 3s 超时 |
| G2 | 所有写操作先 dry-run | `actuator` 统一入口强制两步：`DryRun=All` 成功才正式提交 |
| G3 | 危险操作必须人工审批 | `tasks.status` 先置 `PENDING_APPROVAL`，`POST /tasks/{id}/approve` 才入队 |
| G4 | 自动执行只允许作用于测试工作负载 | 仅 `arbiter.io/managed=true` 且带 `arbiter.io/test=true` 的对象可跳过审批 |
| G5 | RBAC 最小化 | 读全量；写仅限 `deployments/scale`、`deployments` patch、`statefulsets/scale`、`configmaps` patch；Secret 只读且不回显 |
| G6 | 审计只追加 | `audit_logs` 上建触发器，拒绝 UPDATE / DELETE |
| G7 | Secret 不明文外泄 | 接口只返回键名与内容 sha256，永不返回 value |
| G8 | 所有外部调用带超时与 context | code review 检查裸 `http.Get`、无超时 client-go 调用 |
| G9 | 指标过期不得静默使用 | 指标年龄 > 60s 时 `s_load` 权重归零并置 `degraded=true` |

---

## 9. M0 退出检查清单

| # | 检查项 | 状态 |
|---|---|---|
| 1 | 说明书 v2.0 已评审（范围红线、7 组实验、M0–M6 门禁） | 待用户确认 |
| 2 | P0 / P1 / P2 划分已确认（第 7 节） | 待用户确认 |
| 3 | 版本策略已确认（第 4 节，M1 回填实际值） | 待用户确认 |
| 4 | 仓库布局与依赖方向已确认（第 5 节） | 本文档已给出 |
| 5 | 算法公式与权重预设已冻结（`docs/01-algorithm-spec.md`） | 本文档已给出 |
| 6 | 数据库 DDL 已冻结（`db/schema.sql`） | 本文档已给出 |
| 7 | HTTP 契约已冻结（`api/openapi.yaml`） | 本文档已给出 |
| 8 | 安全护栏已确认（第 8 节） | 待用户确认 |
| 9 | 环境基线已记录（第 2 节） | 已实测 |

### 9.1 仍需用户拍板的环境决策（阻塞 M1）

| # | 决策 | 选项 | 影响 |
|---|---|---|---|
| D1 | 是否关闭 Hyper-V 以换取原生虚拟化性能 | 关闭 / 保持开启 | 保持开启时压测数据抖动更大，E1/E4/E7 需增加重复次数 |
| D2 | VM 磁盘位置 | 建议 E 盘（C/E 同属 NVMe，且不占用系统盘） | etcd fsync 延迟直接影响 API Server 稳定性 |
| D3 | 是否允许额外创建一台一次性 VM（单节点 kubeadm k8s，与主集群同版本） | 允许 / 不允许 | 用于 E5 破坏性发布实验，避免污染主集群；E5 期间需与主集群错峰（18 vCPU / 20 线程） |
| D4 | 是否接受"仅 Score 级调度影响 + 专用测试负载"的降级方案 | 接受 / 不接受 | 不接受则需回到 Scheduler Framework 插件路线，与范围红线冲突 |

### 9.2 M1 需要的安装授权（需用户逐项批准，本阶段不执行）

`git`、`Go`、`Docker Desktop 或 Docker Engine`、`kubectl`、`helm`、`kubeadm/kubelet`（VM 内）、`PostgreSQL 16`（容器）、`Node.js LTS`。安装清单与命令将在 M1 开始前单独提交审批。

---

## 10. 变更控制

1. 任何触及第 1 节"冻结"项的改动，必须先写 ADR（`docs/adr/NNNN-title.md`），记录背景、选项、决策、后果。
2. 契约变更顺序固定：`api/openapi.yaml` → `db/schema.sql` → 代码 → 前端类型。禁止代码先行。
3. 新增第三方依赖需在本文件第 6 节登记，并在 ADR 中说明为何标准库不足。
4. 每个里程碑结束：打 Git tag（`m0-freeze`、`m1-env` …）+ VM 快照。
