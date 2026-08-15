# ISUCON13 specialist-tool field verification (2026-08-15)

Issues #46–#54 were exercised on the `isucon13` WSL2 guest at
`ekusiadadus@ssh.almightty.org`. The final isutools revision was `11f3110`, the clean
application revision was `5461a09`, and every official benchmark below passed correctness.

![Final ISUCON13 report after rollback](./images/isutools-isucon13-specialist-20260815.png)

The screenshot is the final A2 PGO-off run: `pass=true`, score `432,917`. It keeps HTTP and
SQL demand, CPU/DB-pool/I/O capacity, and collector completeness in separate evidence lanes.

## Results

| Isolated run | Score | Change from OFF | Meaning |
|---|---:|---:|---|
| instrumentation OFF | 434,794 | — | single-run reference |
| MySQL slow log | 444,057 | +2.130% | functional/overhead evidence, not an improvement claim |
| allocs pair | 439,845 | +1.162% | functional/overhead evidence |
| five-second trace | 439,353 | +1.049% | functional/overhead evidence |
| CPU profile | 431,824 | -0.683% | representative PGO input capture |

The predeclared single-run gate flagged a feature only if it scored at least five percent below
OFF. No feature crossed that gate. Short-run score variance prevents a causal interpretation.

The exact access-log interval contained 87,637,847 bytes and 464,411 parsed requests with zero
malformed, partial, or overflow records. Artifact
`07c09703358bfe3da6b72028a95c9938d3f3b3444ce4951f780d4f25518c8724` was `ready` and bound to
the run ID plus snapshot SHA. The leading cumulative request-time paths were icon reads,
live-comment posts, reaction posts, moderation posts, and reservations.

The exact MySQL slow-log interval contained 3,326,572 bytes, 29 events, and 20 classes with no
partial event. Native output stayed literal-free and portable; the pt-query-digest 3.7.1-4 text
remained restricted. HTTP serving returned 200 for the portable SHA-matching summary and 403 for
the restricted report. Real initialization SQL exceeded the default 1 MiB query-event budget, so
the CLI now exposes bounded `--max-query-bytes` and `--max-line-bytes` controls with an 8 MiB hard cap.

The allocs open/close pair reproduced an 8.31 GiB interval with `go tool pprof -base`.
Goroutine and threadcreate captures retained interval versus cumulative semantics; unsupported
Go 1.24 goroutineleak reported stable `degraded/unsupported`. The five-second trace was complete,
331,712 bytes, and opened with the Go 1.24 trace viewer.

The CPU bundle verified the captured and analyzed binary SHA. Inside a transient systemd
`Delegate=yes` unit, the analysis worker read back cgroup-v2 memory/pids limits, RLIMIT_AS,
SIGSTOP bootstrap, and membership. All twelve pprof recipes became ready for the matching binary;
all twelve were rejected with `binary-match-required` for `/bin/true`.

## PGO decision

The candidate preserved run/snapshot/profile/binary/source/toolchain provenance and built without
changing the source tree. PGO binary SHA was `02d5d207...e49fe6a0`; the rollback binary SHA was
`25e885ef...14e806`.

| Block | Variant | Score | Pass |
|---|---|---:|---|
| A1 | off | 443,553 | true |
| B1 | PGO | 437,791 | true |
| B2 | PGO | 437,677 | true |
| A2 | off | 432,917 | true |

The off median was 438,235 and the PGO median was 437,734, a -0.114% change. The predeclared
+2% adoption threshold was not met, so PGO was rejected and the exact off binary was restored.
This negative result is retained separately from the successful workflow validation.

## Fresh private-isu replay

A second WSL2 guest was rebuilt from private-isu `0dc3be8` and PR head `0dbd692` in the isolated
Compose project `isutools-specialist-fresh`, with separate named volumes and loopback ports. A process-level
`mysqladmin ping` became ready before the `users` table existed, so the incomplete run was aborted and the
validation-only volume was recreated. The final gate required all three application tables and a non-empty page.

Three independent runs passed correctness at score zero: baseline `run-63060cc2f4864931`, CPU diagnostic
`run-dac4864f87c969b1`, and slow-log diagnostic `run-9d085737046fa02b`. Score zero is integration evidence,
not a performance result.

- The offline inspector parsed all 781 access records from 109,569 bytes with no malformed or partial line.
- Exact slow-log coverage held 548,795 bytes, 2,032 events, and 16 classes with no partial event. Artifact
  `eff7bc803007944e7044e7a84e8ac898738c88009294445f647f0b4b93cde6b0` was ready; the normalized summary
  stayed portable and the pt-query-digest 3.7.1-4 report stayed restricted.
- Bundle `5e7ce9b3...f9127aff` matched binary SHA `efdf86ff...13ee23`; standard Go 1.26
  `go tool pprof -top` decoded the 105.7-second CPU profile.
- PGO preparation first rejected the dirty integration tree, then rejected a clean temporary tree because
  snapshot source/toolchain provenance did not match. No private-isu candidate was presented as valid; the
  positive candidate/build and negative A-B-B-A adoption decision remain the ISUCON13 evidence above.

Portable outputs passed a fresh secret scan. CPU profiling and MySQL slow logging were restored to off,
with `long_query_time=10`; the isolated containers remain loopback-only.

See the [full Japanese evidence record](./isucon13-specialist-tools-verification-20260815.md),
[specialist-tool playbook](./SPECIALIST_TOOLS.en.md), and
[threat model](./SECURITY_EXTERNAL_ANALYSIS.md).
