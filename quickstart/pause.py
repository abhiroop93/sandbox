#!/usr/bin/env python3
"""Create a sandbox, install a package and write a file, then pause it. Prints the sandbox id for resume.py.

    python pause.py            # → sandbox <id> paused
    python resume.py <id>
"""
import os
import time

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import NetworkPolicy

cfg = ConnectionConfigSync(domain=os.environ["SRVM_DOMAIN"], api_key=os.environ["SRVM_API_KEY"], use_server_proxy=True)
run = lambda sb, c: "".join(m.text for m in sb.commands.run(c).logs.stdout).strip()

# Internet is blocked by default; pip needs it.
sb = SandboxSync.create("python:3.12-slim", connection_config=cfg,
                        network_policy=NetworkPolicy(default_action="allow", egress=[]))
run(sb, "pip install --user -q --disable-pip-version-check cowsay && echo 'hello demo' > /workspace/notes.txt")
sb.pause()  # /workspace → object storage, pod released
while sb.get_info().status.state != "Paused":
    time.sleep(0.5)
print(f"sandbox {sb.id} paused\nresume it with: python resume.py {sb.id}")
