# srvm: Tests & validation

> Everything below was run on **2026-11-11** on a local kind cluster: Docker Desktop on an Apple M3 Pro, 11 CPUs, ~8 GB VM memory,
> Kubernetes v1.35.0, gVisor `release-20261026.0`, agent-sandbox `v1.0.10`.
>
> **What kind numbers mean:** functional results (isolation, compatibility, data integrity, SDK compatibility) are valid. Latency, ceilings and density reflect a laptop VM,
> not a production cell. The capacity run on a dedicated GKE cluster is on the [roadmap](roadmap.md).

## 1. Automated tests (in the repo)
| Suite | Where | Runs | Result |
|---|---|---|---|
| Go unit tests | `internal/*/…_test.go`, `cmd/srvm-snap/main_test.go` | `make test` | **33 passed** |
| Postgres integration | `internal/store/store_test.go` (skipped without `TEST_DATABASE_URL`) | see below | **4 passed** |
| End-to-end SDK contract (Python) | `tests/e2e/test_sdk_contract.py` (unmodified `opensandbox==1.2.0`) | see below | **7 passed** (50 s) |
| End-to-end pod interruption | `tests/e2e/interruption.py` (needs `kubectl` + `docker`; stops a kind node) | `SRVM_DOMAIN=… SRVM_API_KEY=… python tests/e2e/interruption.py` | **9/9 pass**: eviction save, node-crash fallback, pod protection (§6) |
| End-to-end smoke (Go) | `tests/e2e/go` (unmodified Go SDK `v1.2.0`) | `cd tests/e2e/go && SRVM_DOMAIN=… SRVM_API_KEY=… go run .` | **pass**: create, exec, env, pause, resume (0.43 s, file + env intact), kill |

What they lock in:
- **Isolation invariants (`cell`):**
  - gVisor RuntimeClass, non-root, no SA token, no privilege escalation
  - execd waits for binding; small disk request
  - baseline policy: ingress only from the system namespace, egress only DNS + object store
  - "allow" never opens private ranges or metadata
  - template names are valid and stable
  - shutdown handling: 90 s grace + preStop hold, 30 s node-loss eviction, `safe-to-evict: "false"` on claimed pods,
    terminating pods reported by the watch
- **Network-rule validation (`lifecycle`):** CIDR and IP allowed; FQDN, wildcard and explicit-deny rejected with a clear error.
- **Pod verdicts (`lifecycle`):** healthy / gone / terminating / failed / replaced / container restarted in place.
- **execd client (`execd`):**
  - the token is sent only as a `sha256:` hash
  - binding waits for `/ready`; a `400` fails fast
  - SSE parsing (with and without the `data:` prefix), non-zero exit codes
- **Snapshot tool (`srvm-snap`):**
  - 70 MiB multi-chunk round trip (byte-identical, symlinks, dotfiles)
  - temp files cleaned up; chunk-count mismatch rejected
  - **path-traversal in a malicious snapshot rejected**
  - `prestop` returns at once for unarmed pods, holds while armed, gives up at its deadline
- **API contract (`api`):**
  - the fields the SDK requires (`id`, `status.state`, `entrypoint` as a list, `createdAt`, `image.uri`)
  - error mapping (404 / 409 / 429 + `Retry-After` / 400 / 500)
  - missing key → 401; admin API closed without an admin token
- **Store (Postgres):**
  - keys hash-only and revocable
  - **10 concurrent resumes → exactly 1 winner**
  - **10 concurrent creates with quota 3 → exactly 3 created, 7 rejected**
  - tenant isolation on get/list
- **E2E (SDK → srvm → agent-sandbox → gVisor):**
  - create/exec/env/files
  - warm create < 10 s bound
  - bad key rejected; FQDN rules rejected
  - network isolation for deny and allow profiles, with a listening-peer control
  - **pause/resume preserves a marker, a 5 MB checksum, a symlink and env**
  - list/delete

Run them:
```bash
make test                                                          # unit
kubectl -n srvm-system port-forward svc/postgres 15432:5432 &
TEST_DATABASE_URL="postgres://srvm:$(kubectl -n srvm-system get secret srvm-postgres -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)@127.0.0.1:15432/srvm?sslmode=disable" go test ./internal/store/
kubectl -n srvm-system port-forward svc/srvm 18081:80 &
pip install opensandbox==1.2.0 pytest
SRVM_DOMAIN=127.0.0.1:18081 SRVM_API_KEY=srvm_… pytest -q tests/e2e
SRVM_DOMAIN=127.0.0.1:18081 SRVM_API_KEY=srvm_… python tests/e2e/interruption.py   # disruptive: stops the sandbox node
```
Bugs these tests caught during the build:
1. **`entrypoint` serialized as `null`** for sandboxes created without one. A typed-nil slice in Go; the SDK needs a list.
2. **A rejected claim made sandboxes hang** for the full 5-minute wait. It now fails fast with the cause.
   This was found by e2e and is the reason for the agent-sandbox label allowlist requirement.
3. **A crashed node looked healthy for 5 minutes:** Kubernetes keeps its pods `Running` until eviction (300 s default).
   Found by the node-crash e2e; sandbox pods now tolerate node loss for 30 s.
4. **A retried pause could corrupt its snapshot:** leftover chunks from a failed attempt would be read on hydrate.
   Found in review; the prefix is now cleared before upload.

## 2. Substrate bake-off: agent-sandbox + gVisor (`bakeoff/`, results in `bakeoff/results/kind-20261111/`)
| Check | Result | Target |
|---|---|---|
| Warm claim → Ready (30 claims) | p50 **16 ms**, p99 **65 ms**, 0 failures | < 1 s ✅ |
| First `kubectl exec` | p50 104 ms | — |
| Cold claim (image pre-pulled, 10 claims) | p50 **2.0 s**, p99 3.0 s, 0 failures | < 15 s ✅ |
| Escape suite | **15/15**, including a listening-peer control | 100% ✅ |
| gVisor compatibility (11 steps vs runc) | **11/11 pass** | all ✅ |
| Resume of 100 MB / 500 MB / 1 GB / **2 GB** (tar+zstd, local S3) | 1.2 / 2.4 / 5.5 / **9.0 s**, all intact | 2 GB < 10 s ✅ (marginal; real object storage will be slower) |
| Burst beyond the pool | Within pool ~60 ms; beyond pool cold ~1.5 s; 25 at once → node full, 11 failures | — |
| Ramp (2/s, hold 10 s) | Node capacity exhausted: claim p99 **115 s** while API-server probe p99 stayed **13 ms** | Ceiling not measurable on kind |

gVisor vs runc slowdown:
- **1.1–1.3×:** pip, npm, sklearn, pandas
- **1.5–1.9×:** git clone, sqlite, pytest, 1 GB write
- **~4×:** 20k small files, git metadata ops

The resume bottleneck is unpack under gVisor (~80% of 2 GB resume), not the network.

Findings that changed the design:
- **Disk reservation caps density before CPU/memory.**
  - With only an ephemeral-storage *limit* (6 GiB), K8s reserves the limit, so 9 sandboxes filled a 59 GiB node.
  - The fix is a small request (512 MiB) plus the limit as the cap; this is now in the pod template and a unit test.
- **Without admission control, an exhausted cell makes callers hang for minutes** → roadmap #2.
- **agent-sandbox cold-starts a claim on an empty pool** (~2 s). New images work without a pre-warmed pool.

## 3. Comparison: OpenSandbox 1.2.0 server on the same cluster (gVisor)
We evaluated running OpenSandbox's own server before writing srvm. These observations are specific to **version
1.2.0 on gVisor** in our kind setup; OpenSandbox also targets runc and VM runtimes, where several of them don't apply.
They are why srvm reuses OpenSandbox's API, SDKs and execd but not its server (architecture D3).

| Check | OpenSandbox 1.2.0 | srvm (same tests) |
|---|---|---|
| SDK end to end | ✅ | ✅ |
| API-key auth (none / wrong / right) | 401 / 401 / 200 | 401 / 401 / 200 |
| Cold create p50 / p99 | 1.9 / 2.8 s | 3.0 s (cold), **0.24–0.30 s warm** |
| Escape suite (default config) | **10/13**: K8s API, internet and peer sandboxes reachable (its egress controls are unavailable on gVisor, see next row) | **13/13** |
| `networkPolicy` on gVisor | **Rejected**: "gVisor does not support the nftables rules required by the egress sidecar" | Accepted; enforced at the node |
| 2 GB pause / resume | 64 s pause; resumed pod came up, **workspace files not restored** | **16.5 s / 7.6 s, 37,395 files intact** |

**Why the workspace wasn't restored** (from reading its code; to be reported upstream):
- Pause commits the container rootfs. Under gVisor the committed layer holds only `.gvisor.filestore.<hash>` (3 GiB, opaque), not the files.
- Its snapshot API explicitly refuses gVisor (`services/k8s/snapshot_runtime.py:155`).
- Its pause path (`kubernetes_service.py:1527`) doesn't run that guard.

Other observations:
- Pooled sandboxes can't use networkPolicy, snapshots, volumes or the credential vault (per its spec).
- One kubeconfig per server; docs say "keep one active Lifecycle Server".
- No warm pools when it runs on the agent-sandbox backend (it creates `Sandbox` objects directly).

## 4. srvm via the unmodified SDK (results in `bakeoff/results/kind-20261111-srvm/`)
| Check | Result |
|---|---|
| Create → ready, warm (SDK, end to end) | **0.24–0.30 s** (claim ~60 ms, execd bind ~20 ms) |
| Create → ready, pool exhausted | 3.0 s (SDK retries `404 POD_IP_NOT_AVAILABLE` as designed) |
| Env at create | Delivered via execd binding, no cold start |
| Escape suite, default and deny-all profiles | **13/13** each |
| Allow profile | Internet 200; K8s API, metadata, peer sandbox **blocked** |

Pause/resume, all intact:

| Workspace | Pause | Resume | Hydrate | Files before → after |
|---|---|---|---|---|
| 100 MB | 2.7 s | 0.9 s | 0.7 s | 1,852 → 1,852 |
| 1 GB | 8.2 s | 9.6 s | 9.4 s | 18,710 → 18,710 |
| 2 GB | 16.5 s | 7.6 s | 7.3 s | 37,395 → 37,395 |

**Stock public image** (`python:3.12-slim`, unmodified):
- First create (image pull + new pool) 3.3 s; next create **0.25 s** (warm).
- `pip install --user requests` (egress allow), then pause and resume (**0.5 s**): package still installed, because `HOME=/workspace`.
- An earlier run of the same flow got stuck in `Resuming`: with 2 GiB sandboxes and 2 warm pods per image, the 8 GB node was full and the resumed
  pod was unschedulable. kind sizing was reduced (1 GiB, pool 1). The silent wait is a product gap → roadmap #2.

Note: generating the 2 GB test workspace inside a 2 GiB-memory gVisor sandbox took 262 s (38 s in the substrate bake-off).
That's a sizing signal for write-heavy workloads in small sandboxes, not part of pause/resume.

## 5. Unplanned outage (Docker Desktop stopped)
The kind cluster went down with Docker mid-session.
- **After restart:** nodes, gVisor (`runsc` + containerd config), agent-sandbox, Postgres (keys intact) and both warm pools came back
  without intervention.
- **Gap found:** five rows were stuck in `Pending`/`Resuming` with nothing driving them. They came from integration tests, but a real
  interrupted create behaves the same and would hold tenant quota forever.
- **Fixes:**
  - startup recovery (see roadmap #1)
  - `Recreate` deployment strategy (no overlapping srvm pods)
  - integration tests now clean up their rows
- **After the fixes:** both quickstarts pass (Python 0.28 s warm, Go 2.05 s cold refill), and the stuck rows are `Failed` with
  "pending interrupted by server restart; create or resume again".

## 6. Pod interruption handling (`tests/e2e/interruption.py`, kind, 200 MB random workspace)
| Case | Result |
|---|---|
| Claimed pod protection | `safe-to-evict: "false"` and `srvm.io/bound-restarts` set on the claimed pod |
| **Eviction** (what a node drain does) | `Paused` **2.0 s** after eviction (emergency save: pack 0.4 s, upload 1.4 s); resume → marker and 200 MB sha256 intact |
| **Node crash** (`docker stop` of the only sandbox node, no preStop) | `Paused` on the last snapshot **94.5 s** after the crash ("no route to host" on save); after the node returns, resume → rolled back to the last snapshot as designed (post-snapshot file gone, blob intact) |
| `kubectl delete pod --force --grace-period=0` | **Not a crash:** the kubelet still runs preStop and srvm saved the workspace (1.2 s). Use a node stop to test loss. |
| Regression | SDK contract 7/7, Python + Go quickstarts pass |

- **Crash detection time** is node-monitor grace (~40 s) + `NODE_LOSS_SECONDS` (30, down from Kubernetes' 300) + up to
  10 s reconcile. Before this change srvm would have reported `Running` for 5+ minutes.
- **Unit tests:** `srvm-snap prestop`/`guard`, pod template (grace 90 s, preStop deadline 85 s, 30 s node-loss
  tolerations), `MarkBound`, `WatchTerminating`, `podVerdict` (healthy / gone / terminating / failed / replaced /
  restarted-in-place).
- **Not yet exercised on the cluster:** an in-place container restart (OOM) → re-bind. Covered by unit tests only.
- **Existing warm pods** keep the old spec until used; templates update in place on the next create (`srvm.io/template-rev`).

## 7. OpenSandbox console against srvm (upstream `console/` at ff720a6, unmodified; srvm unmodified)
Built for arm64 from source, deployed to namespace `srvm-console` (`deploy/kind/console/console.yaml`), driven
through its backend's `/api/*` with curl (not clicked through in a browser).

| Feature | Result |
|---|---|
| Tenant login (demo-team key) | ✅ the console checks its own `tenants.toml`, then forwards the key to srvm |
| List / filter / overview, details, renew, delete | ✅ |
| Create (the form's exact body) | ✅ Running in ~1 s; the form's `resourceLimits` is silently ignored |
| Shell | ✅ browser WebSocket → console → srvm `proxy/44772` → execd PTY; kernel `4.19.0-gvisor` |
| Pause / resume | ✅ file in `/workspace` intact after resume on a different pod |
| Endpoints | ✅ but shows the in-cluster host (srvm's `PUBLIC_ENDPOINT` unset) |
| Admin view | ✅ mostly: uses the tenant keys from `tenants.toml`, never srvm's `/admin/v1` |
| Logs / events tabs | ❌ 404: no `diagnostics/logs`, `diagnostics/events` in srvm |
| Snapshots, pools, version | ❌ 404: no `/v1/snapshots`, `/v1/pools`, `/version` in srvm |

Resource use: backend ~73 MB, web ~10 MB. Open: `kubectl -n srvm-console port-forward svc/console-web 18090:80`
→ http://localhost:18090/login (port 18090 is the only allowed CORS origin).

## 8. What is NOT validated yet
| Gap | How it will be closed |
|---|---|
| Per-cell claim ceiling, density, real object-storage resume | GKE capacity run (roadmap #2) |
| srvm with 2+ replicas; crash *during* a pause/resume with live traffic | Work queue + chaos tests (roadmap #1). Startup recovery is validated only on rows left by tests. |
| Admission control behaviour under overload | Roadmap #3 + load test |
| FQDN egress, credential vault | Cilium cell (roadmap §3) |
| TypeScript / Java / C# SDKs against srvm | Add smoke tests like `tests/e2e/go` |
| Kata / VM isolation | Separate trial on GKE nested virtualization or bare metal |
| Long-running soak (leaks, orphan reconciliation at scale) | 24 h soak with mixed create/pause/resume |
