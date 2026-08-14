# private-isu field-feedback verification (2026-08-14)

This record separates merged-code proof, remote runtime proof, and remaining
limits. The target was the existing Windows/WSL2 private-isu environment reached
with `ssh ekusiadadus@ssh.almightty.org`. No private-isu performance tuning was
added: the working tree changes were limited to isutools wiring, its Compose
overrides, and the nginx measurement log.

## Exact revisions

- private-isu: `0dc3be8b5b32d8519e0e841721da3ddf2c6a1542`
- isutools feature merge: `c11247114c1532c640527e96b1d7d7ed7250b67c`
  ([PR #33](https://github.com/ekusiadadus/isutools/pull/33))
- isutools field-fix merge used by the final run:
  `0f71797ab9ca968967b0a610ce98c8871bbd1624`
  ([PR #34](https://github.com/ekusiadadus/isutools/pull/34))
- private-isu Go dependency:
  `v1.4.1-0.20260814003620-0f71797ab9ca`
- private-isu app image:
  `sha256:2b32273fb2b3e3f1abba1b78d6fcd6dbab3e252c3ab8fb4694ccecb186bca847`

Issues #19 through #30 are closed. PR #33 and PR #34 passed the Go 1.24,
race/coverage, MySQL integration, and privileged Linux cgroup jobs before merge.

## Final remote results

| Check | Result |
|---|---|
| readiness | `make check` passed: private-isu HTTP, isutools JSON, and `mysqladmin ping` |
| real benchmark | run `run-8d48e8c3943b9cc5`; `pass=true`, score `0`, success `583`, fail `55` |
| durable snapshot | base `20260814-094450.789688363-000001_gen3_unknown_score0`; SHA-256 `b2af7eb0a5c1bc2b57d254757e78d3f155f459bfa3df1b166952f36f4df27599` |
| `/save` diagnosis | invalid `pass=maybe` returned HTTP 400, `X-Isutools-Reason: invalid-pass`, and bounded JSON without query-value reflection |
| required peers | run `249a7b63-1028-48a5-953f-2852b11060b0` was `valid`; app and db both finished and were ACK-sealed |
| optional peer loss | run `4a8e3b1a-763f-4ccb-acd2-ae2f196f6e40` was `partial`; required app/db succeeded and `optional-missing` retained its configured name with `preflight/unreachable` |
| pprof fetch | bundle `daade2b915422d6eaefd733ed3f40c248c680c489fe6969ed9fc65c69a2e4f39`; exact saved snapshot and profile sidecar verified |
| binary provenance | capture-time and analyzed binary SHA-256 both `e4a119d29d5dcbf013f7c06e1cfded56d6121ba568f13ea3f4d488d9f39b4abe` (`verified`) |
| pprof isolation | Linux cgroup v2, cgroup-fd/SIGSTOP bootstrap, 512 MiB physical memory, swap 0, 4 GiB address space; hard-limit, stopped-state, and membership readbacks all true; temporary cgroup removed |
| flame output | analysis `57756a104b976a93b5d14fb64dcb93f389ea2658e101b1d8e1b82edf47db7d01`; flame `ready`, 1621 nodes; durable published artifact `d8b61ba896975095dd0a23199ec124c4b85aa8dafb8888f53230240193b81303` |

The analysis status is `partial` only because dependency source paths outside
the selected private-isu source root were redacted. Profile coverage is
complete and the flame tree is ready; this is not a missing-profile or
binary-mismatch status.

![Merged private-isu pprof verification](images/private-isu-pprof-field-verification-20260814.png)

![Merged multi-host verification](images/private-isu-multihost-field-verification-20260814.png)

## Reproduction from the control PC

The repository Makefile reads the gitignored `.isutools.mk`. Only environment
paths and the SSH host belong there:

```make
REMOTE_HOST := ekusiadadus@ssh.almightty.org
REMOTE_ROOT := /path/to/private-isu
REMOTE_RESULTS := /path/to/remote/staging
REMOTE_RESULTS_SCP := /path/reachable/by/scp
```

Then:

```bash
make status
make check
make bench
make verify-results
make tunnel
# Open http://127.0.0.1:19191/
```

`make bench` performs `reset -> benchmark -> collect -> save -> SCP`. The
benchmark JSON is parsed with strict numeric `score` and boolean `pass` checks;
an absent or malformed final benchmark object aborts the isutools run instead
of saving a guessed result.

For run-aligned CPU analysis, use the exact base and hash returned by `/save`:

```bash
isutools-pprof preflight --admin http://127.0.0.1:19191 --block-runs 1
isutools-pprof fetch --admin http://127.0.0.1:19191 \
  --snapshot-base "$BASE" --snapshot-sha256 "$HASH" --bundle-dir ./pprof-bundle
isutools-pprof analyze --bundle-dir ./pprof-bundle --binary ./exact-app \
  --source-root ./private-isu/webapp/golang --output ./analysis.json
isutools-pprof publish --admin http://127.0.0.1:19191 \
  --analysis ./analysis.json --expected-current none
```

On Linux the analyzer additionally requires a deliberately delegated cgroup v2
root via `ISUTOOLS_PPROF_CGROUP_ROOT`. It does not downgrade to a soft memory
limit when hard isolation cannot be established.

## File formats and locations

The remote application data directory contains:

- immutable schema-v3 snapshot JSON and self-contained HTML:
  `<timestamp>-<sequence>_gen<generation>_<revision>_score<score>.json|html`
- run CPU data: `cpu_<capture-id>.pprof` plus its immutable
  `cpu_<capture-id>.meta.json`
- derived analysis: `.profile.analysis.<analysis-id>.json`,
  `.profile.render.<artifact-id>.html`, `.profile.current.json`, and an
  append-only `.profile.commit.<sequence>.json`
- multi-host hub data: private mode-0600
  `multihost_YYYYMMDDTHHMMSS.nnnnnnnnnZ.json`

`REMOTE_RESULTS` selects remote Docker staging, `REMOTE_RESULTS_SCP` selects
the path that SCP can read, and `RESULTS_DIR` selects the control-PC directory.
The default local destination is:

```text
~/isutools-private-isu-results/
```

The exact pprof bundle, analysis JSON, app binary, and both multi-host JSON
files from this verification were copied to:

```text
~/isutools-private-isu-results/field-verification-20260814/
```

Neither peer tokens nor DB credentials are copied into that evidence folder.

## Evidence limits

- `score=0` with timeouts proves the reset/benchmark/collect/save/profile/SCP
  integration path; it is not a performance result.
- The two peers were distinct standalone agent processes on one WSL2 host.
  This proves hub protocol, evidence, failure classification, and SSH-safe
  loopback operation, but not clock behavior across two physical machines.
- The private-isu snapshot is partial because the SQL-row collector saw an
  unpaired target boundary after database initialization. The run coordinator
  itself recorded validity `valid`; the degraded collector is explicit rather
  than silently treated as complete.
