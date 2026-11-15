"""End-to-end contract tests: the UNMODIFIED OpenSandbox Python SDK against a running srvm.

    pip install opensandbox==1.2.0 pytest
    SRVM_DOMAIN=127.0.0.1:18081 SRVM_API_KEY=srvm_... pytest -q tests/e2e

Optional: SRVM_IMAGE (default srvm-bakeoff-sandbox:dev, must have python3 + curl).
"""
import os
import time
import uuid
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import NetworkPolicy

DOMAIN = os.environ.get("SRVM_DOMAIN")
KEY = os.environ.get("SRVM_API_KEY")
IMAGE = os.environ.get("SRVM_IMAGE", "srvm-bakeoff-sandbox:dev")
pytestmark = pytest.mark.skipif(not (DOMAIN and KEY), reason="SRVM_DOMAIN / SRVM_API_KEY not set")
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
        SandboxSync.create(IMAGE, connection_config=cfg(key="srvm_bad_key"), ready_timeout=timedelta(seconds=10))


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
