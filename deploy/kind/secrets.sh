#!/usr/bin/env bash
# Creates the server's Secrets on the kind cluster with random values. Idempotent: existing Secrets are kept, so
# re-running never rotates credentials under a running deployment. To rotate, delete the Secret and re-run.
#
#   sandbox-postgres  POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB / DATABASE_URL
#   sandbox           ADMIN_TOKEN (operator API) + S3_ACCESS_KEY / S3_SECRET_KEY (the bake-off kit's in-cluster S3)
#
# Read the admin token:  kubectl -n sandbox-system get secret sandbox -o jsonpath='{.data.ADMIN_TOKEN}' | base64 -d
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=../../bakeoff/env.sh
source bakeoff/env.sh   # S3 credentials of the in-cluster object store (bakeoff/.local-secrets)

ns="${SANDBOX_SYSTEM_NAMESPACE:-sandbox-system}"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

if ! kubectl -n "$ns" get secret sandbox-postgres >/dev/null 2>&1; then
  pw="$(openssl rand -hex 16)"
  kubectl -n "$ns" create secret generic sandbox-postgres \
    --from-literal=POSTGRES_USER=sandbox --from-literal=POSTGRES_PASSWORD="$pw" --from-literal=POSTGRES_DB=sandbox \
    --from-literal=DATABASE_URL="postgres://sandbox:${pw}@postgres.${ns}.svc.cluster.local:5432/sandbox?sslmode=disable" >/dev/null
  echo "created secret $ns/sandbox-postgres"
fi

if ! kubectl -n "$ns" get secret sandbox >/dev/null 2>&1; then
  kubectl -n "$ns" create secret generic sandbox \
    --from-literal=ADMIN_TOKEN="$(openssl rand -hex 24)" \
    --from-literal=S3_ACCESS_KEY="$S3_ACCESS_KEY" --from-literal=S3_SECRET_KEY="$S3_SECRET_KEY" >/dev/null
  echo "created secret $ns/sandbox (admin token: kubectl -n $ns get secret sandbox -o jsonpath='{.data.ADMIN_TOKEN}' | base64 -d)"
fi
