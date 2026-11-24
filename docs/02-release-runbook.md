# 发布与回滚 Runbook

面向本仓库的交付流程：构建 → 生成离线镜像 → 导入集群 → 滚动更新 → 验证 → 回滚。
所有命令均在仓库根目录 `E:\KubeArbiter` 执行（Windows 用 Git Bash / PowerShell，CI 为 Ubuntu）。

## 0. 产物与命名

| 产物 | 路径 | 说明 |
|---|---|---|
| Linux 二进制 | `bin/arbiter-api-linux` | `CGO_ENABLED=0` 静态编译，amd64 |
| OCI 镜像归档 | `bin/arbiter-image-<版本>.tar` | 单层，含二进制（可选 CA 证书），无需 Docker |
| 前端产物 | `internal/webui/dist/` | `npm run build` 直接输出，随 `go:embed` 打进二进制 |
| 部署清单 | `deploy/arbiter/*.yaml` | `image` 字段需与本次版本一致 |

版本号同时出现在 `go.mod` 之外的三个位置：`deploy/arbiter/arbiter.yaml`、`deploy/arbiter/worker.yaml`、`deploy/versions.lock.md`。

## 1. 本地构建

```powershell
# Windows：补齐 PATH 后执行（Git Bash 的 make 也可直接跑 make image）
$env:PATH = 'C:\Program Files\Go\bin;C:\Users\Gracie\.trae-cn\work\nodejs;' + $env:PATH
cd E:\KubeArbiter
make image VERSION=0.9.4
```

`make image` 等价于以下步骤：

```powershell
# 1) 前端（输出到 internal/webui/dist）
cd web; npm ci; npm run build; cd ..

# 2) 交叉编译
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go build -trimpath -ldflags "-s -w" -o bin/arbiter-api-linux ./cmd/arbiter-api

# 3) 拼装 OCI 归档（-ca 可选，Windows 默认没有 /etc/ssl 证书，会跳过）
go run ./cmd/arbiter-image -bin bin/arbiter-api-linux `
  -ref docker.io/kubearbiter/arbiter-api:0.9.4 `
  -out bin/arbiter-image-0.9.4.tar `
  -ca "C:\Program Files\Git\mingw64\ssl\certs\ca-bundle.crt"
```

worker 镜像同理：把 `-bin` 换成 `bin/arbiter-worker-linux`、`-ref` 换成 `.../arbiter-worker:<版本>`。

> `cmd/arbiter-image` 是 `deploy/arbiter/build-image.sh` 的跨平台替代（不再依赖 python3），
> 输出标准 OCI layout tar，节点上 `ctr -n k8s.io images import` 可直接导入。

## 2. CI

`.github/workflows/ci.yml` 在 push main / PR 时执行，单 job 顺序为：

1. `npm ci && npm run build`（先生成真实 dist，满足 `go:embed`）；
2. `go vet ./...`；
3. `go test -race ./...`；
4. `go build ./cmd/arbiter-api`；
5. `go run ./cmd/arbiter-image ...` 对镜像拼装做冒烟校验。

CI 不推送镜像、不接触集群，发布仍是显式操作。

## 3. 校验归档

```powershell
# 读取 manifest digest（写入 versions.lock.md 的就是这个值）
tar -xOf bin\arbiter-image-0.9.4.tar index.json

# 确认层内容
tar -tf bin\arbiter-image-0.9.4.tar
```

`index.json` 中 `annotations."org.opencontainers.image.ref.name"` 必须等于目标镜像引用，
`digest` 即镜像 manifest digest。

## 4. 导入集群

镜像只在控制平面节点 `ka-cp1`（192.168.49.11）导入，两个 Deployment 都用
`nodeSelector: control-plane` 固定在那里。

**首选：SSH + ctr**

```powershell
scp -i C:\Users\Gracie\.ssh\ka-cp1 bin\arbiter-image-0.9.4.tar arbiter@192.168.49.11:/tmp/
ssh -i C:\Users\Gracie\.ssh\ka-cp1 arbiter@192.168.49.11 `
  "sudo ctr -n k8s.io images import /tmp/arbiter-image-0.9.4.tar && sudo ctr -n k8s.io images ls | grep arbiter-api"
```

**兜底：临时特权 Pod**（无 SSH 通道时使用，导入后立即删除）

在 `kubearbiter` 命名空间创建挂载宿主机 containerd socket 与 `/tmp` 的临时 Pod，
执行 `ctr -n k8s.io images import`，确认 `ctr images ls` 可见后删除 Pod 与临时清单。
该操作具有节点级权限，只在必要时使用，并在发布记录中注明。

## 5. 更新与验证

```powershell
$KUBECTL = "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\Kubernetes.kubectl_Microsoft.Winget.Source_8wekyb3d8bbwe\kubectl.exe"

# 1) 改镜像版本（或先改 yaml 再 apply）
& $KUBECTL -n kubearbiter set image deploy/arbiter-api arbiter-api=kubearbiter/arbiter-api:0.9.4
& $KUBECTL -n kubearbiter rollout status deploy/arbiter-api --timeout=180s

# 2) 健康检查与控制台
& $KUBECTL -n kubearbiter port-forward svc/arbiter-webhook 8080:8080
# 另开终端：
curl.exe http://127.0.0.1:8080/healthz
curl.exe -I http://127.0.0.1:8080/
```

浏览器打开 `http://127.0.0.1:8080/`，逐页确认：数据来自接口、图表正常渲染、
无控制台报错。镜像 digest 与 `versions.lock.md` 记录一致后，再更新版本锁。

## 6. 回滚

```powershell
& $KUBECTL -n kubearbiter rollout undo deploy/arbiter-api
& $KUBECTL -n kubearbiter rollout status deploy/arbiter-api --timeout=180s
```

回滚依赖旧镜像仍在节点本地。若节点已清理，先用同一流程导入旧版本 tar
（`bin/arbiter-image-<旧版本>.tar`）再执行 `rollout undo`。

## 7. 排障清单

| 现象 | 先查 |
|---|---|
| Pod `ImagePullBackOff` | 镜像是否在控制平面节点：`sudo ctr -n k8s.io images ls \| grep arbiter` |
| `GET /` 返回 404/纯 API | 构建时是否先生成了 `internal/webui/dist/index.html` |
| 页面图表空白 | 是否新增了未在 `web/src/lib/chart.ts` 注册的图表类型 |
| 导入报 `unexpected EOF` | tar 传输不完整，重新 `scp` 并核对文件大小 |
| 版本锁 digest 对不上 | 重新读取归档 `index.json`，不要凭构建日志填写 |
