# Field-feedback implementation guide

Updated: 2026-08-14

This guide is the operational contract for issues #19–#29. Every feature is
bounded, opt-in where it opens a listener or adds database work, and emits
stable reason codes instead of configuration values, DSNs, cookies, or tokens.

## Safe global shutdown and `/save` diagnosis

`ISUTOOLS=off` is resolved once per process. It returns the raw SQL driver,
leaves handlers unwrapped, does not construct the measurement singleton, does
not start admin or peer listeners, and makes counters/watch APIs no-ops.

`POST /save` returns `X-Isutools-Reason` and a small JSON body on error:

| HTTP | reason | operator action |
|---|---|---|
| 400 | `data-dir-unset` / `invalid-pass` | set `ISUTOOLS_DATA_DIR`; pass only `true` or `false` |
| 409 | `run-not-active` / `mutation-busy` | take one reset→bench→save boundary; do not overlap mutations |
| 413 | `snapshot-too-large` | inspect dropped/oversized sections; the 32 MiB cap is not bypassed |
| 500 | `persist-failed` | verify the data directory, filesystem and free space |

Successful publication reports `saved`. Underlying errors and request query
values are excluded from both the response and the bounded audit record.

## Access-log URI grouping and SQL comment policy

Access-log grouping is applied after query removal and before aggregate, flow,
and story observation:

```bash
export ISUTOOLS_ACCESS_LOG_PATH_RULES='^/posts/[0-9]+$=/posts/*;^/users/[0-9a-f-]+$=/users/*'
export ISUTOOLS_ACCESS_LOG_UNMATCHED=keep      # or collapse
```

Patterns must match the whole path; replacements are constants and cannot
contain regexp captures. There are at most 64 rules / 8 KiB of configuration.
`collapse` maps all misses to `(unmatched)`.

SQL normalization always removes comments. The default
`ISUTOOLS_SQL_COMMENT_TAGS=on` retains one safe leading tag such as
`/* controller:chairs */` for grouping; `off` removes every comment without a
tag prefix. Hints, arbitrary
comments, control bytes, and oversized tags are never retained.

## Echo and framework-neutral route templates

The generic adapter is `httpstats.SetRoutePattern(*http.Request, template)`.
It accepts only a trusted registered template and never falls back to the raw
URL. Router adapters should install `SetRouteNotFound` before routing and
overwrite it after a successful match.

Echo v4:

```go
import "github.com/ekusiadadus/isutools/adapters/echov4"

e := echo.New()
echov4.Install(e)
```

Echo v5 uses the same call from `adapters/echov5`. Named parameters, groups,
wildcards, 404s, handler errors and panics are covered by adapter tests.

## Trusted session labels for nginx

The client-supplied `X-Isutools-Session` is never trusted. Generate a fixed
URL-safe HMAC pseudonym inside the application and return it as an upstream
response header:

```go
adapter := sessionlabel.FromEnv(os.Getenv)
handler := adapter.Middleware(applicationHandler)
```

```bash
export ISUTOOLS_SESSION_COOKIE=session
export ISUTOOLS_SESSION_HMAC_KEY='at-least-32-random-bytes-kept-out-of-git'
```

nginx records `$upstream_http_x_isutools_session`, hides the upstream header
from the public response, and clears the inbound spoofable header. Invalid
cookie/key configuration fails closed and reports only a bounded reason.

## CPU handoff and flame view

Managed run-mode CPU capture serializes the process-wide profiler. A reset
waits a bounded 100 ms for a previous requested stop. If ownership remains,
the new run is explicitly recorded as `skipped/cpu-busy`; a later retry can
start after the owner releases. `/reset` exposes
`X-Isutools-CPU-Profile-State` and `X-Isutools-CPU-Profile-Code`.

The external analyzer publishes a deterministic bounded flame tree only when
the input profile hashes and capture executable match. CPU uses interval
weights; cumulative profiles use signed diff weights. Missing inputs,
unsupported profile types, unverified binaries and binary mismatches render an
explicit unavailable reason, not an empty success. The HTML view caps depth at
64 and nodes at 2048 and embeds input hashes and analyzer version.

## Advisor and database support provenance

Each advisor row carries its rule version, category, source, freshness, scope,
formula, actual value/unit, limitation and docs anchor. Rules are deterministic
and sorted; the dashboard's “why” disclosure renders that provenance.

Database support is per target. The canonical matrix is rendered beside the
runtime state (`supported`, `partial`, `unsupported`, `config-missing`,
`version-unsupported`, `unverified`, `failed`). PostgreSQL SQL aggregation and
pool metrics are supported, while MySQL-only schema, row-efficiency and EXPLAIN
adapters are reported unsupported instead of silently omitted.

## Multi-host over SSH-only tunnels

Two peer forms share protocol/schema version 1:

- embedded: set `ISUTOOLS_PEER=on`, keep a 32-byte-or-longer token in
  `ISUTOOLS_PEER_TOKEN`, and call `isutools.ServePeer` on `127.0.0.1`
- standalone: build `./cmd/isutools-agent` for DB/proxy/DNS hosts; it provides
  host, network, SQL-row, dbinspect, advisor and DB-capability data, plus
  query-plan data only when an explicit least-privilege explain target is configured

Example agent:

```bash
umask 077
go build -o bin/isutools-agent ./cmd/isutools-agent
ISUTOOLS_PEER_TOKEN="$TOKEN" bin/isutools-agent \
  -addr 127.0.0.1:19192 -role db -data-dir /var/lib/isutools-agent \
  -targets /etc/isutools/targets.json
```

`targets.json` must be a regular file owned by the agent user, have no
group/other permission (`mode & 077 == 0`), be at most 64 KiB, and contain
strict JSON. DSNs never appear in handshake, snapshot, health, or errors.

Forward every peer to a distinct loopback port on the hub host:

```bash
ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 29192:127.0.0.1:19192 app-host
ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 29193:127.0.0.1:19192 db-host
```

Create owner-only `peers.json`:

```json
[
  {"name":"app","endpoint":"http://127.0.0.1:29192","token":"replace-with-32-plus-bytes","required":true,"required_capabilities":["run-v1"]},
  {"name":"db","endpoint":"http://127.0.0.1:29193","token":"replace-with-another-32-plus-bytes","required":true,"required_sections":["hoststats","network"]}
]
```

Then run the loopback hub and use its control boundary:

```bash
chmod 600 peers.json
go build -o bin/isutools-hub ./cmd/isutools-hub
bin/isutools-hub -addr 127.0.0.1:19193 -peers peers.json -data-dir ./hub-results
curl -fsS -X POST http://127.0.0.1:19193/reset
# benchmark
curl -fsS -X POST http://127.0.0.1:19193/finish
```

Results are private JSON files named
`multihost_YYYYMMDDTHHMMSS.nnnnnnnnnZ.json`. Participant metrics remain
host-by-host and are never summed. A required failure makes the run invalid;
an optional failure makes it partial. Every started peer is sealed exactly
once by ACK or abort, and a silent hub cannot block the next run beyond the
90-second peer lease. Only literal loopback HTTP origins are accepted, so a
missing SSH tunnel fails preflight instead of falling back to a public route.

The hub page identifies every row by configured peer name, role and persistent
agent ID. It renders the hub-observed start/finish send-to-ack uncertainty
intervals beside each peer's local collector boundary spread. Peer clocks are
never compared or shifted; the same raw timestamps and host-separated sections
remain in JSON. A preflight failure still carries the configured peer name even
when no identity response could be obtained.
