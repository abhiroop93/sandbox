# OpenSandbox console on srvm (experimental)

OpenSandbox's web console (upstream `console/`, an MVP: React web app + FastAPI backend) works against srvm without
changes to either side for the core flows: list, create, details, browser shell, renew, pause/resume, delete.
The Logs/Events tabs and the Snapshots, Pools and Version pages call endpoints srvm doesn't have yet.
Results: [docs/validation.md](../../../docs/validation.md) §7.

Tested with upstream commit `ff720a6`. Upstream publishes amd64 images only, so build from source.

## 1. Build the images
```bash
git clone https://github.com/alibaba/OpenSandbox.git /tmp/opensandbox && cd /tmp/opensandbox && git checkout ff720a6
docker build -t srvm-console-bff:dev -f console/bff/Dockerfile console/bff
docker build -t srvm-console-web:dev -f console/web/Dockerfile console/web
kind load docker-image srvm-console-bff:dev srvm-console-web:dev --name srvm-bakeoff
```

## 2. Create the secrets (never committed)
The console keeps its own list of tenant keys; mint one with `srvm admin create-key <tenant>` first.
```bash
kubectl create namespace srvm-console
kubectl -n srvm-console create secret generic srvm-console-bff \
  --from-literal=BFF_SESSION_SECRET="$(openssl rand -hex 32)" \
  --from-literal=BFF_ADMIN_TOKEN="$(openssl rand -hex 16)"      # the console's own admin password

cat > /tmp/tenants.toml <<EOF
[[tenants]]
name = "demo-team"
namespace = "srvm-sandboxes"
api_keys = ["srvm_..."]          # an srvm API key for this tenant
EOF
kubectl -n srvm-console create secret generic srvm-console-tenants --from-file=tenants.toml=/tmp/tenants.toml
rm /tmp/tenants.toml
```

## 3. Deploy and open
```bash
kubectl apply -f deploy/kind/console/console.yaml
kubectl -n srvm-console port-forward svc/console-web 18090:80     # keep 18090: it's the allowed CORS origin
```
Open http://localhost:18090/login and sign in with the tenant's srvm key. The admin view takes `BFF_ADMIN_TOKEN`:
`kubectl -n srvm-console get secret srvm-console-bff -o jsonpath='{.data.BFF_ADMIN_TOKEN}' | base64 -d`.

## Caveats
- Keys are copied into `tenants.toml`: revoking a key in srvm doesn't log anyone out of the console until the Secret is updated.
- The console's admin token is its own password, unrelated to srvm's `/admin/v1` API.
- Endpoints show the in-cluster host unless srvm's `PUBLIC_ENDPOINT` is set.
