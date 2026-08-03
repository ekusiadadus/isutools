# Implementation and verification status

Updated: 2026-08-03 (Asia/Tokyo)

## Implemented locally

- M1: bounded SQL aggregation, literal masking, snapshot HTML/JSON, build/host metadata,
  loopback admin server
- Dashboard extension: MySQL/MariaDB schema capture, atomic HTML/JSON persistence,
  saved snapshot listing
- M0 gates: collector health/partial, generation-scoped SQL store, serialized reset,
  fixed endpoint methods, non-loopback Bearer authentication
- M2: HTTP middleware, nginx LTSV delta tailer, Linux reset-to-snapshot procstats,
  snapshot schema v3 and HTML sections for all three collectors

SQL and HTTP individually swap to a new generation and wait for measurements that
started in the old generation. Concurrent reset calls are serialized. The admin handler
currently swaps different collector types sequentially, so benchmark automation must wait
for `POST /reset` to return before starting load. A single shared cross-collector swap is a
remaining release-hardening item if resets must be correct under continuous traffic.

## Local evidence

The current worktree passed:

```text
go test -race ./...        PASS (12 packages)
go vet ./...               PASS
aggregate statement cover 86.4%
dbinspect cover            91.1%
accesslog cover            85.7%
httpstats cover            86.8%
procstats cover            81.8%
sqlstats cover             95.0%
web cover                  83.7%
```

The access-log parser also passed a one-second fuzz run. A real TLS HTTP/2 server test was
not run locally because the restricted test environment blocks ordinary test listeners;
the protocol aggregation path is covered with `HTTP/2.0` requests and the full suite's
loopback admin listener was verified with the required test permission.

The M2 benchmark control sequence is `POST /reset` → run benchmark → `POST /collect` →
download/save snapshot. `/collect` serializes with reset, uses a bounded context, and waits
for the configured nginx log to remain quiet before returning.

## private-isu evidence boundary

The already deployed private-isu integration was rechecked read-only: remote `master` is
clean at `01a5b62`, and `webapp/golang/go.mod` still pins `isutools v0.1.0`. That integration
verified the one-line SQL wrapper, loopback compose exposure, build metadata injection,
snapshot save/download, SQL top-five output, Discord notification, and literal masking.
That run identified the existing bottlenecks but did not exercise this worktree's M2 HTTP,
nginx access-log, procstats, schema-v3 health, or Bearer authentication changes.

Therefore the v0.2 code is a local candidate, not a remotely or physically verified release.
Before tagging, update private-isu to this revision and run same-binary, same-host ABBA
comparisons with `ISUTOOLS=off` and enabled, preserving score, p95, errors, revision, dirty
state, and host metadata.

## Known limitations

- accesslog M2 accepts only the explicit nginx LTSV format in
  `examples/nginx-isutools.conf`; combined/Apache formats remain M3 work.
- log rotation draining is best effort. Writes to an old inode after drain and a
  copytruncate file that regrows beyond the old offset between polls cannot be recovered
  reliably; detected rotations/truncations mark the snapshot partial.
- procstats can see only the active PID namespace and is Linux-only.
- normal HTTP requests are implemented. Dedicated WebSocket/SSE connection statistics and
  HTTP/3 compatibility tests remain M4.
- the 592 KB `sql.log` object in private-isu history was not rewritten or force-pushed.
