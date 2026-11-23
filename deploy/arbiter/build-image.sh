#!/usr/bin/env bash
# 离线构建并导入镜像。
# 节点上只有 containerd，没有 docker/nerdctl/buildkit，因此这里直接拼一个单层 OCI 镜像：
# 静态二进制 + CA 证书，无需基础镜像，也不需要联网。
#
# 用法（在节点上以 root 执行）：
#   bash build-image.sh <linux-amd64 二进制> [镜像引用] [输出 tar]
#   bash build-image.sh /tmp/arbiter-api-linux docker.io/kubearbiter/arbiter-api:0.4.0 /tmp/arbiter-image.tar
set -euo pipefail

BIN=${1:?用法: build-image.sh <linux-amd64 二进制> [镜像引用] [输出 tar]}
REF=${2:-docker.io/kubearbiter/arbiter-api:0.4.0}
OUT=${3:-/tmp/arbiter-image.tar}
CREATED=${CREATED:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

[ -f "$BIN" ] || { echo "找不到二进制: $BIN" >&2; exit 1; }

python3 - "$BIN" "$REF" "$OUT" "$CREATED" <<'PY'
import gzip, hashlib, io, json, os, sys, tarfile

bin_path, ref, out_path, created = sys.argv[1:5]
work = out_path + ".d"
if os.path.isdir(work):
    import shutil
    shutil.rmtree(work)
blobs = os.path.join(work, "blobs", "sha256")
os.makedirs(blobs)

def put(data):
    digest = hashlib.sha256(data).hexdigest()
    with open(os.path.join(blobs, digest), "wb") as f:
        f.write(data)
    return digest, len(data)

# 单层：可执行文件 + CA 证书（Go 静态二进制不依赖 libc）。
layer = io.BytesIO()
with tarfile.open(fileobj=layer, mode="w", format=tarfile.PAX_FORMAT) as tar:
    def add(name, data, mode):
        info = tarfile.TarInfo(name)
        info.size, info.mode, info.mtime = len(data), mode, 0
        info.uid = info.gid = 0
        info.uname = info.gname = "root"
        tar.addfile(info, io.BytesIO(data))
    add("usr/local/bin/arbiter-api", open(bin_path, "rb").read(), 0o755)
    ca = "/etc/ssl/certs/ca-certificates.crt"
    if os.path.exists(ca):
        add("etc/ssl/certs/ca-certificates.crt", open(ca, "rb").read(), 0o644)

raw = layer.getvalue()
gz = gzip.compress(raw, compresslevel=9, mtime=0)
diff_id = hashlib.sha256(raw).hexdigest()
layer_digest, layer_size = put(gz)

config = {
    "architecture": "amd64",
    "os": "linux",
    "created": created,
    "config": {
        "User": "65532:65532",
        "Entrypoint": ["/usr/local/bin/arbiter-api"],
        "ExposedPorts": {"8080/tcp": {}, "9443/tcp": {}},
    },
    "rootfs": {"type": "layers", "diff_ids": ["sha256:" + diff_id]},
    "history": [{"created": created, "created_by": "kubearbiter offline image assembly"}],
}
cfg_digest, cfg_size = put(json.dumps(config, separators=(",", ":")).encode())

manifest = {
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {"mediaType": "application/vnd.oci.image.config.v1+json",
               "digest": "sha256:" + cfg_digest, "size": cfg_size},
    "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                "digest": "sha256:" + layer_digest, "size": layer_size}],
}
man_digest, man_size = put(json.dumps(manifest, separators=(",", ":")).encode())

index = {
    "schemaVersion": 2,
    "manifests": [{
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": "sha256:" + man_digest,
        "size": man_size,
        "annotations": {"org.opencontainers.image.ref.name": ref},
    }],
}
with open(os.path.join(work, "index.json"), "w") as f:
    json.dump(index, f)
with open(os.path.join(work, "oci-layout"), "w") as f:
    json.dump({"imageLayoutVersion": "1.0.0"}, f)

# 打成 OCI layout 归档
import shutil
with tarfile.open(out_path, "w", format=tarfile.PAX_FORMAT) as tar:
    for root, _, files in os.walk(work):
        for name in files:
            full = os.path.join(root, name)
            tar.add(full, arcname=os.path.relpath(full, work))
shutil.rmtree(work)
print("镜像归档: %s (%s)" % (out_path, ref))
PY

ctr -n k8s.io images import "$OUT"
ctr -n k8s.io images ls | grep -F "${REF##*/}" || true
