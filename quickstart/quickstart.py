#!/usr/bin/env python3
"""srvm quickstart (Python): create a sandbox, run a few commands, use files, clean up.

    export SRVM_DOMAIN=127.0.0.1:18081      # srvm address (host:port, no scheme)
    export SRVM_API_KEY=srvm_...            # from your platform team / `srvm admin create-key`
    python quickstart.py
"""
import os
import sys
import time
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry

domain, key = os.environ.get("SRVM_DOMAIN"), os.environ.get("SRVM_API_KEY")
if not (domain and key):
    sys.exit("set SRVM_DOMAIN and SRVM_API_KEY (see README.md)")
os.environ.setdefault("OPENSANDBOX_DISABLE_METRICS", "1")

cfg = ConnectionConfigSync(domain=domain, api_key=key, use_server_proxy=True)


def sh(sb, cmd):
    """Run a shell command in the sandbox and return stdout."""
    ex = sb.commands.run(cmd)
    return "".join(m.text for m in ex.logs.stdout).strip()


t0 = time.monotonic()
sb = SandboxSync.create("python:3.12-slim", connection_config=cfg, env={"GREETING": "hello from srvm"},
                        ready_timeout=timedelta(seconds=120))
print(f"sandbox {sb.id} ready in {time.monotonic() - t0:.2f}s\n")

try:
    print("uid         :", sh(sb, "id -u"))
    print("kernel      :", sh(sb, "head -c 45 /proc/version"))
    print("python      :", sh(sb, "python --version"))
    print("env         :", sh(sb, "echo $GREETING"))
    print("cwd         :", sh(sb, "pwd"))

    # files: write a script into the sandbox, run it, read its output back
    sb.files.write_files([WriteEntry(path="/workspace/fib.py", data=(
        "import json\n"
        "a, b = 0, 1\n"
        "seq = []\n"
        "for _ in range(15):\n"
        "    seq.append(a); a, b = b, a + b\n"
        "json.dump(seq, open('/workspace/fib.json', 'w'))\n"
        "print('wrote', len(seq), 'numbers')\n"))])
    print("run script  :", sh(sb, "python /workspace/fib.py"))
    print("read file   :", sb.files.read_file("/workspace/fib.json"))
    print("ls          :", sh(sb, "ls -la /workspace | tail -n +2 | awk '{print $NF}' | tr '\\n' ' '"))
    print("internet    :", sh(sb, "python -c \"import urllib.request as u; u.urlopen('https://pypi.org', timeout=3)\" "
                                  "2>/dev/null && echo reachable || echo blocked"), "(blocked by default; see README)")
finally:
    sb.kill()
    print(f"\nsandbox {sb.id} deleted")
