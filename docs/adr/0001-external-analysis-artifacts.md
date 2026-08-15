# ADR 0001: bounded external-analysis artifacts

- Status: accepted
- Date: 2026-08-15
- Scope: offline access logs, MySQL slow logs, runtime traces, pprof handoff, PGO candidates

## Context

Saved snapshots and profile-analysis artifacts already have immutable bytes, hashes, and current markers. Copying complete ALP, pt-query-digest, or trace output into a snapshot would enlarge immutable reports, expose raw data, and make post-run analysis impossible. A path alone would not prove that the file still contains the analyzed bytes.

## Decision

New post-run analyzers use `isutools.external-analysis/v1`, implemented by `internal/analysisartifact` and described by [`external-analysis-v1.schema.json`](../schemas/external-analysis-v1.schema.json).

1. A run attachment includes run ID, snapshot basename, snapshot schema, and snapshot SHA-256. Publication opens that exact regular snapshot and checks all four values.
2. Inputs and outputs carry role, content SHA-256, byte count, media type, and `portable` or `restricted` visibility. Raw proxy logs, slow logs, and pt-query-digest text are restricted.
3. The producer publishes content-addressed output and an immutable manifest before updating the current marker with an explicit compare-and-swap value. A conflict is returned; it is never retried with the observed value.
4. `ready` requires a producer identity and at least one complete output. Partial, unsupported, failed, and invalid states remain distinct.
5. Strict consumers reject unknown fields in v1. Display consumers use the bounded header inspector and show unknown schemas as `unsupported/unknown-schema`.
6. The Web index calls the same store verification as the JSON endpoint. Only verified portable output is linked. Restricted output cannot be fetched through the generic file endpoint.

Producer-specific fields live under a bounded, versioned extension key. Host absolute paths, DSNs, arbitrary argv, credentials, raw URL query values, and SQL literals are not portable metadata.

## Publication and crash behavior

All files are regular, no-follow, mode `0600`. Temporary files are never listed. Immutable publication is no-replace. A crash before current-marker replacement leaves an unreferenced immutable file, not a ready artifact. A stale marker, missing commit, mismatched manifest hash, or mismatched snapshot is displayed as invalid.

The snapshot itself is never rewritten. Snapshot retention is operator-controlled. Runtime profile retention keeps 20 complete groups up to 512 MiB and now treats trace plus sidecar as one group; it does not recognize or delete external-analysis files. External outputs are therefore not silently orphaned by profile cleanup. When deleting a saved run, operators must export or remove its snapshot, activation commits, manifests, and content-addressed outputs as one audited operation; automatic cross-kind deletion is deliberately deferred.

## Compatibility and migration

The existing profile-analysis wire format stays unchanged. `isutools-pprof` writes bundle v2 with command schema, tested Go version, and optional trace, while its loader still accepts bundle v1. No rewrite of saved snapshots, profile analysis, or v1 bundles is required.

## Trade-offs

- Content-addressed files can remain after a crash. This is safer than publishing partial output; a future garbage collector may remove only unreferenced old content.
- Raw human reports are not visible in self-contained HTML. Operators inspect them on the control host.
- External analysis is attached after a run, so historical immutable HTML cannot be retroactively changed. The run's `current UI` and `/external-analysis` show the verified attachment.

## Rejected alternatives

- Independent envelopes per analyzer: duplicates CAS, validation, and visibility policy.
- Embedding raw reports in snapshot JSON: leaks sensitive input and breaks immutable post-run analysis.
- A database or remote object store: breaks portable file-only operation.
- Persisting filesystem paths only: cannot detect replacement or path escape.
