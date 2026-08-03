# Implementation and verification status

Updated: 2026-08-04 (Asia/Tokyo) — current release: **v1.0.0**

## Implemented and released

- **M1 (v0.1.0)**: bounded SQL aggregation (sharded, log2-bucket p95), literal
  masking, snapshot HTML/JSON, build/host metadata, loopback admin server
- **v0.2.x**: dashboard + snapshot persistence (`POST /save`, `GET /files/`),
  MySQL/MariaDB schema capture (dbinspect via the first observed DSN),
  collector health / `partial`, generation-scoped SQL store, serialized reset,
  admin auth 3 modes (loopback free / Bearer+`?token=`+cookie / explicit
  `ISUTOOLS_ALLOW_UNAUTHENTICATED=1` opt-in for SSH-tunnel +
  `127.0.0.1`-publish Docker topologies)
- **M2 (v0.2.x)**: HTTP middleware (h1/h2), nginx LTSV delta tailer with
  rotation handling and bounded `POST /collect`, Linux reset-to-snapshot
  procstats (PID-reuse detection, top 1core=100% convention)
- **v0.3.x**: run index at `/` with timestamp-id detail pages
  (`GET /<run-id>`), live report moved to `/live`, benchmark score persisted
  into snapshot meta + shown in every report header, all timestamps pinned
  to JST (`time.FixedZone`, tzdata-free)
- **v0.4.0**: pprof — `GET /pprof/` endpoints (stdlib only, no new deps) +
  automatic CPU profile per `POST /reset` (`ISUTOOLS_PPROF_SECONDS`),
  profiles listed on the dashboard and served via `GET /files/`
- **v0.5.0**: whole-machine CPU utilization over the bench interval
  (`proc.cpuTotal`: busy/user/sys/iowait/steal/idle, top `%Cpu(s)`
  convention) rendered in the Processes section; JSON access-log lines
  auto-detected, accepting both isutools keys and alp defaults
  (`body_bytes` / `response_time`)

- **v0.6.0**: advisor — detects unconfigured ISUCON-critical settings
  (DSN interpolateParams, MySQL sizing, nginx gzip/keepalive/etc via
  ISUTOOLS_NGINX_CONF, kernel somaxconn/port-range/nofile, GOMAXPROCS vs
  cgroup quota); field-verified (+16% score from its first three findings)
- **v0.7.0**: ISUTOOLS_PATH_RULES path normalization, counters API
  (isutools.Count), WebSocket/SSE connection separation with active gauge
- **v1.0.0**: run diff view (GET /diff?a=&b= with per-run diff links),
  session User Flow aggregation (sess: log field -> top transitions),
  k6 scenario example, ABBA overhead gate script, save serialization

## Test evidence

Every release: `go vet ./...` PASS, `go test -race ./...` PASS (12 packages),
aggregate coverage 86%+ (per-package floor 81%+). CI enforces vet + race +
80% coverage gate; benchmarks are informational
(`BenchmarkObserve` ~164ns/op, 0 allocs, worst-case single hot key ×8 threads).
The access-log parser also passed fuzzing. A real TLS HTTP/2 listener test is
environment-gated; the protocol label path is covered with `HTTP/2.0` requests.

## Field verification (private-isu, WSL2 Docker, 2026-08-03..04)

Real benchmarks measured exclusively with isutools while tuning private-isu.
The benchmark control sequence is `POST /reset` → bench → `POST /save?score=`,
with snapshots, JSON, and CPU profiles archived per run:

| stage | score | isutools evidence that drove the change |
|---|---|---|
| baseline (Go impl) | 0 (fail 55) | SQL top: comments query 450s/1.1k calls; DB Schema: no index |
| + indexes, batched N+1 | 19,290 | SQL section totals; procstats mysqld 57% |
| + static images via nginx | 34,224 | accesslog upstime `-` ratio, HTTP bytes |
| + in-process sha512 digest | 45,812 | HTTP: POST /login 63s → 17s |
| + write-through placement fix | 111,756 | httpstats: app-served images collapse |
| + db pool / GOMAXPROCS / nginx keepalive | **299,668** | cpuTotal 11.6% busy vs per-process caps (server 84%/1core); 502 storm diagnosed from bench fails |

## Known hardening gaps (tracked for v1.0 — see DESIGN.md §10.5)

1. httpstats path-normalization rule injection (`/@user` cardinality)
2. snapshot diff view (prev generation stored, no UI comparison yet)
3. generic counter/gauge API
4. WebSocket/SSE connection separation (designed in §5.2.1, not implemented)
5. `collect`/`save` resource caps (review P0-6 residual)
6. formalized on/off ABBA overhead gate as the release criterion
7. cross-collector shared generation gate: SQL and HTTP swap independently,
   so benchmark automation must wait for `POST /reset` to return before load
   starts (the reference bench.sh does)
