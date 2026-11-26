# Moving to Firecracker (design note, not planned work)

> When to do this: the triggers in [roadmap.md §2](roadmap.md#2-runtime-gvisor-today-firecracker-when-scale-or-speed-matters).
> Why gVisor first: [architecture.md](architecture.md) D5 and "The founding constraint". This note records what a
> Firecracker runtime would change, so the decision can be made with its cost in view.

## Why consider it
| Need | What Firecracker (run by a host agent on each node) gives |
|---|---|
| More creates per cell | Sandbox starts no longer go through the Kubernetes API, so throughput scales with nodes |
| Faster create/resume, or keeping running processes across pause | Memory snapshots: restore a running VM in ~100 ms instead of re-hydrating files |
| Density, cost per sandbox | Many lightweight VMs per node, no pod per sandbox |
| A VM boundary | Hardware-virtualization isolation instead of a user-space kernel |

Today's numbers to beat ([validation.md](validation.md)): ~0.3 s warm create through the SDK; resume 0.9 s for
100 MB, ~8–10 s for 1–2 GB. The per-cell claim ceiling (~30/s) is agent-sandbox's published figure, **not yet
measured by us**; the GKE capacity run (roadmap #2) should come first, since it may push the trigger far out.

## What stays the same
| Part | Why it survives |
|---|---|
| OpenSandbox API, SDKs, `internal/api` | Clients never see the runtime |
| Tenants, keys, quotas, Postgres (`internal/store`) | Runtime-independent; the schema gains a host id and a snapshot type |
| Lifecycle state machine (`internal/lifecycle`) | Same states, CAS transitions and generation fencing; pause gains a memory-snapshot path |
| execd inside the sandbox | Runs inside the VM instead of a container; bound the same way after start |
| `sandbox-snap` file snapshots | Still useful for fork, moving between runtimes, and as a fallback |
| Interruption handling | Same model: save on planned shutdown, fall back to the last snapshot on a crash ("pod lost" becomes "host lost") |
| Kubernetes for the server itself | The server and the host agents still run on Kubernetes; only sandboxes stop being pods |

## What has to be built or replaced
1. **A runtime interface in place of `internal/cell`** (days). `lifecycle` calls the agent-sandbox implementation
   directly today. Extract an interface (start, wait-ready, release, list, network policy, watch) with gVisor as one
   implementation. Firecracker becomes a second, chosen per template, so both can run side by side.

2. **A host agent on every KVM node** (the core build). A privileged daemon (a DaemonSet on a KVM node pool) that
   starts Firecracker VMs through its `jailer` (chroot, seccomp, cgroups), keeps pools of ready VMs or restores them
   from snapshots, reports capacity and health, and exposes an API that the server calls instead of creating claims.
   It becomes the most security-sensitive component in the system.

3. **Images: OCI image → VM disk.** Firecracker boots a kernel and a block device, not a container image. Needs a
   pipeline that converts images to root disks and injects execd and an init, a maintained guest kernel, per-node
   caching and sharing of disks, and later on-demand block loading. Kubernetes and containerd do all of this today.

4. **Networking.** NetworkPolicy doesn't apply to VMs. Needs per-VM tap devices, address allocation, host-level
   rules (nftables or eBPF) reproducing today's guarantees (deny by default; no metadata, cluster or other sandboxes;
   optional internet), and a route from the server to each VM. The escape suite must pass 13/13 again.

5. **Placement and capacity in the server.** No Kubernetes scheduler for sandboxes: the server picks the host, manages per-host
   warm pools, bin-packs, drains hosts and detects lost hosts.

6. **Memory snapshots (the main reason to do this).**
   - Pause: Firecracker writes VM state plus a memory file; upload both. Memory files are as large as the VM's RAM,
     so diff snapshots and compression are needed.
   - Resume on another host: load memory on demand with `userfaultfd` instead of downloading it first.
   - Sub-100 ms create: restore clones of a "booted, execd running" template snapshot. Each clone must be re-seeded
     (random number state, clock, identity) before use.

7. **Infrastructure and operations.** KVM-capable node pools (bare metal or nested virtualization), guest kernel and
   Firecracker version management, new metrics, runbooks and an updated threat model.

## Suggested order
1. Extract the runtime interface (gVisor keeps working).
2. Host agent MVP: boot a VM from a prebuilt disk, run execd, deny-all networking.
3. Image pipeline.
4. Placement, warm pools, host-failure handling.
5. Memory snapshots, then on-demand restore and template cloning.
6. Hardening and escape-suite parity.

Order of magnitude: months of focused work, most of it in steps 2–5.

## Before building: evaluate what exists
Reusing any of the image pipeline, networking or snapshot/restore code removes months of work.
- **E2B's open-source infrastructure** (Apache-2.0): Firecracker-based, with an orchestrator, in-sandbox daemon and
  snapshot handling. It uses Nomad rather than Kubernetes, so expect to reuse components, not the whole stack.
- **OpenSandbox's FastSandbox** host agent (requires `/dev/kvm`).
- **firecracker-containerd**: Firecracker behind a containerd interface.
- **Kata Containers with Firecracker** (`kata-fc`): VM isolation without any of the above, but no scale or snapshot
  gains (roadmap §2). Worth doing first if the only need is a VM boundary.
