# srvm: Architecture & key decisions

> Status: experimental; validated end to end on kind (Nov 2026). Evidence for every decision: [validation.md](validation.md).
> What's next: [roadmap.md](roadmap.md). How to run it: [operations.md](operations.md).

**srvm** is a platform for running **untrusted code** (LLM/agent-generated and user code) in isolated sandboxes on
Kubernetes. It is **sandbox-first** (Daytona-style: long-lived, stateful, driven through an API), and it exposes the
**OpenSandbox API**, so the unmodified OpenSandbox SDKs, CLI and MCP server work against it.

## The founding constraint: runs on any Kubernetes cluster
srvm must run on any conformant Kubernetes cluster (GKE, EKS, OCI, on-prem, and kind on a laptop) using only
standard Kubernetes building blocks, with no special hardware and no cloud-specific services in the core. Most
decisions below follow from this:

| Decision | Because of portability |
|---|---|
| gVisor, not Kata or Firecracker (D5) | Needs no KVM or nested virtualization; VM runtimes are later opt-in pools on KVM-capable nodes |
| agent-sandbox for pods and warm pools (D4) | A Kubernetes SIG project built on standard CRDs; not Nomad, a custom node agent or a hosted service |
| Workspaces as snapshots in S3-compatible storage (D7) | Every cloud and on-prem has an S3 API; no zonal disks (PVCs) or cloud-specific filesystems |
| Plain NetworkPolicy for isolation (D8) | Enforced by any policy-capable CNI; Cilium-only features (FQDN rules) are additive |
| Postgres as the only database (D6) | Available managed everywhere, or as a pod |
| Cells, one per cluster (D10) | Growth and multi-cloud by adding clusters, not by depending on one provider's scaling |

What "any cluster" still requires: a CNI that enforces NetworkPolicy, gVisor (`runsc`) on the sandbox nodes
(built into GKE as GKE Sandbox; installed on the node image elsewhere), and an S3-compatible endpoint the sandboxes
can reach. The one planned exception is a host-agent runtime with Firecracker (roadmap §2), which would need
KVM-capable nodes; it is an opt-in tier for when scale or speed demands it, not a requirement of the core.

## Requirements that drove the design
| Requirement | Target |
|---|---|
| Untrusted code | Kernel-level isolation, no route to cluster/cloud credentials, other sandboxes or the K8s API |
| Fast create | p99 < 1 s on a warm hit; p99 < 15 s cold |
| Ephemeral code execution **and** persistent workspaces | Pause/resume with files intact; compute released while paused |
| Portable | Any conformant K8s (GKE, EKS, OCI, on-prem); see the founding constraint above |
| Operable | Few components; stateless except Postgres + object storage |
| Tenancy | Internal teams first; external later without re-architecture |
| Scale | Horizontal (cells) and vertical (tuning, then a host-agent runtime) |
| Functions later | Modal-style `@function` / `.remote()` / `.map()` on the same primitives (D1) |

**Non-goals for v1:** GPU sandboxes (deferred; nothing in the design prevents them), building images for users (bring
your own OCI image), VM-grade isolation (a later second runtime class, D5), billing and self-serve signup (metering
exists from day one), and browser/VNC desktops until a computer-use need appears.

## Architecture
```
 OpenSandbox SDKs (py/ts/go/java/c#) · CLI · MCP            ← reused unmodified
                │  OpenSandbox Lifecycle API + execd API (header OPEN-SANDBOX-API-KEY)
                ▼
 ┌──────────────────────── srvm (one Go binary, stateless) ────────────────────────┐
 │ api        lifecycle routes · API-key auth · admin API (tenants/keys)            │
 │ proxy      streams exec/files/PTY to execd; injects per-sandbox execd token,      │
 │            strips the API key                                                     │
 │ lifecycle  state machine (CAS + generation fencing) · async create/pause/resume · │
 │            expiry · orphan reconciliation · quotas                                │
 │ cell       the only code that knows sandboxes are pods                            │
 └───────┬──────────────────────────────────────────────┬───────────────────────────┘
         │ Postgres: tenants, keys, sandboxes, usage     │ K8s API (one cell = one cluster)
         ▼                                               ▼
 ┌──────────── cell (any K8s cluster) ──────────────────────────────────────────────┐
 │ agent-sandbox controller: SandboxTemplate + SandboxWarmPool per image,           │
 │                           SandboxClaim per sandbox                               │
 │ sandbox pod (gVisor, uid 1000, no SA token):                                     │
 │   execd (OpenSandbox, waits for binding) · srvm-snap (snapshot tool) · user image│
 │ NetworkPolicy: baseline deny + per-sandbox egress                                │
 └──────────────────────────────────┬───────────────────────────────────────────────┘
                                    │ presigned URLs (no credentials in the sandbox)
                                    ▼
                 S3-API object storage: /workspace snapshots
```

**Create (warm path, ~0.3 s end to end via the SDK):**
1. Check auth and quota, then insert the sandbox as `Pending`. The API returns `202`.
2. Asynchronously: create a `SandboxClaim`, and agent-sandbox hands over a ready pod (~60 ms).
3. Call `POST /internal/init` on execd with the sandbox id, the hash of a new per-generation token, and the env (~20 ms).
4. Apply the sandbox's NetworkPolicy and mark it `Running`.
5. Meanwhile the SDK polls `GET endpoint`; srvm answers `404 POD_IP_NOT_AVAILABLE` (which the SDK retries) until `Running`.

**Pause:**
1. Run `srvm-snap pack` via execd: `/workspace` becomes tar+zstd, split into 64 MiB chunks.
2. Presign PUT URLs; `srvm-snap upload` sends the chunks.
3. Record `Paused` with the snapshot prefix, then release the claim. The pod is deleted, never reused.

**Resume:**
1. Claim *any* warm pod from the template and bind it with generation+1.
2. Presign GET URLs; `srvm-snap hydrate` restores `/workspace`.
3. Mark `Running`.

---

## FAQ
**Why build this instead of using what OpenSandbox has?** We do use most of it: its API, SDKs and in-sandbox daemon
(execd), unmodified, and its console works against srvm. We replaced only its server, the part that manages pods,
auth, pause/resume and networking, because on gVisor + agent-sandbox it didn't meet our needs (next answer). Clients
can't tell the difference. → D2, D3

**Why not OpenSandbox's server?** With v1.2.0 on gVisor: no warm pools on the agent-sandbox backend, pause didn't
restore the workspace, `networkPolicy` is rejected (10/13 escape checks), and it runs as one replica per cluster.
srvm: 0.3 s warm create, 2 GB pause/resume intact, 13/13. → D3, validation.md §3

**Why agent-sandbox underneath?** It's the Kubernetes SIG project for exactly this (sandbox pods + warm pools),
measured at 16 ms p50 warm claims, portable to any cluster, and it does nothing else, so the product logic stays
ours. → D4

**Why gVisor and not Firecracker?** gVisor runs on any Kubernetes node; Firecracker needs KVM (bare metal or
nested virtualization). Once scale or speed matters, we move to Firecracker via a host agent; Kata is the earlier
step if only a VM boundary is needed. → D5, roadmap §2, [firecracker.md](firecracker.md)

**What happens to a sandbox if its pod dies?** Drains and evictions: the workspace is saved during shutdown. Crashes:
it falls back to the last snapshot. Running processes never survive. → D7

**Could Envoy replace srvm?** Only its proxy part. The plan is Envoy in front for TLS and streaming, asking srvm
(via `ext_authz`) where each sandbox lives. → roadmap §4

## Key decisions

### D1. Sandbox-first; functions later as a layer
The primitive is a sandbox built from **template (image)**, **warm pool** and **exec**. A Modal-style functions layer
becomes "claim a warm sandbox → exec the entrypoint → return the result → recycle", with no new primitive.
*Rejected:* functions-first, because turning a stateless function engine into stateful sandboxes is harder than the reverse.

### D2. Public contract = the OpenSandbox API; reuse its SDKs unmodified
srvm implements OpenSandbox's lifecycle spec (subset) and proxies its execd spec (1.0.0) verbatim.
- **Why:** five SDKs, CLI, MCP and a console exist and are Apache-2.0. Users get a known API, and the backend can
  change without touching user code.
- **Risk:** the lifecycle spec is 0.1.0 with no stable v1 planned. **Mitigation:** pin the spec version, run e2e SDK contract tests in CI,
  and add extensions only additively.

### D3. Our own server (one lightweight component) instead of OpenSandbox's server and gateway
srvm is one Go binary: control plane, API-key auth and data-plane proxy.

Why we didn't run OpenSandbox's server: for our combination (gVisor + agent-sandbox) version 1.2.0 didn't fit.
Measured or read in its code (validation.md §3); much of it doesn't apply on runc or VM runtimes:
- On the agent-sandbox backend it creates a fresh pod per sandbox (no warm pools).
- On its own backend under gVisor, pause/resume **didn't restore the workspace**: the commit captures gVisor's opaque
  overlay file. Its snapshot API refuses gVisor; its pause path lacks that guard (to be reported upstream).
- It **rejects networkPolicy on gVisor** (its egress sidecar needs nftables).
- One cluster per server and a single active replica; quotas delegated to ResourceQuota (failures surface as timeouts).

Why we didn't run its gateway in front:
- API-key auth lives inside its Python server.
- Its gateway doesn't inject execd tokens and isn't aware of srvm's states (`Pending` → retryable 404, `Paused` → reject).

**The data path never goes through the Kubernetes API.** Exec, file and PTY traffic is high-volume streaming, so
srvm proxies it straight to the pod IP (never `kubectl exec` or the API-server proxy, which would load the API
server and put cluster credentials on the data path). execd accepts only srvm's per-sandbox token. Consequence:
srvm needs network reachability to pod IPs in every cell it serves.

Why one process: the proxy needs lifecycle state on every request (current pod IP, per-generation token, state,
tenant). A separate gateway would need its own synced copy plus an extra hop, for no gain at one cell's scale.
Splitting is mechanical (separate packages) and is on the roadmap with explicit triggers.

### D4. Substrate = kubernetes-sigs/agent-sandbox warm pools; bind after claim
One `SandboxTemplate` + `SandboxWarmPool` per image (created on first use), and one `SandboxClaim` per sandbox
generation (deterministic name).
- **Warm pods are generic.** Everything per-sandbox (identity, execd token hash, env) is pushed **after** the claim via
  execd's `POST /internal/init`. A running pod's env and volumes can't be changed, so per-sandbox env or volumes on
  the pod itself would rule out every warm pod and force a cold start.
- Per-sandbox labels go through the claim's `additionalPodMetadata`. agent-sandbox requires their domain to be allowlisted
  (`allowed-label-domains: srvm.io`); this is an install requirement.
- **Why agent-sandbox:**
  - **Fast:** warm claim p50 16 ms / p99 65 ms measured on kind; ~0.3 s end-to-end create through the SDK.
  - **Standard:** an official Kubernetes SIG project (Apache-2.0, v1beta1 API) that runs on any conformant cluster,
    so the portability requirement holds.
  - **Narrow:** it only manages pods and warm pools. Tenancy, quotas, snapshots and network policy stay in srvm.
- *Rejected:*

  | Option | Why not |
  |---|---|
  | OpenSandbox's BatchSandbox/Pool operator | Pool mode excludes networkPolicy, snapshots and volumes |
  | Our own warm-pool controller | Rebuilds a maintained SIG project (~1k lines: refill, claim races, orphans) for the same API-server ceiling. The fallback if agent-sandbox stalls. |
  | Host agent on each node, no Kubernetes for sandboxes (E2B/Modal style) | Fastest and densest, enables memory snapshots, but a large build (scheduling, networking, images, node failure). Kept as tier 4 (D10). |
  | Nomad or raw containerd | Gives up portability: every target already runs Kubernetes |
  | Hosted sandboxes (E2B, Daytona, Modal) | Not multi-cloud; untrusted code and internal data at a third party |

  Switching substrate means a new `internal/cell` package; lifecycle, API and storage don't change.

### D5. Isolation = gVisor RuntimeClass; Kata as a later second class
- **Why gVisor:** portable (no KVM / nested virtualization), and validated with 13/13 escape checks and 11/11 workload steps.
- **Cost:** about 4× slower small-file I/O; most other workloads are 1.1–1.9×.
- **Why not Firecracker (or another microVM) now:**
  - It needs `/dev/kvm`: bare metal or nested-virtualization nodes. That rules out many managed node types, some
    clouds, and local development (Docker Desktop on macOS has no KVM).
  - On Kubernetes it comes through Kata (`kata-fc`), which also needs devmapper storage and more memory per sandbox.
  - Its real advantage over gVisor is **memory snapshots**: pause/resume that keeps running processes, in ~100 ms.
    srvm's pause saves files only (D7). If users need processes to survive pause, that is the trigger to add it.
- **Kata (`kata-clh` or `kata-fc`)** comes later as an opt-in `isolation: vm` template on bare metal or nested-virtualization pools.
  It brings a real guest kernel and a VM boundary at the cost of `/dev/kvm`, devmapper (fc) and more memory per sandbox.
  Kata adds isolation only: each sandbox is still a pod created through the Kubernetes API. When scale or speed is
  the constraint, the route is Firecracker run by a host agent (D10 tier 4); triggers are in roadmap §2.
- **Pod hardening:**
  - non-root uid 1000, no privilege escalation, all capabilities dropped
  - no service-account token, no service links
  - RuntimeDefault seccomp
  - CPU, memory and disk limits
  - tainted dedicated node pool

### D6. Postgres is the source of truth for product state; CRDs for pod state only
- **Postgres holds** tenants, keys (hashed), sandboxes and usage events. **CRDs hold** pod state only.
- **CAS state transitions:** concurrent resumes give exactly one winner (tested). There is never more than one pod per sandbox.
- **A generation number** is bumped on every (re)start and fences claims and snapshots.
- **One instance, many sessions:** any number of exec, file or shell sessions share the sandbox's single pod. Sessions
  never create pods; new pods come only from create, resume and (planned) fork. Two pods for one sandbox would each
  hydrate `/workspace` and race on upload: split-brain and silent data loss, which CAS + fencing rule out.
- **Pods are never reused.** A pod that ran a sandbox is deleted on pause or terminate, never returned to a pool, so
  no data or planted process leaks to the next tenant. Warm pools refill with fresh pods.
- *Planned:* isolation between concurrent workers comes from **fork** (a new sandbox from a snapshot, sharing
  chunks), not from multiple instances; optional advisory locks for coordinated writers.

### D7. Persistent workspace = `/workspace` snapshots in object storage
- **What persists:** `/workspace` is a local emptyDir while running. On pause it's chunked and uploaded; on resume it's
  hydrated into **any** warm pod.
- **How it moves:** `srvm-snap` is injected by an init container and moves bytes directly to and from object storage using
  short-lived presigned URLs. **No storage credentials in the sandbox**, and path-traversal is rejected on hydrate.
- **Scope:** only `/workspace` persists. Processes, memory and other paths don't.
- **Pod interruptions:** the workspace lives on the pod while running, so anything since the last pause depends on the
  pod surviving. srvm layers its defences:
  1. **Avoid:** claimed pods are annotated `safe-to-evict: "false"` so the cluster autoscaler never scales their node down.
  2. **Save on graceful shutdown** (drain, eviction, preemption with notice): a `preStop` hook holds the pod for up to
     `SHUTDOWN_GRACE_SECONDS` (90) while srvm, which watches pods, runs the normal pause. The hook only waits (the save
     is server-driven: the sandbox has no storage credentials and can't reach srvm). Result: `Paused`, "workspace
     files were saved, running processes were not".
  3. **Detect loss** (crash, OOM-killed node, forced delete): every 10 s srvm compares `Running` sandboxes with their
     pods. A gone or replaced pod → `Paused` on the last snapshot, or `Failed` if there is none. A sandbox container
     restarted *in place* (e.g. OOM) keeps its files: srvm re-binds execd and records the restart.
  4. **Bound the loss on crashes:** periodic incremental autosave (roadmap, needs incremental snapshots).
  A new snapshot never overwrites the last good one: it is written under the new generation's prefix and the sandbox
  switches to it only after a complete upload.
- **Rejected:**

  | Option | Why not |
  |---|---|
  | PVC per workspace | Forces a cold start; zonal; slow attach |
  | Rootfs → registry commit (OpenSandbox) | Didn't restore files under gVisor in our test; 64 s pause for 2 GB |
  | JuiceFS always-on | Kept as an upgrade for shared or huge workspaces |
  | gcsfuse / Mountpoint | One-object-per-file semantics break git/npm; fine for read-mostly data volumes |

### D8. Network isolation at the node (NetworkPolicy), not inside the sandbox
- **Baseline per sandbox namespace:**
  - Ingress only from the srvm system namespace.
  - Egress only to DNS and the object store.
  - Never sandbox↔sandbox, the K8s API or metadata.
- **Per-sandbox egress** (`networkPolicy.defaultAction: allow` and/or CIDR rules) is a label-selected NetworkPolicy. "Allow"
  never opens private ranges.
- **FQDN rules** need an FQDN-aware CNI (Cilium) and are rejected with a clear 400 until then.
- *Rejected:* OpenSandbox's in-pod egress sidecar, which is incompatible with gVisor.
- *Planned:* **FQDN allowlists** per template/tenant (pypi, npm, GitHub, LLM APIs) via Cilium FQDN policy, because IP
  rules can't express "pypi.org only" and CDN IPs change. **Credential vault:** secrets are injected on the way out
  by an egress proxy, never placed in the sandbox as env vars (hostile code would exfiltrate them).

### D9. Tenancy: operator-minted API keys; quotas enforced before claiming
- Tenants and API keys (SHA-256 hashed, shown once) are minted by an operator (`srvm admin …` or `/admin/v1/*` with an
  admin token). No SSO in v1.
- Per-tenant max-running quota is checked atomically before a pod is claimed: `429` + `Retry-After`, never a timeout.
- All sandboxes share a namespace per cell; isolation comes from gVisor + NetworkPolicy. *Rejected:* namespace-per-tenant,
  which fragments warm pools, and K8s ResourceQuota, which is per-namespace and fails as a timeout instead of a clean error.
- **Metering:** append-only `usage_events` from day one: showback now, billing later.
- **srvm is the authorization boundary**, so every lifecycle action needs tests and (planned) an audit log.
- *Planned for external tenants:* org → project → API keys, service accounts as first-class identities, SSO (OIDC)
  for console users, and quotas on vCPU, memory and snapshot GB, not just running count. Going external then adds
  billing, signup and abuse controls, not a re-architecture.

### D10. Scale with cells (horizontal) and per-cell tuning / host-agent runtime (vertical)
Per-cell limits:

| Limit | Value | Source |
|---|---|---|
| Sustained claims per cell | ~30/s | agent-sandbox's measured etcd/API-server ceiling |
| Running sandboxes per cell | ~10–20k | hypothesis |
| Paused workspaces | Unbounded | They live in Postgres + object storage, not the cluster |

At the limits, srvm must apply admission control instead of overloading a cell:
- a per-cell rate budget at ~70% of the measured ceiling;
- spill-over to another cell;
- interactive traffic prioritized over batch;
- a queue with a deadline;
- then `503` + `Retry-After`.

On kind we observed what happens without it: callers hung up to 115 s while the API server stayed healthy.

Scaling tiers:
1. Tune the cell.
2. Add nodes and shard pools.
3. Add up to ~6 cells.
4. A **host-agent runtime** (long-lived host pods running many sandboxes, as in OpenSandbox's FastSandbox, E2B or Modal), so K8s writes scale with nodes rather than sandboxes.

`cell_id` is in the schema from day one; v1 runs one cell. All Kubernetes-specific code sits behind `internal/cell`,
so a host-agent runtime (tier 4) replaces that package without changing the API. Paused workspaces cost no cell
capacity, so idle auto-pause (D15) directly raises effective capacity.
*Rejected:* unlimited cells (ops, quota and control-plane cost grow with each), a host-agent runtime from day one
(months of work and a privileged agent before the need is proven), and no admission control (pool exhaustion, cold
starts and retry storms cascade).

### D11. Reuse vs build
| Reused | Built |
|---|---|
| agent-sandbox (Apache-2.0): pods, templates, warm pools, claims | srvm server: API, auth, proxy, lifecycle, quotas, reconciliation |
| gVisor (Apache-2.0): isolation | srvm-snap: presigned chunked workspace snapshots |
| OpenSandbox SDKs/CLI/MCP + execd (Apache-2.0) | Network policy model (baseline + per-sandbox) |
| Kubernetes NetworkPolicy (Cilium later) | Admin CLI/API for tenants and keys |
| Postgres, any S3-API object store | |

### D12. UI and preview URLs (decided, not built)
- **Console:** a thin SPA on the public API only. Candidate: **OpenSandbox's console** (`console/` upstream, "MVP":
  React web app + FastAPI backend-for-frontend). It is a client of the standard lifecycle API (`LIFECYCLE_API_BASE`)
  and opens its shell through `/sandboxes/{id}/proxy/44772`, both of which srvm implements. Caveats from its docs:
  tenant keys are copied into a static `tenants.toml` ConfigMap (no minting/revocation, duplicates srvm's keys), and
  the pools page uses OpenSandbox-specific endpoints. **Trialled against srvm unmodified (validation.md §7): core
  tenant flows work** (list, create, details, shell, renew, pause/resume, delete); logs/events, snapshots, pools and
  version fail on endpoints srvm doesn't have.
- **Ops:** Grafana, not a custom admin UI.
- **Preview URLs:** `https://<port>-<id>.<preview-domain>` on a **separate registrable domain** (untrusted HTML must not
  share cookies with the console), with signed per-subdomain cookies.

### D13. Read-mostly data via host-side CSI mounts (decided, not built)
Model weights, datasets and corpora are too big to copy into each sandbox. They will be exposed as an OpenSandbox
`volume`, backed per cloud by the **gcsfuse CSI** or **Mountpoint-S3 CSI** driver, mounted **host-side** and read-only
by default, so bucket credentials never enter the sandbox. Volumes are declared on the template (fixed when the warm
pod is created), because per-claim volumes force a cold start.
*Rejected:* a FUSE client inside the sandbox (needs privileges and credentials inside hostile code); copying data on start.

### D14. Names scoped to (project, owner); idempotent get-or-create (decided, not built)
Integrators want to find a workspace again without storing srvm ids (e.g. `agent-{user_id}`).
- Every sandbox keeps a globally unique `id`, plus an optional `name` alias, unique per **(project, owner)**. The
  owner is the service account or user that created it, not the API key (keys rotate).
- `get_or_create(name)` is idempotent and race-safe (a unique index) and auto-resumes a paused sandbox. A spec
  mismatch on an existing name returns `409`.
- *Rejected:* global or per-project names (collisions between teams, developers and services), per-key names (break
  on rotation), ids only (every integrator builds its own name → id store).

### D15. Idle auto-pause and TTL (decided, not built)
The biggest cost lever: abandoned agent sandboxes dominate spend.
- **Idle** = no session activity **and** no tracked process (`exec`, background commands) for `idle_timeout`.
  Persistent sandboxes are paused; ephemeral ones terminated. Open-but-silent shells and untracked daemons don't
  keep a sandbox alive; anything that must run unattended is started as a tracked background command.
- **TTL** caps every running period (implemented today as `timeout` / renew-expiration).
- Activity is recorded in srvm's proxy, so it needs no agent in the sandbox.

## Security model
**Threat model:** sandbox code is hostile. It will try to escape to the node, reach cloud metadata or credentials,
attack other sandboxes or srvm itself, abuse egress (exfiltration, mining, attacking third parties), and attack users
through preview pages.

| Layer | Control | Status |
|---|---|---|
| Kernel | gVisor: syscalls go to a user-space kernel, not the host's | ✅ |
| Pod | Tainted node pool; no SA token; non-root; no privilege escalation; caps dropped; seccomp; CPU/memory/disk limits | ✅ |
| Network | Default deny; no sandbox↔sandbox, K8s API, metadata or internal services; per-sandbox egress | ✅ (FQDN rules planned) |
| Credentials | None in the sandbox: presigned URLs for snapshots; host-side mounts (D13) and an egress vault (D8) planned | ✅ / planned |
| Data path | Per-sandbox execd token injected by srvm; the caller's API key never reaches the sandbox | ✅ |
| Tenancy | Tenant checked on every call; quotas before claiming; hashed keys | ✅ (audit log planned) |
| Preview | Separate registrable domain, per-subdomain cookies, private by default (D12) | planned |

Verified by the escape suite (13/13) and the demo (`quickstart/demo.py`).

## Operability targets
- **SLOs:** warm create p99 < 1 s, cold create p99 < 15 s, pool hit rate > 95%, exec availability 99.9%, resume p99 per GB.
- **Per-cell dashboards:** pool depth vs claim rate, claim latency, API-server write QPS/latency (an early warning for
  the claim ceiling), orphan count, snapshot upload/hydrate throughput, egress denials.
- **Statelessness:** everything except Postgres and object storage can be killed and rescheduled; srvm rebuilds its
  view from Postgres plus a list of claims and pods.
- **Runbooks to write:** drain/upgrade a cell, Postgres failover, rotate credentials, warm pool exhausted, gVisor upgrade.

## Landscape: what we took and what we rejected
| System | Took | Rejected |
|---|---|---|
| **Daytona** | Sandbox-first product shape, stop/resume, dashboard ideas | Its code (AGPL-3.0; own runners, not K8s) |
| **Modal** | The functions programming model (`.remote()`, `.map()`) and its focus on cold starts; our functions layer is claim → run → recycle on the same pools (D1) | Proprietary; its own scheduler on cloud VMs |
| **E2B** | The in-sandbox daemon pattern (its `envd`, our use of execd); the reference design for a future Firecracker tier (roadmap §2) | Firecracker + Nomad as the starting point: needs KVM nodes and isn't Kubernetes |
| **OpenSandbox** | API contract, SDKs, execd + its `/internal/init` binding, console (trial) | Server, gateway, egress sidecar (gVisor-specific reasons, see D3) |
| **agent-sandbox** | The substrate: templates, warm pools, claims | Its sandbox router: no auth by default, and its optional modes check a Kubernetes identity or a signed token rather than srvm tenants; it doesn't know sandbox states or add the per-sandbox execd token. Per-sandbox pod settings (env, volumes): a running warm pod can't change them, so they'd force a cold start; srvm sends them to execd after the claim instead (D4) |
