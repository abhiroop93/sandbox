# OpenSandbox API coverage
Compared against upstream `specs/sandbox-lifecycle.yml` and `specs/execd-api.yaml` (checked 2026-11-15).

**Lifecycle API, implemented by the server (10 of 30 operations):** create, list (state/metadata filters), get, delete,
pause, resume (full `/workspace` kept), renew-expiration, `endpoints/{port}`, server proxy (`proxy/{port}/…`, swaps
the API key for the execd token), `metrics/events` (accepted, discarded).

**Lifecycle API, not implemented:**
| Missing | Size | Notes |
|---|---|---|
| Snapshots: create / list / get / delete, create from `snapshotId` (fork) | Medium | Pause snapshots exist; expose them as named snapshots |
| Runtime network policy: GET / PUT / PATCH / DELETE `networkpolicy` | Small (CIDR) | Policy is set only at create today; FQDN needs Cilium |
| Credential vault (8 endpoints) + `credentialProxy` | Large | Upstream it is an egress sidecar in the pod; roadmap |
| Templates API (4) + `templateId` | Medium | Templates are implicit, one per image |
| PATCH `metadata` | Tiny | |
| Diagnostics: context / logs / events | Small | Map to pod events and container logs |

**Create fields:**
| Field | Today |
|---|---|
| `volumes`, `snapshotId`, FQDN `networkPolicy` rules | Rejected with 400 |
| `entrypoint` | Accepted, **not executed** (API-driven sandboxes; see "Entrypoint execution") |
| `resourceLimits` / `resourceRequests` | **Silently ignored**: one fixed size (roadmap: resource classes) |
| `lifecycle` (hooks, idle/renewal policy), `platform`, `secureAccess`, `extensions`, `image.auth`, `credentialProxy`, `templateId` | **Silently ignored** |

Silently ignored fields are a correctness risk (a client asking for 4 CPUs gets the default); first fix when work
starts: reject them with 400 until supported.

**In-sandbox API (execd, ~60 operations, passed through unmodified):**
| Area | Status |
|---|---|
| Commands (foreground/background, status, logs, interrupt), bash sessions | Tested (SDK contract, quickstarts) |
| Files and directories | Read/write tested; other operations use the same proxy, not individually tested |
| Metrics (`/metrics`, `/metrics/watch`) | Untested; values may reflect gVisor's view, not pod limits |
| Code interpreter (`/code`, contexts) | Only with an image that bundles OpenSandbox's interpreter (Jupyter) |
| Filesystem as another uid/gid (`/v1/filesystem/{uid}/{gid}/…`) | Expected to fail: sandboxes run as non-root uid 1000 |
| Isolated sessions (`/v1/isolated/…`) | Expected to fail: nested namespaces/overlayfs as non-root under gVisor; unverified |

Suggested order when picked up: reject ignored fields → patch metadata, diagnostics, CIDR network rules → resource
classes → snapshots/fork → credential vault + FQDN (with Cilium).
