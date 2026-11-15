# srvm: sandboxes for untrusted code on Kubernetes

srvm runs untrusted code (AI-agent-generated or user-submitted) in isolated, persistent sandboxes on any Kubernetes
cluster. It speaks the [OpenSandbox](https://github.com/alibaba/OpenSandbox) API, so the **unmodified OpenSandbox
SDKs** (Python, TypeScript, Go, Java, C#) work against it.

- **Isolated:** every sandbox runs under [gVisor](https://gvisor.dev), as a non-root user, with deny-by-default networking
  (no Kubernetes API, cloud metadata, internal services or other sandboxes).
- **Fast:** ~0.3 s from `create` to a ready sandbox, using [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) warm pools.
- **Persistent:** pause saves `/workspace` to S3-compatible object storage and frees the pod; resume restores it on any
  free pod. Installed packages survive (`HOME=/workspace`).
- **Resilient:** if the cluster drains or evicts a pod, srvm saves the workspace before it goes; if a node crashes,
  the sandbox falls back to its last snapshot.
- **Portable:** runs on any conformant Kubernetes cluster (GKE, EKS, OCI, on-prem, kind) with standard building
  blocks: no KVM, no cloud-specific services. This constraint shaped most of the design ([why](docs/architecture.md#the-founding-constraint-runs-on-any-kubernetes-cluster)).
- **Small:** one Go binary (API, auth, proxy, lifecycle) plus Postgres and an object store.

> **Status: experimental.** Validated end to end on a local kind cluster, not yet run in production. srvm is a
> single replica, and the kind setup uses throwaway storage. See [the roadmap](docs/roadmap.md) for what's missing.

## Try it locally (about 15 minutes)
Needs Docker (8 GB+ memory; 12 GB recommended), [kind](https://kind.sigs.k8s.io), kubectl, helm, Go 1.25+, Python 3 and [uv](https://docs.astral.sh/uv/).
```bash
cd bakeoff && ./00-cluster-kind.sh && ./01-install.sh && cd ..   # kind + gVisor + agent-sandbox + S3
kubectl apply -f deploy/kind/runtimeclass.yaml -f deploy/kind/agent-sandbox-config.yaml
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller
make deploy                                               # build, generate secrets, deploy Postgres + srvm
kubectl -n srvm-system port-forward svc/srvm 18081:80 &

# mint an API key (shown once)
kubectl -n srvm-system exec deploy/srvm -- /srvm admin create-tenant demo-team 20
kubectl -n srvm-system exec deploy/srvm -- /srvm admin create-key demo-team

export SRVM_DOMAIN=127.0.0.1:18081 SRVM_API_KEY=srvm_...
cd quickstart && uv venv && uv pip install -r requirements.txt
.venv/bin/python demo.py
```
The demo walks through four acts: untrusted code trying to escape (all blocked), installing a package and running
real work, pause/resume with files intact, and surviving a pod eviction. Details: [quickstart/README.md](quickstart/README.md).

## Using it
Any OpenSandbox SDK, pointed at srvm, with `use_server_proxy=True`:
```python
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

cfg = ConnectionConfigSync(domain="127.0.0.1:18081", api_key="srvm_...", use_server_proxy=True)
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
| [docs/api-coverage.md](docs/api-coverage.md) | Which OpenSandbox endpoints and fields srvm supports |

## Repository layout
```
cmd/srvm            server + admin CLI              internal/api        OpenSandbox API, auth, proxy to execd
cmd/srvm-snap       in-sandbox snapshot tool        internal/lifecycle  state machine, pause/resume, interruptions
deploy/kind         manifests for a kind cluster    internal/cell       agent-sandbox pools/claims, NetworkPolicy
deploy/kind/console OpenSandbox console (optional)  internal/store      Postgres (state, keys, quotas)
quickstart          quickstart and demo scripts     internal/execd      execd binding + command client
tests/e2e           SDK contract + interruption     internal/snapshot   presigned object-store access
bakeoff             cluster setup + benchmarks
```

Tests: `make test` (unit); end-to-end suites in `tests/e2e` (see [docs/validation.md](docs/validation.md)).

## Credits
srvm builds on [OpenSandbox](https://github.com/alibaba/OpenSandbox) (API, SDKs, execd),
[kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) (warm pools) and
[gVisor](https://gvisor.dev) (isolation). All Apache-2.0.

## License
[Apache-2.0](LICENSE)
