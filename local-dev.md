# beacon — local development

Goal: validate metric shape + emission against a real mesh0 project before any Helm changes ship.

## Constraint: macOS has no cgroups

Beacon reads `/sys/fs/cgroup/...` and `/proc/...` — Linux-only paths. On macOS these don't exist, so **all real-data testing happens inside a Linux container.** Docker Desktop's Linux VM is what we test against.

Unit tests for parsers (`parseMemoryMax`, `parseCpuStat`, etc.) take fixture strings as input, so they run natively on macOS via `go test ./...`.

## Phase 1 — stdout-only (no mesh0 account required)

Build and run with `BEACON_DRY_RUN=1` to print events to stdout instead of POSTing.

```bash
cd ~/mesh0/beacon

# Build a local image (Linux amd64 by default on Apple Silicon you can use --platform)
docker build -t beacon:dev .

# Run with realistic limits
docker run --rm \
  --memory=256m \
  --cpus=0.5 \
  -e BEACON_DRY_RUN=1 \
  -e BEACON_INTERVAL_SECONDS=5 \
  -e BEACON_POD_NAME=local-dev \
  -e BEACON_DEPLOYMENT=local \
  -e BEACON_CONTAINER=beacon \
  beacon:dev
```

Expected output (one line per tick):

```json
{"event_name":"pod.health","timestamp":"2026-05-16T...","attributes":{"mem_used_bytes":3145728,"mem_limit_bytes":268435456,"cpu_pct":-1,...}}
{"event_name":"pod.health","timestamp":"2026-05-16T...","attributes":{"mem_used_bytes":3158016,"mem_limit_bytes":268435456,"cpu_pct":0.04,...}}
```

### Sanity-check against `docker stats`

In a second terminal:

```bash
docker stats --no-stream <container_id>
```

Compare:
- `MEM USAGE` ≈ `mem_used_bytes` (within ~1 MiB)
- `MEM LIMIT` == `mem_limit_bytes` (exact)
- `CPU %` ≈ `cpu_pct` after second tick

If these line up, the cgroup reads are correct.

### Generate load to verify CPU% and throttling

```bash
docker run --rm --cpus=0.5 alpine sh -c "while true; do :; done" &
# In another terminal, point beacon at the load container's cgroup by running them in
# the same pod-like setup, OR just run beacon's busy-loop test fixture (see test/load.sh)
```

Easier: bake a `--self-load` flag into beacon for local-only that burns one core for 5 s, then idles. Lets you watch `cpu_pct` climb to ~100, then drop, and `cpu_throttled_usec` increment when `--cpus=0.5` is in effect.

## Phase 2 — emit to a real mesh0 project

Create a throwaway mesh0 project + API key for local testing (don't use the production `api_key_full`):

1. In the mesh0 UI, create project `beacon-local-dev`.
2. Mint an API key with write scope.
3. Export it:

```bash
export MESH0_API_KEY=m0_xxx_yourtestkey
```

Run beacon pointed at the real endpoint:

```bash
docker run --rm \
  --memory=256m \
  --cpus=0.5 \
  -e MESH0_API_KEY=$MESH0_API_KEY \
  -e BEACON_INTERVAL_SECONDS=10 \
  -e BEACON_POD_NAME=local-$(whoami) \
  -e BEACON_DEPLOYMENT=local \
  -e BEACON_CONTAINER=beacon \
  -e BEACON_LOG_LEVEL=debug \
  beacon:dev
```

Verify in the mesh0 UI:
- Events of `event_name=pod.health` arrive within `interval_seconds + ~2s`.
- `attributes.pod_name = local-<you>` so you can filter your test stream out of any noise.
- All numeric fields are non-null (except `cpu_pct=-1` on the very first event).

### Debug a failing send

Hit beacon's local health endpoint:

```bash
docker exec <container_id> wget -qO- http://127.0.0.1:8128/healthz
# {"ok":true,"last_send_ok_at":"...","send_failures":0}
```

If `send_failures > 0`, run with `BEACON_LOG_LEVEL=debug` to see the HTTP response body from mesh0.

## Phase 3 — run beside a real workload (optional, consumer-specific)

Validate that beacon produces sensible numbers under realistic load alongside whatever workload you're integrating with. This step is intentionally generic — beacon doesn't care what the other container is.

Pick any container you can drive load against (Redis, Postgres, nginx, an HTTP app, etc.) and start it with explicit limits, then run beacon with matching limits in the same docker network:

```bash
docker run --rm --name beacon-local \
  --memory=256m --cpus=0.5 \
  -e MESH0_API_KEY=$MESH0_API_KEY \
  -e BEACON_INTERVAL_SECONDS=10 \
  -e BEACON_POD_NAME=local-$(whoami) \
  -e BEACON_DEPLOYMENT=workload \
  -e BEACON_CONTAINER=workload \
  beacon:dev
```

Drive load against the workload container and confirm in the mesh0 UI:
- `cpu_pct` reacts to load.
- `mem_used_bytes` is in the expected range for the configured limit.
- `cpu_throttled_usec` increments only when `--cpus` is below what the workload needs.


## Phase 4 — unit tests

Parsers are isolated and run on macOS:

```bash
cd ~/mesh0/beacon
go test ./...
```

Test fixtures live in `testdata/`:
- `cgroup_memory_max_unlimited.txt` → contains literal `max`
- `cgroup_memory_max_1g.txt` → `1073741824`
- `cgroup_cpu_max_quota.txt` → `50000 100000`
- `cgroup_cpu_max_unlimited.txt` → `max 100000`
- `proc_uptime.txt`, `proc_loadavg.txt`, `cpu_stat.txt`

Target coverage for `collect.go`: 100% of parsing branches. The emission path uses an `http.RoundTripper` interface so tests can substitute a fake client.

## What we are NOT testing locally

- **Sidecar lifecycle in k8s** — k8s 1.29 native sidecar restart behavior, liveness probe failure handling. Validate on a real staging cluster, not locally.
- **High-cardinality fleet behavior** — req/s and connection-reuse patterns at 8 k pods. Load-test the mesh0 ingest endpoint separately; beacon's per-pod load is trivial.
- **Helm template rendering** — that's a consumer's responsibility; their CI runs `helm template` + `helm lint`.

## Exit criteria before tagging a release

1. Phase 1 + Phase 2 run cleanly on a local laptop.
2. Numbers in mesh0 UI match `docker stats` within 5 %.
3. `go test ./...` passes with > 90 % coverage on `collect.go`.
4. `docker image inspect beacon:dev --format '{{.Size}}'` ≤ 8 MB (target 5 MB; 8 MB is a hard ceiling).
5. RSS measured via `docker stats` for the beacon container is < 10 MiB after 5 minutes idle.
