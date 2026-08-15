# isutools

[![Go Reference](https://pkg.go.dev/badge/github.com/ekusiadadus/isutools.svg)](https://pkg.go.dev/github.com/ekusiadadus/isutools)
[![CI](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml/badge.svg)](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)

**English** | [日本語](./README.md)

**Keep SQL, HTTP, proxy logs, pprof, host resources, and the score from one ISUCON benchmark in one evidence set.**

isutools is a Go measurement toolkit for answering three questions: what to fix next, whether the change
actually helped, and whether the run is complete enough to compare. It provides a live dashboard and a
self-contained HTML report.

![isutools report saved from ISUCON13](docs/images/isutools-isucon13-wsl-score12079.png)

## Install in three minutes

Go 1.24 or newer is required.

```bash
go get github.com/ekusiadadus/isutools@latest
```

Wrap the database driver name and HTTP handler.

```go
db, err := sql.Open(isutools.SQLDriverName("mysql"), dsn)
if err != nil {
	log.Fatal(err)
}

log.Fatal(http.ListenAndServe(":8080", isutools.HTTP(mux)))
```

Save every benchmark with the same boundary.

```bash
curl -fsS -X POST http://127.0.0.1:19191/reset
# benchmark command
curl -fsS -X POST 'http://127.0.0.1:19191/save?score=12345'
```

The admin server listens on `127.0.0.1:19191` by default. Keep it private and use SSH forwarding.
See the [integration guide](./docs/INTEGRATION.md) for DB-pool, EXPLAIN, nginx, and pprof setup.

## What the report answers

| Feature | Simple answer |
|---|---|
| Bottleneck Overview | Which of SQL, HTTP, DB pool, CPU, or I/O deserves the next check |
| SQL / HTTP | Cumulative cost and p95, not just one slow request |
| Runs / Diff | Score, failure, total, count, and average changes between runs |
| User Flow | The top 20 page transitions actually taken by one pseudonymous session |
| Scenario Stories | Observed request sequences and counts for each explicit scenario |
| Profiles / Host | Whether time is spent executing CPU work or waiting on DB, I/O, or connections |
| Offline / specialist tools | Safely bind old logs, slow-query outliers, pprof/trace, and PGO to the same run |
| Collector Health | Whether missing, truncated, or invalid evidence makes a run unsafe to compare |

JSON, the live dashboard, and self-contained HTML all come from the same run. The report presents
candidates and evidence instead of declaring a cause. Score and correctness remain the acceptance gates.
Published pprof analysis is available from a saved run's `current UI`; immutable historical HTML is never rewritten.

## User Flow and Scenario Stories

`isutools.HTTP` middleware turns a Cookie into an HMAC pseudonymous session and aggregates it with registered
route templates inside the application. Raw cookies and session tokens never need to enter the proxy log, so
User Flow and Scenario Stories work with proxies other than nginx.

```bash
export ISUTOOLS_FLOW_LABELS=on
export ISUTOOLS_SESSION_COOKIE=SESSIONID
export ISUTOOLS_SESSION_HMAC_KEY='random-32-byte-or-longer-secret-not-in-git'
export ISUTOOLS_SCENARIO=isucon13_official
export ISUTOOLS_FLOW_SOURCE=middleware
```

Adapters cover every framework used by a published ISUCON Go reference app: Gorilla mux, Martini, Goji v2,
Echo v3/v4/v5, httprouter, and chi v5, plus Gin.

```go
echov4.Install(e)
e.GET("/checkout", checkout, echov4.Scenario("checkout"))

ginadapter.Install(r)
r.GET("/checkout", ginadapter.Scenario("checkout"), checkout)

chiv5.Install(r)
r.With(chiv5.Scenario("checkout")).Get("/checkout", checkout)
```

`ISUTOOLS_FLOW_LABELS=off` disables only flow labels; `ISUTOOLS=off` disables all measurement.
Public `X-Isutools-Session` and `X-Isutools-Scenario` headers are never trusted directly.
`ISUTOOLS_FLOW_SOURCE=proxy` selects the legacy trusted-response-header path; `off` disables flow collection.
See the [all-round compatibility matrix](./docs/isucon-compatibility.md) and
[proxy examples](./examples/proxies/README.md).

## Measured stories

### private-isu: score 0 to 541,650

One day of dogfooding in one controlled environment improved private-isu from an initial score of 0 to
`541,650` with zero failures. This is a result for that environment, workload, and change history—not a
general performance guarantee.

1. Tie each revision and score to `reset → benchmark → save`.
2. Prioritize repeated post and user reads using cumulative SQL and HTTP time.
3. Use the diff to check both removed cost and new regressions, then keep only correctness-passing changes.

The run list retains failed experiments and rollbacks instead of hiding them.

![Measured private-isu run history](docs/images/dashboard-runs.png)

The pictured comparison moved from score `140,914` to `541,650`. The diff shows two dominant
`posts JOIN users` queries dropping from cumulative `352.2s` and `109.6s` to zero, while newly added query
cost remains visible in red. This keeps “what disappeared” and “what grew” next to the score.

![Measured private-isu SQL diff](docs/images/diff-view.png)

[Full tuning record (Japanese)](https://ekusiadadus.com/ja/blog/private-isu-500k-with-isutools)

### ISUCON13: empty flows to measured journeys

We added pseudonymous session and scenario labels to the Go reference application in `matsuu/wsl-isucon`.
ON/OFF smoke tests, header-spoof protection, and the official benchmark all ran on WSL2. The pictured run was
`pass=true`, score `11,928`, with 11,701 proxy-log lines and the top 20 User Flows and Scenario Stories.
After the HTTP compatibility review fix, the current binary was revalidated at `pass=true`, score `11,983`.

Scenario Stories show, for example, 49 sessions taking
`POST /api/icon → GET /api/tag → POST /api/livestream/reservation`.

![Measured ISUCON13 Scenario Stories](docs/images/isutools-isucon13-scenario-stories.png)

User Flow exposes high-frequency loops that endpoint totals alone cannot show, including 647 transitions
from reaction reads to reaction posts.

![Measured ISUCON13 User Flow](docs/images/isutools-isucon13-user-flow.png)

This proves the measurement path; it is not a performance-improvement claim. The ten-block ABBA run did not
establish the strict two-percent overhead gate, so the failure is retained in the
[field verification record](./docs/isucon13-wsl-flow-verification-20260814.md).

## Coverage

| Area | Support |
|---|---|
| Database / KV | MySQL / MariaDB / PostgreSQL / SQLite through `database/sql`; Redis command collector |
| HTTP | Go `net/http`, Gorilla mux, Martini, Goji v2, Echo v3/v4/v5, httprouter, Gin, and chi v5 |
| Proxy logs | nginx/OpenResty, Apache/OpenLiteSpeed, H2O, Envoy, Caddy, HAProxy, Traefik, lighttpd, Varnish, ATS, IIS, and Squid |
| Runtime | CPU, mutex, block, heap, allocs, goroutine, threadcreate, supported goroutineleak, and trace |
| Host | Linux procfs / sysfs / cgroup v2, network, and DB pool |
| Output | Live dashboard, JSON, self-contained HTML, run diff, and multi-host hub |

## Main settings

| Environment variable | Purpose |
|---|---|
| `ISUTOOLS=off` | Disable all measurement |
| `ISUTOOLS_ADDR` | Admin server; default `127.0.0.1:19191` |
| `ISUTOOLS_DATA_DIR` | Durable snapshot and profile directory |
| `ISUTOOLS_ACCESS_LOG` | Proxy access log |
| `ISUTOOLS_ACCESS_LOG_FORMAT` | Explicit decoder (`isutools-ltsv`, `isutools-json-v1`, `caddy-json`, `traefik-json`, or `iis-w3c`) |
| `ISUTOOLS_FLOW_LABELS` | Set User Flow / Scenario Stories to `on`, `off`, or `auto` |
| `ISUTOOLS_FLOW_SOURCE` | Flow source; default `auto`, or `middleware`, `proxy`, `off` |
| `ISUTOOLS_PPROF_SECONDS` | Benchmark-scoped CPU-profile duration |
| `ISUTOOLS_TRACE_SECONDS` | Short 1–30 second execution trace; default off and exclusive with managed profiles |
| `ISUTOOLS_TIMELINE` | Opt in to a bounded run timeline |

Detailed settings, APIs, endpoints, EXPLAIN grants, and multi-host procedures live in the focused documents:

- [Integration guide](./docs/INTEGRATION.md)
- [ALP / pt-query-digest / pprof / PGO playbook](./docs/SPECIALIST_TOOLS.en.md)
- [External-analysis threat model and limits](./docs/SECURITY_EXTERNAL_ANALYSIS.md)
- [Design and security boundaries](./DESIGN.md)
- [Implementation status](./docs/IMPLEMENTATION_STATUS.md)
- [Field feedback and operational notes](./docs/FIELD_FEEDBACK.md)
- [private-isu example](./examples/private-isu/README.md)
- [ISUCON13 WSL2 example](./examples/isucon13-wsl/README.md)
- [ISUCON14 case study](./docs/case-studies/isucon14-20260805.md)

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
```

Adapters are separate Go modules. CI validates the root and every framework adapter independently.

## License

[MIT](./LICENSE)
