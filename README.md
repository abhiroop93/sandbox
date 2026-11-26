# Sandboxes for untrusted code on Kubernetes

This server runs untrusted code (AI-agent-generated or user-submitted) in isolated, persistent sandboxes on any Kubernetes
cluster. It speaks the [OpenSandbox](https://github.com/alibaba/OpenSandbox) API, so the **unmodified OpenSandbox
SDKs** (Python, TypeScript, Go, Java, C#) work against it.

- **Isolated:** every sandbox runs under [gVisor](https://gvisor.dev), as a non-root user, with deny-by-default networking
  (no Kubernetes API, cloud metadata, internal services or other sandboxes).
- **Fast:** ~0.3 s from `create` to a ready sandbox, using [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) warm pools.
- **Persistent:** pause saves `/workspace` to S3-compatible object storage and frees the pod; resume restores it on any
  free pod. Installed packages survive (`HOME=/workspace`).
- **Resilient:** if the cluster drains or evicts a pod, the server saves the workspace before it goes; if a node crashes,
  the sandbox falls back to its last snapshot.
- **Portable:** runs on any conformant Kubernetes cluster (GKE, EKS, OCI, on-prem, kind) with standard building
  blocks: no KVM, no cloud-specific services. This constraint shaped most of the design ([why](docs/architecture.md#the-founding-constraint-runs-on-any-kubernetes-cluster)).
- **Small:** one Go binary (API, auth, proxy, lifecycle) plus Postgres and an object store.

> **Status: experimental.** Validated end to end on a local kind cluster, not yet run in production. The server is a
> single replica, and the kind setup uses throwaway storage. See [the roadmap](docs/roadmap.md) for what's missing.

## Try it locally (about 15 minutes)
Needs Docker (8 GB+ memory; 12 GB recommended), [kind](https://kind.sigs.k8s.io), kubectl, Go 1.26+, Python 3 and [uv](https://docs.astral.sh/uv/).
```bash
cd bakeoff && ./00-cluster-kind.sh && ./01-install.sh && cd ..   # kind + gVisor + agent-sandbox + S3
kubectl apply -f deploy/kind/runtimeclass.yaml -f deploy/kind/agent-sandbox-config.yaml
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller
make deploy                                               # build, generate secrets, deploy Postgres + the server
kubectl -n sandbox-system port-forward svc/sandbox 18081:80 &

# SANDBOX_DOMAIN: where the server is reachable (host:port, no scheme): here, the port-forward above
export SANDBOX_DOMAIN=127.0.0.1:18081

# SANDBOX_API_KEY: create a tenant (20 = max running sandboxes), then mint a key for it.
# create-key prints {"keyId", "apiKey", ...}; the apiKey is shown only once, so this captures it directly.
kubectl -n sandbox-system exec deploy/sandbox -- /sandbox admin create-tenant demo-team 20
export SANDBOX_API_KEY=$(kubectl -n sandbox-system exec deploy/sandbox -- /sandbox admin create-key demo-team \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["apiKey"])')

cd quickstart && uv venv --clear && uv pip install -r requirements.txt
.venv/bin/python demo.py          # press Enter between acts; add --no-pause to run straight through
```
Lost the key? It can't be recovered (only a hash is stored): mint another with `create-key`. More on tenants and
keys: [docs/operations.md §4](docs/operations.md#4-operating-tenants-keys-usage).

The first sandbox pulls its image (up to a minute); after that, creates take well under a second. The demo walks through four acts: untrusted code trying to escape (all blocked), installing a package and running
real work, pause/resume with files intact, and surviving a pod eviction. Details: [quickstart/README.md](quickstart/README.md).

## Using it
Any OpenSandbox SDK, pointed at the server, with `use_server_proxy=True`:
```python
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

cfg = ConnectionConfigSync(domain="127.0.0.1:18081", api_key="sandbox_...", use_server_proxy=True)
sb = SandboxSync.create("python:3.12-slim", connection_config=cfg)
print(sb.commands.run("python -c 'print(6*7)'").logs.stdout[0].text)
sb.pause()                     # /workspace → object storage, pod released
```
Which OpenSandbox endpoints are supported: [docs/api-coverage.md](docs/api-coverage.md).

## Documentation
| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | FAQ (why not OpenSandbox's server, why agent-sandbox, why not Firecracker), design, key decisions (D1–D15) with rejected alternatives, security model |
| [docs/operations.md](docs/operations.md) | Setup, configuration, tenants and API keys, troubleshooting |
| [docs/validation.md](docs/validation.md) | Every test and measurement, with results (raw data in `bakeoff/results/`) |
| [docs/roadmap.md](docs/roadmap.md) | What's next, including when to move from gVisor to Firecracker |
| [docs/firecracker.md](docs/firecracker.md) | What a Firecracker runtime would change (design note) |
| [docs/api-coverage.md](docs/api-coverage.md) | Which OpenSandbox endpoints and fields the server supports |

## Repository layout
```
cmd/sandbox          server + admin CLI              internal/api        OpenSandbox API, auth, proxy to execd
cmd/sandbox-snap     in-sandbox snapshot tool        internal/lifecycle  state machine, pause/resume, interruptions
deploy/kind          manifests for a kind cluster    internal/cell       agent-sandbox pools/claims, NetworkPolicy
deploy/kind/console  OpenSandbox console (optional)  internal/store      Postgres (state, keys, quotas)
quickstart           quickstart and demo scripts     internal/execd      execd binding + command client
tests/e2e            SDK contract + interruption     internal/snapshot   presigned object-store access
bakeoff              cluster setup + benchmarks
```

Tests: `make test` (unit); end-to-end suites in `tests/e2e` (see [docs/validation.md](docs/validation.md)).

## Credits
This project builds on [OpenSandbox](https://github.com/alibaba/OpenSandbox) (API, SDKs, execd),
[kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) (warm pools) and
[gVisor](https://gvisor.dev) (isolation). All Apache-2.0.

## License
[Apache-2.0](LICENSE)
