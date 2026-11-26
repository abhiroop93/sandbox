#!/usr/bin/env python3
"""Demo: is it safe, is it useful, does it remember, does it survive the cluster?  (~3 minutes)

    export SANDBOX_DOMAIN=127.0.0.1:18081 SANDBOX_API_KEY=sandbox_...
    python demo.py              # press Enter between acts (narrate while it waits)
    python demo.py --no-pause   # run straight through
    python demo.py --no-kubectl # skip the parts that need cluster access (peer probe, pod view, eviction)

Uses only the unmodified OpenSandbox SDK, plus kubectl for the "look behind the curtain" moments.
"""
import json
import logging
import os
import subprocess
import sys
import time
import urllib.request
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import NetworkPolicy

domain, key = os.environ.get("SANDBOX_DOMAIN"), os.environ.get("SANDBOX_API_KEY")
if not (domain and key):
    sys.exit("set SANDBOX_DOMAIN and SANDBOX_API_KEY (see README.md)")
os.environ.setdefault("OPENSANDBOX_DISABLE_METRICS", "1")
logging.getLogger("opensandbox").setLevel(logging.CRITICAL)  # SDK retries while a sandbox starts are expected noise
PAUSE = "--no-pause" not in sys.argv
KUBECTL = "--no-kubectl" not in sys.argv
NS = "sandboxes"
IMAGE = "python:3.12-slim"
cfg = ConnectionConfigSync(domain=domain, api_key=key, use_server_proxy=True)

G, R, B, D, X = "\033[32m", "\033[31m", "\033[1m", "\033[2m", "\033[0m"

# Runs inside the sandbox: try to reach host:port, print OPEN or BLOCKED.
PROBE = """import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
try:
    socket.create_connection((host, port), timeout=3).close()
    print("OPEN")
except Exception as e:
    print("BLOCKED", type(e).__name__)
"""


def act(n, title, story):
    print(f"\n{B}Act {n}: {title}{X}\n{D}{story}{X}")
    if PAUSE:
        input(f"{D}[Enter]{X} ")


def sh(sb, cmd):
    ex = sb.commands.run(cmd)
    return "".join(m.text for m in ex.logs.stdout).strip()


def line(label, ok, detail=""):
    print(f"  {G + '✔' if ok else R + '✘'}{X} {label:<52} {D}{detail}{X}")


def state(sid):
    req = urllib.request.Request(f"http://{domain}/v1/sandboxes/{sid}", headers={"OPEN-SANDBOX-API-KEY": key})
    return json.load(urllib.request.urlopen(req, timeout=10))["status"]


def wait_state(sid, want, timeout=120):
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout:
        st = state(sid)
        if st["state"] in want:
            return st, time.monotonic() - t0
        time.sleep(0.5)
    return state(sid), time.monotonic() - t0


def kubectl(*args, stdin=None):
    return subprocess.run(["kubectl", "-n", NS, *args], input=stdin, text=True, capture_output=True)


def pods_of(sid):
    return kubectl("get", "pods", "-l", f"sandbox.io/sandbox-id={sid}", "-o", "jsonpath={.items[*].metadata.name}").stdout.split()


def peer_ip(exclude_pod):
    """IP of some other sandbox pod (a warm one is fine: its execd listens on 44772)."""
    out = kubectl("get", "pods", "-l", "sandbox.io/sandbox=true", "-o",
                  "jsonpath={range .items[*]}{.metadata.name} {.status.podIP}{\"\\n\"}{end}").stdout
    for ln in out.splitlines():
        name, _, ip = ln.partition(" ")
        if ip and name != exclude_pod:
            return ip
    return None


def probes(sb, internet_expected):
    sb.files.write_files([WriteEntry(path="/tmp/probe.py", data=PROBE)])
    probe = lambda h, p: sh(sb, f"python3 /tmp/probe.py {h} {p}")
    token = sh(sb, "test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo FOUND || echo none")
    line("steal the Kubernetes service-account token", token == "none", "no token mounted")
    r = probe("kubernetes.default.svc.cluster.local", 443)
    line("call the Kubernetes API", r.startswith("BLOCKED"), r)
    r = probe("169.254.169.254", 80)
    line("steal cloud credentials (metadata server)", r.startswith("BLOCKED"), r)
    r = probe("postgres.sandbox-system.svc.cluster.local", 5432)
    line("reach our internal database", r.startswith("BLOCKED"), r)
    if KUBECTL:
        ip = peer_ip(pods_of(sb.id)[0] if pods_of(sb.id) else "")
        if ip:
            r = probe(ip, 44772)
            line("attack another tenant's sandbox", r.startswith("BLOCKED"), f"{ip}: {r}")
    r = probe("pypi.org", 443)
    if internet_expected:
        line("reach the public internet (allowed for this sandbox)", r == "OPEN", r)
    else:
        line("exfiltrate data to the internet", r.startswith("BLOCKED"), r)


print(f"{B}Sandboxes for untrusted code{X}. OpenSandbox API, Kubernetes, gVisor.\n"
      f"{D}server {domain}, unmodified OpenSandbox Python SDK{X}")

# ------------------------------------------------------------------------------------------------ Act 1
act(1, "Is it safe?", "An AI agent wrote some code. We don't trust it. Give it a sandbox and let it try to break out.")
t0 = time.monotonic()
sb = SandboxSync.create(IMAGE, connection_config=cfg, ready_timeout=timedelta(seconds=120))
print(f"  sandbox ready in {B}{time.monotonic() - t0:.2f}s{X}  {D}({sb.id}){X}")
try:
    line("runs as an unprivileged user", sh(sb, "id -u") == "1000", "uid " + sh(sb, "id -u"))
    k = sh(sb, "head -c 32 /proc/version")
    line("isolated kernel (gVisor), not the host's", "gvisor" in k, k)
    probes(sb, internet_expected=False)
finally:
    sb.kill()

# ------------------------------------------------------------------------------------------------ Act 2
act(2, "Is it useful?", "Same agent, now allowed onto the internet: it installs a package and does real work.\n"
    "Internal addresses stay closed even with internet on.")
time.sleep(0 if PAUSE else 3)  # let the warm pool refill so this create is warm too
t0 = time.monotonic()
sb = SandboxSync.create(IMAGE, connection_config=cfg, ready_timeout=timedelta(seconds=120),
                        network_policy=NetworkPolicy(default_action="allow", egress=[]))
sid = sb.id
print(f"  sandbox ready in {B}{time.monotonic() - t0:.2f}s{X}  {D}({sid}){X}")
try:
    probes(sb, internet_expected=True)
    t1 = time.monotonic()
    sh(sb, "pip install --user -q --disable-pip-version-check cowsay 2>&1 | tail -1")
    line("pip install a package", sh(sb, "python -c 'import cowsay; print(1)'") == "1", f"{time.monotonic() - t1:.1f}s")
    sb.files.write_files([WriteEntry(path="/workspace/analysis.py", data=(
        "import json, statistics\n"
        "data = [12, 7, 3, 21, 8, 13, 5]\n"
        "json.dump({'mean': statistics.mean(data), 'max': max(data)}, open('/workspace/result.json', 'w'))\n"))])
    sh(sb, "python /workspace/analysis.py")
    res = sb.files.read_file("/workspace/result.json")
    line("run the agent's script, read the result back", "mean" in res, res)
    print(sh(sb, "python -m cowsay -t 'work done'"))

    # -------------------------------------------------------------------------------------------- Act 3
    act(3, "Does it remember?", "Pause: the workspace goes to object storage and the machine is released (costs nothing).\n"
        "Resume: any free machine picks it up, files and installed packages intact.")
    t1 = time.monotonic()
    sb.pause()
    st, _ = wait_state(sid, {"Paused", "Failed"})
    line("paused, workspace saved", st["state"] == "Paused", f"{time.monotonic() - t1:.1f}s")
    if KUBECTL:
        t2 = time.monotonic()
        while pods_of(sid) and time.monotonic() - t2 < 20:  # released pod takes a moment to finish shutting down
            time.sleep(0.5)
        n = len(pods_of(sid))
        line("no machine running for it while paused", n == 0, f"{n} pods")
    t1 = time.monotonic()
    sb = SandboxSync.resume(sid, connection_config=cfg, resume_timeout=timedelta(seconds=120))
    line("resumed", True, f"{time.monotonic() - t1:.1f}s")
    line("script and result still there", "mean" in sh(sb, "cat /workspace/result.json"))
    line("installed package still there", sh(sb, "python -c 'import cowsay; print(1)'") == "1")

    # -------------------------------------------------------------------------------------------- Act 4
    if KUBECTL:
        act(4, "Does it survive the cluster?", "Kubernetes evicts the machine (a node drain, an upgrade, a scale-down).\n"
            "the server notices, saves the workspace before the machine goes away, and it resumes elsewhere.")
        sh(sb, "echo 'written just before the eviction' > /workspace/last-minute.txt")
        time.sleep(1)
        pod = pods_of(sid)[0]
        ev = json.dumps({"apiVersion": "policy/v1", "kind": "Eviction", "metadata": {"name": pod, "namespace": NS}})
        r = kubectl("create", "--raw", f"/api/v1/namespaces/{NS}/pods/{pod}/eviction", "-f", "-", stdin=ev)
        line("cluster evicts the sandbox's machine", r.returncode == 0, r.stderr.strip() or pod)
        st, secs = wait_state(sid, {"Paused", "Failed"})
        line("the server saved it during shutdown", st["state"] == "Paused", f"{secs:.1f}s: {st.get('message', '')[:60]}…")
        sb = SandboxSync.resume(sid, connection_config=cfg, resume_timeout=timedelta(seconds=120))
        f = sh(sb, "cat /workspace/last-minute.txt 2>/dev/null")
        line("last-minute file survived", f == "written just before the eviction", f)
finally:
    try:
        SandboxSync.connect(sid, connection_config=cfg, skip_health_check=True).kill()
    except Exception:  # noqa: BLE001
        pass
print(f"\n{B}Done.{X} Safe by default, useful when allowed, remembers its work, survives the cluster.")
