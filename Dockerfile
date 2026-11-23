# 运行镜像只放交叉编译好的静态二进制，构建过程不在容器内做，避免在集群节点上拉 Go 工具链。
# 镜像源与 deploy/versions.lock.md 保持一致（Docker Hub 直连在当前环境不可用）。
FROM docker.m.daocloud.io/library/alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

COPY bin/arbiter-api /usr/local/bin/arbiter-api

EXPOSE 8080 9443
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/arbiter-api"]
