#!/usr/bin/env python3
"""Local-kind durability checks: autosave thaw plus server-restart replay.

Requires SANDBOX_DOMAIN, SANDBOX_API_KEY and a KUBECONFIG scoped to the local
kind cluster. Restarts only deployment/sandbox; never stops a cluster node.
"""
import json
import os
import subprocess
import time
import urllib.request
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry

DOMAIN = os.environ.get("SANDBOX_DOMAIN")
KEY = os.environ.get("SANDBOX_API_KEY")
IMAGE = os.environ.get("SANDBOX_IMAGE", "python:3.12-slim")
if not (DOMAIN and KEY and os.environ.get("KUBECONFIG")):
    raise SystemExit("set SANDBOX_DOMAIN, SANDBOX_API_KEY and a scoped KUBECONFIG")
os.environ.setdefault("OPENSANDBOX_DISABLE_METRICS", "1")
CFG = ConnectionConfigSync(domain=DOMAIN, api_key=KEY, use_server_proxy=True,
                           request_timeout=timedelta(seconds=300))


def kubectl(*args, check=True):
    return subprocess.run(["kubectl", *args], text=True, capture_output=True, check=check).stdout.strip()


def request(method, sid, suffix=""):
    req = urllib.request.Request(f"http://{DOMAIN}/v1/sandboxes/{sid}{suffix}", method=method,
                                 headers={"OPEN-SANDBOX-API-KEY": KEY})
    with urllib.request.urlopen(req, timeout=20) as response:
        return response.status, response.read()


def status(sid):
    _, body = request("GET", sid)
    return json.loads(body)["status"]


def wait_state(sid, wanted, timeout=180):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = status(sid)
            if last["state"] in wanted:
                return last
            if last["state"] == "Failed":
                raise AssertionError(last.get("message", "sandbox failed"))
        except OSError:
            pass  # the deliberately restarted server may briefly refuse connections
        time.sleep(0.5)
    raise AssertionError(f"timed out waiting for {wanted}; last status: {last}")


def generation(sid):
    # Kind's local Postgres socket is used for assertion only; no credentials are printed.
    sql = f"SELECT generation FROM sandboxes WHERE id = '{sid}'"
    value = kubectl("-n", "sandbox-system", "exec", "deploy/postgres", "--",
                    "psql", "-U", "sandbox", "-d", "sandbox", "-Atc", sql)
    assert value.isdigit(), value
    return int(value)


def out(sb, command):
    result = sb.commands.run(command)
    return "".join(line.text for line in result.logs.stdout).strip()


def set_autosave(value):
    kubectl("-n", "sandbox-system", "set", "env", "deploy/sandbox", f"AUTOSAVE_INTERVAL_SECONDS={value}")
    kubectl("-n", "sandbox-system", "rollout", "status", "deploy/sandbox", "--timeout=180s")


def restart_server():
    kubectl("-n", "sandbox-system", "rollout", "restart", "deploy/sandbox")
    kubectl("-n", "sandbox-system", "rollout", "status", "deploy/sandbox", "--timeout=180s")


WRITER = """import time
size = 4096
with open('/workspace/active.bin', 'wb', buffering=0) as f:
    i = 0
    while True:
        header = f'{i}:'.encode()
        block = header.ljust(size - 1, b'x') + b'\\n'
        if f.write(block) != size:
            raise RuntimeError('partial write')
        i += 1
        time.sleep(0.005)
"""

VERIFIER = """size = 4096
raw = open('/workspace/active.bin', 'rb').read()
assert raw and len(raw) % size == 0, ('torn final block', len(raw))
for i in range(len(raw) // size):
    header = f'{i}:'.encode()
    assert raw[i*size:(i+1)*size] == header.ljust(size - 1, b'x') + b'\\n', ('bad block', i)
print('archive intact')
"""

original = kubectl("-n", "sandbox-system", "get", "deploy/sandbox", "-o",
                   "jsonpath={.spec.template.spec.containers[0].env[?(@.name=='AUTOSAVE_INTERVAL_SECONDS')].value}") or "0"
sb = None
try:
    set_autosave("15")
    sb = SandboxSync.create(IMAGE, connection_config=CFG, ready_timeout=timedelta(seconds=120))
    sid = sb.id
    sb.files.write_files([WriteEntry(path="/workspace/writer.py", data=WRITER),
                          WriteEntry(path="/workspace/verify.py", data=VERIFIER)])
    pid = out(sb, "nohup python3 /workspace/writer.py >/workspace/writer.log 2>&1 & echo $!")
    time.sleep(0.5)
    before = int(out(sb, "stat -c %s /workspace/active.bin"))
    deadline = time.monotonic() + 45
    while True:
        info = sb.get_info()
        message = info.status.message or ""
        if info.status.state == "Running" and ("workspace autosaved" in message or "workspace unchanged" in message):
            break
        assert time.monotonic() < deadline, f"autosave did not complete: {info.status}"
        time.sleep(0.5)
    process_state = out(sb, f"awk '{{print $3}}' /proc/{pid}/stat")
    assert process_state != "T", "autosave left the writer stopped"
    time.sleep(0.5)
    after = int(out(sb, "stat -c %s /workspace/active.bin"))
    assert after > before, "writer did not continue after autosave thaw"
    assert generation(sid) == 1
    print("PASS autosave publishes while Running and thaws the writer")

    sb.pause()
    restart_server()
    wait_state(sid, {"Paused"})
    assert generation(sid) == 1, "replayed pause changed generation"
    sb = SandboxSync.resume(sid, connection_config=CFG, resume_timeout=timedelta(seconds=120))
    assert generation(sid) == 2
    assert out(sb, "python3 /workspace/verify.py") == "archive intact"
    print("PASS pause survives a server restart with complete archive and stable generation")

    out(sb, "echo replayed > /workspace/replay.txt")
    # Raw resume returns 202 immediately, allowing the server restart to race a Resuming job.
    sb.pause()
    wait_state(sid, {"Paused"})
    code, _ = request("POST", sid, "/resume")
    assert code == 202
    restart_server()
    wait_state(sid, {"Running"})
    assert generation(sid) == 3, "replayed resume allocated another generation"
    sb = SandboxSync.connect(sid, connection_config=CFG)
    assert out(sb, "cat /workspace/replay.txt") == "replayed"
    print("PASS resume survives a server restart without allocating another generation")
finally:
    if sb is not None:
        try:
            SandboxSync.connect(sb.id, connection_config=CFG, skip_health_check=True).kill()
        except Exception:
            pass
    set_autosave(original)
