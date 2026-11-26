# Bake-off results (kind-20261111, target=kind)

> **kind run:** functional results (compat, escape, resume correctness) are valid; latency, ceiling
> and density numbers reflect a laptop VM, not a cell. Re-run on GKE for capacity numbers.

## Exit criteria

| Check | Measured | Target | |
|---|---|---|---|
| Warm claim p99 | 0.065s | < 1s | ✅ |
| Cold claim p99 | 2.999s | < 15s | ✅ |
| Sustained claim ceiling | 0/s | ≥ 20/s | ❌ |
| Resume 2048 MB end-to-end | 8.97s | < 10s | ✅ |
| Workspace intact after resume | True | true | ✅ |
| gVisor compat | 11/11 | all | ✅ |
| Escape suite | 15/15 | 100% | ✅ |

## burst

```json
{
  "test": "burst",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:56:47",
  "pool": "locked",
  "results": {
    "5": {
      "claim_ready_s": {
        "n": 5,
        "p50": 0.061,
        "p90": 0.062,
        "p99": 0.062,
        "max": 0.062,
        "mean": 0.061
      },
      "failures": 0,
      "pool_size": 5,
      "beyond_pool": 0
    },
    "10": {
      "claim_ready_s": {
        "n": 10,
        "p50": 0.085,
        "p90": 1.496,
        "p99": 1.501,
        "max": 1.501,
        "mean": 0.776
      },
      "failures": 0,
      "pool_size": 5,
      "beyond_pool": 5
    },
    "25": {
      "claim_ready_s": {
        "n": 14,
        "p50": 1.015,
        "p90": 2.047,
        "p99": 2.051,
        "max": 2.051,
        "mean": 1.215
      },
      "failures": 11,
      "pool_size": 5,
      "beyond_pool": 20
    }
  }
}
```

## compat

```json
{
  "test": "compat",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:30:26",
  "rows": [
    {
      "step": "pip_install",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 21.13,
      "runc_s": 17.72,
      "slowdown_x": 1.19
    },
    {
      "step": "numpy_matmul",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 0.97,
      "runc_s": 3.61,
      "slowdown_x": 0.27
    },
    {
      "step": "pandas_groupby",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 0.51,
      "runc_s": 0.38,
      "slowdown_x": 1.34
    },
    {
      "step": "sklearn_fit",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 3.43,
      "runc_s": 3.1,
      "slowdown_x": 1.11
    },
    {
      "step": "git_clone",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 2.51,
      "runc_s": 1.57,
      "slowdown_x": 1.6
    },
    {
      "step": "git_ops",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 0.08,
      "runc_s": 0.02,
      "slowdown_x": 4.0
    },
    {
      "step": "npm_install",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 3.31,
      "runc_s": 2.67,
      "slowdown_x": 1.24
    },
    {
      "step": "pytest_200",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 0.49,
      "runc_s": 0.26,
      "slowdown_x": 1.88
    },
    {
      "step": "sqlite_200k",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 0.2,
      "runc_s": 0.13,
      "slowdown_x": 1.54
    },
    {
      "step": "small_files_20k",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 1.98,
      "runc_s": 0.47,
      "slowdown_x": 4.21
    },
    {
      "step": "big_file_1g",
      "gvisor_ok": true,
      "runc_ok": true,
      "gvisor_s": 3.19,
      "runc_s": 1.84,
      "slowdown_x": 1.73
    }
  ]
}
```

## density

```json
{
  "test": "density",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T16:02:31",
  "sandbox_nodes": 1,
  "max_ready": 10,
  "per_node": 10.0,
  "pending_at_stop": 2,
  "steps": [
    {
      "replicas": 10,
      "ready_s": 2.0
    },
    {
      "replicas": 20,
      "ready": 18,
      "timeout": true
    }
  ],
  "nodes": [
    {
      "node": "sandbox-bakeoff-worker2",
      "allocatable": {
        "cpu": "11",
        "ephemeral-storage": "61202244Ki",
        "hugepages-1Gi": "0",
        "hugepages-2Mi": "0",
        "hugepages-32Mi": "0",
        "hugepages-64Ki": "0",
        "memory": "8025220Ki",
        "pods": "110"
      }
    }
  ],
  "note": "tiny = 100m/128Mi. Limits hit: CPU/mem requests, maxPods (110), or runsc overhead"
}
```

## escape

```json
{
  "test": "escape",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:06:01",
  "all_pass": true,
  "checks": [
    {
      "check": "gvisor_runtime",
      "pass": true,
      "detail": "Linux version 4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016"
    },
    {
      "check": "non_root",
      "pass": true,
      "detail": "uid=1000"
    },
    {
      "check": "no_sa_token",
      "pass": true,
      "detail": "absent"
    },
    {
      "check": "host_procs_hidden",
      "pass": true,
      "detail": "pids=5"
    },
    {
      "check": "no_mount",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "no_sysctl_write",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "no_raw_socket",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "metadata_blocked",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "metadata_dns_blocked",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "k8s_api_blocked",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "internet_blocked",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "peer_sandbox_blocked",
      "pass": true,
      "detail": "blocked"
    },
    {
      "check": "dns_works",
      "pass": true,
      "detail": "ok"
    },
    {
      "check": "object_store_reachable",
      "pass": true,
      "detail": "ok"
    },
    {
      "check": "peer_server_listening_control",
      "pass": true,
      "detail": "peer http.server answers on localhost (proves peer_sandbox_blocked is real)"
    }
  ],
  "note": "metadata checks are only meaningful on a cloud cluster"
}
```

## latency_cold

```json
{
  "test": "latency_cold",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:17:48",
  "claim_ready_s": {
    "n": 10,
    "p50": 2.012,
    "p90": 2.022,
    "p99": 2.999,
    "max": 2.999,
    "mean": 2.077
  },
  "failures": 0,
  "note": "image pre-pulled; excludes node provisioning (+30-90s on cloud) and image pull"
}
```

## latency_warm

```json
{
  "test": "latency_warm",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:17:08",
  "pool": "locked",
  "claim_ready_s": {
    "n": 30,
    "p50": 0.016,
    "p90": 0.027,
    "p99": 0.065,
    "max": 0.065,
    "mean": 0.019
  },
  "first_exec_via_kubectl_s": {
    "n": 10,
    "p50": 0.104,
    "p90": 0.131,
    "p99": 0.177,
    "max": 0.177,
    "mean": 0.111
  },
  "failures": 0,
  "note": "first_exec includes kubectl exec overhead; sandbox data path (gateway\u2192sandboxd) will be faster"
}
```

## ramp

```json
{
  "test": "ramp",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:59:27",
  "pool": "locked",
  "hold_s": 10,
  "duration_s": 90,
  "steps": [
    {
      "rate_per_s": 2,
      "issued": 179,
      "claim_ready_s": {
        "n": 172,
        "p50": 3.338,
        "p90": 92.538,
        "p99": 114.646,
        "max": 115.141,
        "mean": 21.002
      },
      "failures": 7,
      "frac_ready_under_2s": 0.151,
      "pool_ready_min": 0,
      "api_probe_s": {
        "n": 155,
        "p50": 0.005,
        "p90": 0.01,
        "p99": 0.013,
        "max": 0.018,
        "mean": 0.006
      }
    }
  ],
  "max_healthy_rate_per_s": 0,
  "healthy_def": "claim p99 < 5.0s, 0 failures, API probe p99 < 1s"
}
```

## resume

```json
{
  "test": "resume",
  "target": "kind",
  "run": "kind-20261111",
  "at": "2026-11-11T15:20:00",
  "pool": "ws",
  "results": [
    {
      "size_mb": 100,
      "gen": {
        "op": "gen",
        "secs": 1.965,
        "detail": {
          "files_small": 1823,
          "files_medium": 27,
          "files_large": 2,
          "bytes": 104857600
        }
      },
      "snapshot": {
        "op": "snapshot",
        "pack_secs": 0.415,
        "upload_secs": 0.844,
        "total_secs": 1.26,
        "compressed_bytes": 70739235,
        "chunks": 2
      },
      "resume_claim_s": 0.048,
      "hydrate": {
        "op": "hydrate",
        "download_secs": 0.233,
        "unpack_secs": 0.509,
        "total_secs": 0.742
      },
      "resume_end_to_end_s": 1.16,
      "intact": true,
      "digest": {
        "files": 1852,
        "bytes": 106159586,
        "listing_sha": "9feb07f67dc9268c"
      }
    },
    {
      "size_mb": 500,
      "gen": {
        "op": "gen",
        "secs": 8.826,
        "detail": {
          "files_small": 9017,
          "files_medium": 117,
          "files_large": 8,
          "bytes": 524288000
        }
      },
      "snapshot": {
        "op": "snapshot",
        "pack_secs": 1.834,
        "upload_secs": 1.71,
        "total_secs": 3.544,
        "compressed_bytes": 352683582,
        "chunks": 6
      },
      "resume_claim_s": 0.029,
      "hydrate": {
        "op": "hydrate",
        "download_secs": 0.385,
        "unpack_secs": 1.533,
        "total_secs": 1.918
      },
      "resume_end_to_end_s": 2.43,
      "intact": true,
      "digest": {
        "files": 9142,
        "bytes": 525326522,
        "listing_sha": "26dc7298dd551c90"
      }
    },
    {
      "size_mb": 1024,
      "gen": {
        "op": "gen",
        "secs": 17.737,
        "detail": {
          "files_small": 18432,
          "files_medium": 260,
          "files_large": 18,
          "bytes": 1073741824
        }
      },
      "snapshot": {
        "op": "snapshot",
        "pack_secs": 3.886,
        "upload_secs": 2.556,
        "total_secs": 6.442,
        "compressed_bytes": 721974186,
        "chunks": 11
      },
      "resume_claim_s": 0.04,
      "hydrate": {
        "op": "hydrate",
        "download_secs": 1.079,
        "unpack_secs": 3.771,
        "total_secs": 4.85
      },
      "resume_end_to_end_s": 5.5,
      "intact": true,
      "digest": {
        "files": 18710,
        "bytes": 1074093209,
        "listing_sha": "262b14dfbbdc60c1"
      }
    },
    {
      "size_mb": 2048,
      "gen": {
        "op": "gen",
        "secs": 38.024,
        "detail": {
          "files_small": 36866,
          "files_medium": 497,
          "files_large": 32,
          "bytes": 2147483648
        }
      },
      "snapshot": {
        "op": "snapshot",
        "pack_secs": 10.576,
        "upload_secs": 6.988,
        "total_secs": 17.564,
        "compressed_bytes": 1443963890,
        "chunks": 22
      },
      "resume_claim_s": 0.085,
      "hydrate": {
        "op": "hydrate",
        "download_secs": 1.466,
        "unpack_secs": 7.025,
        "total_secs": 8.492
      },
      "resume_end_to_end_s": 8.97,
      "intact": true,
      "digest": {
        "files": 37395,
        "bytes": 2148290000.0,
        "listing_sha": "48cfa70f51872d2f"
      }
    }
  ],
  "note": "prototype: tar+zstd+64MB chunks, s5cmd 32 workers. Production = content-addressed incremental chunks"
}
```
