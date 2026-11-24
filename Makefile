# KubeArbiter 常用构建入口。
# Windows 请在 Git Bash / WSL 下执行 make；CI 与 Linux 构建机行为一致。

GO      ?= go
NPM     ?= npm
VERSION ?= 0.9.3
IMAGE_REF ?= docker.io/kubearbiter/arbiter-api:$(VERSION)
BIN       ?= bin/arbiter-api-linux
IMAGE_OUT ?= bin/arbiter-image-$(VERSION).tar

.PHONY: all web vet test build image clean

all: image

# 前端产物直接写入 internal/webui/dist，供 go:embed 打包。
web:
	cd web && $(NPM) ci && $(NPM) run build

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

build: web
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/arbiter-api

# 单层 OCI 归档，节点上 ctr -n k8s.io images import 即可。
image: build
	$(GO) run ./cmd/arbiter-image -bin $(BIN) -ref $(IMAGE_REF) -out $(IMAGE_OUT)

clean:
	rm -f $(BIN) $(IMAGE_OUT)
