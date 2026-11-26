#!/usr/bin/env python3
"""Generate a realistic-ish workspace of SIZE_MB under /workspace/ws.
Mix: 15% many small text files (node_modules/.git-like), 25% medium text, 60% large binary (incompressible).
"""
import os, sys, random
size_mb = int(sys.argv[1]); root = sys.argv[2] if len(sys.argv) > 2 else "/workspace/ws"
total = size_mb * 1024 * 1024
rnd = random.Random(42)
words = [b"import", b"def", b"return", b"const", b"function", b"class", b"self", b"value", b"data", b"\n", b"    "]
def text(n):
    out = bytearray()
    while len(out) < n:
        out += rnd.choice(words) + b" "
    return bytes(out[:n])
def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f: f.write(data)
small_budget, med_budget = int(total * .15), int(total * .25)
large_budget = total - small_budget - med_budget
i = 0; used = 0
while used < small_budget:                 # small: 1-16 KB, nested dirs
    n = rnd.randint(1024, 16384)
    write(f"{root}/node_modules/pkg{i//200}/lib/f{i}.js", text(n)); used += n; i += 1
used = 0; j = 0
while used < med_budget:                   # medium: 100 KB - 2 MB
    n = rnd.randint(100_000, 2_000_000)
    write(f"{root}/src/mod{j//20}/file{j}.py", text(n)); used += n; j += 1
used = 0; k = 0
os.makedirs(f"{root}/data", exist_ok=True)
while used < large_budget:                 # large: 16-64 MB random
    n = min(rnd.randint(16, 64) * 1024 * 1024, large_budget - used)
    with open(f"{root}/data/blob{k}.bin", "wb") as f:
        rem = n
        while rem > 0:
            c = min(rem, 8 * 1024 * 1024); f.write(os.urandom(c)); rem -= c
    used += n; k += 1
print(f'{{"files_small":{i},"files_medium":{j},"files_large":{k},"bytes":{total}}}')
