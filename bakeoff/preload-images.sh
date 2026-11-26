#!/usr/bin/env bash
# kind only: pull images on the host and import them into every kind node, so the nodes never pull them themselves.
#   ./preload-images.sh <image> [<image>...]
# Why: pulls from inside kind nodes can hang on some networks, and `kind load docker-image` fails for multi-platform
# images on Docker Desktop ("content digest ... not found"). Importing only the local platform avoids both.
# Best effort: if an image can't be preloaded, the node pulls it as usual.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh
[[ "$TARGET" == "kind" ]] || exit 0

platform="linux/$(docker version -f '{{.Server.Arch}}')"
with_timeout() { perl -e 'alarm shift; exec @ARGV' "$@"; }   # portable timeout (macOS has no `timeout`)

for img in "$@"; do
  pulled=""
  for attempt in 1 2 3; do
    if docker image inspect "$img" >/dev/null 2>&1 || with_timeout 180 docker pull -q "$img" >/dev/null; then
      pulled=1; break
    fi
    log "pull of $img timed out or failed (attempt $attempt/3)"
  done
  if [[ -z "$pulled" ]]; then
    log "WARNING: could not pull $img on the host; the cluster will pull it itself"
    continue
  fi
  for node in $(kind get nodes --name "$CLUSTER_NAME"); do
    docker save "$img" | docker exec -i "$node" ctr --namespace=k8s.io images import --platform "$platform" - >/dev/null \
      || log "WARNING: could not import $img into $node"
  done
  log "preloaded $img ($platform) into $(kind get nodes --name "$CLUSTER_NAME" | wc -l | tr -d ' ') nodes"
done
