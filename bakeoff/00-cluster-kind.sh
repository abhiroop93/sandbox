#!/usr/bin/env bash
# Local kind cluster: 1 control-plane, 1 system worker, 1 sandbox worker (tainted) with gVisor installed.
# Functional numbers only: latency/ceiling/density on kind are NOT representative of a real cell.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

docker info >/dev/null 2>&1 || die "Docker daemon not running (start Docker Desktop; ≥ 8 GB memory, 12+ GB recommended)"

if kind get clusters | grep -qx "$CLUSTER_NAME"; then
  log "kind cluster $CLUSTER_NAME already exists"
else
  log "creating kind cluster $CLUSTER_NAME"
  cat > /tmp/kind-$CLUSTER_NAME.yaml <<KIND
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
  labels: { sandbox.io/pool: system }
- role: worker
  labels: { sandbox.io/pool: sandbox }
KIND
  kind create cluster --name "$CLUSTER_NAME" --config /tmp/kind-$CLUSTER_NAME.yaml ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"}
fi

SANDBOX_NODE=$(kubectl get nodes -l sandbox.io/pool=sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl taint node "$SANDBOX_NODE" sandbox.io/sandbox=true:NoSchedule --overwrite

# --- install gVisor into the sandbox node ---
ARCH=$(docker exec "$SANDBOX_NODE" uname -m)          # aarch64 | x86_64
TMP=$(mktemp -d)
log "downloading gVisor $GVISOR_RELEASE ($ARCH)"
curl -fsSL -o "$TMP/gvisor.tar.bz2" "https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$ARCH/gvisor.tar.bz2"
curl -fsSL -o "$TMP/gvisor.tar.bz2.sha512" "https://storage.googleapis.com/gvisor/releases/release/$GVISOR_RELEASE/$ARCH/gvisor.tar.bz2.sha512"
( cd "$TMP" && expected=$(awk '{print $1}' gvisor.tar.bz2.sha512) && actual=$(shasum -a 512 gvisor.tar.bz2 | awk '{print $1}') \
  && [[ "$expected" == "$actual" ]] ) || die "gVisor checksum mismatch"
mkdir -p "$TMP/x" && tar -xjf "$TMP/gvisor.tar.bz2" -C "$TMP/x"
docker cp "$TMP/x/." "$SANDBOX_NODE:/usr/local/bin/"
docker exec "$SANDBOX_NODE" /usr/local/bin/runsc --version

# --- register runsc with containerd (v2 or v3 config schema) ---
docker exec "$SANDBOX_NODE" bash -c '
set -e
CFG=/etc/containerd/config.toml
grep -q "runtimes.runsc" $CFG && { echo "runsc already configured"; exit 0; }
if grep -qE "^version *= *3" $CFG; then
  cat >> $CFG <<TOML

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
TOML
else
  cat >> $CFG <<TOML

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
TOML
fi
systemctl restart containerd
'
sleep 5
kubectl wait --for=condition=Ready node/"$SANDBOX_NODE" --timeout=120s
rm -rf "$TMP"
log "kind cluster ready; sandbox node: $SANDBOX_NODE (gVisor installed)"
