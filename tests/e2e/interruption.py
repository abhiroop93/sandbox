#!/usr/bin/env python3
"""Pod-interruption e2e: what survives when the cluster takes a sandbox's pod away.

    export SANDBOX_DOMAIN=127.0.0.1:18081 SANDBOX_API_KEY=sandbox_...
    python tests/e2e/interruption.py            # needs kubectl access to the cell

1. Eviction (what a node drain does): the server saves the workspace during the grace period → Paused → resume → files intact.
2. Node crash (docker stop of the kind node): → Paused on the last snapshot → resume → newer changes lost, old intact.
   (A `kubectl delete --force` is NOT a crash: the kubelet still runs preStop, and the server saves the workspace.)
3. Pod-level check: claimed pod is annotated safe-to-evict=false.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

domain, key = os.environ["SANDBOX_DOMAIN"], os.environ["SANDBOX_API_KEY"]
os.environ.setdefault("OPENSANDBOX_DISABLE_METRICS", "1")
cfg = ConnectionConfigSync(domain=domain, api_key=key, use_server_proxy=True)
NS = "sandboxes"
failures = []


def check(name, ok, detail=""):
    print(f"  [{'PASS' if ok else 'FAIL'}] {name} {detail}")
    if not ok:
        failures.append(name)


def api(path):
    req = urllib.request.Request(f"http://{domain}/v1/sandboxes/{path}", headers={"OPEN-SANDBOX-API-KEY": key})
    return json.load(urllib.request.urlopen(req, timeout=10))


def wait_state(sid, want, timeout=150):
    t0 = time.monotonic()
    while time.monotonic() - t0 < timeout:
        st = api(sid)["status"]
        if st["state"] in want:
            return st, time.monotonic() - t0
        time.sleep(1)
    return api(sid)["status"], time.monotonic() - t0


def sh(sb, cmd):
    ex = sb.commands.run(cmd)
    return "".join(m.text for m in ex.logs.stdout).strip()


def pod_of(sid):
    out = subprocess.check_output(["kubectl", "-n", NS, "get", "pods", "-l", f"sandbox.io/sandbox-id={sid}",
                                   "-o", "jsonpath={.items[*].metadata.name}"], text=True).split()
    return out[0] if out else None


def kubectl(*args, stdin=None):
    return subprocess.run(["kubectl", "-n", NS, *args], input=stdin, text=True, capture_output=True)


sb = SandboxSync.create("python:3.12-slim", connection_config=cfg, ready_timeout=timedelta(seconds=120))
sid = sb.id
print(f"sandbox {sid}")
try:
    # 200 MB of incompressible data + a small marker, so the save takes real time.
    sh(sb, "head -c 200000000 /dev/urandom > /workspace/blob && sha256sum /workspace/blob > /workspace/blob.sha"
           " && echo v1 > /workspace/marker")
    want_sha = sh(sb, "cut -d' ' -f1 /workspace/blob.sha")
    time.sleep(2)  # let the server's post-Running protect step finish
    pod = pod_of(sid)
    ann = json.loads(kubectl("get", "pod", pod, "-o", "jsonpath={.metadata.annotations}").stdout or "{}")
    print("\n3. pod protection")
    check("safe-to-evict=false on claimed pod", ann.get("cluster-autoscaler.kubernetes.io/safe-to-evict") == "false", str(ann))

    print("\n1. eviction (node drain)")
    ev = json.dumps({"apiVersion": "policy/v1", "kind": "Eviction", "metadata": {"name": pod, "namespace": NS}})
    r = kubectl("create", "--raw", f"/api/v1/namespaces/{NS}/pods/{pod}/eviction", "-f", "-", stdin=ev)
    check("eviction accepted", r.returncode == 0, r.stderr.strip())
    st, secs = wait_state(sid, {"Paused", "Failed"})
    check("sandbox Paused after eviction", st["state"] == "Paused", f"({secs:.1f}s) {st.get('message', '')}")
    sb = SandboxSync.resume(sid, connection_config=cfg, resume_timeout=timedelta(seconds=120))
    check("marker intact", sh(sb, "cat /workspace/marker") == "v1")
    check("200 MB blob intact", sh(sb, "sha256sum /workspace/blob | cut -d' ' -f1") == want_sha)

    print("\n2. node crash (sandbox node's container stopped: no warning, no preStop)")
    sh(sb, "echo v2 > /workspace/marker && echo new > /workspace/after-snapshot")
    time.sleep(2)
    node = kubectl("get", "pod", pod_of(sid), "-o", "jsonpath={.spec.nodeName}").stdout
    subprocess.run(["docker", "stop", "-t", "0", node], check=True, capture_output=True)
    try:
        st, secs = wait_state(sid, {"Paused", "Failed"}, timeout=300)
        check("sandbox Paused on last snapshot", st["state"] == "Paused", f"({secs:.1f}s after crash) {st.get('message', '')}")
    finally:
        subprocess.run(["docker", "start", node], check=True, capture_output=True)
        subprocess.run(["kubectl", "wait", "--for=condition=Ready", f"node/{node}", "--timeout=180s"], capture_output=True)
    sb = SandboxSync.resume(sid, connection_config=cfg, resume_timeout=timedelta(seconds=180))
    check("rolled back to last snapshot (marker v1)", sh(sb, "cat /workspace/marker") == "v1")
    check("post-snapshot file lost, as expected", sh(sb, "ls /workspace/after-snapshot 2>/dev/null || echo none") == "none")
    check("blob still intact", sh(sb, "sha256sum /workspace/blob | cut -d' ' -f1") == want_sha)
finally:
    try:
        SandboxSync.connect(sid, connection_config=cfg, skip_health_check=True).kill()
    except Exception as e:  # noqa: BLE001
        print("cleanup:", e)
print("\nRESULT:", "PASS" if not failures else f"FAIL {failures}")
sys.exit(1 if failures else 0)
