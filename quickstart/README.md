# srvm quickstart

Small programs that use the **official OpenSandbox SDKs, unmodified** (srvm speaks the OpenSandbox API):

| File | What it does |
|---|---|
| `quickstart.py`, `go/main.go` | Create a sandbox, run commands, write and read files, delete it |
| `demo.py` | A ~3-minute demo: isolation, real work, pause/resume, surviving an eviction (§3) |
| `pause.py`, `resume.py` | Pause a sandbox in one run, resume it in another |

## 1. Set `SRVM_DOMAIN` and `SRVM_API_KEY`
Every program reads two environment variables:

| Variable | What it is | Where it comes from |
|---|---|---|
| `SRVM_DOMAIN` | The address of the srvm server: `host:port`, no `http://` | Local kind cluster: your port-forward, `127.0.0.1:18081`. Shared deployment: the srvm hostname your platform team gives you. |
| `SRVM_API_KEY` | Your tenant's API key, `srvm_<id>_<secret>` | Minted by whoever operates srvm, with `srvm admin create-key <tenant>`. On your own kind cluster, that's you (below). Shown only once; it can't be recovered, only replaced. |

**Using a shared srvm:** ask the platform team for both values, then:
```bash
export SRVM_DOMAIN=<srvm host>  SRVM_API_KEY=<your key>
```

**Running srvm yourself on kind** (after the setup in the [main README](../README.md#try-it-locally-about-15-minutes)):
```bash
# SRVM_DOMAIN: expose srvm on localhost. Keep this running; re-run it after a redeploy or Docker restart.
kubectl -n srvm-system port-forward svc/srvm 18081:80 &
export SRVM_DOMAIN=127.0.0.1:18081

# SRVM_API_KEY: you are the operator. Create a tenant (20 = max running sandboxes), mint a key for it,
# and capture the "apiKey" field from the JSON that create-key prints.
srvm-admin() { kubectl -n srvm-system exec deploy/srvm -- /srvm admin "$@"; }
srvm-admin create-tenant demo-team 20
export SRVM_API_KEY=$(srvm-admin create-key demo-team | python3 -c 'import json,sys; print(json.load(sys.stdin)["apiKey"])')
```
Both variables live only in the current shell: export them again in a new terminal (mint a new key if you didn't
save the old one, and revoke the old one with `srvm-admin revoke-key <keyId>`; `srvm-admin list-keys` shows ids).
More on tenants and keys: [docs/operations.md §4](../docs/operations.md#4-operating-tenants-keys-usage).

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
