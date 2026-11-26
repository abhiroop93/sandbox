#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"; source env.sh
if [[ "$TARGET" == "kind" ]]; then kind delete cluster --name "$CLUSTER_NAME"
else gcloud container clusters delete "$CLUSTER_NAME" --project "$GKE_PROJECT" --zone "$GKE_ZONE" --quiet; fi
