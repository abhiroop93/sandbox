"""End-to-end contract tests: the UNMODIFIED OpenSandbox Python SDK against a running server.

    pip install opensandbox==1.2.0 pytest
    SANDBOX_DOMAIN=127.0.0.1:18081 SANDBOX_API_KEY=sandbox_... pytest -q tests/e2e

Optional: SANDBOX_IMAGE (default bakeoff-sandbox:dev, must have python3 + curl).
"""
import json
import os
import time
import urllib.error
import urllib.request
import uuid
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import NetworkPolicy

DOMAIN = os.environ.get("SANDBOX_DOMAIN")
KEY = os.environ.get("SANDBOX_API_KEY")
IMAGE = os.environ.get("SANDBOX_IMAGE", "bakeoff-sandbox:dev")
pytestmark = pytest.mark.skipif(not (DOMAIN and KEY), reason="SANDBOX_DOMAIN / SANDBOX_API_KEY not set")
os.environ.setdefault("OPENSANDBOX_DISABLE_METRICS", "1")


def cfg(key=None):
    return ConnectionConfigSync(domain=DOMAIN, api_key=key or KEY, use_server_proxy=True,
                                request_timeout=timedelta(seconds=300))


def out(sb, cmd):
    ex = sb.commands.run(cmd)
    return "".join(l.text for l in ex.logs.stdout).strip()


@pytest.fixture
def sandbox():
    sb = SandboxSync.create(IMAGE, connection_config=cfg(), env={"E2E": "yes"}, metadata={"suite": "e2e"},
                            ready_timeout=timedelta(seconds=120))
    yield sb
    sb.kill()


def test_create_exec_env_files(sandbox):
    assert out(sandbox, "id -u") == "1000"
    assert out(sandbox, "echo $E2E") == "yes"
    assert "gvisor" in out(sandbox, "cat /proc/version")
    sandbox.files.write_files([WriteEntry(path="/workspace/a.txt", data="hello")])
    assert sandbox.files.read_file("/workspace/a.txt") == "hello"
    assert sandbox.get_info().status.state == "Running"


def test_warm_create_is_fast():
    t0 = time.monotonic()
    sb = SandboxSync.create(IMAGE, connection_config=cfg(), ready_timeout=timedelta(seconds=120))
    took = time.monotonic() - t0
    sb.kill()
    # Generous bound: warm path measured ~0.3s; cold fallback ~3s. Catches regressions to minutes-long paths.
    assert took < 10, f"create took {took:.1f}s"


def test_bad_api_key_rejected():
    with pytest.raises(Exception):
        SandboxSync.create(IMAGE, connection_config=cfg(key="sandbox_bad_key"), ready_timeout=timedelta(seconds=10))


def test_fqdn_network_rules_rejected():
    with pytest.raises(Exception) as e:
        SandboxSync.create(IMAGE, connection_config=cfg(), ready_timeout=timedelta(seconds=30),
                           network_policy=NetworkPolicy(default_action="deny",
                                                        egress=[{"action": "allow", "target": "pypi.org"}]))
    assert "FQDN" in str(e.value) or "INVALID_REQUEST" in str(e.value)


def test_network_isolation_default_and_allow():
    peer = SandboxSync.create(IMAGE, connection_config=cfg(), ready_timeout=timedelta(seconds=120))
    try:
        out(peer, "nohup python3 -m http.server 8000 >/dev/null 2>&1 &")
        time.sleep(2)
        ip = out(peer, "hostname -i").split()[0]
        assert out(peer, "curl -s -o /dev/null -w '%{http_code}' -m3 http://127.0.0.1:8000/") == "200"  # control
        for policy, internet in ((None, False), (NetworkPolicy(default_action="allow", egress=[]), True)):
            kw = {"network_policy": policy} if policy else {}
            sb = SandboxSync.create(IMAGE, connection_config=cfg(), ready_timeout=timedelta(seconds=120), **kw)
            try:
                code = lambda u: out(sb, f"curl -sk -o /dev/null -w '%{{http_code}}' -m5 {u} || true")
                assert (code("https://pypi.org/") == "200") == internet
                assert code("https://10.96.0.1:443/version") in ("000", "")      # K8s API never reachable
                assert code(f"http://{ip}:8000/") in ("000", "")                  # other sandboxes never reachable
                assert code("http://169.254.169.254/") in ("000", "")             # metadata never reachable
            finally:
                sb.kill()
    finally:
        peer.kill()


def test_pause_resume_preserves_workspace(sandbox):
    marker = uuid.uuid4().hex
    out(sandbox, f"mkdir -p /workspace/repo/.git && echo {marker} > /workspace/repo/.git/HEAD "
                 f"&& head -c 5000000 /dev/urandom > /workspace/repo/blob && ln -s .git/HEAD /workspace/repo/link")
    before = out(sandbox, "sha256sum /workspace/repo/blob")
    sid = sandbox.id
    sandbox.pause()
    deadline = time.time() + 300
    while sandbox.get_info().status.state != "Paused":
        assert time.time() < deadline, "pause did not complete"
        time.sleep(0.5)
    resumed = SandboxSync.resume(sandbox_id=sid, connection_config=cfg(), resume_timeout=timedelta(seconds=120))
    assert out(resumed, "cat /workspace/repo/.git/HEAD") == marker
    assert out(resumed, "sha256sum /workspace/repo/blob") == before
    assert out(resumed, "readlink /workspace/repo/link") == ".git/HEAD"
    assert out(resumed, "echo $E2E") == "yes"  # env re-bound on the new pod


def test_pause_with_active_writer_creates_complete_archive(sandbox):
    writer = (
        "import time\n"
        "size = 4096\n"
        "with open('/workspace/active.bin', 'wb', buffering=0) as f:\n"
        "    i = 0\n"
        "    while True:\n"
        "        header = f'{i}:'.encode()\n"
        "        block = header.ljust(size - 1, b'x') + b'\\n'\n"
        "        if f.write(block) != size:\n"
        "            raise RuntimeError('partial block write')\n"
        "        i += 1\n"
        "        time.sleep(0.005)\n")
    verifier = (
        "size = 4096\n"
        "raw = open('/workspace/active.bin', 'rb').read()\n"
        "assert raw and len(raw) % size == 0, ('torn final block', len(raw))\n"
        "for i in range(len(raw) // size):\n"
        "    header = f'{i}:'.encode()\n"
        "    expected = header.ljust(size - 1, b'x') + b'\\n'\n"
        "    assert raw[i*size:(i+1)*size] == expected, ('bad block', i)\n"
        "print('archive intact')\n")
    sandbox.files.write_files([WriteEntry(path="/workspace/writer.py", data=writer),
                               WriteEntry(path="/workspace/verify.py", data=verifier)])
    out(sandbox, "nohup python3 /workspace/writer.py >/workspace/writer.log 2>&1 &")
    time.sleep(0.3)
    sandbox.pause()
    deadline = time.time() + 120
    while sandbox.get_info().status.state != "Paused":
        assert time.time() < deadline, "pause with active writer did not complete"
        time.sleep(0.2)
    resumed = SandboxSync.resume(sandbox_id=sandbox.id, connection_config=cfg(), resume_timeout=timedelta(seconds=120))
    assert out(resumed, "python3 /workspace/verify.py") == "archive intact"


def test_unsupported_create_fields_rejected():
    body = json.dumps({"image": {"uri": IMAGE}, "resourceLimits": {"cpu": "4"}}).encode()
    req = urllib.request.Request(f"http://{DOMAIN}/v1/sandboxes", data=body, method="POST",
                                 headers={"OPEN-SANDBOX-API-KEY": KEY, "Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=10)
        raise AssertionError("resourceLimits must be rejected, not ignored")
    except urllib.error.HTTPError as exc:
        assert exc.code == 400
        assert exc.read()


def test_list_and_delete():
    sb = SandboxSync.create(IMAGE, connection_config=cfg(), metadata={"suite": "e2e-list"}, ready_timeout=timedelta(seconds=120))
    sid = sb.id
    sb.kill()
    deadline = time.time() + 30
    while True:
        try:
            state = SandboxSync.connect(sandbox_id=sid, connection_config=cfg(), skip_health_check=True).get_info().status.state
        except Exception:
            break
        if state == "Terminated":
            break
        assert time.time() < deadline
        time.sleep(0.5)
