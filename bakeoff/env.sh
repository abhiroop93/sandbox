# shellcheck shell=bash
# All bake-off parameters. Override any of them via environment before sourcing, e.g.
#   TARGET=gke GKE_PROJECT=my-proj ./00-cluster-gke.sh
: "${TARGET:=kind}"                          # kind | gke
: "${CLUSTER_NAME:=srvm-bakeoff}"
: "${NS:=srvm-bakeoff}"                      # namespace for sandboxes
: "${SYS_NS:=srvm-system}"                   # namespace for object store etc.

# Versions (pinned)
: "${AGENT_SANDBOX_VERSION:=v1.0.10}"
: "${GVISOR_RELEASE:=20261026}"              # gs://gvisor/releases/release/<this>/
: "${KIND_NODE_IMAGE:=}"                     # empty = kind default

# Sandbox templates / pools
: "${SANDBOX_IMAGE:=srvm-bakeoff-sandbox:dev}"   # kind: built locally; gke: push to a registry and override
: "${POOL_SIZE:=$([[ $TARGET == kind ]] && echo 5 || echo 50)}"           # kind must fit Docker Desktop (~8 GB)
: "${SANDBOX_CPU:=$([[ $TARGET == kind ]] && echo 250m || echo 500m)}"
: "${SANDBOX_MEM:=$([[ $TARGET == kind ]] && echo 256Mi || echo 1Gi)}"
# 'open'/'runc' templates (compat test)
: "${COMPAT_MEM:=1Gi}"
# 'ws' template for the 2 GB workspace resume test
: "${WS_CPU:=2}"
: "${WS_MEM:=$([[ $TARGET == kind ]] && echo 2Gi || echo 4Gi)}"
: "${WS_REPLICAS:=$([[ $TARGET == kind ]] && echo 1 || echo 2)}"
: "${WORKSPACE_LIMIT:=6Gi}"                  # per-sandbox disk cap (2 GB workspace + tar/chunks headroom)
: "${WORKSPACE_REQUEST:=512Mi}"              # disk the scheduler RESERVES per sandbox. If unset, K8s uses the limit
                                             # (6Gi), and disk reservation caps density before CPU/memory does

# Runtime class
if [[ "$TARGET" == "gke" ]]; then
  : "${RUNTIME_CLASS:=gvisor}"               # provided by GKE Sandbox
else
  : "${RUNTIME_CLASS:=gvisor}"               # created by 02-templates.sh (handler runsc)
fi

# Object storage (S3 API). kind: in-cluster rclone S3 server. gke: GCS via S3 interop (HMAC keys).
if [[ "$TARGET" == "gke" ]]; then
  : "${S3_ENDPOINT:=https://storage.googleapis.com}"
  : "${S3_BUCKET:?set S3_BUCKET (GCS bucket name)}"
  : "${S3_ACCESS_KEY:?set S3_ACCESS_KEY (GCS HMAC access id)}"
  : "${S3_SECRET_KEY:?set S3_SECRET_KEY (GCS HMAC secret)}"
else
  : "${S3_ENDPOINT:=http://s3.${SYS_NS}.svc.cluster.local:8080}"
  : "${S3_BUCKET:=srvm-snapshots}"
  # Credentials for the in-cluster S3 server: random, generated once, kept in bakeoff/.local-secrets (gitignored).
  # Sandboxes can reach the object store, so these must never be well-known values.
  if [[ -z "${S3_ACCESS_KEY:-}" || -z "${S3_SECRET_KEY:-}" ]]; then
    _secrets="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.local-secrets"
    if [[ ! -f "$_secrets" ]]; then
      (umask 077; printf 'S3_ACCESS_KEY=srvm-%s\nS3_SECRET_KEY=%s\n' "$(openssl rand -hex 4)" "$(openssl rand -hex 24)" > "$_secrets")
    fi
    # shellcheck disable=SC1090
    source "$_secrets"
  fi
fi

# GKE only
: "${GKE_PROJECT:=}"
: "${GKE_REGION:=asia-south1}"
: "${GKE_ZONE:=asia-south1-a}"
: "${GKE_SANDBOX_MACHINE:=n2-standard-32}"
: "${GKE_SANDBOX_NODES:=5}"

export TARGET CLUSTER_NAME NS SYS_NS AGENT_SANDBOX_VERSION GVISOR_RELEASE KIND_NODE_IMAGE SANDBOX_IMAGE \
  POOL_SIZE SANDBOX_CPU SANDBOX_MEM COMPAT_MEM WS_CPU WS_MEM WS_REPLICAS WORKSPACE_LIMIT WORKSPACE_REQUEST RUNTIME_CLASS S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY \
  S3_SECRET_KEY GKE_PROJECT GKE_REGION GKE_ZONE GKE_SANDBOX_MACHINE GKE_SANDBOX_NODES

log() { printf '\033[1;34m[bakeoff]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[bakeoff] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
