# beacon

**The system-health sidecar that disappears into the noise.**

A 2.5 MB static binary that ships pod-level health metrics from cgroup v2 + `/proc` to your observability backend over HTTPS. One goroutine. One ticker. No queues, no buffers, no surprises.

If you've ever profiled a "lightweight" sidecar and watched it eat 80 MB of RSS at idle, beacon is the antidote.

---

## Why beacon

| | beacon | typical agent |
|---|---|---|
| Image size | **2.5 MB** scratch | 60–200 MB |
| Idle RSS | **≤ 5 MiB** | 30–100 MiB |
| CPU at 30 s interval | **≤ 0.001 cores** | 0.01–0.05 cores |
| Syscalls per tick | **6 reads + 1 POST** | dozens |
| Goroutines | **2** (ticker + healthz) | 20+ |
| Persistent state | **none** | spool files, retry queues |
| Workload assumptions | **none** — runs next to Redis, Postgres, nginx, Node, anything | language/runtime-specific |

Built for the case where you have 8,000 pods and the agent's *own* overhead is the thing pushing you to a bigger node.

---

## How it stays small

beacon is built around a single principle: **a long-lived process emitting on a ticker forever cannot afford to leak anything.** Every design choice falls out of that.

- **No keep-alives.** At 1 request per 30 s the TLS handshake is invisible (~3 ms), and disabling connection pooling removes an entire class of leaks: idle-conn pool growth, HTTP/2 PING-stream leaks, server-side half-close edge cases. HTTP/1.1 only — fewer state-machine corners.
- **No retry queue, no buffer.** Best-effort send. If a tick fails, the next one stands on its own. A health sidecar that buffers events to disk is a health sidecar that fills disks.
- **`-1` for missing data, never panic.** Every metric defaults to `-1` ("not available") so a single unreadable file doesn't poison the tick — useful when `/sys/fs/cgroup/memory.swap.current` doesn't exist on this kernel and you'd rather know than crash.
- **Single goroutine for sampling.** The ticker, the read, the marshal, and the POST all happen on the same goroutine. There is no work queue. There is no fan-out.
- **Verified leak-free in CI.** Three invariants enforced by the test suite over 1,000 emits:
  1. `runtime.NumGoroutine()` stays flat
  2. `/proc/self/fd` count stays flat (Linux)
  3. `runtime.MemStats.HeapInuse` stays within 2× of baseline

The result: drop beacon into a pod and forget it exists.

---

## What it collects

All from `/sys/fs/cgroup/...` (cgroup v2 unified hierarchy) and `/proc/...`. No syscalls beyond `read(2)`. No `/proc/<pid>` walks. No netlink. No eBPF.

| Field | Source |
|---|---|
| `mem_used_bytes` | `memory.current` |
| `mem_limit_bytes` | `memory.max` |
| `mem_swap_used_bytes` | `memory.swap.current` |
| `cpu_usage_usec` | `cpu.stat` |
| `cpu_pct` | derived: Δusage / Δwall |
| `cpu_quota_cores` | `cpu.max` |
| `cpu_throttled_usec` | `cpu.stat` |
| `uptime_s` | `/proc/uptime` |
| `load_1` / `load_5` / `load_15` | `/proc/loadavg` |
| `procs_running` | `/proc/loadavg` |

One event per tick. ~400 bytes serialized JSON. At 30 s × 8,000 pods that's 267 req/s globally — trivial for any modern ingest pipeline.

cgroup v1 is not supported. Targets Kubernetes 1.25+ with the unified hierarchy.

---

## Quick start

```bash
docker run --rm \
  --memory=256m --cpus=0.5 \
  -e MESH0_API_KEY=$MESH0_API_KEY \
  -e BEACON_POD_NAME=my-pod \
  -e BEACON_DEPLOYMENT=my-app \
  ghcr.io/mesh0-ai/beacon:latest
```

No config files. No flags. Everything via env vars.

---

## Configuration

| Var | Default | Notes |
|---|---|---|
| `MESH0_API_KEY` | *(required)* | mesh0 write key |
| `MESH0_ENDPOINT` | `https://api.mesh0.ai/v1/events` | override for staging |
| `BEACON_INTERVAL_SECONDS` | `30` | clamped to `[5, 300]` |
| `BEACON_POD_NAME` | — | downward API `metadata.name` |
| `BEACON_NODE_NAME` | — | downward API `spec.nodeName` |
| `BEACON_NAMESPACE` | — | downward API `metadata.namespace` |
| `BEACON_DEPLOYMENT` | — | free-form label, e.g. `backend`, `redis` |
| `BEACON_CONTAINER` | = `BEACON_DEPLOYMENT` | useful for multi-container pods |
| `BEACON_LABELS` | `""` | comma-separated `k=v` pairs merged into `attributes` |
| `BEACON_DRY_RUN` | `0` | `1` → log events to stdout, no POST, no API key needed |
| `BEACON_LOG_LEVEL` | `info` | `debug` for local |

### Custom labels

beacon doesn't bake any consumer-specific keys into its schema. Bring your own:

```
BEACON_LABELS="env=prod,team=platform,region=us-east-1,customer_id=acme"
```

Reserved core keys (`pod_name`, `mem_used_bytes`, etc.) can't be overridden — beacon drops conflicts and logs at `warn`.

---

## Kubernetes (Helm sidecar)

```yaml
- name: beacon
  image: ghcr.io/mesh0-ai/beacon:latest
  imagePullPolicy: IfNotPresent
  resources:
    requests: { cpu: 5m,  memory: 8Mi }
    limits:   { cpu: 50m, memory: 32Mi }
  env:
    - name: BEACON_POD_NAME
      valueFrom: { fieldRef: { fieldPath: metadata.name } }
    - name: BEACON_NODE_NAME
      valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
    - name: BEACON_NAMESPACE
      valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
    - name: BEACON_DEPLOYMENT
      value: my-app
    - name: MESH0_API_KEY
      valueFrom:
        secretKeyRef: { name: mesh0-credentials, key: api_key }
  livenessProbe:
    httpGet: { path: /healthz, port: 8128, host: 127.0.0.1 }
    periodSeconds: 30
    failureThreshold: 3
  securityContext:
    runAsNonRoot: true
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
    capabilities: { drop: ["ALL"] }
```

The liveness probe is bound to `127.0.0.1` only — beacon never exposes anything on the pod network.

A full reference Helm fragment lives in [plan.md](plan.md).

---

## Event shape

```json
{
  "event_name": "pod.health",
  "timestamp": "2026-05-16T13:45:00.000Z",
  "attributes": {
    "pod_name": "backend-7f8d-9k2lm",
    "node_name": "gke-prod-pool-x-123",
    "namespace": "prod",
    "deployment": "backend",
    "container": "backend",
    "uptime_s": 12345,
    "mem_used_bytes": 524288000,
    "mem_limit_bytes": 1073741824,
    "mem_swap_used_bytes": 0,
    "cpu_pct": 4.2,
    "cpu_quota_cores": 1.0,
    "cpu_throttled_usec": 12000,
    "load_1": 0.42,
    "load_5": 0.35,
    "load_15": 0.31,
    "procs_running": 2,
    "env": "prod",
    "team": "platform"
  }
}
```

---

## Building

```bash
make build      # local Go binary
make test       # go test -count=1 ./...
make image      # 2.5 MB scratch image
make push       # tag + push to ghcr.io/mesh0-ai/beacon
```

Releases happen on `v*` git tags via GitHub Actions — see [`.github/workflows/release.yml`](.github/workflows/release.yml). Multi-arch (`linux/amd64` + `linux/arm64`).

---

## What beacon does *not* do

By design:

- **No process-level metrics.** beacon reports the container, not the workload inside it. If you need PHP-FPM worker counts or Postgres backend stats, that's a workload-specific exporter.
- **No log shipping.** Logs go through your existing pipeline.
- **No traces or spans.** If you have mesh0's metrics-agent for spans, beacon coexists with it; it doesn't replace it.
- **No retries.** A dropped tick is a dropped tick. Next one's in 30 seconds.
- **No cgroup v1.** Kubernetes 1.25+ only.

The whole point is the binary you forget about. Anything more ambitious belongs in a different binary.

---

## License

TBD.
