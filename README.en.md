# isutools

[![Go Reference](https://pkg.go.dev/badge/github.com/ekusiadadus/isutools.svg)](https://pkg.go.dev/github.com/ekusiadadus/isutools)
[![CI](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml/badge.svg)](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)

**English** | [日本語](./README.md)

All-in-one profiling module for ISUCON-style tuning. With a **one-line change**
to your app it measures SQL / HTTP / nginx access logs / processes & CPU /
DB schema / pprof, and lets you review every run through a **pre-sorted
dashboard** and **self-contained snapshots**. Includes a reproducible ABBA
overhead gate.

Per-benchmark run history is listed with score and git revision; clicking a
row opens every measurement captured during that run:

![isutools dashboard: per-benchmark run history with scores and git revisions](docs/images/dashboard-runs.png)

- Integration details: **[DB, nginx/Apache, pprof, prerequisites](./docs/INTEGRATION.md)** (in Japanese)
- Design doc: [DESIGN.md](./DESIGN.md) / Implementation status: [docs/IMPLEMENTATION_STATUS.md](./docs/IMPLEMENTATION_STATUS.md) (in Japanese)
- License: MIT / Runtime: Go 1.24+
- Track record (dogfooding): tuned private-isu for one day using only this
  module's measurements — **score 0 → 541,650** (0 fails).
  [Full write-up on the blog (Japanese)](https://ekusiadadus.com/ja/blog/private-isu-500k-with-isutools)

## Quick Start

```go
import "github.com/ekusiadadus/isutools"

db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // just rewrite your existing sqlx.Open

// To measure HTTP as well, wrap your existing handler once
http.ListenAndServe(":8080", isutools.HTTP(handler))
```

Register the underlying driver with `database/sql` (e.g. via a blank import)
before this call. MySQL / MariaDB / PostgreSQL (via database/sql) all use the
same one line. On successful registration the admin server starts once on
`127.0.0.1:19191`. If registration fails, `SQLDriverName` **fails open** to
the raw driver name, and any missing data is always recorded in
`meta.partial` / `meta.health` (it never breaks your app's startup).
For per-driver imports, DSNs, and pgxpool constraints, see
[Integration Guide §2](./docs/INTEGRATION.md#2-db-ドライバへの接続).

## Endpoints (admin server)

| Route | Description |
|---|---|
| `GET /` | **Run list** (JST timestamp, gen, rev, score). Click a row for details |
| `GET /<run-id>` | Details of a saved run (high-precision IDs with collision protection, alongside legacy second-precision IDs) |
| `GET /live` | Live report of the current measurement (pre-sorted by total time, descending) |
| `GET /snapshot.html` | Download a self-contained HTML snapshot (double-click to view locally) |
| `GET /json` | Machine-readable snapshot (includes `prev` = previous generation) |
| `GET /pprof/` | net/http/pprof (profiles of the app process) |
| `GET /diff?a=<id>&b=<id>` | **Diff between two runs** (total/count/avg; rows with differing counts are not declared improvements) |
| `POST /reset` | Reset the generation (call before a bench run). Also starts automatic CPU profiling |
| `POST /collect` | Wait for and collect buffered nginx logs with a deadline |
| `POST /save?score=N` | Persist the current generation as html+json staging with caps (the HTML appears in the list only after the JSON is published) |
| `GET /files/<name>` | Fetch saved html / json / pprof files |

## What's in the report

- **meta**: time (**always JST**), git rev (+dirty), generation number, score,
  **host info (CPU model / core count / memory GB / OS)**
- **Collector Health**: per-collector status and missing-data (`partial`) warnings
- **DB Schema**: tables, row counts, and **index list** as of generation start
  (evidence of "what indexes existed before the run")
- **SQL**: per normalized query — total/count/errors/avg/p95/max (string and
  numeric literals masked as `?`)
- **HTTP**: per-request latency and byte counts as seen by the app
- **Proxy Access Log**: nginx/Caddy/explicit JSON (separate reqtime/upstime,
  bytes, cache, 304, etc.)
- **Processes**: per-process CPU/RSS during the bench window (top-compatible,
  1 core = 100%) plus **CPU total: N% busy (user/sys/iowait/idle)** — see at a
  glance whether you are actually saturating the hardware
- **User Flow**: top 20 page transitions per session (from the `sess:` field
  in the proxy log — a measured view of how users actually navigate the app;
  also useful for validating k6 scenarios)
- **Scenario Stories**: aggregates the request sequence of each safe
  `scenario` label + pseudo `sess` into top user stories per scenario
  (a minimal foundation for GA4-style flows)
- **Counters**: in-app counters via `isutools.Count("cache_hit")` (reset per
  generation)
- **Advisor**: flags unconfigured ISUCON staples, plus HTTP/3/QUIC migration
  readiness (server/TLS/Alt-Svc/fallback/UDP/edge/measured protocol/
  retransmission & drop evidence)
- **Snapshots / CPU Profiles**: list of past runs and profiles (selectable
  from the dashboard; each row links to a diff against the previous run)

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `ISUTOOLS` | (on) | `off` disables everything (returns the raw driver name; zero extra work on the query path) |
| `ISUTOOLS_ADDR` | `127.0.0.1:19191` | Admin server bind. `off` disables only the admin server (SQL aggregation continues) |
| `ISUTOOLS_ALLOW_UNAUTHENTICATED` | — | `1` explicitly allows a non-loopback bind. **Only for Docker setups with an SSH tunnel + publish restricted to `127.0.0.1`** (shows a security warning, kept separate from measurement `partial`) |
| `ISUTOOLS_DATA_DIR` | — | Where snapshots / profiles are persisted (the backing store of the run list) |
| `ISUTOOLS_ACCESS_LOG` | — | Path to the nginx/Caddy/Apache/Envoy log. **LTSV / JSON lines auto-detected** (see the integration guide for supported formats) |
| `ISUTOOLS_NGINX_LOG` | — | Legacy name; used as a fallback only when `ACCESS_LOG` is unset |
| `ISUTOOLS_PPROF_SECONDS` | 0 | Automatically capture an N-second CPU profile after reset (covers the entire bench window) |
| `ISUTOOLS_GIT_HASH` / `_DIRTY` | — | Inject rev info when a Docker build lacks embedded VCS information |
| `ISUTOOLS_PATH_RULES` | — | HTTP path normalization rules (`regex=replacement;...`, each pair split at the last `=`) |
| `ISUTOOLS_NGINX_CONF` | — | nginx conf inspected by the advisor (file or directory) |
| `ISUTOOLS_PROXY_CONF` / `_KIND` | — / auto | nginx/Caddy/Envoy config read by the HTTP/3 advisor. Prefer the generic name; kind is `nginx` / `caddy` / `envoy` |
| `ISUTOOLS_HTTP3_UDP443` | — | State the result measured from an external client as `reachable` / `blocked`; firewall/NAT is never guessed from inside the process |
| `ISUTOOLS_HTTP3_EDGE` / `_EDGE_ENABLED` | — | Explicit evidence of the LB/CDN name and whether HTTP/3 is enabled at that edge (`true` / `false`) |
| `ISUTOOLS_HTTP3_QUIC_METRICS` | — | Proxy QUIC counter JSON reloaded at snapshot time. Diagnoses retransmission rate and UDP drops |
| `ISUTOOLS_CACHE_METRICS` | — | App-side cache counter JSON (`hits` / `misses` / `evictions`) reloaded at snapshot time. Diagnoses hit rate and pre-expiry evictions |

## Additional libraries and prerequisites

pprof is part of the Go standard library, so the instrumented app needs no
extra package. Automatic CPU profiling needs a writable `ISUTOOLS_DATA_DIR`,
and `go tool pprof` only at analysis time. procstats also adds no package but
requires Linux `/proc` and PID-namespace permissions. k6, curl, jq, and
Graphviz are external commands for specific workflows, not runtime libraries
of isutools. For the per-feature required/optional matrix, see
[Integration Guide §1](./docs/INTEGRATION.md#1-必須任意の全体像).

## nginx configuration (access-log collection)

The LTSV format in
[examples/nginx-isutools.conf](./examples/nginx-isutools.conf) is recommended.
Do URI grouping with an nginx `map` and keep both the aggregation key as
`uri:$uri_group` and the raw path as `rawuri:$uri` (the parser ignores unknown
keys). JSON format (`log_format ... escape=json '{...}'`) works as-is with the
same key names, or with **alp's default keys (`body_bytes` /
`response_time`)**. JSON is not required: a line starting with `{` is parsed
as JSON, anything else as LTSV. The standard combined log is never guessed.
Apache needs explicit JSON and `%D` microsecond conversion, so example
configs, permissions, Docker volumes, and how to build a safe `sess` are
collected in
[Integration Guide §4–5](./docs/INTEGRATION.md#4-nginx-アクセスログ).

## HTTP/3 / QUIC migration advisor

The advisor inspects nginx/Caddy/Envoy configs, local Linux UDP/443
listeners, and per-`proto` counts, 5xx, and p95 in the proxy access log.
When a reverse proxy terminates TLS, the app's `r.Proto` does not represent
the client protocol, so the client-facing protocol from `ISUTOOLS_ACCESS_LOG`
takes precedence. With an external LB/CDN terminating, however, even the
origin log is not client-facing, so edge analytics or measurements from an
external client are required.
LB/CDN, external firewall/NAT, and QUIC retransmission counters are never
auto-guessed; without explicit evidence the check reports `skip`. For example
configs, judgment limits, Caddy native JSON, and Envoy telemetry, see
[Integration Guide §6](./docs/INTEGRATION.md#6-http3--quic-readiness-advisor).

This feature diagnoses migration readiness; isutools itself does not become
an HTTP/3 server. It adds no runtime dependency such as `quic-go`, and
connection testing with a real listener and external clients remains a
separate task.

## Benchmark script template

```bash
ADMIN=http://localhost:19191
curl -X POST $ADMIN/reset                  # start a generation (auto CPU profiling begins)
<run your benchmark>
curl -X POST "$ADMIN/save?score=$score"    # persist → adds one row to the dashboard
curl $ADMIN/json | jq '.sql[:5]'           # check the top 5 on the spot
```

## Screenshots

**Advisor** — automatically detects "ISUCON staples that are not configured"
(prepared statements, gzip, buffer pool, kernel parameters, GOMAXPROCS).
A checklist that flips to ok as you fix each item:

![isutools advisor: detects unconfigured ISUCON-critical settings](docs/images/report-advisor.png)

**Diff view** — shows per-query / per-path total-time deltas between two
runs. See at a glance whether you actually improved or the bottleneck merely
moved:

![isutools diff view: per-query total-time deltas between two runs](docs/images/diff-view.png)

## Security model (summary)

Viewing is designed around an **SSH tunnel**; there is no application-level
token mechanism. Loopback binds work out of the box. Non-loopback binds are
allowed only with an explicit `ALLOW_UNAUTHENTICATED=1` and reachability
restricted via Docker publish, firewall, and SSH. Without the opt-in,
non-loopback fails closed. The full contract and rationale are in DESIGN.md
chapter 4. When mounting `isutools.Handler()` on your own router, access
control is the caller's responsibility.

## Scenario load testing with k6

Load generation uses [k6](https://k6.io) as-is (nothing is reimplemented).
[examples/k6-private-isu.js](./examples/k6-private-isu.js) contains an
example scenario: login → timeline → post detail → author page.
`POST /reset` → run k6 → `POST /save` lines up the scenario's SQL / HTTP /
User Flow, as seen from the server, on the dashboard.
The bundled example sends `X-Isutools-Session: k6-vu-N-iter-M` instead of a
raw cookie, and the nginx example records only that non-secret ID as `sess:`.
Additionally, recording `X-Isutools-Scenario: login_and_browse` as
`scenario:` makes the number of pseudo-sessions following the same request
sequence and the top journeys appear in Scenario Stories. Never put raw
cookies, bearer tokens, emails, etc. into `sess` / `scenario`. These are
spoofable measurement labels and must not be used for authentication or
authorization. For real-user measurement, strip the external headers and
overwrite the pseudo-ID at a trusted app/proxy. Details in
[Integration Guide §4](./docs/INTEGRATION.md#scenario-stories最小のファネルフロー基盤).

## Overhead validation (ABBA)

[examples/abba.sh](./examples/abba.sh) runs blocks of four consecutive
benches (off → on → on → off) at least three times. It records the identical
binary/image fingerprint, a fixed warm-up, score, p95, error rate, and a
paired 95% CI into TSV plus provenance, and gates on the CI upper bound. The
required BENCH output format is documented at the top of the script and in
[Integration Guide §7](./docs/INTEGRATION.md#7-pprofprocstats負荷生成ツール).

## v1.0 status

Implemented: SQL (MySQL/MariaDB/PostgreSQL) / HTTP (h1/h2) / nginx logs
(LTSV+JSON, rotation-following) / procstats + **CPU total** / dbinspect
(MySQL) / pprof (endpoint + automatic capture) / snapshot-first dashboard
(run list, timestamp-ID details, live) / fixed JST / score recording /
collector health / generation management.

v1.0's path normalization, diffs, counters, WebSocket/SSE separation, User
Flow, and resource caps are implemented. A guessing parser for Apache
combined logs, a gqlgen operation adapter, and HTTP/3 real-listener
integration tests are explicit non-goals for 1.x. For the current local
verification scope and the exact state including evidence added after the
release tag, see the
[implementation status](./docs/IMPLEMENTATION_STATUS.md).

---

"ISUCON" is a trademark or registered trademark of SAKURA internet Inc.
This project is an unofficial tool unaffiliated with the operators of
[isucon.net](https://isucon.net/).
