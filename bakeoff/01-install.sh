#!/usr/bin/env bash
# Installs agent-sandbox, namespaces, the bake-off sandbox image (kind), and an S3-API object store (kind).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

log "installing agent-sandbox $AGENT_SANDBOX_VERSION"
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/sandbox-with-extensions.yaml"
kubectl -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s

kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl create ns "$SYS_NS" --dry-run=client -o yaml | kubectl apply -f -

if [[ "$TARGET" == "kind" ]]; then
  log "building sandbox image $SANDBOX_IMAGE"
  docker build -t "$SANDBOX_IMAGE" image/
  kind load docker-image "$SANDBOX_IMAGE" --name "$CLUSTER_NAME"

  log "deploying in-cluster S3 server (rclone serve s3)"
  kubectl -n "$SYS_NS" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: { name: s3, labels: { app: s3 } }
spec:
  replicas: 1
  selector: { matchLabels: { app: s3 } }
  template:
    metadata: { labels: { app: s3 } }
    spec:
      nodeSelector: { srvm.io/pool: system }
      containers:
      - name: rclone
        image: rclone/rclone:1.71.2
        args: ["serve","s3","--auth-key","$S3_ACCESS_KEY,$S3_SECRET_KEY","--addr",":8080","/data"]
        ports: [{ containerPort: 8080 }]
        volumeMounts: [{ name: data, mountPath: /data }]
      initContainers:
      - name: mkbucket
        image: busybox:1.36
        command: ["sh","-c","mkdir -p /data/$S3_BUCKET"]
        volumeMounts: [{ name: data, mountPath: /data }]
      volumes: [{ name: data, emptyDir: {} }]
---
apiVersion: v1
kind: Service
metadata: { name: s3 }
spec:
  selector: { app: s3 }
  ports: [{ port: 8080, targetPort: 8080 }]
YAML
  kubectl -n "$SYS_NS" rollout status deploy/s3 --timeout=180s
fi

log "setting up python venv for bench.py"
if command -v uv >/dev/null; then
  uv venv --allow-existing -q .venv && uv pip install -q --python .venv/bin/python kubernetes==31.0.0
else
  python3 -m venv .venv && .venv/bin/pip install -q kubernetes==31.0.0
fi
log "install done"
