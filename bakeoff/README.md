# Bake-off kit: agent-sandbox + gVisor

Sets up the kind cluster srvm runs on, and measures the substrate: warm/cold claim latency, burst and ramp
behaviour, density, gVisor compatibility, an escape suite and 2 GB workspace pause/resume. Results and what they
changed are in [`../docs/validation.md`](../docs/validation.md) (raw output in `results/`).
Pinned: **agent-sandbox v1.0.10**, **gVisor 20261026**, s5cmd 2.3.0, rclone 1.71.2 (kind S3 server).

> **kind vs GKE.** On kind, functional results (gVisor compatibility, escape suite, resume correctness, relative
> gVisor/runc slowdown) are valid. **Latency, claim-rate ceiling and density reflect a laptop VM, not a cell.**
> Re-run the same kit with `TARGET=gke` on a *dedicated* cluster for capacity numbers.

## Layout
```
env.sh              all parameters (override via env)
00-cluster-kind.sh  kind: 1 control-plane, 1 system worker, 1 tainted sandbox worker + gVisor (runsc) in containerd
00-cluster-gke.sh   GKE: dedicated cluster + GKE Sandbox (gVisor) node pool
01-install.sh       agent-sandbox, namespaces, sandbox image (kind), in-cluster S3 (kind), python venv
02-templates.sh     RuntimeClass, SandboxTemplates + WarmPools, NetworkPolicies
preload-images.sh   kind: pull images on the host and import them into the nodes (used by 01-install.sh)
bench.py            latency | cold | burst | ramp | density | resume | compat | escape | report
image/              sandbox image: python 3.12, node, git, zstd, s5cmd + in-sandbox scripts (snap/escape/compat)
results/<run>/      JSON per test + summary.md
99-teardown.sh
```

## Templates and pools
| Pool | Runtime | Purpose |
|---|---|---|
| `locked` | gVisor, non-root, no SA token, egress = DNS + object store only | srvm baseline. Latency, burst, ramp, escape |
| `open` | gVisor, internet egress | compat (pip / npm / git) |
| `runc` | runc on system nodes | baseline for gVisor slowdown |
| `ws` | gVisor, 2 vCPU / 4 GiB | persistent-workspace resume up to 2 GB |
| `tiny` | gVisor, 100m / 128Mi, starts at 0 | density |
| `cold` | the `locked` template, 0 replicas | cold path |

## Run: local kind
Prereqs: Docker Desktop (≥ 8 GB memory; 12+ GB recommended for the 2 GB resume test, then set `WS_MEM=4Gi`), kind,
kubectl (≥ 1.33 recommended), python3. kind defaults are sized for ~8 GB: pool 5 × 256Mi, `ws` 1 × 2Gi.
```bash
cd bakeoff
./00-cluster-kind.sh && ./01-install.sh && ./02-templates.sh
export RUN=$(date +%Y%m%d-%H%M)          # all results land in results/$RUN
B() { .venv/bin/python bench.py "$@"; }   # function, so it works in bash and zsh

# 1. substrate
B latency --n 100
B cold --n 20
B burst --sizes 10,25,50
B ramp --rates 2,5,10,20 --duration 120     # kind: small rates; the VM saturates first
B density --step 10 --max 100

# 2. workspaces, compatibility, security
B resume --sizes 100,500,1024,2048
B compat
B escape
B report                                   # → results/$RUN/summary.md
./99-teardown.sh
```

## Run: dedicated GKE cluster (capacity numbers)
```bash
export TARGET=gke GKE_PROJECT=<project> S3_BUCKET=<gcs-bucket> S3_ACCESS_KEY=<hmac-id> S3_SECRET_KEY=<hmac-secret>
docker buildx build --platform linux/amd64 -t <registry>/srvm-bakeoff-sandbox:dev --push image/
export SANDBOX_IMAGE=<registry>/srvm-bakeoff-sandbox:dev
./00-cluster-gke.sh && ./01-install.sh && ./02-templates.sh
B() { .venv/bin/python bench.py "$@"; }   # function, so it works in bash and zsh
B latency --n 200; B cold --n 30; B burst --sizes 50,100,200
B ramp --rates 10,20,30,40,60 --duration 300     # NEVER on a shared cluster
B density --step 25 --max 400
B resume; B compat; B escape; B report
./99-teardown.sh
```

## What each test measures
| Test | Metric | Feeds |
|---|---|---|
| `latency` | Claim → Ready on a warm pool (+ first `kubectl exec`) | Warm create p99 < 1s |
| `cold` | Claim on an empty pool (image pre-pulled) | Cold create p99 < 15s |
| `burst` | N parallel claims, including beyond pool size | Burst behaviour, pool refill |
| `ramp` | Sustained claims/s steps; claim p99, failures, API probe p99, pool minimum | **L1 ceiling** (stops at first unhealthy step) |
| `density` | Max Ready `tiny` sandboxes per sandbox node | **L3** |
| `resume` | gen → snapshot → release → new warm pod → hydrate → verify, for each size | **L5**, 2 GB resume < 10s, integrity |
| `compat` | Real workload steps on gVisor vs runc | Compatibility + slowdown |
| `escape` | SA token, non-root, host procs, mount, sysctl, raw socket, metadata, K8s API, internet, peer sandbox; DNS + object store as positive controls | 100% pass |

## Known limitations (prototype, not the product)
- **Exec uses `kubectl exec`.** srvm's real data path (gateway → sandboxd) is faster.
- **The resume prototype is a full tar + zstd + chunk upload, not content-addressed incremental chunks.** It gives the
  upper bound for a first pause.
- **The prototype injects S3 credentials into the sandbox at exec time.** In production, sandboxd uses short-lived
  presigned URLs or host-side upload, and credentials never enter the sandbox (docs/architecture.md D8).
- **Whether a claim on a 0-replica pool cold-starts** (`cold` test) depends on agent-sandbox semantics. The test
  reports failures if it doesn't.
