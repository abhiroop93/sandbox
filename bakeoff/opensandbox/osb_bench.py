#!/usr/bin/env python3
"""OpenSandbox spike: drive OpenSandbox (release-1.2.0, gVisor) through its own Python SDK.

    .venv-osb/bin/python osb_bench.py smoke
    .venv-osb/bin/python osb_bench.py cold --n 10
    .venv-osb/bin/python osb_bench.py escape
    .venv-osb/bin/python osb_bench.py pause --sizes 100,1024,2048

Results → ../results/<RUN>/osb_<test>.json (same RUN as the sandbox bake-off for side-by-side reports).
Uses the bake-off sandbox image (bakeoff-sandbox:dev, already on the kind nodes), so the
in-sandbox scripts (snap.sh/escape.sh) are identical to the agent-sandbox runs.
"""
from __future__ import annotations

import argparse, json, os, statistics, time
from datetime import datetime, timedelta
from pathlib import Path

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule

RUN = os.environ.get("RUN") or (Path(__file__).parent.parent / ".last_run").read_text().strip()
OUT = Path(__file__).parent.parent / "results" / RUN
IMAGE = os.environ.get("OSB_IMAGE", "bakeoff-sandbox:dev")
CFG = ConnectionConfigSync(domain=os.environ.get("OSB_DOMAIN", "127.0.0.1:18080"),
                           api_key=os.environ.get("OSB_API_KEY", "spike-key"),
                           use_server_proxy=True, request_timeout=timedelta(seconds=600))
ENTRY = ["sleep", "infinity"]


def summarize(xs):
    xs = sorted(xs)
    if not xs: return {"n": 0}
    p = lambda q: round(xs[min(len(xs) - 1, int(round(q / 100 * (len(xs) - 1))))], 3)
    return {"n": len(xs), "p50": p(50), "p90": p(90), "p99": p(99), "max": round(xs[-1], 3),
            "mean": round(statistics.mean(xs), 3)}

def save(test, data):
    OUT.mkdir(parents=True, exist_ok=True)
    data = {"test": f"osb_{test}", "system": os.environ.get("SYSTEM_LABEL", "opensandbox release-1.2.0 (batchsandbox, gvisor)"), "run": RUN,
            "at": datetime.now().isoformat(timespec="seconds"), **data}
    (OUT / f"osb_{test}.json").write_text(json.dumps(data, indent=2)); print(json.dumps(data, indent=2))

def create(**kw):
    t0 = time.monotonic()
    sb = SandboxSync.create(IMAGE, entrypoint=ENTRY, connection_config=CFG,
                            timeout=timedelta(minutes=30), ready_timeout=timedelta(seconds=300), **kw)
    return sb, time.monotonic() - t0

def run(sb, cmd):
    """Run a shell command via execd (bounded by CFG.request_timeout = 600s)."""
    ex = sb.commands.run(cmd)
    out = "".join(l.text for l in (ex.logs.stdout or []))
    err = "".join(l.text for l in (ex.logs.stderr or []))
    return out, err, ex

def state(sid):
    import httpx
    r = httpx.get(f"http://{CFG.domain}/v1/sandboxes/{sid}", headers={"OPEN-SANDBOX-API-KEY": CFG.api_key}, timeout=30)
    r.raise_for_status(); st = r.json()["status"]; return st["state"], st.get("message", "")

def wait_state(sid, want, timeout=900):
    t0 = time.monotonic(); seen = []
    while time.monotonic() - t0 < timeout:
        s, msg = state(sid)
        if not seen or seen[-1] != s: seen.append(s)
        if s == want: return time.monotonic() - t0, seen
        if s in ("Failed", "Terminated", "Error"): raise RuntimeError(f"{sid} -> {s}: {msg}")
        time.sleep(0.5)
    raise TimeoutError(f"{sid} not {want} after {timeout}s; states={seen}")

def jl(s): return [json.loads(l) for l in s.splitlines() if l.strip().startswith("{")]


def t_smoke(a):
    sb, s = create()
    try:
        out, err, _ = run(sb, "id; cat /proc/version; dmesg 2>/dev/null | head -2; echo PATH=$PATH")
        save("smoke", {"create_s": round(s, 3), "stdout": out, "stderr": err, "id": sb.id})
    finally:
        sb.kill()

def t_cold(a):
    create_s, exec_s, fails = [], [], 0
    for _ in range(a.n):
        try:
            sb, s = create(); create_s.append(s)
            t0 = time.monotonic(); run(sb, "true"); exec_s.append(time.monotonic() - t0)
            sb.kill()
        except Exception as e:
            fails += 1; print("create failed:", repr(e)[:300])
    save("cold", {"create_to_ready_s": summarize(create_s), "first_exec_s": summarize(exec_s), "failures": fails,
                  "note": "non-pool create via SDK (server proxy), image pre-loaded on node"})

def t_escape(a):
    rows = {}
    for label, kw in {"default": {},
                      "deny_all_egress": {"network_policy": NetworkPolicy(default_action="deny", egress=[])}}.items():
        peer, _ = create(); att = None
        try:
            run(peer, "nohup python3 -m http.server 8000 >/dev/null 2>&1 &"); time.sleep(2)
            peer_ip = run(peer, "hostname -i")[0].split()[0]
            att, _ = create(**kw)
            out, err, _ = run(att, f"/opt/sandbox/escape.sh {peer_ip}")
            rows[label] = jl(out) or [{"check": "script", "pass": False, "detail": (err or out)[:300]}]
        except Exception as e:
            rows[label] = [{"check": "setup", "pass": False, "detail": repr(e)[:300]}]
        finally:
            peer.kill()
            if att: att.kill()
    save("escape", {"profiles": rows,
                    "all_pass_default": all(r["pass"] for r in rows.get("default", [])),
                    "all_pass_deny_all": all(r["pass"] for r in rows.get("deny_all_egress", []))})

def t_pause(a):
    res = []
    for mb in a.sizes:
        r = {"size_mb": mb}
        try:
            sb, s = create(); r["create_s"] = round(s, 2)
            gout, gerr, _ = run(sb, f"/opt/sandbox/snap.sh gen {mb}")
            r["gen"] = jl(gout)[-1] if jl(gout) else {"error": (gerr or gout)[:300]}
            d1 = jl(run(sb, "/opt/sandbox/snap.sh digest")[0])[-1]
            sid = sb.id
            t0 = time.monotonic(); sb.pause(); r["pause_call_s"] = round(time.monotonic() - t0, 2)
            secs, states = wait_state(sid, "Paused"); r["pause_total_s"] = round(time.monotonic() - t0, 2)
            r["pause_states"] = states; sb.close()
            t0 = time.monotonic(); sb = SandboxSync.resume(sandbox_id=sid, connection_config=CFG)
            r["resume_call_s"] = round(time.monotonic() - t0, 2)
            wait_state(sid, "Running"); r["resume_total_s"] = round(time.monotonic() - t0, 2)
            t0 = time.monotonic(); out = run(sb, "/opt/sandbox/snap.sh digest")[0]
            r["first_exec_after_resume_s"] = round(time.monotonic() - t0, 2)
            d2 = jl(out)[-1] if jl(out) else {"error": out[:200]}
            r.update(intact=(d1 == d2), digest_before=d1, digest_after=d2)
            sb.kill()
        except Exception as e:
            r["error"] = repr(e)[:500]
        res.append(r); print(json.dumps(r))
    save("pause_resume", {"results": res,
                          "note": "OpenSandbox pause = commit sandbox rootfs to OCI image (registry kind-registry:5000); "
                                  "resume = recreate pod from that image. Workspace written to /workspace in rootfs."})


def main():
    ap = argparse.ArgumentParser(); sp = ap.add_subparsers(dest="cmd", required=True)
    sp.add_parser("smoke").set_defaults(fn=t_smoke)
    p = sp.add_parser("cold"); p.add_argument("--n", type=int, default=10); p.set_defaults(fn=t_cold)
    sp.add_parser("escape").set_defaults(fn=t_escape)
    p = sp.add_parser("pause"); p.add_argument("--sizes", type=lambda s: [int(x) for x in s.split(",")],
                                               default=[100, 1024, 2048]); p.set_defaults(fn=t_pause)
    a = ap.parse_args(); print(f"run={RUN} image={IMAGE} → {OUT}"); a.fn(a)

if __name__ == "__main__":
    main()
