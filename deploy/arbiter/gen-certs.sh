#!/usr/bin/env bash
# 生成 admission webhook 用的自签证书，注入 Secret，并把 CA 写进 MutatingWebhookConfiguration 的 caBundle。
# 只在控制平面节点执行，需要能访问集群的 kubectl。
#
# 顺序：先 kubectl apply -f deploy/arbiter/arbiter.yaml，再执行本脚本。
#   bash gen-certs.sh [输出目录，默认 /tmp/arbiter-webhook-tls]
# 私钥只留在该目录，不进入仓库。
set -euo pipefail

NS=kubearbiter
SVC=arbiter-webhook
DIR=${1:-/tmp/arbiter-webhook-tls}
mkdir -p "$DIR"
cd "$DIR"

if [ ! -f ca.crt ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout ca.key -out ca.crt -subj "/CN=kubearbiter-webhook-ca"
fi

cat > server.cnf <<EOF
[req]
distinguished_name=dn
req_extensions=v3_req
prompt=no
[dn]
CN=${SVC}.${NS}.svc
[v3_req]
keyUsage=keyEncipherment,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=@alt
[alt]
DNS.1=${SVC}
DNS.2=${SVC}.${NS}
DNS.3=${SVC}.${NS}.svc
DNS.4=${SVC}.${NS}.svc.cluster.local
EOF

openssl req -new -newkey rsa:2048 -nodes -keyout tls.key -out server.csr -config server.cnf
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 825 -extensions v3_req -extfile server.cnf

kubectl -n "$NS" create secret tls arbiter-webhook-tls \
  --cert=tls.crt --key=tls.key --dry-run=client -o yaml | kubectl apply -f -

CA_B64=$(base64 -w0 ca.crt)
kubectl patch mutatingwebhookconfiguration kubearbiter-pod-placement --type=json \
  -p "[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"$CA_B64\"}]"

echo "证书已写入 Secret ${NS}/arbiter-webhook-tls，caBundle 已更新"
