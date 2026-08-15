# isutools / ALP / pt-query-digest / pprof field playbook

Checked 2026-08-15 against [ALP](https://github.com/tkuchiki/alp), [pt-query-digest 3.7.1-4](https://docs.percona.com/percona-toolkit/pt-query-digest.html), [pprof](https://github.com/google/pprof/blob/main/doc/README.md), [Go diagnostics](https://go.dev/doc/diagnostics), and [Go PGO](https://go.dev/doc/pgo).

Use ALP or `isutools inspect accesslog` for ad-hoc filtering of an old proxy log. Use Performance Schema for whole-run query cost and a slow log/pt-query-digest for individual lock spikes and outliers. Use `go tool pprof` for interactive graph/source/disassembly/tag analysis and `go tool trace` for a short scheduler, GC, syscall, and latency window. isutools does not replace those specialist UIs; it binds their evidence to the same run, snapshot SHA, binary SHA, limits, and publication policy as HTTP, SQL, host data, correctness, and score.

```bash
isutools inspect accesslog --file access.log --format isutools-ltsv \
  --where 'status >= 500 && duration >= 100ms' \
  --percentiles 50,90,95,99 --sort p99_ns --reverse --output markdown

isutools-pprof fetch --admin http://127.0.0.1:19191 \
  --snapshot-base SNAPSHOT_BASE --snapshot-sha256 SNAPSHOT_SHA --bundle-dir ./bundle
isutools-pprof recipes --bundle-dir ./bundle --binary ./matching-server \
  --source-root ./source --output shell
```

Cumulative open/close profiles use `-base`; only independent-run comparisons use `-diff_base`, and normalization is explicit. A trace recipe uses only `go tool trace`. A binary mismatch, incomplete pair, missing source, or incomplete trace is never marked ready.

When attaching an access log to a run, provide its start/end device, inode, offset, and proxy clock. isutools also requires `end_offset - start_offset` to equal the input file size; absent or inconsistent coverage is published as `partial`, never as an exact run interval.

All new runtime capture is default off. Enable one diagnostic at a time with `ISUTOOLS_ALLOCS_PROFILE`, `ISUTOOLS_GOROUTINE_PROFILE`, `ISUTOOLS_THREADCREATE_PROFILE`, supported `ISUTOOLS_GOROUTINELEAK_PROFILE`, or `ISUTOOLS_TRACE_SECONDS=1..30`. Go's diagnostics guidance warns that diagnostic facilities can interfere, so do not promote a diagnostic run into a score-adoption run.

For MySQL, explicitly enable the slow log outside the application and record start/end device, inode, offset, and DB clock. The input byte count must equal the offset span or coverage becomes `partial`. The portable result contains hashes and aggregates, not SQL literals; pt-query-digest text remains restricted. Large intentional multi-line statements may raise `--max-query-bytes` from its 1 MiB default, but never beyond the 8 MiB hard cap; `--max-line-bytes` is bounded separately.

PGO is experimental and opt-in:

```bash
isutools-pgo prepare --snapshot SNAPSHOT.json --snapshot-sha256 SNAPSHOT_SHA \
  --profile cpu_CAPTURE.pprof --binary ./captured-server --source-dir ./clean-source \
  --main-package ./cmd/server --output-dir ./pgo-candidate \
  --rationale 'representative passing official workload'
isutools-pgo build --candidate-dir ./pgo-candidate --source-dir ./clean-source --variant pgo
isutools-pgo build --candidate-dir ./pgo-candidate --source-dir ./clean-source --variant off
```

The candidate never modifies the source tree or overwrites an existing directory. `build` compiles only the same clean revision with fixed argv and records each PGO/off binary hash, size, build time, and Go/PGO build info in a private manifest. Predeclare an ABBA comparison and retain every pass flag, raw score, snapshot SHA, and binary SHA. General Go PGO improvements are not an ISUCON performance guarantee.

See the [Japanese playbook](./SPECIALIST_TOOLS.md) for complete attach and coverage commands, the [ISUCON13 field verification](./isucon13-specialist-tools-verification-20260815.en.md) for raw pass/score and rollback evidence, and the [threat model](./SECURITY_EXTERNAL_ANALYSIS.md) for limits and visibility rules.
