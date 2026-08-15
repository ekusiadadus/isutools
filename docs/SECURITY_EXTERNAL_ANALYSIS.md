# External analysis threat model and limits

Reviewed against the implementation on 2026-08-15. These controls apply in addition to loopback-only admin access over SSH.

## Trust boundaries and data classes

| Boundary | Accepted evidence | Never portable |
|---|---|---|
| proxy/application → offline CLI | bounded regular file or stdin | Cookie, Authorization, raw query values, public session headers |
| MySQL → slow-log analyzer | explicit operator-owned slow-log interval | DSN, password, raw SQL literal, user/host name |
| target → control host | exact snapshot/profile/trace hashes | unverified path or mutable “latest” name |
| external tool → artifact store | allowlisted argv, bounded stdout | stderr, environment dump, arbitrary Perl `--filter`, plugin or DSN |
| artifact store → Web/public report | verified `portable` output | `restricted` pt-query-digest text and raw inputs |
| profile → pprof/PGO | matching binary SHA and explicit source | automatically uploaded binary/source, server-side arbitrary command |

Pseudonymous bounded session IDs, normalized routes, hashed query classes, aggregate metrics, stable reason codes, content hashes, and version identifiers may be portable.

## Resource limits

| Feature | Input / line | Cardinality / work | Output / time / concurrency | Failure state |
|---|---:|---:|---:|---|
| access inspector | default 1 GiB / 1 MiB; hard 4 GiB / 8 MiB | 1,000,000 records, 10,000 keys, 16 percentiles; hard 5,000,000 / 100,000 | synchronous explicit CLI; exact samples retained within the input budget | bounded error; no artifact |
| filter grammar | 4 KiB expression | 64 nodes, depth 8, 16 set values, 8 regexps, bounded RE2 | no Go/Perl/shell execution | stable parse code |
| slow-log parser | default 256 MiB / 1 MiB / 1 MiB query; hard 1 GiB / 8 MiB / 8 MiB | 1,000,000 events, 10,000 classes; hard 5,000,000 / 100,000 | post-run only | partial or bounded error |
| pt-query-digest | parser input limit | fixed `--limit=20`, fixed order/report format | 60 s wall/CPU, 512 MiB address space, 16 MiB output, one child per explicit CLI | absent/version/isolation/timeout/output code; never ready raw output |
| pprof analyzer | 32 MiB/profile, 2 MiB manifests | 8 attempts, 32 sample types, 50 top nodes, 2,048 flame nodes | 60 s default, max 5 min; 512 MiB cgroup RSS and 4 GiB address space where supported | failed derived artifact |
| runtime trace | default 64 MiB, hard 256 MiB | one active process-wide owner | default 5 s, hard 30 s, 16 MiB free reserve | raw file removed unless sidecar publication completes |
| external envelope | 2 MiB manifest, 32 refs | 64 diagnostics, 8 extensions / 64 KiB each | 10 min and 4 GiB representable ceilings | strict invalid/unsupported |
| PGO candidate | 64 MiB CPU profile, 256 KiB manifest | one main package, clean source revision | new 0700 directory; files 0600; no overwrite | no ready marker |

## Process and filesystem controls

- pt-query-digest is invoked through code-owned `prlimit` argv without a shell. Executable lookup is restricted to system binary directories; `RLIMIT_AS`, `RLIMIT_CPU`, and `RLIMIT_NOFILE` are applied before the analyzer starts. Flags are code-owned and environment variables are allowlisted. Stderr is classified, not copied.
- pprof input is parsed in the established hard-isolated worker. Handoff recipes are data and are never executed by the server.
- All attached input uses no-follow regular-file access. Publication uses directory-bound safe filesystem operations, content-addressed no-replace writes, and explicit CAS.
- CPU profiling, execution trace, and manual CPU/trace endpoints share one process-wide owner. Conflicts return HTTP 409 `profiler-busy`.
- `ISUTOOLS=off` does not apply runtime rates, start trace/profile goroutines, write artifacts, or wrap request handlers. Explicit offline CLIs remain available because they do not mutate the target process.
- Runtime diagnostic features and MySQL slow logging default off. Diagnostic runs are not score-adoption runs.

## Regression gates

CI runs secret-negative fixtures, malformed/oversized/deep input tests, CSV formula injection tests, shell/newline tests, symlink/path tests, content-hash and snapshot/binary mismatch tests, profiler-owner tests, and fuzz targets for access logs, slow logs, and artifact manifests. A restricted `analysis-*` output receives HTTP 403 even when its filename is known.

No unresolved High-severity issue was identified in this implementation review. Field validation must still scan the actual remote artifact directory for Cookie, token, DSN, and SQL literal material before release.
