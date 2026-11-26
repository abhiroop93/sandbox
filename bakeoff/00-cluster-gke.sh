#!/usr/bin/env bash
# Dedicated, throwaway GKE Standard cluster with a GKE Sandbox (gVisor) node pool.
# NEVER run the ramp test against a shared cluster: it deliberately saturates the API server.
set -euo pipefail
cd "$(dirname "$0")"; TARGET=gke; source env.sh
[[ -n "$GKE_PROJECT" ]] || die "set GKE_PROJECT"

log "creating GKE cluster $CLUSTER_NAME in $GKE_PROJECT/$GKE_ZONE"
gcloud container clusters create "$CLUSTER_NAME" --project "$GKE_PROJECT" --zone "$GKE_ZONE" \
  --num-nodes 3 --machine-type e2-standard-8 --enable-dataplane-v2 \
  --node-labels sandbox.io/pool=system --workload-pool "$GKE_PROJECT.svc.id.goog"

gcloud container node-pools create sandbox --cluster "$CLUSTER_NAME" --project "$GKE_PROJECT" --zone "$GKE_ZONE" \
  --machine-type "$GKE_SANDBOX_MACHINE" --num-nodes "$GKE_SANDBOX_NODES" --image-type cos_containerd \
  --sandbox type=gvisor --node-labels sandbox.io/pool=sandbox --node-taints sandbox.io/sandbox=true:NoSchedule \
  --max-pods-per-node 110

gcloud container clusters get-credentials "$CLUSTER_NAME" --project "$GKE_PROJECT" --zone "$GKE_ZONE"
log "GKE cluster ready. Push the sandbox image (see README) and set SANDBOX_IMAGE before 01-install.sh"
