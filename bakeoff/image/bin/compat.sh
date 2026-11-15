#!/usr/bin/env bash
# Workload compatibility + speed, run in 'open' (gVisor) and 'runc' sandboxes. One JSON line per step.
cd /workspace && rm -rf compat && mkdir compat && cd compat
step() { local name=$1; shift; local t0=$(date +%s.%N); if bash -c "$*" >/tmp/$name.log 2>&1; then ok=true; else ok=false; fi
  local t1=$(date +%s.%N); printf '{"step":"%s","ok":%s,"secs":%s}\n' "$name" "$ok" "$(python3 -c "print(round($t1-$t0,2))")"; }
step pip_install    "pip install --user -q numpy pandas scikit-learn requests"
step numpy_matmul   "python3 -c 'import numpy as n; a=n.random.rand(1500,1500); [a@a for _ in range(5)]'"
step pandas_groupby "python3 -c 'import pandas as p, numpy as n; d=p.DataFrame({\"k\":n.random.randint(0,1000,2_000_000),\"v\":n.random.rand(2_000_000)}); d.groupby(\"k\").v.agg([\"mean\",\"sum\"])'"
step sklearn_fit    "python3 -c 'from sklearn.ensemble import RandomForestClassifier as R; from sklearn.datasets import make_classification as m; X,y=m(20000,20); R(50,n_jobs=1).fit(X,y)'"
step git_clone      "git clone -q --depth 50 https://github.com/psf/requests.git"
step git_ops        "cd requests && git log --oneline | wc -l && git status && git diff HEAD~10 --stat"
step npm_install    "mkdir n && cd n && npm init -y >/dev/null && npm install --silent lodash express"
step pytest_200     "mkdir t && python3 -c \"
import pathlib
for i in range(20):
    pathlib.Path(f't/test_{i}.py').write_text(''.join(f'def test_{j}():\n    assert sum(range({j}+100))>0\n' for j in range(10)))\" && python3 -m pytest -q t"
step sqlite_200k    "python3 -c 'import sqlite3; c=sqlite3.connect(\"x.db\"); c.execute(\"create table t(a,b)\"); c.executemany(\"insert into t values(?,?)\",((i,str(i)) for i in range(200000))); c.commit(); c.execute(\"select count(*),sum(a) from t\").fetchone()'"
step small_files_20k "python3 -c 'import os; os.makedirs(\"sf\"); [open(f\"sf/{i}\",\"w\").write(\"x\"*512) for i in range(20000)]; [os.stat(f\"sf/{i}\") for i in range(20000)]'"
step big_file_1g    "dd if=/dev/zero of=big bs=1M count=1024 conv=fsync && rm big"
