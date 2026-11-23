# 版本锁（M1 回填）

- 回填日期：2026-11-22
- 规则（`docs/00-freeze.md` §4）：只记录实测输出，不凭记忆填写；未安装项标为"待安装"，不得预填版本。
- 校验环境：Windows 11，本机 shell（部分命令需完整路径，因当前 shell 的 PATH 未刷新）。

## 1. 已安装组件

| 组件 | 实际版本 | 安装日期 | 校验命令与实测输出 |
|---|---|---|---|
| Go | go1.27.4 windows/amd64 | 2026-11-22（M0 已装） | `go version` → `go version go1.27.4 windows/amd64` |
| Git | 2.56.1.windows.1 | 2026-11-22（M0 已装） | `git --version` → `git version 2.56.1.windows.1` |
| gcc (mingw-w64) | 16.2.0 (UCRT, posix-seh, Brecht Sanders r3) | 2026-11-22（M0 已装） | `gcc --version` → `gcc.exe (MinGW-W64 x86_64-ucrt-posix-seh, built by Brecht Sanders, r3) 16.2.0` |
| kubectl | v1.37.2（Kustomize v5.8.1） | 2026-11-22 | `kubectl version --client` → `Client Version: v1.37.2` |
| Helm | v3.20.0（go1.25.6，GitTreeState=clean） | 2026-11-22 | `helm version` → `version.BuildInfo{Version:"v3.20.0", ...}` |
| Python | 3.13.17 | 2026-11-22 | `python --version` → `Python 3.13.17` |
| Kubernetes 集群（kubeadm） | v1.37.2，3 节点 | 2026-11-23 | `kubectl get nodes` → ka-cp1/ka-w1/ka-w2 均 Ready，v1.37.2 |
| containerd | 2.2.1 | 2026-11-23 | 节点 `CONTAINER-RUNTIME` 列 → `containerd://2.2.1` |
| Flannel | v0.28.11 | 2026-11-23 | `kubectl -n kube-flannel get ds` → 3/3 READY |

> Helm 说明：winget 默认装到 v4.3.2，与冻结包"Helm 3.x 稳定版"冲突，已卸载并改装 v3.20.0。
> kubectl/helm 二进制位于 `%LOCALAPPDATA%\Microsoft\WinGet\Packages\...`，当前 shell 的 PATH 需重开后才生效。
> Python 说明：安装到 `%LOCALAPPDATA%\Programs\Python\Python313\`，仅用于 `scripts/analysis/` 离线统计，不参与服务构建与部署。

## 2. Go 模块（实际解析版本）

| 模块 | 版本 | 校验命令 |
|---|---|---|
| `github.com/gin-gonic/gin` | v1.10.1 | `go list -m github.com/gin-gonic/gin` |
| `github.com/google/uuid` | v1.6.0 | `go list -m github.com/google/uuid` |
| `k8s.io/client-go` | v0.37.2 | `go list -m k8s.io/client-go` |
| `github.com/jackc/pgx/v5` | v5.13.0 | `go list -m github.com/jackc/pgx/v5` |
| `k8s.io/api` | v0.37.2 | `go list -m k8s.io/api` |
| `k8s.io/apimachinery` | v0.37.2 | `go list -m k8s.io/apimachinery` |

> gin 说明：最新 v1.12.0 会引入 `quic-go`、`mongo-driver/v2` 等间接依赖，超出依赖白名单的意图，故锁 v1.10.1。
> client-go 说明：与 kubectl v1.37.2 同 minor（v0.37.2），满足冻结包"偏差不超过 ±1"。
> `go.mod` 的 `go` 指令随依赖要求升到 **1.26.0**（gin 阶段 1.25.0，client-go 阶段 1.26.0），本机工具链 go1.27.4 可正常构建与 `-race` 测试。

## 3. 待安装 / 待回填

| 组件 | 计划版本 | 阻塞原因 |
|---|---|---|
| Docker Desktop | — | 用户决定不安装；PostgreSQL 改用集群内 containerd 直接运行 |
| PostgreSQL 16 | 16.16（Debian 16.16-1.pgdg13+1），`postgres@sha256:3950ad7c3148ca2175ef1b6c36c9efbf78957ae8fbe8b3a5d7c35af50500382f` | 已部署：`kubearbiter/postgres-0`，hostPath `/var/lib/kubearbiter/pg`，trust 认证仅集群内可达 |
| ingress-nginx | 与集群兼容的稳定版 | M2 前回填 `helm list -n ingress-nginx` |
| Node.js LTS | LTS | M5 前回填 |
