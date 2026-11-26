#!/usr/bin/env python3
"""Resume a sandbox paused by pause.py and show that its files and installed package survived.

    python resume.py <sandbox-id>
"""
import os
import sys
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

if len(sys.argv) != 2:
    sys.exit("usage: python resume.py <sandbox-id>   (pause.py prints it)")
cfg = ConnectionConfigSync(domain=os.environ["SANDBOX_DOMAIN"], api_key=os.environ["SANDBOX_API_KEY"], use_server_proxy=True)
run = lambda sb, c: "".join(m.text for m in sb.commands.run(c).logs.stdout).strip()

sb = SandboxSync.resume(sys.argv[1], connection_config=cfg, resume_timeout=timedelta(seconds=60))
print(run(sb, "cat /workspace/notes.txt && python -m cowsay -t 'still here'"))  # file and pip install both survived
sb.kill()
