# Implementation and verification status

Updated: 2026-08-07 (Asia/Tokyo) — current release: **v1.3.0**

`v1.0.0` tag = `faa7ca8`. `v1.1.0` tag = `f4e7c3c` (merge of PR #1),
`v1.2.0` tag = `833515e`, and `v1.3.0` tag = `26aa2bb` (merge of PR #14).
The existing v1.2 sections below remain historical evidence; their numbers are
not silently reused for the current working tree.

## Unreleased working-tree implementation (2026-08-06)

The `feat/v1.2.0-foundation` working tree after v1.3.0 contains these additional
changes. They are implemented and locally verified, but are not described as
released until a PR is merged and a later tag exists:

- issue #12 documentation: SSH loopback forwarding now states the network-
  namespace prerequisite, gives an SSH-lifetime-scoped WSL2 guest relay,
  keepalive/fail-fast options, four-layer diagnostics, and explicitly forbids
  exposing unauthenticated port 19191 on `0.0.0.0`
- issue #15 recovery: `/save` may recover only an exact `(RunID, Epoch)` whose
  bounded in-process ledger proves a `started-ttl` terminal event; it persists
  the supplied score/pass plus aborted/invalid/partial recovery provenance and
  leaves other terminal causes fail-closed at HTTP 409
- issue #16 timeline: bounded run/epoch-fenced HTTP, SQL, DB-pool, process, and
  host-resource buckets; deterministic phase detection; evidence-windowed
  `correlation-suspect` critical-path candidates; secret-safe route identities;
  persisted score/pass outcome; and evidence-backed diff contradictions
- pprof external analysis: opt-in run-aligned CPU ownership, cumulative
  profiles, immutable capture evidence, hard-isolated external analysis,
  binary provenance, root-scoped artifact I/O, explicit publication CAS, and
  derived HTML

Current-tree verification:

| check | environment | result |
|---|---|---|
| race + shuffle + coverage | Go 1.26.5, darwin/arm64 | PASS, aggregate **87.1%** |
| vet | Go 1.26.5, darwin/arm64 | PASS |
| lint | `golangci-lint run ./...` | PASS, **0 issues** |
| Actions lint | actionlint v1.7.12 | PASS |
| vulnerability scan | govulncheck v1.6.0 | PASS, **0 reachable vulnerabilities** (one required-module advisory is unreachable from this code) |
| new direct dependency licenses | google/pprof Apache-2.0; x/sys BSD-3-Clause | compatible; license files inspected in the resolved module versions |
| scripts | shellcheck, `bash -n`, ABBA contract | PASS |
| minimum Go | Go 1.24.x, Linux/arm64 Docker | all-package test + vet PASS |
| hard worker | privileged cgroup v2, Go 1.24.13 Linux/arm64 | birth membership, SIGSTOP gate, hard memory/swap/pids/RLIMIT/pidfd checks, synthetic profile, real `runtime/pprof` CPU profile, OOM kill, parent survival, and subsequent analysis all PASS |

The pprof external-analysis Darwin crash-fault test, private-isu ABBA release
gate, remote GitHub Actions result for the new cgroup job, and deployment
behavior are still unverified. Plan 10 multi-host aggregation remains
intentionally unimplemented.

## Implemented and released

- **M1 (v0.1.0)**: bounded SQL aggregation (sharded, log2-bucket p95), literal
  masking, snapshot HTML/JSON, build/host metadata, loopback admin server
- **v0.2.x**: dashboard + snapshot persistence (`POST /save`, `GET /files/`),
  MySQL/MariaDB schema capture (dbinspect via the first observed DSN),
  collector health / `partial`, generation-scoped SQL store, serialized reset,
  historical admin auth modes (v1.1.0 removes Bearer/query-token auth and
  standardizes on SSH-only reachability)
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
- **v1.1.0**: the post-release hardening listed below, plus new advisor
  checks — HTTP/3/QUIC readiness (nginx/Caddy/Envoy config, UDP/443,
  explicit edge/network evidence, protocol traffic, QUIC telemetry),
  cache strategy (proxy_cache advisory, proxy_cache_lock, Set-Cookie
  ignore hazard, app-cache hit-rate/eviction telemetry via
  `ISUTOOLS_CACHE_METRICS`), ECH readiness (`ssl_ech_file`, key-rotation
  window, `$ssl_ech_status` logging) — `docs/INTEGRATION.md`, and a
  Go 1.24 compat CI job. The DESIGN.md §7 on-target ABBA gate was
  **explicitly waived** for this tag (recorded in the tag annotation);
  the remote multi-block rerun remains pending
- **v1.2.0**: run lifecycle coordinator, DB target registry, four new
  measurement packages (`sqlrows` / `hoststats` / `netstats` / `dbpool`),
  EXPLAIN capture, six new advisor checks and six new dashboard sections
  — detailed below

## v1.2.0 contents

Verified against the source on this tree; the flag column is what the code
that parses the variable actually does, not what the plans proposed.

| area | what shipped | flag (default) | platform |
|---|---|---|---|
| `internal/runctl` | run lifecycle `Controller`: `StartRun` / `FinishRun` / `AbortRun` / `Ack` / `AckBy` / `Await` / `Status` / `SnapshotOf` / `Sweep`; 9 `RunState` values, 3 `Validity` values, monotonic `Epoch` fencing, nonce idempotency, preempt, `SerializeInitialize` | — (always on) | any |
| `internal/runctl` budgets | one authority for the time hierarchy: `StartRunBudget`/`FinishSyncBudget` 6s > `PhaseStartBaselineBudget`/`PhaseFinishFinalBudget` 5s > `PerCollectorBaselineBudget` 3.5s > `PerTargetBudget` 1s; generation side 500ms > 100ms; `DrainBudget` 10s, `SnapshotBuildBudget` 5s, `EnrichBudget` 2s, `FinishLease` 20s. `Budgets.Validate` rejects an inverted table at registration | — | any |
| `sqlstats/registry.go` | DB target registry: stable auto `TargetID` (`<alias>-<26-char base32 of sha256[:16] of the canonical driver+net+addr+database tuple>`), `Purpose` = `app`/`stats`/`explain`, `RegisterDBTarget` / `RegisterDBInspector` / `Inspect` / `Targets` / `Target` / `TargetIDForDSN` / `Features` / `Notes` / `CloseDBInspectors`, `MaxTargets` = 16 | — | any |
| `sqlrows` | per-digest rows examined vs rows sent over the run, from `performance_schema.events_statements_summary_by_digest` sampled at both boundaries; `DigestTextFetchLimit` = 200 (delta computed over every digest, truncated after subtraction) | `ISUTOOLS_SQLROWS` (on) | any (needs MySQL performance_schema) |
| `hoststats` | memory / disk / PSI / cgroup v2 limits / host identity, from procfs + sysfs + cgroup2, with the cgroup scope always reported alongside the numbers | `ISUTOOLS_HOSTSTATS` (on), `ISUTOOLS_ROLE`, `ISUTOOLS_CGROUP_SCOPE`, `ISUTOOLS_CGROUP_PATH` | Linux only — `New` returns `ErrUnsupportedOS` elsewhere and the collector is not registered |
| `netstats` | TCP socket summary (point observation at the closing boundary), per-NIC interval byte/packet/error/drop deltas + Mbit/s rates, link speed and MTU. Display-only: no value feeds an advisor threshold | `ISUTOOLS_NETSTATS` (on) | Linux only — registration is skipped with "/proc/net is only available on Linux" |
| `dbpool` | `database/sql` pool statistics per registered target via `WatchDBPool(targetID, *sql.DB)`; `MaxPools` = 16; only registry-known `TargetID`s are accepted (`ErrUnknownTarget`). Display-only, deliberately no threshold | `ISUTOOLS_DBPOOL` (on) | any |
| `queryplan` | EXPLAIN of the run's top digests using MySQL's own `QUERY_SAMPLE_TEXT`; runs once per run inside `runctl.EnrichBudget` (2s), never on a dashboard GET; `SessionBudget` 300ms / `SampleBudget` 100ms / `PerDigestBudget` 250ms | `ISUTOOLS_EXPLAIN` (**off** — opt-in), `ISUTOOLS_EXPLAIN_TOP` (10, capped at 200), `ISUTOOLS_EXPLAIN_DSN` / `ISUTOOLS_EXPLAIN_DRIVER` (`mysql`) | MySQL 8.0.17+ only; older MySQL and MariaDB report `CodeUnsupported` |
| `advisor` | `nginx-upstream-uds`, `nginx-listen-backlog`, `go-pgo` — opportunities rather than defects, so they emit `StatusInfo`/`StatusSkip` and never warn — plus `plan-full-scan` / `plan-filesort` / `plan-temporary`, which do warn, fed from the query-plan section | existing conf/env inputs | any |
| `web` | five new measurement sections — `SQL 行効率`, `Query Plans`, `DB Pool`, `Host`, `Network` — plus a `Profiles` section for the runtime profile pairs | — | any |
| `isutools` | `ResetNow` / `ResetNowWithNonce` / `ResetNowOpts` / `SerializeInitialize`; runtime profile pairs (mutex / block / heap) captured at both boundaries with `go tool pprof -diff_base` commands and a measured residual | `ISUTOOLS_MUTEX_FRACTION`, `ISUTOOLS_BLOCK_RATE_NS`, `ISUTOOLS_HEAP_PROFILE` (all **off**) | any |

Two properties of the registry are load-bearing enough to state separately,
because the row-efficiency numbers are only trustworthy if they hold:

- a `PurposeStats` / `PurposeExplain` connection is rebuilt **without a default
  database**, so MySQL attributes the collector's own statements to a NULL
  schema, and the target schema is a bound parameter (`WHERE SCHEMA_NAME = ?`)
  rather than `DATABASE()`
- that is verified rather than assumed. A URL-form DSN cannot be rebuilt, so
  the connection keeps the application's schema; `sqlrows` probes
  `performance_schema.threads` on its own connection and **skips** such a
  target with `inspector-default-db` instead of publishing contaminated numbers

**`PurposeExplain` never falls back.** A target without an explain credential
is skipped and the reason recorded; the application credential is not used as a
substitute. The privileges are verified on the very connection EXPLAIN will run
on — `SET ROLE NONE`, `CURRENT_ROLE()` read back, every granted role expanded
with `SHOW GRANTS ... USING` and judged against a closed allowlist.

## Test evidence

v1.2.0 verification (2026-08-05, Go 1.26.5, darwin/arm64, on the
`feat/v1.2.0-foundation` tree):

| check | command | result |
|---|---|---|
| vet | `go vet ./...` | PASS (exit 0, no output) |
| race + shuffle | `go test -race -shuffle=on ./...` | PASS, **20 packages**, all `ok` |
| coverage | `go test -race -coverprofile=/tmp/c.out ./...` then `go tool cover -func` | aggregate **93.1%** |

Per-package coverage is not uniform and is not claimed to be: the highest are
`dbpool`, `netstats` and `internal/generation` at 100.0%, the lowest is
`internal/sysinfo` at 81.8%. CI enforces the documented **aggregate** 80% gate.
Aggregate coverage rose from 85.0% (v1.1.0, 14 packages) to 93.1% (v1.2.0,
20 packages).

CI (`.github/workflows/ci.yml`) additionally runs, and these are enforced
rather than reported:

- `go test ./...` on Go 1.24.x (the module's declared minimum)
- the `examples/abba.sh` script contract test (`examples/abba_test.sh`)
- a **MySQL 8.4 service job** running `go test -tags=integration ./sqlrows -run
  '^TestNoSelfContamination'` — the self-contamination property above is
  therefore checked against a real server on every push, not only reasoned about
- `internal/agg` benchmarks, informational only

Not rerun on this tree: `golangci-lint`, `govulncheck`, the access-log parser
fuzz run and `BenchmarkObserve`. The v1.1.0 figures for those (0 issues, no
known vulnerabilities, ~761k fuzz executions, 153.2 ns/op / 0 allocs/op on
Apple M3) were recorded on the pre-merge hardening tree and are **not** claimed
for v1.2.0.

A real TLS HTTP/2 listener, an HTTP/3 listener, a deployed target and a
physical/remote benchmark are not implied by any of the local results above.

## Review evidence

The v1.2.0 implementation went through four adversarial review rounds against
the code (as opposed to the five review rounds the plan set went through
against the documents):

| round | findings | outcome |
|---|---|---|
| 1 | 8 | all addressed |
| 2 | 7 | all addressed |
| 3 | 6 | all addressed |
| 4 | **0** | clean |

Separately, a security round on plan 09 (EXPLAIN) **blocked the feature**: the
privilege check failed open. An empty `SHOW GRANTS` result — a proxy stripping
the result set, a driver returning nothing for a form it did not recognise —
yielded no roles to expand and passed the allowlist vacuously, which would have
let EXPLAIN run on a credential whose privileges were never established. It is
fixed by construction: `parseGrants` rejects empty output outright, on the
reasoning that every MySQL account has at least `GRANT USAGE ON *.*`, so zero
lines means the read did not happen. The security matrix in
`queryplan/security_test.go` pins that case together with role-granted DML,
non-neutralisable roles, nested roles and non-closing role graphs.

## Field verification (private-isu, v1.2.0)

Measured on private-isu with the v1.2.0 tree:

- the benchmark **passed** with the full collector set enabled
- `hoststats`, `netstats` and `sqlrows` produced real data (not empty sections,
  not `partial` placeholders)
- the collector's own queries were **confirmed absent** from the measured
  schema's digest table — the design property above, observed on a live server
  rather than inferred

## v1.2.0 ABBA overhead observation (not a passed §7 gate)

Recorded on private-isu, 2 blocks / 8 runs:

```text
off avg: 556196 / on avg: 546150
```

Read honestly, this is a **1.81% score cost for running isutools**, not a
gain: `examples/abba.sh` defines score overhead as `(off − on) / off`, and
`(556196 − 546150) / 556196 = +1.81%`. The last comparable observation is
v1.0.0's **−0.58%**, where the "on" runs scored *higher* than the "off" runs —
the opposite sign. (v1.1.0 has no ABBA number of its own; its §7 gate was
waived.) So the honest reading is that **v1.2.0 costs measurably more than
v1.0.0 did**, which is unsurprising given four new collectors, and that it sits
just under the DESIGN.md §7 2% ceiling with almost no margin.

This observation cannot satisfy the §7 gate, for a reason that is mechanical
rather than a matter of judgement: **2 blocks is below `examples/abba.sh`'s own
minimum of 3**, which the script enforces by exiting 2 (`ABBA_BLOCKS must be
>= 3 for a confidence interval`). With two blocks no paired 95% confidence
interval can be formed, so "+1.81% < 2%" is a point estimate with no interval
attached. §7 requires the interval. The gate therefore remains **pending**, as
it has since v1.1.0, and the honest summary of the number is: closer to the
ceiling than any previous release, and not yet bounded.

## Not verified at v1.2.0

Stated explicitly so it is not read out of the sections above:

- **ISUCON14 verification was still in progress** when v1.2.0 was cut. No
  ISUCON14 result is claimed.
- **EXPLAIN has no live-MySQL integration test.** `queryplan` is covered at
  94.7% entirely by fakes: a scripted `sqlstats.Querier` and a `database/sql`
  driver returning canned rows. The CI MySQL job covers `sqlrows`
  self-contamination only. So the statement *sequence* EXPLAIN issues is pinned,
  but its behaviour against a real server — actual `SHOW GRANTS` dialects, real
  `QUERY_SAMPLE_TEXT` truncation, real EXPLAIN output shapes — is not.
- The §7 ABBA gate, as above.

## Known limitations (v1.2.0)

1. **The first run after an application restart cannot pair `sqlrows`.** A DB
   target enters the registry when the proxy driver opens its first connection
   (`observeDSN` in `dsnCapturingDriver.Open`), and `database/sql` opens
   lazily. If the opening boundary is taken before any query has run, the
   target exists only at the closing boundary and the row is published with
   `unpaired-boundary` / "the target was only present at the closing boundary",
   degrading the run to `partial`. Calling `RegisterDBTarget` at startup avoids
   it.
2. **`dbinspect` and `advisor` inspection queries still land in the measured
   schema.** Both open a raw connection from `sqlstats.FirstConn()` — the
   application's own DSN, default database included — so their statements are
   attributed to the application schema in `performance_schema`. They are run
   *before* `StartRun` (`captureDB()` in the reset path), so they stay outside
   the measured interval; they are not outside the measured schema. Only the
   registry's `PurposeStats` / `PurposeExplain` connections get the
   no-default-database treatment.
3. **A runtime profile pair approximates the run, and includes a post-finish
   tail.** mutex/block/heap profiles are process-cumulative, so a run is the
   difference of two captures. The opening capture happens slightly after the
   boundary is fixed (`HeadLossNs`, excluded from the difference) and the
   closing capture slightly after the freeze returns (`TailExcessNs`, included
   in it). `ApproxErrorNs = HeadLossNs + TailExcessNs` is displayed on every
   pair together with an unconditional notice — unconditional on purpose, since
   a reader who sees the notice only on bad runs would read its absence as
   "this one is exact", which no pair ever is.
4. **nginx configuration inspection is static, not `nginx -T`.** When
   `ISUTOOLS_PROXY_CONF` names an entrypoint file, its include graph is expanded
   with cycle, file-count and byte bounds; symlink targets are de-duplicated.
   Directory mode remains best-effort discovery of every `*.conf`, so an
   inactive fragment can still be included. The advisor explicitly says that
   the result is not proof of the running nginx master's effective settings.

## Not implemented

- **Multi-host / peer protocol (plan 10): single host only.** There is no
  `PeerHandler`, no `cmd/isutools-agent`, no `ISUTOOLS_PEER`, and no wire
  protocol in the tree. `runctl` reserves three constants for it
  (`AckedByHub`, `AckedByLease`, `ReasonHubAbort`) and nothing more. Every number isutools
  reports describes the host the library is linked into. `plans/10-multi-host.md`
  is the remaining design document.

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

The hardened `examples/abba.sh` requires at least three blocks, fixed
warm-up, a stable binary/image fingerprint, score/p95/error rate, TSV
provenance, and paired 95% CI gates. It has passed its local script contract
(also enforced in CI since v1.1.0); it has **not yet been rerun on
private-isu** at three or more blocks. v1.1.0 was tagged with this §7 gate
explicitly waived, and the v1.2.0 observation above is two blocks.

## Post-release hardening (released in v1.1.0)

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
   completed in v1.1.0
6. ~~ABBA template~~ → hardened + CI script-contract test (v1.1.0); the remote
   multi-block gate is still pending — the v1.2.0 run is two blocks, one short
7. ~~cross-collector shared generation gate~~ → v1.2.0: every collector is
   registered on one `runctl.Controller` and fenced by the same
   `(runID, epoch)`, and the measured boundary spread is recorded in the
   snapshot against `SpreadLimitGeneration` (50ms) / `SpreadLimitBoundary`
   (1.5s). The operational requirement survives: `POST /reset` fixes the
   boundary before it answers 204 with `X-Isutools-Run-Id`, so benchmark
   automation must still wait for that response before load starts (the
   reference bench.sh does)
