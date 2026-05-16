# beacon — per-pod system-health sidecar

## Goal

A tiny static-binary sidecar that can be dropped into any Kubernetes pod (or any Linux container) to periodically sample basic system health from cgroup v2 + `/proc` and ship it to mesh0.

**Non-goals**: process-level metrics, log shipping, distributed traces (mesh0's existing metrics-agent + UDS sink already cover spans).

## Design constraints

- Static binary, `FROM scratch` image, target ≤ 6 MB compressed.
- Per-pod RSS ≤ 5 MB, average CPU ≤ 0.001 cores at 30 s interval.
- Zero runtime config files. Everything via env vars (downward API + secret mount).
- No persistent state. No retry queue. Best-effort send; drop on failure.
- **Self-contained transport.** Talks directly to the mesh0 HTTPS API. Does not require the metrics-agent UDS sidecar to be present in the pod.
- **Workload-agnostic.** No assumptions about what's running in the other containers — works the same alongside Redis, Postgres, nginx, Node, PHP, Go, etc.

## Architecture

```
┌──────────────────────────── pod ────────────────────────────┐
│                                                             │
│  ┌────────────┐         ┌──────────────────────────────┐    │
│  │ main app   │         │ beacon sidecar               │    │
│  │ (anything) │         │  ticker (interval)           │    │
│  │            │         │   ├─ read cgroup + /proc     │    │
│  │            │         │   ├─ build event             │    │
│  └────────────┘         │   └─ POST → api.mesh0.ai     │    │
│                         └──────────────────────────────┘    │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

Single goroutine. Single ticker. One reused `http.Client` (keep-alive on).

## Layout

```
~/mesh0/beacon/
├── plan.md           # this file
├── local-dev.md      # local validation workflow
├── main.go           # entry, signal handling, ticker loop
├── collect.go        # cgroup v2 + /proc readers
├── emit.go           # http client + payload
├── config.go         # env → Config struct
├── Dockerfile        # multi-stage, scratch final
├── Makefile          # build, image, push
├── go.mod
└── go.sum
```

Single Go package, no `internal/`. Total target: < 500 LOC.

## Metrics collected

All from cgroup v2 (`/sys/fs/cgroup/...`) + `/proc`. No syscalls beyond `read(2)`.

| Field | Source | Notes |
|---|---|---|
| `mem_used_bytes` | `memory.current` | container RSS+cache |
| `mem_limit_bytes` | `memory.max` | `"max"` literal → `-1` |
| `mem_swap_used_bytes` | `memory.swap.current` | optional, `-1` if file absent |
| `cpu_usage_usec` | `cpu.stat` → `usage_usec` line | raw counter |
| `cpu_pct` | derived: Δusage / Δwall × 100 | per cpu-equivalent, NOT normalized to quota |
| `cpu_quota_cores` | `cpu.max` → `quota / period` | `"max"` → `-1` |
| `cpu_throttled_usec` | `cpu.stat` → `throttled_usec` | useful signal even at low CPU |
| `uptime_s` | `/proc/uptime` first field | container ns uptime |
| `load_1` / `load_5` / `load_15` | `/proc/loadavg` | host-level; cheap and surprisingly useful |
| `procs_running` | `/proc/loadavg` 4th field | "X/Y" → take X |

### CPU% calculation

Store `lastUsageUsec` + `lastSampleMonotonic` between ticks. First tick emits `cpu_pct = -1` (no baseline). Thereafter:

```
delta_cpu_us  = current_usage_usec - last_usage_usec
delta_wall_us = now_monotonic_us  - last_sample_us
cpu_pct = (delta_cpu_us / delta_wall_us) * 100   // % of one core
```

We deliberately do **not** normalize to `cpu_quota_cores`. Let the dashboard do that — keeps the binary simple and the raw number more useful for unconstrained pods.

### Cgroup v1 fallback

Targets k8s 1.25+ with cgroup v2 unified hierarchy. We will **not** support cgroup v1. If `/sys/fs/cgroup/cgroup.controllers` doesn't exist, log once and emit only `/proc` fields with cgroup fields set to `-1`.

## Event shape

POSTed as a single JSON object per tick to `${MESH0_ENDPOINT}` (default `https://api.mesh0.ai/v1/events`). Schema matches what `metrics-agent` already sends:

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

    "custom_label_a": "value",
    "custom_label_b": "value"
  }
}
```

Single event per tick. ~400 bytes serialized. We do **not** batch — at 30 s interval with N pods we're at N/30 req/s globally, which is trivial.

### Custom labels

beacon does not bake any consumer-specific keys into its event schema. Consumers attach their own (environment, team, customer id, region, anything) by setting `BEACON_LABELS` (see below), and they are merged into `attributes` verbatim. This keeps beacon decoupled from any one product's data model.

## Configuration (env vars)

| Var | Default | Notes |
|---|---|---|
| `MESH0_API_KEY` | (required, no default) | mesh0 write key |
| `MESH0_ENDPOINT` | `https://api.mesh0.ai/v1/events` | override for staging/test |
| `BEACON_INTERVAL_SECONDS` | `30` | clamped to `[5, 300]` |
| `BEACON_POD_NAME` | — | typically downward API `metadata.name` |
| `BEACON_NODE_NAME` | — | typically downward API `spec.nodeName` |
| `BEACON_NAMESPACE` | — | typically downward API `metadata.namespace` |
| `BEACON_DEPLOYMENT` | — | free-form label, e.g. `backend`, `redis` |
| `BEACON_CONTAINER` | same as `BEACON_DEPLOYMENT` | free-form; useful when one pod has many containers |
| `BEACON_LABELS` | `""` | comma-separated `key=value` pairs merged into `attributes` |
| `BEACON_DRY_RUN` | `0` | `1` → log events to stdout instead of POSTing (no API key required) |
| `BEACON_LOG_LEVEL` | `info` | `debug` for local |

All read once at startup; no hot reload. Pod restart applies config changes.

### `BEACON_LABELS` format

```
BEACON_LABELS="env=prod,team=platform,region=us-east-1"
```

Each `k=v` is added to `attributes` as a string. Reserved core keys (e.g. `pod_name`, `mem_used_bytes`) cannot be overridden — beacon drops conflicts and logs at `warn`.

## HTTP client

beacon is required to be 100% leak-free: a long-lived process emitting on a ticker forever has no tolerance for goroutine/connection/memory growth. The transport is configured to make leaks structurally impossible, not "carefully avoided."

**Keep-alives are disabled.** At 1 request per `interval_seconds` (default 30) the TCP+TLS handshake cost is invisible and removes an entire class of failure modes (idle-conn pool growth, HTTP/2 PING-stream leaks, server-side half-close edge cases).

```go
transport := &http.Transport{
    DisableKeepAlives:     true,    // forces conn close after each request
    MaxIdleConns:          0,
    MaxIdleConnsPerHost:   -1,      // do not pool
    IdleConnTimeout:       1 * time.Second,
    TLSHandshakeTimeout:   3 * time.Second,
    ResponseHeaderTimeout: 4 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    ForceAttemptHTTP2:     false,   // HTTP/1.1 only — fewer state-machine corners
}
client := &http.Client{
    Transport: transport,
    Timeout:   5 * time.Second,     // upper bound on the entire request
}
```

**Send loop contract** (enforced by code review + a unit test that injects a slow/erroring server):

```go
func send(ctx context.Context, client *http.Client, body []byte) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
    if err != nil { return err }
    req.Header.Set("Authorization", "Bearer "+apiKey)
    req.Header.Set("Content-Type", "application/json")
    req.Close = true  // belt-and-suspenders: ask the server to close too

    resp, err := client.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()
    _, _ = io.Copy(io.Discard, resp.Body) // drain so close releases the FD cleanly

    if resp.StatusCode/100 != 2 {
        return fmt.Errorf("non-2xx: %d", resp.StatusCode)
    }
    return nil
}
```

Three invariants the test suite verifies:

1. **No goroutine growth.** Test harness runs 10,000 emits against a local server, then asserts `runtime.NumGoroutine()` is the same as before the loop (±1 for test scheduling).
2. **No FD growth.** Same loop, asserts the open-FD count via `/proc/self/fd` (Linux test only) is flat.
3. **No heap growth across emits.** Loop with `runtime.GC()` + `runtime.ReadMemStats` checkpoints; assert `HeapInuse` doesn't drift upward over 1,000 emits.

On non-2xx or transport error: log at `warn`, increment an in-memory `send_failures` counter, drop the event. No retry, no buffer. The next tick stands on its own.

`send_failures` and `last_send_ok_at` are exposed via the local `/healthz` for diagnosability.

## Health endpoint

Tiny `http.Server` on `127.0.0.1:8128` (don't expose to pod network):

- `GET /healthz` → 200 with `{ "ok": true, "last_send_ok_at": "...", "send_failures": 0 }`
- Used by k8s liveness probe.

Probe is optional — beacon dying shouldn't kill the workload pod. We'll set `livenessProbe` on the sidecar **only**, not the pod-level. If beacon crashes too many times, k8s restarts just the sidecar (k8s 1.29+ sidecar containers support this natively via `restartPolicy: Always` in init container slot).

## Lifecycle

```
main()
  ├─ parse env → Config (fail fast on missing API key unless DRY_RUN)
  ├─ detect cgroup version (warn + degrade on v1)
  ├─ start /healthz server (goroutine)
  ├─ build http.Client
  ├─ ticker := time.NewTicker(interval)
  ├─ sample() once immediately, emit
  └─ loop:
       select {
         case <-ticker.C:    sample + emit
         case sig := <-sigs: graceful shutdown
       }

graceful shutdown
  ├─ stop ticker
  ├─ optional: one final emit (yes — captures shutdown signal)
  └─ exit 0
```

## Docker image

```Dockerfile
# build stage
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always)" \
    -o /out/beacon .

# final
FROM scratch
COPY --from=build /out/beacon /beacon
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
USER 65532:65532
ENTRYPOINT ["/beacon"]
```

Expected size: ~5 MB.

Registry: `ghcr.io/mesh0-ai/beacon:vX.Y.Z` (same org as `metrics-agent`).

## Resource budget at scale

Per pod:

| | value |
|---|---|
| RSS | ≤ 5 Mi |
| CPU avg | ≤ 0.001 cores |
| HTTPS req/s | 1 / interval_seconds |
| Egress bytes/event | ~400 B |

At 8,000 pods cluster-wide on a 30 s interval that's 267 req/s and ~40 GiB of RSS aggregated across the fleet. Per node with 50 pods it's ~250 MiB. If that ever becomes a concern, the answer is a DaemonSet variant of beacon, not optimizing the per-pod sidecar further.

## Phased rollout

1. **Initial binary** — `BEACON_DRY_RUN=1` mode emitting to stdout. Validate metrics against `docker stats` (see [local-dev.md](local-dev.md)).
2. **HTTPS emitter** — POST to a dev mesh0 project. Confirm events appear.
3. **Image + CI** — push to `ghcr.io/mesh0-ai/beacon` via GitHub Actions on tag.
4. **First consumer integration** — wire into one workload in a staging environment behind a feature flag.
5. **Dashboard in mesh0 UI** — promoted columns for `mem_used_bytes`, `cpu_pct`, `cpu_throttled_usec`. Single "pod health" table view.
6. **Expand consumers** — additional workloads/products as they opt in.

## Open questions to resolve before coding

1. **API key scope** — does mesh0 support narrower-than-`full` write keys, or is reusing the full write key the only option today? Affects whether consumers need a new key minted just for beacon.
2. **Event vs metric** — mesh0 sends `events` today. Are pod-health samples better modeled as a separate `metrics` endpoint upstream, or fine as repeated events? Quick call with the mesh0 team.
3. **Per-container detail** — should beacon emit per-container stats from `/sys/fs/cgroup/<container-id>/...` when multiple containers exist in a pod? Adds complexity. **Default: no** — one event per pod is enough. Revisit if signal is missing.
4. **Aggregated stats endpoint** — does mesh0 API support batching to amortize TLS handshakes? Single connection keep-alive already handles this for us; not urgent.
5. **Restart isolation in k8s < 1.29** — native sidecar containers require 1.29+. Older clusters: sidecar lifecycle ties to pod lifecycle (acceptable; just means beacon restarts with pod).

---

# Reference: Helm sidecar wiring

This is a generic example showing how a consumer can drop beacon into their own Helm charts. It is **not** part of beacon's surface area — beacon ships as a binary + image only.

```yaml
{{- define "beaconSidecar" -}}
- name: beacon
  image: {{ .Values.beacon.image.repository }}:{{ .Values.beacon.image.tag }}
  imagePullPolicy: IfNotPresent
  resources:
    requests:  { cpu: 5m,   memory: 8Mi }
    limits:    { cpu: 50m,  memory: 32Mi }
  env:
    - name: BEACON_POD_NAME
      valueFrom: { fieldRef: { fieldPath: metadata.name } }
    - name: BEACON_NODE_NAME
      valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
    - name: BEACON_NAMESPACE
      valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
    - name: BEACON_DEPLOYMENT
      value: {{ .deployment | quote }}
    - name: BEACON_CONTAINER
      value: {{ .container | default .deployment | quote }}
    - name: BEACON_LABELS
      value: {{ .Values.beacon.labels | default "" | quote }}
    - name: BEACON_INTERVAL_SECONDS
      value: {{ .Values.beacon.interval_seconds | default 30 | quote }}
    - name: MESH0_API_KEY
      valueFrom:
        secretKeyRef:
          name: {{ .Values.beacon.secret.name }}
          key:  {{ .Values.beacon.secret.key }}
  livenessProbe:
    httpGet: { path: /healthz, port: 8128, host: 127.0.0.1 }
    periodSeconds: 30
    failureThreshold: 3
  securityContext:
    runAsNonRoot: true
    readOnlyRootFilesystem: true
    allowPrivilegeEscalation: false
    capabilities: { drop: ["ALL"] }
{{- end -}}
```

Include from each deployment template:

```yaml
{{- if .Values.beacon.enabled }}
{{- include "beaconSidecar" (dict "Values" .Values "deployment" "myapp") | nindent 8 }}
{{- end }}
```

Minimal `values.yaml`:

```yaml
beacon:
  enabled: false
  interval_seconds: 30
  image:
    repository: ghcr.io/mesh0-ai/beacon
    tag: latest
  secret:
    name: mesh0-credentials
    key:  api_key
  labels: "env=prod,team=platform"   # merged into event attributes verbatim
```
