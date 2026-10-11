# Request dependency timing validation

Base: `d5639c74f51da02964fefb511ecf89949ff98912` (v1.9.0).

The optional `ISUTOOLS_REQUEST_WORK=1` feature attributes completed context-aware
SQL and outgoing HTTP calls to endpoint routes. `clientstats.WrapTransport`
works with an existing HTTP client and a static dependency label. It does not
change concurrency, SQL indexes, application caching or transaction scope.
The optional HTTP `work` object is additive; the JSON schema remains version 3,
with producer capability `request-work-v1`. Old snapshots remain unmeasured.

Validation performed:

- `go test -race -coverprofile=... ./...`: passed, total coverage 84.0%.
- `go vet ./...`: passed.
- `make build-tools`: all six operator binaries built.
- SQL fake-driver plus real delayed HTTP upstream integration: SQL and two
  serial outgoing calls are attributed; a detached call is excluded. Wall-time
  components sum to the request duration. Saved JSON retains timing and excludes
  outgoing URL/query/header/body and SQL literals.
- Real HTTP and TLS tests cover redirects, connection reuse, trace composition,
  cancellation, EOF/read errors, Close and forwarding CloseIdleConnections.
- Concurrent interval union, truncation, label overflow, reset generation,
  panic and streaming exclusions are tested. Snapshot mutation cannot change
  the collector. Old JSON, mixed coverage denominators and HTML label escaping
  are tested.
- Independent read-only review found one completion-contract ambiguity:
  an error from Close after EOF cannot alter a finalized call. The public docs
  now explicitly state this behavior and a regression test covers it.
- A synthetic saved HTML fixture was generated and its contents verified by
  template tests. Browser visual inspection was blocked by the browser tool's
  file-URL policy; it is not claimed as completed.

No ISUCON score improvement is claimed. Target-host on/off ABBA and the
DESIGN.md section 7 score-overhead gate remain unverified: the configured
private-isu host rejected SSH authentication. The feature is off by default;
this release follows the explicit request to release diagnostics and carries
that measurement limitation rather than asserting the performance gate passed.
No existing benchmark host, database, application or historical saved run was
modified. To investigate an application, upgrade the library, wrap its outgoing
client and propagate the incoming request context, then capture a new run.
