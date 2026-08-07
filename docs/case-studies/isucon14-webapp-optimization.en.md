# How we optimized an ISUCON14 Go web application with benchmark-scoped evidence

This is a technical companion to the isutools repository. It describes how we
used one-change/one-benchmark artifacts while optimizing the reference
ISUCON14 application from a comparable 95,656-point baseline to an accepted
569,078-point run.

The score is not a general performance claim. It belongs to one WSL2 reference
environment with nginx TLS termination, a Go application, MySQL, and the
ISUCON14 benchmarker. Runs with a higher raw score were rejected when they
introduced benchmark correctness errors.

## The application and the feedback loop

ISURIDE is a ride-dispatch web application. Users request rides, autonomous
chairs report their locations, a matcher assigns chairs to rides, and both
clients poll for state changes. The benchmarker's simulated world advances in
30 ms ticks, so a small delay in a frequently called endpoint can slow the
whole ride lifecycle.

We wrapped every benchmark in the same measurement boundary:

```text
reset -> run benchmark -> finish/save -> attach score and correctness result
```

The saved artifact contains SQL and HTTP demand, nginx access logs, DB-pool
waits, process and host resources, schema and query-plan evidence, collector
health, and a source revision. Each application change has a corresponding
commit, benchmark log, and artifact. A change is accepted only after checking
both score and correctness.

## 1. Remove the serial dispatch ceiling

The initial matcher assigned at most one ride every 500 ms. Improving the HTTP
handlers could therefore create more waiting rides without increasing the
assignment rate.

We changed it to a rolling Hungarian matcher. On each iteration it takes a
window of unassigned rides and currently free chairs, computes a cost from
pickup distance and chair speed, and commits a batch of assignments. ISURIDE
contains two distant regions, so the cost model avoids cross-region dispatches
and uses an age-based fallback to prevent starvation.

This is not a lifelong scheduler. It does not revoke committed assignments,
predict where busy chairs will finish, or optimize across future windows. It is
a repeated optimization of the currently visible bipartite graph.

We also added an assignment-event export and trajectory viewer. The comparable
early replay contained 1,810 assignments and 491 dispatches with Manhattan
distance at least 400. The accepted 567,276-point run contained 10,760
assignments, no cross-region dispatches, and an average dispatch distance of
13.25. The visualization was useful because a score alone did not show whether
the matcher was sending chairs across regions.

## 2. Stop rebuilding hot state from MySQL

Location updates, ride statuses, availability checks, and notifications are
all on the ride critical path. We moved the repeatedly read state behind
explicit in-process caches:

- latest chair location and accumulated distance;
- latest ride status;
- active chairs available to the matcher;
- stable chair model speeds;
- notification responses guarded by a ride generation.

Cache invalidation was the main correctness work. A notification response is
not reused while an `app_sent_at` or `chair_sent_at` queue entry is pending.
Status insertion, assignment, and ride updates advance a generation, and a
response built across a generation change is discarded.

For unchanged notifications we kept the JSON API and added event-driven long
polling rather than switching the benchmark contract to SSE. App-notification
requests fell from roughly 1.55 million to 145 thousand in the comparison run;
chair-notification requests fell from roughly 379 thousand to 56 thousand.
The remaining long HTTP duration is mostly intentional waiting, not CPU time.

## 3. Shorten coordinate and payment transactions

A chair does not move again until its coordinate POST returns, so coordinate
latency directly limits simulated movement. We kept the latest coordinate and
distance in memory, removed redundant SELECTs after INSERT, and reduced the
normal transaction boundary.

At 325,004 points, the coordinate endpoint handled 107,643 calls at a 73.6 ms
average. In the later 567,276-point run, it handled 191,528 calls at a 2.2 ms
average. The request count increased by 77.9% while average latency fell by
97.0%.

Ride evaluation originally performed an external payment request while a DB
transaction and row lock were held. We send the payment first with the ride ID
as an idempotency key, then open a short transaction only for the locked ride
check, evaluation update, and completion state.

This optimization exposed a server/client visibility race: the server could
make a completed chair available before the benchmark client had observed the
evaluation response. An immediate-release version scored 637,737 but produced
13 nearby-chair consistency errors. We split the release boundary so matching
can reuse a chair before the nearby endpoint republishes it. The accepted
567,276-point run had no such error.

## 4. Treat pool waits as evidence, not an instruction

The DB pool reached 100 open connections and accumulated substantial wait
time. Increasing the limit from 100 to 200 looked like a direct fix, but the
score regressed from 567,276 to 546,855. Allowing more concurrent pollers also
increased contention in MySQL; it did not guarantee that more ride completions
would finish.

We reverted the pool change and reduced query count and transaction residence
time instead. A pool-wait metric is the location of waiting. It does not, by
itself, say whether the pool, the database, or the workload mix should change.

## 5. Remove a late N+1 without overstating the score delta

The accepted 567,276-point artifact still showed two short but frequent
queries in `GET /api/app/rides`:

```sql
SELECT * FROM chairs WHERE id = ?;
SELECT * FROM owners WHERE id = ?;
```

Each query ran 90,348 times. Together they consumed 155.7 seconds of SQL demand
over the benchmark. The chair-by-ID cache already existed, so the history
handler reused it. We added an owner-by-ID cache, populated both access-token
and ID indexes from the same immutable owner row, and cleared the cache on
initialize. Tests cover cache hits and database-fill behavior.

| Metric | Before | After |
|---|---:|---:|
| Score | 567,276 | **569,078** |
| Chair-by-ID SELECT | 90,348 / 82.14 s | **11 / 0.002 s** |
| Owner-by-ID SELECT | 90,348 / 73.56 s | **3 / 0.001 s** |
| `GET /api/app/rides` average | 6.62 ms | **4.06 ms** |
| `GET /api/app/rides` p95 | 33.55 ms | **16.78 ms** |
| DB-pool wait count | 65,991 | **50,323** |
| DB-pool wait total | 922.15 s | **575.28 s** |

The score increased by 1,802 points, about 0.32%. One benchmark pair is not a
precise causal estimate. We accepted the change because the targeted SQL demand
almost disappeared, endpoint tail latency and pool waits moved in the same
direction, cache invariants were tested, and the benchmark correctness gate
still passed. The small score change also shows that this N+1 was no longer the
system-wide limiting stage.

## What the profiler could and could not tell us

The profiler identified reusable performance facts:

- short SQL multiplied by request count dominated DB demand;
- DB-pool waits changed with workload shape;
- long-poll wall time was not equivalent to CPU saturation;
- a local N+1 fix changed endpoint p95 and pool contention;
- some apparently faster runs were missing or contaminated evidence.

It could not infer ISURIDE-specific invariants such as when a chair may become
visible to each client, which fields are safe to cache, or what a benchmark
error code means. Those decisions required the application specification,
source code, tests, and benchmark logs.

The operational rule was therefore simple: one intended change, one benchmark,
one saved artifact, and an explicit accept or revert decision. Failed runs were
kept because they explain why the next implementation exists.

## Try the measurement workflow

The minimal Go integration is:

```go
db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
http.ListenAndServe(":8080", isutools.HTTP(handler))
```

The dashboard binds to `127.0.0.1:19191` and is intended to be reached through
SSH forwarding. See the repository README and integration guide for supported
databases, reverse-proxy log formats, least-privilege EXPLAIN credentials, and
artifact health rules.
