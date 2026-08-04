# Implementation and verification status

Updated: 2026-08-04 (Asia/Tokyo) — current release: **v1.0.0**

`v1.0.0` tag = `faa7ca8`. The fixes under “post-release hardening” below are
currently local/unreleased changes and must not be attributed to that tag.

## Implemented and released

- **M1 (v0.1.0)**: bounded SQL aggregation (sharded, log2-bucket p95), literal
  masking, snapshot HTML/JSON, build/host metadata, loopback admin server
- **v0.2.x**: dashboard + snapshot persistence (`POST /save`, `GET /files/`),
  MySQL/MariaDB schema capture (dbinspect via the first observed DSN),
  collector health / `partial`, generation-scoped SQL store, serialized reset,
  historical admin auth modes (the post-release working tree removes
  Bearer/query-token auth and standardizes on SSH-only reachability)
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

Current working tree verification (2026-08-04, Go 1.26.5): `go vet ./...` PASS,
`go test -race -shuffle=on -coverprofile=... ./...` PASS (14 packages), aggregate
coverage **85.0%**. Package coverage is not uniformly 80%: the root package is
70.7%; CI enforces the documented **aggregate** 80% gate and separately runs
`go test ./...` on Go 1.24.x. `golangci-lint run ./...` reports 0 issues,
`govulncheck ./...` reports no known vulnerabilities, and the access-log parser
completed a 10-second fuzz run (~761k executions) without a failure.
`BenchmarkObserve` was 153.2 ns/op, 0 B/op, 0 allocs/op on Apple M3.

A real TLS HTTP/2 listener, HTTP/3 listener, deployed target, and physical/remote
benchmark are not implied by those local results.

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

## Historical v1.0.0 ABBA observation (not a complete release gate)

Recorded on private-isu (2026-08-04, off→on→on→off):

```text
mode=off score=361203
mode=on  score=364162
mode=on  score=361929
mode=off score=360734
off avg: 360968 / on avg: 363045
ABBA overhead: -0.58% (gate: < 2%) -> PASS
```

This four-run score observation has only one ABBA block, no p95/error-rate
series, no confidence interval, and no archived binary fingerprint. It is
therefore useful historical evidence but cannot establish “zero overhead” or
satisfy the release-gate contract in DESIGN.md §7. Also, the evidence commit
`9924ddc` is 11 minutes newer than tag `v1.0.0` (`faa7ca8`).

The hardened `examples/abba.sh` now requires at least three blocks, fixed
warm-up, a stable binary/image fingerprint, score/p95/error rate, TSV
provenance, and paired 95% CI gates. It has passed its local script contract;
it has **not yet been rerun on private-isu**.

## Post-release hardening in the current working tree (unreleased)

- SQL normalization removes all comments, masks PG dollar quotes and
  hex/binary/scientific literals, bounds safe tags and the final identity
- confirmed WebSocket/SSE connections detach from reset generations; rejected
  upgrades stay in HTTP; Hijack close/wire bytes and duration p95 are tracked
- run IDs are collision-free, legacy ambiguous IDs fail explicitly, snapshots
  and reads are size-bounded, and concurrent mutations fail fast
- counter/session/flow identities are bounded and surface dropped/partial health
- access-log collection has context and per-call byte limits; nginx LTSV/JSON
  and explicit Apache JSON `%D` microseconds are documented
- diff separates total/count/avg and does not color unequal-count totals as an
  improvement; health recovers after successful collect/reset
- DB driver, nginx/Apache, pprof/procstats/k6 prerequisites are documented in
  `docs/INTEGRATION.md`
- HTTP/3/QUIC readiness advisor inspects nginx/Caddy/Envoy config, local
  UDP/443, explicit edge/network evidence, client-facing protocol traffic,
  and optional retransmit/drop counters; it does not claim a real listener test
- scenario stories group bounded `METHOD URI` journeys by explicit non-secret
  scenario label and pseudonymous session; raw authentication data is rejected
- the retained Bearer/query-token implementation was removed to match the
  authoritative SSH-only security decision; non-loopback remains explicit
  opt-in and fail-closed otherwise

## Resolved in v1.0.0 (previously "known hardening gaps") (tracked for v1.0 — see DESIGN.md §10.5)

1. ~~path-normalization rules~~ → `ISUTOOLS_PATH_RULES` (v0.7.0)
2. ~~snapshot diff view~~ → `GET /diff?a=&b=` (v1.0.0)
3. ~~counter/gauge API~~ → `isutools.Count`/`AddCount` (v0.7.0)
4. ~~WS/SSE separation~~ → connection stats + active gauge (v0.7.0)
5. ~~save caps~~ → initial serialization in v1.0.0; size/concurrency/read caps
   completed in the unreleased hardening tree
6. ~~ABBA template~~ → hardened locally; remote multi-block gate still pending
7. cross-collector shared generation gate — still open (1.x): benchmark
   automation must wait for `POST /reset` to return before load starts
   (the reference bench.sh does)
