#!/usr/bin/env bash
# OpenSandbox spike on the bake-off kind cluster: its own backend (BatchSandbox + Pool), gVisor,
# local registry for pause/resume (rootfs commit), no ingress gateway (SDK uses server proxy).
set -euo pipefail
cd "$(dirname "$0")"
: "${OSB_SRC:?path to an OpenSandbox checkout at release-1.2.0}"
: "${CLUSTER:=sandbox-bakeoff}"
: "${OSB_API_KEY:=spike-key}"
CHARTS="$OSB_SRC/manifests/charts"; REG=kind-registry:5000
log() { printf '\033[1;34m[osb]\033[0m %s\n' "$*"; }

log "freeing memory: shrinking the bake-off pools"
for p in open runc ws tiny cold; do kubectl -n sandbox-bakeoff patch sandboxwarmpool $p --type merge -p '{"spec":{"replicas":0}}' >/dev/null; done
kubectl -n sandbox-bakeoff patch sandboxwarmpool locked --type merge -p '{"spec":{"replicas":2}}' >/dev/null

log "local registry $REG"
docker inspect kind-registry >/dev/null 2>&1 || docker run -d --restart=always --name kind-registry --network kind registry:2.8.3 >/dev/null
for n in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$n" bash -c "
    set -e
    mkdir -p /etc/containerd/certs.d/$REG
    printf '[host.\"http://$REG\"]\n  capabilities = [\"pull\", \"resolve\", \"push\"]\n  skip_verify = true\n' > /etc/containerd/certs.d/$REG/hosts.toml
    if ! grep -q 'config_path = \"/etc/containerd/certs.d\"' /etc/containerd/config.toml; then
      if grep -qE '^version *= *3' /etc/containerd/config.toml; then sec='plugins.\"io.containerd.cri.v1.images\".registry'
      else sec='plugins.\"io.containerd.grpc.v1.cri\".registry'; fi
      printf '\n[%s]\n  config_path = \"/etc/containerd/certs.d\"\n' \"\$sec\" >> /etc/containerd/config.toml
      systemctl restart containerd
    fi"
done
sleep 5; kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null

log "namespaces (owned by the base Helm release) + API key"
for ns in opensandbox-system opensandbox; do
  kubectl create ns $ns --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl label ns $ns app.kubernetes.io/managed-by=Helm --overwrite >/dev/null
  kubectl annotate ns $ns meta.helm.sh/release-name=base meta.helm.sh/release-namespace=opensandbox-system --overwrite >/dev/null
done
kubectl -n opensandbox-system create secret generic opensandbox-api-key --from-literal=api-key="$OSB_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

log "helm: base, controller, server"
helm upgrade --install base "$CHARTS/base" -n opensandbox-system --wait
helm upgrade --install opensandbox-controller "$CHARTS/controller" -n opensandbox-system -f values-controller.yaml --wait --timeout 5m
helm upgrade --install opensandbox-server "$CHARTS/server" -n opensandbox-system -f values-server.yaml --wait --timeout 5m
kubectl -n opensandbox-system get pods
log "done. API: kubectl -n opensandbox-system port-forward svc/opensandbox-server 18080:80  (key: $OSB_API_KEY)"
