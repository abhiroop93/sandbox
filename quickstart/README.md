# Quickstart

Small programs that use the **official OpenSandbox SDKs, unmodified** (the server speaks the OpenSandbox API):

| File | What it does |
|---|---|
| `quickstart.py`, `go/main.go` | Create a sandbox, run commands, write and read files, delete it |
| `demo.py` | A ~3-minute demo: isolation, real work, pause/resume, surviving an eviction (§3) |
| `pause.py`, `resume.py` | Pause a sandbox in one run, resume it in another |

## 1. Set `SANDBOX_DOMAIN` and `SANDBOX_API_KEY`
Every program reads two environment variables:

| Variable | What it is | Where it comes from |
|---|---|---|
| `SANDBOX_DOMAIN` | The address of the server: `host:port`, no `http://` | Local kind cluster: your port-forward, `127.0.0.1:18081`. Shared deployment: the server hostname your platform team gives you. |
| `SANDBOX_API_KEY` | Your tenant's API key, `sandbox_<id>_<secret>` | Minted by whoever operates the server, with `sandbox admin create-key <tenant>`. On your own kind cluster, that's you (below). Shown only once; it can't be recovered, only replaced. |

**Using a shared server:** ask the platform team for both values, then:
```bash
export SANDBOX_DOMAIN=<server host>  SANDBOX_API_KEY=<your key>
```

**Running the server yourself on kind** (after the setup in the [main README](../README.md#try-it-locally-about-15-minutes)):
```bash
# SANDBOX_DOMAIN: expose the server on localhost. Keep this running; re-run it after a redeploy or Docker restart.
kubectl -n sandbox-system port-forward svc/sandbox 18081:80 &
export SANDBOX_DOMAIN=127.0.0.1:18081

# SANDBOX_API_KEY: you are the operator. Create a tenant (20 = max running sandboxes), mint a key for it,
# and capture the "apiKey" field from the JSON that create-key prints.
sandbox-admin() { kubectl -n sandbox-system exec deploy/sandbox -- /sandbox admin "$@"; }
sandbox-admin create-tenant demo-team 20
export SANDBOX_API_KEY=$(sandbox-admin create-key demo-team | python3 -c 'import json,sys; print(json.load(sys.stdin)["apiKey"])')
```
Both variables live only in the current shell: export them again in a new terminal (mint a new key if you didn't
save the old one, and revoke the old one with `sandbox-admin revoke-key <keyId>`; `sandbox-admin list-keys` shows ids).
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
env         : hello from sandbox
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
4. **Does it survive the cluster?** The pod is evicted; the server saves the workspace during shutdown; resume, files intact.

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
