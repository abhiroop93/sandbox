# srvm quickstart

Two tiny programs, Python and Go, that create a sandbox, run a few commands, write and read files, then delete it.
They use the **official OpenSandbox SDKs, unmodified**; srvm speaks the OpenSandbox API.

## 1. Point at srvm and get an API key
```bash
# local kind cluster: expose srvm on localhost
kubectl -n srvm-system port-forward svc/srvm 18081:80 &

# operator: mint a key (tenant must exist; see docs/operations.md §4)
srvm-admin() { kubectl -n srvm-system exec deploy/srvm -- /srvm admin "$@"; }
srvm-admin create-tenant demo-team 20
srvm-admin create-key demo-team          # copy "apiKey" from the output: it's shown once

export SRVM_DOMAIN=127.0.0.1:18081       # host:port, no scheme
export SRVM_API_KEY=srvm_...
```

## 2a. Python
```bash
cd quickstart
python3 -m venv .venv && . .venv/bin/activate     # or: uv venv && . .venv/bin/activate
pip install -r requirements.txt                    # opensandbox==1.2.0
python quickstart.py
```

## 2b. Go
```bash
cd quickstart/go
go run .
```

## Expected output (either language)
```
sandbox 6c3c8133-… ready in 0.30s      ← ~0.3s warm; first run for an image is cold (~3s, more if the image must be pulled)
uid         : 1000                    ← unprivileged user
kernel      : Linux version 4.19.0-gvisor #1 SMP …   ← you're inside gVisor
python      : Python 3.12.x
env         : hello from srvm
cwd         : /workspace
run script  : wrote 15 numbers
read file   : [0, 1, 1, 2, 3, 5, 8, …]
...
sandbox 6c3c8133-… deleted
```

## 3. Demo (~3 minutes): `demo.py`
One command, four acts, press Enter between them while you narrate:
```bash
python demo.py               # interactive
python demo.py --no-pause    # straight through (~1 minute)
python demo.py --no-kubectl  # without cluster access: skips the peer probe, pod view and eviction
```
1. **Is it safe?** Untrusted code tries the SA token, Kubernetes API, metadata server, our Postgres, another
   sandbox and the internet: all blocked.
2. **Is it useful?** With internet allowed: `pip install`, run a script, read the result. Internal addresses stay blocked.
3. **Does it remember?** Pause (no pod while paused), resume: files and installed packages intact.
4. **Does it survive the cluster?** The pod is evicted; srvm saves the workspace during shutdown; resume, files intact.

Run it once beforehand to warm the image's pool. Wait a few seconds between runs (the kind pool holds one warm pod).

## Next steps
- **Internet access:** pass `network_policy=NetworkPolicy(default_action="allow", egress=[])` (Python) or
  `NetworkPolicy: &opensandbox.NetworkPolicy{DefaultAction: "allow"}` (Go) to `create`.
- **Persistent workspace:** `pause()`, wait for `Paused`, then `resume()`. Files under `/workspace` survive.
  See [docs/operations.md §6](../docs/operations.md#6-users-getting-started).
- **Troubleshooting:**

  | Error | Fix |
  |---|---|
  | `401` | Wrong or revoked key |
  | `429` | Tenant quota reached |
  | Ready timeout | See operations.md §5 |
