# OpenSandbox console on the server (experimental)

OpenSandbox's web console (upstream `console/`, an MVP: React web app + FastAPI backend) works against the server without
changes to either side for the core flows: list, create, details, browser shell, renew, pause/resume, delete.
The Logs/Events tabs and the Snapshots, Pools and Version pages call endpoints the server doesn't have yet.
Results: [docs/validation.md](../../../docs/validation.md) §7.

Tested with upstream commit `ff720a6`. Upstream publishes amd64 images only, so build from source.

## 1. Build the images
```bash
git clone https://github.com/alibaba/OpenSandbox.git /tmp/opensandbox && cd /tmp/opensandbox && git checkout ff720a6
docker build -t sandbox-console-bff:dev -f console/bff/Dockerfile console/bff
docker build -t sandbox-console-web:dev -f console/web/Dockerfile console/web
kind load docker-image sandbox-console-bff:dev sandbox-console-web:dev --name sandbox-bakeoff
```

## 2. Create the secrets (never committed)
The console keeps its own list of tenant keys; mint one with `sandbox admin create-key <tenant>` first.
```bash
kubectl create namespace sandbox-console
kubectl -n sandbox-console create secret generic sandbox-console-bff \
  --from-literal=BFF_SESSION_SECRET="$(openssl rand -hex 32)" \
  --from-literal=BFF_ADMIN_TOKEN="$(openssl rand -hex 16)"      # the console's own admin password

cat > /tmp/tenants.toml <<EOF
[[tenants]]
name = "demo-team"
namespace = "sandboxes"
api_keys = ["sandbox_..."]          # a sandbox API key for this tenant
EOF
kubectl -n sandbox-console create secret generic sandbox-console-tenants --from-file=tenants.toml=/tmp/tenants.toml
rm /tmp/tenants.toml
```

## 3. Deploy and open
```bash
kubectl apply -f deploy/kind/console/console.yaml
kubectl -n sandbox-console port-forward svc/console-web 18090:80     # keep 18090: it's the allowed CORS origin
```
Open http://localhost:18090/login and sign in with the tenant's API key. The admin view takes `BFF_ADMIN_TOKEN`:
`kubectl -n sandbox-console get secret sandbox-console-bff -o jsonpath='{.data.BFF_ADMIN_TOKEN}' | base64 -d`.

## Caveats
- Keys are copied into `tenants.toml`: revoking a key in the server doesn't log anyone out of the console until the Secret is updated.
- The console's admin token is its own password, unrelated to the server's `/admin/v1` API.
- Endpoints show the in-cluster host unless the server's `PUBLIC_ENDPOINT` is set.
