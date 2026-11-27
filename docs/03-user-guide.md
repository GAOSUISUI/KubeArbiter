# KubeArbiter 功能与使用指南

本文面向第一次接触项目的使用者：先说明它解决什么问题、由哪些部分组成，再给出控制台操作路径、本地运行与集群部署方法、常用 API 和排障入口。

- 面向评审/答辩的完整规格见 [KubeArbiter项目说明书.md](../KubeArbiter项目说明书.md)
- 算法公式、原因码与评分的唯一来源见 [01-algorithm-spec.md](01-algorithm-spec.md)
- 构建、镜像导入与回滚的逐步命令见 [02-release-runbook.md](02-release-runbook.md)

---

## 1. 项目是什么

KubeArbiter 是一个**面向 Kubernetes 的多租户容量仲裁与放置决策平台**。它不替换 kube-scheduler，而是：

1. 只读采集节点、Pod 与 kubelet 指标，形成集群快照；
2. 对候选节点做硬过滤 + 七维加权评分，给出"放哪、为什么"的可解释决策；
3. 把决策转成受控任务，经人工审批后由 worker 先 dry-run、再向 Deployment/StatefulSet 注入 `nodeAffinity`；
4. 全过程落库并写审计，前端控制台可回放、对比和导出。

**能力边界**

| 做 | 不做 |
|---|---|
| 只读全量集群视图、指标采集与降级 | 改造 kube-scheduler / Scheduler Framework 插件 |
| 放置模拟、决策记录、策略权重管理 | CNI、Service 转发、网络底层诊断 |
| 任务审批、dry-run、受控写与审计 | eBPF、内核参数与 cgroup 调优 |
| 公平性指标与配额推演 | GPU 调度、多集群纳管、Istio |
| Mutating Webhook 注入 nodeAffinity | 自动跳过人工审批执行危险操作 |

---

## 2. 系统组成

| 组件 | 作用 | 入口 |
|---|---|---|
| `arbiter-api` | Gin HTTP 服务、决策核、Webhook、内嵌控制台 | `:8080`，Webhook `:9443` |
| `arbiter-worker` | 领取已审批任务，dry-run 后写集群，回写状态与审计 | 无 HTTP 业务端口，指标 `:9101` |
| Web 控制台 | Vue 3 + ECharts，构建后嵌入 `arbiter-api` 二进制 | 浏览器访问 `http://<api>/` |
| PostgreSQL 16 | 策略、决策、任务、审计、SLO 持久化 | 集群内 `postgres.kubearbiter.svc:5432` |
| `arbiter-bench` | 离线策略对比基准，生成 Markdown/JSON 报告 | 命令行 |
| `cmd/arbiter-image` | 离线拼装单层 OCI 镜像，不依赖 Docker | 命令行 |

**一次完整数据流**

```
informer / kubelet 指标
        │
        ▼
集群快照 ──► 放置模拟（过滤 + 评分）──► decisions 表
                                          │
                                    提交执行（apply）
                                          ▼
                                    tasks（待审批）
                                          │ 人工审批
                                          ▼
                            worker：dry-run ─► patch ─► 结果/日志
                                          │
                                          ▼
                                     audit_logs
```

没有 PostgreSQL 时，`arbiter-api` 自动退化为进程内存储，适合本地体验；重启后数据清空。

---

## 3. 功能清单

### 3.1 容量与租户视图

- 节点维度：`allocatable`、`requested`、EWMA 实际用量、Pod 数、Ready 状态、拓扑标签。
- 租户维度：配额（CPU/内存）、已请求量、主导份额、Jain 公平指数。
- 指标新鲜度：kubelet 指标超过阈值未更新时标记为缺失，决策降级并给出 `METRICS_MISSING` 一类警告，而不是静默使用旧数据。

对应接口：`/api/v1/capacity/overview`、`/api/v1/capacity/nodes`、`/api/v1/capacity/tenants`。

### 3.2 放置决策

- **硬过滤**：节点 Ready、不可调度、CPU/内存余量、Taint/Toleration、nodeSelector、nodeAffinity、拓扑分布、hostPort、租户配额。被淘汰的节点会返回具体原因码与证据，不会只给一句"无可用节点"。
- **七维评分**：CPU 余量、内存余量、均衡、装箱、实际负载、公平性、碎片，归一化后按策略权重加权为 0–100 分。
- **策略类型**：`BALANCED`、`BIN_PACK`、`FAIR_SHARE`、`LOAD_AWARE`。
- **公平模式**：`WEIGHTED_SHARE`（按基线份额计算）与 `DRF`（按节点级主导份额试算）。
- **可解释输出**：候选排名、每维得分与贡献、过滤原因、耗时、快照哈希、是否降级。
- **快照校验**：决策落库时记录快照哈希；集群变化后原决策不能直接执行，必须重新模拟（返回 `DECISION_STALE`）。
- 每次模拟都会写入一条决策记录，可用于回放与对比。

对应接口：`POST /api/v1/placement/simulate`、`GET /api/v1/placement/decisions`。

### 3.3 策略管理

策略就是一组七维权重，总和保存时自动归一化到 100%。可新建、编辑、删除、设为默认，并选择策略类型与公平模式。控制台支持用色块直观看到权重分布。

对应接口：`/api/v1/policies` 的增删改查。

### 3.4 公平性分析

- Jain 公平指数与租户配额/实际占用对比（CPU、内存可切换）。
- **配额推演**：在本地按容量比例线性叠加主导份额，预估"再给某租户追加资源"后的份额与 Jain 指数变化，不写入任何数据。
- **决策对比**：任选两条决策，逐字段比较榜首节点、过滤数量、耗时，并用雷达图对比七维得分。
- 决策历史支持按选中节点分布、决策耗时查看散点。

### 3.5 任务审批与审计

- 决策"提交执行"后生成 `PLACEMENT_APPLY` 任务，初始状态为 `PENDING_APPROVAL`。
- 审批人不能是任务创建人（`admin` 例外）；`allow_auto_execute` 一律拒绝，必须人工审批。
- 支持单条/批量审批、取消（仅 `PENDING`、`PENDING_APPROVAL`、`QUEUED` 可取消）、任务详情与日志时间线、复制任务 ID。
- 任务状态：`PENDING → PENDING_APPROVAL → QUEUED → RUNNING → SUCCEEDED / FAILED / RETRYING / CANCELLED`。
- 审计日志只追加，可按动作、对象、操作者筛选，并导出 CSV。
- 当前端到端闭环的任务类型是 `PLACEMENT_APPLY`；其余类型（`SCALE`、`PREEMPT`、`RELEASE_PROMOTE` 等）已在契约和状态机中定义，尚未接入执行器。

对应接口：`/api/v1/tasks`、`/api/v1/tasks/:id/approve`、`/api/v1/tasks/:id/cancel`、`/api/v1/audit-logs`。

### 3.6 Admission Webhook

- 只处理带 `arbiter.io/managed=true` 命名空间的 Pod `CREATE`。
- 只注入 `spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution`，且不覆盖用户已有的同名约束。
- `failurePolicy: Ignore`、超时 5s，任何解析或内部错误都放行，绝不阻断业务发布。
- 注入的 Pod 会带 `arbiter.io/decision-id`、`arbiter.io/tenant` 注解，便于反查。

对应接口：`POST /webhook/mutate-pods`（HTTPS）。

### 3.7 离线基准与 SLO

- `arbiter-bench` 在固定随机种子下对比多种策略，输出控制台条形图、Markdown 与可选 JSON 报告，用于复现实验数据。
- 控制台的"SLO 治理"页支持 SLO 定义增删改查、当前达成、错误预算、三档燃尽率与数据完整度；平台内置采集 `arbiter-api` 自身的可用性，其他服务需外部写入分钟桶。
- 数据不足时页面显示"数据积累中"，不会用 0 值代替；`total_requests=0` 显示"无请求"。

---

## 4. 控制台使用指南

### 4.1 通用操作

| 位置 | 功能 |
|---|---|
| 左侧栏 | 八个页面导航；切换身份（`X-Actor`）；修改 API 地址 |
| 顶部栏 | 当前页面标题、命令面板、API 健康状态（20s 轮询）、手动刷新 |
| `Ctrl/⌘ + K` | 命令面板：搜索页面、任务、切换身份 |
| `?` | 快捷键帮助 |
| `g` 然后 `s/f/p/n/t/m/e/l` | 快速跳转模拟/公平性/策略/节点/任务/监控/事件/SLO 页 |
| `Ctrl/⌘ + Enter` | 在模拟工作台立即运行一次模拟 |

身份仅用于审计与审批规则，不是认证。请求头为 `X-Actor`，默认 `anonymous`；审批人与创建人相同时会被拒绝（`admin` 除外）。

### 4.2 页面说明

| 页面 | 回答的问题 | 关键操作 |
|---|---|---|
| 模拟工作台 | 这个负载放哪、为什么 | 选模板或填 workload → 选租户/策略 → 拖权重 → 运行模拟；查看排名、贡献、雷达、过滤原因；提交执行 |
| 公平性与历史 | 分配合不合理 | 查看 Jain 与配额对比；做配额推演；选两条决策对比；查看历史与耗时散点 |
| 策略管理 | 换权重会怎样 | 新建/编辑策略、切换策略类型与公平模式、设默认、删除 |
| 节点与容量 | 集群还剩多少、谁在压 | 节点卡片、压力气泡图、单节点 CPU/内存趋势、自动刷新 |
| 任务与审计 | 系统做了什么、谁批的 | 筛选任务、批量审批/取消、看详情与日志、导出审计 CSV |
| 监控大盘 | 集群现在健康吗、资源用在哪 | 节点就绪/CPU/内存/事件 KPI、集群趋势、健康分、节点与租户表；30s 自动刷新 |
| 异常事件 | 出了什么问题、为什么 | 按状态/类型/严重度筛选；查看观测事实与诊断推断；对未解决事件重新诊断 |
| SLO 治理 | 服务水平达没达标 | 查看/新建/编辑/删除 SLO；看当前达成、预算余量、燃尽窗口与完整度 |

模拟工作台还提供：场景模板（Web 服务、批处理、缓存、Sidecar）、权重预设（均衡/装箱/负载感知/公平优先）、最近 8 条决策回看、与基线决策的逐节点分数差、以及"把当前权重另存为策略"。

### 4.3 推荐演示流程

1. 打开"节点与容量"，确认三节点容量与指标新鲜度。
2. 到"模拟工作台"选"Web 服务"模板，运行一次，说明选中节点与各维贡献。
3. 切换"装箱优先"/"公平优先"预设，观察选中节点与分数变化。
4. 把当前权重保存为新策略，到"策略管理"确认。
5. 回到模拟结果点"提交执行"，到"任务与审计"用另一个身份审批。
6. 观察任务状态与日志，最后在审计列表确认创建、审批、执行记录。
7. 打开"监控大盘"，说明节点健康分、资源趋势与开放事件；指标降级时会显示原因。
8. 打开"异常事件"，查看观测事实与诊断推断，并演示"重新诊断"。
9. 打开"SLO 治理"，说明 `arbiter-api` 的可用性 SLO 与"数据积累中"的含义。

---

## 5. 快速开始

### 5.1 环境要求

| 依赖 | 版本/说明 |
|---|---|
| Go | 1.26 及以上（以 `go.mod` 为准） |
| Node.js | 22 LTS（仅前端构建需要） |
| kubectl | 与集群版本匹配，默认分支为 v1.37.2 |
| Kubernetes | 可选；没有集群时 API 可启动，但模拟/容量接口返回不可用 |
| PostgreSQL | 可选；不配置 DSN 时使用内存存储 |

### 5.2 本地运行

后端（默认 `:8080`，自动读取 `KUBECONFIG` / `~/.kube/config`）：

```powershell
cd E:\KubeArbiter
go run ./cmd/arbiter-api
```

前端开发服务器（`http://127.0.0.1:5173`，`/api` 代理到 `:8080`）：

```powershell
cd E:\KubeArbiter\web
npm ci
npm run dev
```

打开 `http://127.0.0.1:5173/#/simulator`。若要连接其他环境的 API，可在左下角"修改"里填写完整地址。

如需持久化并验证任务执行，再启动 worker（必须与 API 共用同一个 PostgreSQL）：

```powershell
$env:ARBITER_PG_DSN = 'postgres://kubearbiter@127.0.0.1:5432/kubearbiter?sslmode=disable'
go run ./cmd/arbiter-worker
```

### 5.3 构建

```powershell
$env:PATH = 'C:\Program Files\Go\bin;C:\Users\Gracie\.trae-cn\work\nodejs;' + $env:PATH
cd E:\KubeArbiter
make image VERSION=0.9.4
```

产物：

- `bin/arbiter-api-linux`：Linux amd64 静态二进制，已内嵌前端；
- `bin/arbiter-image-0.9.4.tar`：单层 OCI 归档，可在节点直接 `ctr import`。

手动步骤与参数说明见 [02-release-runbook.md](02-release-runbook.md)。

### 5.4 部署到集群

```powershell
$KUBECTL = "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\Kubernetes.kubectl_Microsoft.Winget.Source_8wekyb3d8bbwe\kubectl.exe"

# 1) PostgreSQL（schema 只需首次执行；PowerShell 不支持 "<" 重定向，用管道送入）
& $KUBECTL apply -f deploy/postgres/postgres.yaml
& $KUBECTL -n kubearbiter wait --for=condition=Ready pod/postgres-0 --timeout=120s
Get-Content db\schema.sql -Raw | & $KUBECTL -n kubearbiter exec -i postgres-0 -- psql -U kubearbiter -d kubearbiter
# 已有数据库升级时，再执行一次事件类型迁移
Get-Content db\migrations\0003_aiops_incident_types.sql -Raw | & $KUBECTL -n kubearbiter exec -i postgres-0 -- psql -U kubearbiter -d kubearbiter

# 2) 导入镜像（仅控制平面节点 ka-cp1）
scp -i C:\Users\Gracie\.ssh\ka-cp1 bin\arbiter-image-0.9.4.tar arbiter@192.168.49.11:/tmp/
ssh -i C:\Users\Gracie\.ssh\ka-cp1 arbiter@192.168.49.11 `
  "sudo ctr -n k8s.io images import /tmp/arbiter-image-0.9.4.tar"

# 3) 应用清单与 Webhook 证书
& $KUBECTL apply -f deploy/arbiter/arbiter.yaml
# 在 ka-cp1 上执行，生成证书、创建 Secret 并回填 caBundle
scp -i C:\Users\Gracie\.ssh\ka-cp1 deploy\arbiter\gen-certs.sh arbiter@192.168.49.11:/tmp/
ssh -i C:\Users\Gracie\.ssh\ka-cp1 arbiter@192.168.49.11 "bash /tmp/gen-certs.sh"

# 4) worker
& $KUBECTL apply -f deploy/arbiter/worker.yaml
& $KUBECTL -n kubearbiter rollout status deploy/arbiter-api
& $KUBECTL -n kubearbiter rollout status deploy/arbiter-worker
```

注意：两个 Deployment 目前都用 `nodeSelector` 固定在控制平面节点，镜像也只在 ka-cp1 导入；迁移到生产时应改为工作节点并配置反亲和。

### 5.5 验证

```powershell
& $KUBECTL -n kubearbiter port-forward svc/arbiter-webhook 8080:8080
curl.exe http://127.0.0.1:8080/healthz
```

浏览器打开 `http://127.0.0.1:8080/`。`/healthz` 返回 `{"status":"ok"}`；若依赖异常会列出失败的检查项（`k8s`、`postgres`）。

---

## 6. API 速查

统一约定：

- 成功：`{"code":0,"data":...,"trace_id":"..."}`
- 失败：`{"code":40001,"message":"...","details":[...],"trace_id":"..."}`
- 列表使用 `?cursor=&limit=`，返回 `next_cursor`
- 写请求支持 `Idempotency-Key`；同键同体返回原结果，同键异体返回 `40901`
- 身份通过 `X-Actor` 传递

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查与依赖状态 |
| GET/POST | `/api/v1/policies` | 策略列表 / 新建 |
| PUT/DELETE | `/api/v1/policies/:id` | 更新 / 删除策略 |
| GET | `/api/v1/capacity/overview` | 集群容量总览与 Jain 指数 |
| GET | `/api/v1/capacity/nodes` | 节点列表与指标新鲜度 |
| GET | `/api/v1/capacity/tenants` | 租户配额与占用 |
| POST | `/api/v1/placement/simulate` | 放置模拟并落库 |
| GET | `/api/v1/placement/decisions` | 决策历史 |
| GET | `/api/v1/placement/decisions/:id` | 单条决策详情 |
| POST | `/api/v1/placement/apply` | 决策转任务（需审批） |
| GET | `/api/v1/tasks` | 任务列表 |
| GET | `/api/v1/tasks/:id` | 任务详情与日志 |
| POST | `/api/v1/tasks/:id/approve` | 审批任务 |
| POST | `/api/v1/tasks/:id/cancel` | 取消任务 |
| GET | `/api/v1/audit-logs` | 审计日志 |
| GET | `/api/v1/incidents` | 异常事件列表（可按状态/类型/严重度/范围筛选） |
| GET | `/api/v1/incidents/:id` | 事件详情与最新诊断 |
| POST | `/api/v1/diagnosis/run` | 对未解决事件重新诊断 |
| GET | `/api/v1/monitor/overview` | 集群/节点/租户概览与健康分 |
| GET | `/api/v1/monitor/series?window=1h` | 节点与集群短期指标序列 |
| GET/POST | `/api/v1/slo` | SLO 定义列表 / 新建 |
| PUT/DELETE | `/api/v1/slo/:service/:sliType` | 更新 / 删除 SLO 定义 |
| GET | `/api/v1/slo/:service` | SLO 详情与燃尽窗口 |
| GET | `/api/v1/slo/:service/budget` | 错误预算与燃尽率 |

示例：

```bash
# 模拟一次放置
curl -s http://127.0.0.1:8080/api/v1/placement/simulate \
  -H 'Content-Type: application/json' \
  -H 'X-Actor: alice' \
  -d '{
    "workload": {"name":"web-api","namespace":"team-a","kind":"Deployment",
                 "replicas":3,"requests":{"cpu_milli":500,"mem_bytes":536870912}},
    "tenant_id": "team-a",
    "policy_id": "<策略 UUID>",
    "weights": {"s_cpu":0.18,"s_mem":0.18,"s_balance":0.25,"s_pack":0.10,
                "s_load":0.15,"s_fair":0.10,"s_frag":0.04}
  }'

# 把决策转成待审批任务
curl -s http://127.0.0.1:8080/api/v1/placement/apply \
  -H 'Content-Type: application/json' -H 'X-Actor: alice' \
  -H 'Idempotency-Key: demo-apply-1' \
  -d '{"decision_id":"<决策 UUID>"}'

# 用另一个身份审批
curl -s -X POST http://127.0.0.1:8080/api/v1/tasks/<任务 UUID>/approve \
  -H 'X-Actor: bob'
```

完整字段与枚举以 [api/openapi.yaml](../api/openapi.yaml) 为准。

---

## 7. 配置项

### arbiter-api

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ARBITER_ADDR` | `:8080` | HTTP 监听地址 |
| `ARBITER_PG_DSN` | 空 | PostgreSQL DSN；为空时使用内存存储 |
| `ARBITER_TLS_CERT` / `ARBITER_TLS_KEY` | 空 | 同时提供才启动 Webhook TLS 服务 |
| `ARBITER_WEBHOOK_ADDR` | `:9443` | Webhook 监听地址 |
| `ARBITER_DETECT_INTERVAL` | `30s` | 故障检测扫描周期 |
| `ARBITER_SELF_SLO` | 开启 | 设为 `off` 关闭平台自身 SLO 定义与采集 |

### arbiter-worker

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ARBITER_PG_DSN` | 必填 | 必须与 API 指向同一数据库 |
| `ARBITER_WORKER_ID` | 主机名-PID | 写入审计的操作者标识（最长 63 字符） |
| `ARBITER_WORKER_CONCURRENCY` | `2` | 并发执行任务数 |
| `ARBITER_WORKER_POLL_INTERVAL` | `2s` | 轮询任务间隔 |
| `ARBITER_WORKER_REAP_INTERVAL` | `30s` | 回收过期租约间隔 |
| `ARBITER_WORKER_LEASE_GRACE` | `30s` | 租约宽限期 |
| `ARBITER_WORKER_METRICS_ADDR` | `:9101` | Prometheus 指标监听地址 |

---

## 8. 已知限制与注意事项

1. **认证尚未落地**：`X-Actor` 只是审计标识，可被伪造；`admin` 为固定名称。生产使用前必须接入真实认证与 RBAC。
2. **单副本/固定节点**：api 与 worker 当前固定在控制平面节点，镜像只在 ka-cp1 本地，不具备高可用。
3. **执行范围有限**：worker 只会给 Deployment/StatefulSet 注入 `nodeAffinity`；其他任务类型尚未接入执行器。
4. **SLO 数据来源有限**：平台只自动采集 `arbiter-api` 自身可用性；其他服务的分钟桶需要外部写入，否则显示"数据积累中"。
5. **指标依赖 kubelet**：指标缺失会降级而不是阻塞决策；页面会显示"缺失/降级"。
6. **前端需先构建**：仓库只保留 `internal/webui/dist/.gitkeep`，未构建前端时 `go build` 仍可编译，但控制台返回纯 API。
7. **快照时效**：集群变化后旧决策不能执行，必须重新模拟。
8. **Webhook 证书**：`gen-certs.sh` 生成自签证书并在集群内轮换，私钥不入库。
9. **指标历史在进程内**：默认保留 6 小时，API 重启后清空；需要长期历史时应接 Prometheus。
10. **事件类型迁移**：部署到已有数据库前需执行 `db/migrations/0003_aiops_incident_types.sql`，否则新类型事件写入会被 CHECK 拒绝。

---

## 9. 排障

| 现象 | 排查方向 |
|---|---|
| 页面顶部"API 不可达" | 后端是否启动、port-forward 是否存活、左下角 API 地址是否正确 |
| 模拟返回 503 | 没有可用 kubeconfig 或 informer 尚未完成首次同步 |
| 提交执行报"决策已过期" | 集群快照变化，重新模拟后再提交 |
| 审批报错 | 审批人与创建人相同，换一个 `X-Actor`（`admin` 除外） |
| 任务一直 QUEUED | worker 是否运行、`ARBITER_PG_DSN` 是否与 API 一致 |
| 执行失败 | 查看任务日志与 `error_code`；确认目标工作负载是 Deployment/StatefulSet |
| 图表空白 | 是否新增了未在 `web/src/lib/chart.ts` 注册的 ECharts 类型 |
| 镜像拉取失败 | 镜像是否已导入 ka-cp1，`ctr -n k8s.io images ls` 是否可见 |
| 监控趋势为空 | informer 是否同步、kubelet 指标是否可达；`degraded` 为 true 时看 `metrics_at` |
| 事件列表为空 | 集群当前确实无异常，或检测器未启动（查看 API 日志与 `ARBITER_DETECT_INTERVAL`） |
| 部署后事件写入报 CHECK 失败 | 执行 `db/migrations/0003_aiops_incident_types.sql` |
| SLO 显示"数据积累中" | 分钟桶尚未覆盖一个完整窗口；等待或检查 `/api/v1/` 是否有请求 |

更完整的发布与回滚步骤见 [02-release-runbook.md](02-release-runbook.md)。

---

## 10. 文档索引

| 文档 | 内容 |
|---|---|
| [KubeArbiter项目说明书.md](../KubeArbiter项目说明书.md) | 项目定位、架构、实验与验收标准 |
| [00-freeze.md](00-freeze.md) | 范围冻结、版本策略、安全护栏 |
| [01-algorithm-spec.md](01-algorithm-spec.md) | 放置/公平/SLO 算法与原因码 |
| [04-aiops.md](04-aiops.md) | 故障检测规则、健康分、内置 SLI 与限制 |
| [02-release-runbook.md](02-release-runbook.md) | 构建、镜像、部署、回滚 |
| [benchmark.md](benchmark.md) | 策略对比基准报告 |
| [api/openapi.yaml](../api/openapi.yaml) | HTTP 契约唯一来源 |
| [db/schema.sql](../db/schema.sql) | 数据库 DDL |
