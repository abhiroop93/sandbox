# srvm: Setup, configuration & usage

Two audiences:
- **DevOps / operators** (§1–§5): run the platform and mint keys.
- **Users** (§6): get a key and write code against the OpenSandbox SDK.

---

## 1. What you deploy
| Component | Where | Notes |
|---|---|---|
| gVisor (`runsc`) + RuntimeClass `gvisor` | Sandbox node pool (tainted) | GKE: GKE Sandbox node pool. Elsewhere: install runsc + containerd config. |
| agent-sandbox `v1.0.10` (+ extensions) | `agent-sandbox-system` | **Must allowlist the `srvm.io` label domain** (§3) |
| Postgres | Managed (Cloud SQL / RDS) in prod; a pod on kind | Only stateful dependency besides object storage |
| S3-API object storage + bucket | GCS (HMAC/S3 interop), S3, OCI, MinIO… | Workspace snapshots. Sandboxes reach it via presigned URLs only. |
| **srvm** (Deployment + Service) | `srvm-system` | One stateless binary. Sandboxes run in `srvm-sandboxes`. |

## 2. Local setup (kind): about 15 minutes
Prereqs: Docker Desktop (≥ 8 GB; 12+ GB recommended), kind, kubectl, helm, Go ≥ 1.25, Python 3 + `uv`.
```bash
cd bakeoff && ./00-cluster-kind.sh && ./01-install.sh && ./02-templates.sh && cd ..
#   kind cluster + gVisor on a tainted sandbox node, agent-sandbox, in-cluster S3 (rclone), RuntimeClass "gvisor"
kubectl apply -f deploy/kind/agent-sandbox-config.yaml
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller   # allowlist is read at startup
make deploy                       # build → image srvm:dev → kind load → random Secrets (once) → apply Postgres + srvm
kubectl -n srvm-system port-forward svc/srvm 18081:80      # API at http://127.0.0.1:18081
```
Health check: `curl 127.0.0.1:18081/health` → `{"status":"healthy"}`.

**GKE / other clusters:**
- Create a dedicated sandbox node pool with gVisor (GKE: `--sandbox type=gvisor`), labelled `srvm.io/pool=sandbox` and tainted
  `srvm.io/sandbox=true:NoSchedule`.
- Install agent-sandbox and the allowlist ConfigMap.
- Push `srvm:<tag>` to your registry and set `image` / `HELPER_IMAGE`.
- Point `DATABASE_URL` and `S3_*` at managed services (§3).
- Put TLS ingress in front of the `srvm` Service.

## 3. Configuration (srvm environment variables)
| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required) | Postgres URL. Schema is applied automatically at start. |
| `ADMIN_TOKEN` | — | Enables `/admin/v1/*` (header `X-SRVM-ADMIN-TOKEN`). Empty means the admin HTTP API is disabled. |
| `SANDBOX_NAMESPACE` / `SYSTEM_NAMESPACE` | `srvm-sandboxes` / `srvm-system` | Where sandboxes run / where srvm + object store run (allowed to reach execd) |
| `RUNTIME_CLASS` | `gvisor` | RuntimeClass for sandbox pods |
| `EXECD_IMAGE` | `docker.io/opensandbox/execd:release-1.2.0` | execd injected into every sandbox |
| `HELPER_IMAGE` | `srvm:dev` | Image providing `/srvm-snap` (same image as srvm) |
| `POOL_SIZE` | `2` | Warm pods per image (pool created on first use of an image) |
| `SANDBOX_CPU` / `SANDBOX_MEMORY` | `500m` / `1Gi` (kind manifest: `1` / `2Gi`) | Size of every sandbox (resource classes are on the roadmap) |
| `SANDBOX_DISK_REQUEST` / `SANDBOX_DISK_LIMIT` | `512Mi` / `6Gi` | Ephemeral-storage reserved / hard cap (workspace + snapshot staging). Keep the request small. |
| `DEFAULT_EGRESS` | `deny` | Egress when a create has no `networkPolicy` (`deny` = DNS + object store only) |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_SECURE` | in-cluster rclone | Snapshot storage. The endpoint must be reachable **from sandboxes** (presigned URLs). |
| `OBJECT_STORE_PORT`, `OBJECT_STORE_APP_LABEL` | `8080`, `s3` | Baseline NetworkPolicy egress to the object store pods |
| `PUBLIC_ENDPOINT` | request `Host` | `host[:port]` clients use to reach srvm (returned in endpoint URLs; no scheme) |
| `SHUTDOWN_GRACE_SECONDS` | `90` | Pod `terminationGracePeriodSeconds`. When the cluster shuts a sandbox pod down (drain, eviction), the preStop hook holds it this long while srvm saves the workspace. Larger workspaces need more; it also slows node drains. |
| `NODE_LOSS_SECONDS` | `30` | Sandbox pods on a NotReady/unreachable node are evicted after this (Kubernetes default 300). srvm then falls back to the last snapshot. |
| `LISTEN` | `:8080` | Listen address |

**agent-sandbox label allowlist** (required; `deploy/kind/agent-sandbox-config.yaml`):
```yaml
apiVersion: v1
kind: ConfigMap
metadata: { name: agent-sandbox-config, namespace: agent-sandbox-system }
data:
  allowed-label-domains: |
    srvm.io
    sandbox.users.io
```
Without it, claims fail with `InvalidMetadata`, and srvm marks the sandbox `Failed` with that message.

## 4. Operating: tenants, keys, usage
```bash
srvm-admin() { kubectl -n srvm-system exec deploy/srvm -- /srvm admin "$@"; }   # a function works in bash and zsh

srvm-admin create-tenant search-agents 20   # 1. create the tenant (20 = max concurrently running sandboxes → 429)
srvm-admin create-key search-agents         # 2. mint a key: prints {"keyId","apiKey"} ONCE; hand apiKey to the team
srvm-admin list-keys search-agents          #    [{"keyId","tenant","createdAt","revoked"}]: the key itself is never shown again
srvm-admin revoke-key key_1a2b3c4d          #    immediate on the serving replica (≤ 30 s auth cache elsewhere)
srvm-admin usage                            #    per-tenant event counts (created/started/paused/resumed/terminated/expired/failed)
```
**Rotating a key:** mint a new key, hand it over, then revoke the old `keyId`. A tenant can hold several active keys at once.
**Lost key:** keys are stored only as SHA-256 hashes and can't be recovered. Mint a new one and revoke the old one.
**Minting for a tenant that doesn't exist** → `404 TENANT_NOT_FOUND`: create the tenant first.

The same operations over HTTP (admin token from the `srvm` secret; the admin API is disabled if `ADMIN_TOKEN` is empty):
```bash
T=$(kubectl -n srvm-system get secret srvm -o jsonpath='{.data.ADMIN_TOKEN}' | base64 -d)
curl -XPOST -H "X-SRVM-ADMIN-TOKEN: $T" localhost:18081/admin/v1/tenants -d '{"id":"search-agents","maxRunning":20}'
curl -XPOST -H "X-SRVM-ADMIN-TOKEN: $T" localhost:18081/admin/v1/tenants/search-agents/keys
curl        -H "X-SRVM-ADMIN-TOKEN: $T" localhost:18081/admin/v1/keys?tenant=search-agents
curl -XDELETE -H "X-SRVM-ADMIN-TOKEN: $T" localhost:18081/admin/v1/keys/key_1a2b3c4d
```

## 5. Troubleshooting
| Symptom | Check |
|---|---|
| Create stays `Pending` / resume stays `Resuming`, SDK times out | `kubectl -n srvm-sandboxes get sandboxclaims` shows Ready reason/message. `InvalidMetadata` → label allowlist (§3). Pending pods → node full (`kubectl describe pod`). srvm doesn't yet surface unschedulable reasons in `status.message` (roadmap #2). |
| Sandbox `Failed` | `GET /v1/sandboxes/{id}` → `status.message` has the failing stage (`claim`, `wait-ready`, `execd-bind`, `network-policy`, `hydrate`) |
| Pause returns to `Running` with a message | Snapshot failed but nothing was lost (the pod was kept). Check object-store reachability from the sandbox namespace and `S3_*`. |
| `Paused` with "pod was shut down by the cluster … workspace files were saved" | A drain, eviction or scale-down hit the pod and srvm saved it in the grace period. Resume to continue; processes must be restarted. |
| `Paused`/`Failed` with "pod no longer exists" or "pod was replaced" | The pod died without warning (node crash, force delete). `Paused`: resumes from the last snapshot; changes since then are lost. `Failed`: it was never paused, nothing to restore. |
| `Running` with "sandbox container restarted (OOMKilled …)" | The container restarted inside the same pod. Files are intact; processes were lost. Consider a larger `SANDBOX_MEMORY`. |
| Node drain takes ~90 s per node | Expected: armed sandbox pods are held while their workspace is saved. Pause sandboxes before draining to make it fast. |
| Pods can't schedule even though CPU/memory are free | Ephemeral-storage requests. Keep `SANDBOX_DISK_REQUEST` small. |
| Cold creates for a new image | Expected on first use: a pool is created, then later creates are warm. Pre-pull hot images on sandbox nodes. |
| After Docker / node restart | The cluster comes back on its own. Re-run your port-forward. Operations in flight during the outage show `Failed` ("interrupted by server restart"); create or resume again. On kind, Postgres data survives a restart but not a pod *recreation* (emptyDir). |
| Logs | `kubectl -n srvm-system logs deploy/srvm` (JSON). `sandbox running` / `sandbox paused` lines carry claim, bind, hydrate, pack and upload timings. |

---

## 6. Users: getting started
You need **an API key** (ask the platform team) and the srvm address.
```bash
pip install opensandbox==1.2.0          # the official OpenSandbox SDK; nothing srvm-specific to install
export SRVM_DOMAIN=srvm.internal.example.com SRVM_API_KEY=srvm_…
```
```python
import os
import time
from datetime import timedelta
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import NetworkPolicy

cfg = ConnectionConfigSync(domain=os.environ["SRVM_DOMAIN"], api_key=os.environ["SRVM_API_KEY"],
                           use_server_proxy=True)       # always use the server proxy with srvm

# Ephemeral code execution
sb = SandboxSync.create("python:3.12-slim", connection_config=cfg, env={"MODE": "test"})
print(sb.commands.run("python -c 'print(6*7)'").logs.stdout[0].text)          # 42
sb.files.write_files([WriteEntry(path="/workspace/data.csv", data="a,b\n1,2\n")])
print(sb.files.read_file("/workspace/data.csv"))
sb.kill()

# Persistent workspace: keep files under /workspace
ws = SandboxSync.create("python:3.12-slim", connection_config=cfg, timeout=timedelta(hours=8),
                        network_policy=NetworkPolicy(default_action="allow", egress=[]))   # internet for pip
ws.commands.run("pip install --user requests")      # HOME=/workspace, so user installs persist across pause
ws.pause()                                             # returns immediately; the snapshot runs in the background
while ws.get_info().status.state != "Paused":          # wait before resuming (resume during Pausing → 409)
    time.sleep(0.5)
ws = SandboxSync.resume(sandbox_id=ws.id, connection_config=cfg, resume_timeout=timedelta(seconds=120))
ws.renew(timedelta(hours=8))                           # extend expiry
ws.kill()                                              # deletes the sandbox and its snapshots
```

**Go** (official OpenSandbox Go SDK, unmodified; tested: `tests/e2e/go`):
```bash
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go@v1.2.0   # module path still says alibaba/ (repo moved); resolves fine
```
```go
cfg := opensandbox.ConnectionConfig{Domain: os.Getenv("SRVM_DOMAIN"), APIKey: os.Getenv("SRVM_API_KEY"),
    UseServerProxy: true}                                   // always use the server proxy with srvm
sb, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
    Image: "python:3.12-slim", Env: map[string]string{"MODE": "test"}})
ex, err := sb.RunCommand(ctx, "python -c 'print(6*7)'", nil)   // ex.Stdout[0].Text == "42\n"
err = sb.Pause(ctx)                                          // async: poll sb.GetInfo until opensandbox.StatePaused
sb, err = opensandbox.ResumeSandbox(ctx, cfg, sb.ID(), opensandbox.ReadyOptions{Timeout: 2 * time.Minute})
err = sb.Kill(ctx)
```
TypeScript, Java/Kotlin and C# SDKs exist too (same API). They should work through the same contract but are **not tested against srvm yet**.

**Rules of the road:**
| Topic | Behaviour |
|---|---|
| Isolation | Your code runs in gVisor as uid 1000 with no cluster credentials. Other sandboxes, cloud metadata and the K8s API are unreachable. |
| Network | Default is **no internet** (DNS only). Ask for it per sandbox: `network_policy=NetworkPolicy(default_action="allow", egress=[])`. Allow rules accept IPs/CIDRs. Domain names (FQDN) are not supported yet and return `400`. |
| What survives pause | **Only `/workspace`** (also your `HOME`, so `pip install --user` / `npm` caches persist). Running processes, memory and files elsewhere don't. Put system packages in your image. |
| Speed | Creates are ~0.3 s once an image has a warm pool; the first create for a new image is slower (cold). Resume takes about 1 s per 100–300 MB of workspace. |
| Pause / resume are async | `pause()` returns at once (state `Pausing` → `Paused`). Wait for `Paused` before `resume()`. A resume while `Pausing` gets `409` naming the current state. `resume()` itself waits until the sandbox is reachable. |
| Expiry | `timeout` (default 10 min in the SDK) is a hard expiry. Extend with `renew()`. Expired sandboxes and their snapshots are deleted. |
| Quotas | Too many running sandboxes → `429` with `Retry-After`. Pause or kill idle ones. |
| Many sessions | Any number of concurrent commands and file operations share one sandbox (one pod). For parallel, isolated work, create more sandboxes. |
| Not supported yet | `volumes`, `snapshotId`, per-sandbox `resourceLimits` (fixed size), credential vault, FQDN rules, running `entrypoint` (sandboxes are API-driven; start long-running processes with `commands.run`). |
