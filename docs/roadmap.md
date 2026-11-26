# Roadmap

> Status: the server runs end to end on kind with the unmodified OpenSandbox SDKs ([validation.md](validation.md)). It is
> not production-ready: §1 is what stands between it and one production cell. Decisions D1–D15 are in
> [architecture.md](architecture.md); OpenSandbox endpoint gaps are in [api-coverage.md](api-coverage.md).

## 1. Production-ready single cell
Ordered by risk: losing work, breaking isolation, waking someone up.

| # | Item | Why |
|---|---|---|
| 1 | **Validate multiple server replicas** | Postgres jobs now lease, retry and fence lifecycle work; deterministic generations and per-sandbox advisory locks protect replay. Kind uses one replica. Run sustained multiple-replica and failover tests before increasing replicas. |
| 2 | **Admission control + a real capacity run** | An exhausted cell makes callers hang (115 s on kind). Per-cell rate budget, queue with deadline, `503` + `Retry-After`, and unschedulable reasons in `status.message`. Measure the per-cell ceiling on a dedicated GKE cluster (`bakeoff/` with `TARGET=gke`) to set the budget. |
| 3 | **Operate it:** metrics, dashboards, TLS, CI | Prometheus metrics (create/claim/hydrate latency, pool depth, API-server write latency), alerts and runbooks; TLS ingress (keys travel in headers); CI running unit + kind e2e tests. |
| 4 | **Idle auto-pause** | The biggest cost and capacity lever: abandoned sandboxes dominate spend (D15). |
| 5 | **Incremental/deduplicated snapshots** | Optional periodic full, process-quiesced autosave now bounds the crash recovery point. Add content-addressed or incremental chunks to reduce repeated upload time, storage and remaining resume cost. |

## 2. Runtime: gVisor today, Firecracker when scale or speed matters
gVisor is the right start: it runs on any Kubernetes node, including laptops and managed node pools (D5). But it is
not where this ends. **Once scale or speed becomes the constraint, we must evaluate switching the runtime to
Firecracker microVMs run by a host agent on each node** (E2B/Modal style, D10 tier 4). Each one is a clear trigger:

| Trigger | What Firecracker + host agent gives |
|---|---|
| Claim rate approaches the per-cell ceiling (~30/s, item 2) across several cells | Sandbox starts no longer go through the Kubernetes API, so creates scale with nodes, not with API-server writes |
| Users need pause/resume to keep running processes, or create/resume well under 100 ms | Memory snapshots: restore a running VM in ~100 ms instead of re-hydrating files |
| Density or cost per sandbox matters | Many lightweight VMs per large node, without a pod per sandbox |
| A tenant needs a VM boundary | Hardware-virtualization isolation instead of a user-space kernel |

Cost: KVM-capable nodes (bare metal or nested virtualization), and a substantial build (scheduling, networking,
image distribution, node-failure handling). The API and lifecycle are unaffected: it replaces `internal/cell`.
What it would take, step by step: [firecracker.md](firecracker.md).

**Kata Containers is the smaller, earlier step for isolation only.** Kata runs each pod inside a lightweight VM
(Firecracker or Cloud Hypervisor) behind a RuntimeClass, so it slots into today's architecture unchanged: a second
template with `runtimeClassName: kata-fc`, the same agent-sandbox warm pools, the same the server. It gives a VM boundary and
full kernel compatibility (no gVisor syscall gaps, no 4× small-file I/O cost). It does **not** fix scale or speed:
every sandbox is still a pod created through the Kubernetes API, and Kata doesn't expose VM memory snapshots through
Kubernetes. Add a Kata pool when a workload needs a VM boundary or gVisor compatibility; plan the Firecracker host
agent when the scale or speed triggers above fire.

## 3. Product completeness
| Item | Notes |
|---|---|
| **Domain allowlists + credential vault** | Cilium FQDN egress rules (rejected with 400 today) and secrets injected at egress, never in the sandbox (D8) |
| **Snapshots API and fork** | Named snapshots and create-from-snapshot, built on pause snapshots (D6) |
| **Resource classes** | Support a few sizes; today matching CPU/memory values apply the one configured class and every other request is rejected with 400 |
| **Preview URLs** | `https://<port>-<id>.<preview-domain>` on a separate domain (D12) |
| **Console** | OpenSandbox's console already works for core flows (validation.md §7); add diagnostics logs/events and remove its copy of tenant keys |

## 4. Later
| Item | Trigger |
|---|---|
| **More cells** | Sustained claim rate > 70% of the per-cell budget, or a new region/cloud (`cell_id` is already in the schema) |
| **Split the proxy into Envoy** | Streaming needs to scale separately, or multi-cell / preview URLs arrive. Envoy carries the bytes; `ext_authz` asks the server for the pod address and token. |
| **Functions layer** | `@function` / `.remote()` / `.map()` on claim → exec → recycle (D1) |
| **GPU sandboxes** | gVisor `nvproxy` or Kata, GPU node pools, read-only model volumes (D13) |

**Smaller items, not tracked individually:** snapshot and record garbage collection (terminated rows and superseded
upload attempts need collection); report OpenSandbox's gVisor pause bug upstream and an
execd init-auth fix; resume-while-pausing; entrypoint execution; SSO for console users; usage showback.

## Signals to re-evaluate decisions
| Signal | Re-evaluate |
|---|---|
| OpenSandbox's server gains warm pools on agent-sandbox, gVisor persistence and multi-cluster | Running their server instead of ours (D3) |
| agent-sandbox ships portable snapshots or auto-suspend | Parts of `sandbox-snap` and idle handling |
| Small-file-heavy workloads dominate (gVisor's 4× cost) | Kata pool for those templates (§2) |
