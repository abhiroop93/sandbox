#!/usr/bin/env bash
# RuntimeClass (kind), SandboxTemplates, WarmPools and NetworkPolicies.
#   locked : gVisor, non-root, no SA token, default-deny egress (DNS + object store only)  ← the baseline
#   open   : same but egress to the internet allowed (compat tests: pip/npm/git)
#   runc   : no gVisor, on system nodes (baseline for slowdown comparison)
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

if [[ "$TARGET" == "kind" ]]; then
  kubectl apply -f - <<YAML
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: $RUNTIME_CLASS }
handler: runsc
scheduling:
  nodeSelector: { sandbox.io/pool: sandbox }
  tolerations: [{ key: sandbox.io/sandbox, operator: Exists, effect: NoSchedule }]
YAML
fi

template() {  # name runtimeClass|"" pool-label [replicas] [cpu] [mem]
  local name=$1 rc=$2 pool=$3 cpu=${5:-$SANDBOX_CPU} mem=${6:-$SANDBOX_MEM}
  cat <<YAML
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata: { name: $name, namespace: $NS }
spec:
  networkPolicyManagement: Unmanaged          # the server manages policy itself (below)
  podTemplate:
    metadata:
      labels: { sandbox.io/sandbox: "true", sandbox.io/template: $name }
    spec:
      ${rc:+runtimeClassName: $rc}
      automountServiceAccountToken: false
      enableServiceLinks: false
      nodeSelector: { sandbox.io/pool: $pool }
      tolerations: [{ key: sandbox.io/sandbox, operator: Exists, effect: NoSchedule }]
      securityContext: { runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, runAsNonRoot: true,
                         seccompProfile: { type: RuntimeDefault } }
      containers:
      - name: sandbox
        image: $SANDBOX_IMAGE
        imagePullPolicy: IfNotPresent
        command: ["sleep","infinity"]
        workingDir: /workspace
        securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ["ALL"] } }
        resources:
          requests: { cpu: "$cpu", memory: "$mem", ephemeral-storage: "$WORKSPACE_REQUEST" }
          limits:   { cpu: "$cpu", memory: "$mem", ephemeral-storage: "$WORKSPACE_LIMIT" }
        volumeMounts: [{ name: workspace, mountPath: /workspace }]
      volumes:
      - name: workspace
        emptyDir: { sizeLimit: "$WORKSPACE_LIMIT" }
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata: { name: $name, namespace: $NS }
spec:
  replicas: ${4:-$POOL_SIZE}
  sandboxTemplateRef: { name: $name }
---
YAML
}

{
  template locked "$RUNTIME_CLASS" sandbox
  template open   "$RUNTIME_CLASS" sandbox 2 1 "$COMPAT_MEM"   # compat needs real memory (pip, pandas)
  template runc   ""               system  2 1 "$COMPAT_MEM"
  template ws     "$RUNTIME_CLASS" sandbox "$WS_REPLICAS" "$WS_CPU" "$WS_MEM"     # persistent-workspace resume test (2 GB)
  template tiny   "$RUNTIME_CLASS" sandbox 0 100m 128Mi     # density test (bench.py scales it)
  # cold: a pool with 0 replicas over the locked template → every claim takes the cold path
  cat <<YAML
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata: { name: cold, namespace: $NS }
spec:
  replicas: 0
  sandboxTemplateRef: { name: locked }
YAML
} | kubectl apply -f -

# NetworkPolicies: deny all ingress to sandboxes (incl. sandbox->sandbox); egress per template.
kubectl -n "$NS" apply -f - <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: sandbox-default-deny }
spec:
  podSelector: { matchLabels: { sandbox.io/sandbox: "true" } }
  policyTypes: [Ingress, Egress]
  ingress: []
  egress:
  - to: [{ namespaceSelector: {}, podSelector: { matchLabels: { k8s-app: kube-dns } } }]
    ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
  - to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: $SYS_NS } }, podSelector: { matchLabels: { app: s3 } } }]
    ports: [{ port: 8080, protocol: TCP }]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: sandbox-open-internet }
spec:
  podSelector: { matchExpressions: [{ key: sandbox.io/template, operator: In, values: [open, runc] }] }
  policyTypes: [Egress]
  egress:
  - to: [{ ipBlock: { cidr: 0.0.0.0/0, except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16] } }]
YAML

log "waiting for warm pools"
for p in locked open runc ws; do  # tiny/cold intentionally start at 0
  for i in $(seq 1 120); do
    ready=$(kubectl -n "$NS" get sandboxwarmpool "$p" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    want=$(kubectl -n "$NS" get sandboxwarmpool "$p" -o jsonpath='{.spec.replicas}')
    [[ "${ready:-0}" -ge "$want" ]] && { log "pool $p ready ($ready/$want)"; break; }
    sleep 5
  done
done
kubectl -n "$NS" get sandboxwarmpools
