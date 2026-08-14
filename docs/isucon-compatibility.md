# ISUCON compatibility matrix

This document is the implementation-facing companion to [issue #45](https://github.com/ekusiadadus/isutools/issues/45).
It records every publicly released ISUCON problem repository audited on 2026-08-14.
“Supported” means that the named application can use the framework adapter or
framework-neutral `net/http` middleware, the data store has a bounded
instrumentation contract, and the edge has an explicit access-log decoder or a
canonical JSON template. It does not claim that a configuration was benchmarked
on every historical VM image.

## All published rounds

| Round | Go application/router | Data store | HTTP/L4 edge and notable services | isutools path |
|---|---|---|---|---|
| 1 qualifier/final | no Go reference app | MySQL | unified frontend not confirmed in the public repo | SQL collector; choose an explicit edge decoder for the deployment |
| 2 qualifier/final | no Go reference app | MySQL | unified reverse proxy not confirmed; PHP sample uses Apache :5000 | SQL collector; Apache canonical JSON for that sample |
| 3 qualifier | Gorilla mux | MySQL, memcached session store | unified frontend not confirmed; Go app :5000 | `adapters/gorillamux`, SQL, counters; choose an explicit edge decoder |
| 3 final | Gorilla mux | MySQL | Apache | Gorilla adapter, SQL, Apache JSON |
| 4 qualifier | Martini | MySQL | nginx | `adapters/martini`, SQL, nginx LTSV |
| 4 final | Martini | Redis | application edge | Martini adapter, `MeasureRedis` |
| 5 qualifier | Gorilla mux | MySQL | nginx | Gorilla adapter, SQL, nginx LTSV |
| 5 final | Gorilla mux | PostgreSQL | nginx, tenki service | Gorilla adapter, `database/sql`, nginx LTSV |
| 6 qualifier | Gorilla mux | MySQL | nginx | Gorilla adapter, SQL, nginx LTSV |
| 6 final | Goji v2/pat | MySQL | nginx HTTP + stream, SSE, Consul | `adapters/gojiv2`, SQL, nginx HTTP/L4 templates |
| 7 qualifier | Echo pre-v4 | MySQL | nginx | `adapters/echov3`, SQL, nginx LTSV |
| 7 final | Gorilla mux + WebSocket | MySQL | nginx | Gorilla adapter, SQL, connection metrics |
| 8 qualifier | Echo pre-v4 | MySQL | H2O | Echo v3 adapter, SQL, canonical H2O JSON |
| 8 final | httprouter | MySQL 8 | nginx, black-box services | `adapters/httprouter`, SQL, nginx LTSV |
| 9 qualifier | chi v5 | MySQL 8 | nginx, payment/shipment | `adapters/chiv5`, SQL, nginx LTSV |
| 9 final | Goji v2 | MySQL 8 | nginx, gRPC black box | Goji adapter, SQL, nginx LTSV |
| 10 qualifier | Echo pre-v4 | MySQL | nginx | Echo v3 adapter, SQL, nginx LTSV |
| 10 final | Echo v4 | MySQL 8 | Envoy | `adapters/echov4`, SQL, canonical Envoy JSON |
| 11 qualifier | Echo v4 | MariaDB 10.3 | nginx, JIA mock | Echo v4 adapter, SQL, nginx LTSV |
| 11 final | Echo v4 | MySQL | nginx | Echo v4 adapter, SQL, nginx LTSV |
| 12 qualifier | Echo v4 | MySQL 8 + tenant SQLite | nginx; Redis installed but not the Go data path | Echo v4 adapter, `database/sql`, nginx LTSV |
| 12 final | Echo v4 | MySQL 8 | nginx | Echo v4 adapter, SQL, nginx LTSV |
| 13 | Echo v4 | MySQL 8 + PowerDNS MySQL | nginx, DNS water torture | Echo v4 adapter, SQL, nginx LTSV |
| 14 | chi v5 | MySQL 8 | nginx, payment mock, optional SSE | chi v5 adapter, SQL, nginx LTSV/connection metrics |

The source of truth is the official repositories under the
[isucon organization](https://github.com/isucon). The exact file evidence and
repository links are retained in issue #45 instead of duplicating a long audit
trail here.

## Framework contracts

| Family | Adapter | Route identity |
|---|---|---|
| `net/http` | `isutools.HTTP` | Go 1.22 `Request.Pattern` or safe not-found constant |
| Gorilla mux | `adapters/gorillamux` | `GetPathTemplate` after routing |
| Martini | `adapters/martini` | registration-time constant passed to `Route` |
| Goji v2/pat | `adapters/gojiv2` | registration-time constant passed to `Route` |
| Echo v3 import path | `adapters/echov3` | `Context.Path` |
| Echo v4/v5 | `adapters/echov4`, `adapters/echov5` | `Context.Path` |
| httprouter | `adapters/httprouter` | registration-time constant passed to `Handle` |
| chi v5 | `adapters/chiv5` | `RoutePattern` after routing |
| Gin | `adapters/gin` | `FullPath` |

No adapter falls back to a raw URL path. This prevents IDs, tokens, and
unbounded 404 paths from becoming metric identities.

## Database and storage contracts

| Store/client | Support | Boundary |
|---|---|---|
| MySQL / MariaDB | `SQLDriverName` via `database/sql`; schema/advisor/EXPLAIN where capability evidence is available | normalized SQL only; DSN credentials excluded from reports |
| PostgreSQL (`pgx` stdlib, `lib/pq`) | `SQLDriverName` via `database/sql` | query timing/count/error; native `pgxpool` is not intercepted |
| SQLite (`database/sql` drivers) | `SQLDriverName` via `database/sql` | query timing/count/error; no MySQL-only schema claims |
| Redis-compatible clients | `ObserveRedis` / `MeasureRedis` | first command token only; keys, values, args, errors, and DSNs discarded |
| memcached / application caches | `Count` / `AddCount` | caller-chosen bounded non-secret counter names |

The root compatibility test exercises the same proxy contract with MySQL,
MariaDB, pgx-shaped, and SQLite-shaped `database/sql` drivers. The independent
`integration/sqlcompat` module additionally runs real lib/pq PostgreSQL and
go-sqlite3 connections in CI.

## Edge decoder contracts

Set `ISUTOOLS_ACCESS_LOG` to the file and select one of these values with
`ISUTOOLS_ACCESS_LOG_FORMAT`:

| Format | Duration unit | Intended products |
|---|---:|---|
| `isutools-ltsv` | seconds | nginx, OpenResty |
| `isutools-json-v1` | explicit `_ns`, `_us`, `_ms`, or `_sec` field | Apache, H2O, Envoy, HAProxy, lighttpd, Varnish, Apache Traffic Server, OpenLiteSpeed, Squid and converters |
| `caddy-json` | Caddy native seconds | Caddy |
| `traefik-json` | Traefik native nanoseconds | Traefik Proxy |
| `iis-w3c` | IIS `time-taken` milliseconds | IIS/HTTP.sys W3C fixed field selection |
| `auto` | legacy JSON seconds or LTSV | compatibility only; do not use for a new deployment |

The canonical schema identifier is `isutools.http-access.v1`. A line must have
`method`, `uri`, `status`, one explicit duration field, and optionally bytes,
protocol, upstream duration, cache/content type, and trusted response labels.
Conflicting duration fields are rejected rather than guessed.

### Edge evidence classes

| Products | Evidence class | Verification |
|---|---|---|
| nginx/OpenResty | native-config-validated + decoder-fixture | nginx 1.28.0 `nginx -t`; LTSV golden record |
| Apache/OpenLiteSpeed | native-config-validated / schema-compatible | Apache httpd 2.4.65 `httpd -t`; OpenLiteSpeed uses the documented Apache-compatible format contract |
| H2O | native-config-validated + decoder-fixture | H2O 2.2.5 config test; canonical JSON golden record |
| Envoy | native-config-validated + decoder-fixture | Envoy 1.34.13 modern config and 1.33.11 legacy config; canonical JSON golden record |
| Caddy | native-config-validated + decoder-fixture | Caddy 2.10.2 config validation; native JSON golden record |
| HAProxy, Traefik, lighttpd, Varnish, ATS, IIS, Squid | decoder-fixture + schema-compatible | product-specific format/unit fixture; config fragment still requires the installed version's native validator |
| nginx stream/TCP | L4-limited | separate `isutools.l4-connection.v1`; never represented as HTTP URI/status |
| all deployments | certified | none; reserved for a named deployed version with native validation, traffic, collection, and saved-report evidence |

`native-config-validated` is deliberately versioned. It is not a certification
of every historical VM image or a claim that a proxy was benchmarked in this
working tree. `integration/test-proxy-configs.sh` pins the repeatable native
checks; the 12-product fixture table pins parser semantics and duration units.

## User scenarios and proxy independence

`isutools.HTTP` uses `sessionlabel` middleware. It strips public flow headers,
HMAC-pseudonymizes one configured cookie, and emits only the pseudonym and an
explicit bounded scenario. The default `ISUTOOLS_FLOW_SOURCE=auto` records
journeys inside middleware, using registered route templates. Therefore the
scenario feature does not depend on nginx.

Available values are:

- `auto`: prefer the proxy-independent middleware collector and fall back to legacy proxy labels only when it has no observations;
- `middleware`: use only the middleware collector;
- `proxy`: legacy trusted *response*-header fields from the access log;
- `off`: no flow collector.

Middleware and proxy observations are never combined in the report, preventing
double counting. Request-header flow fields are never trusted.

## Primary product references

- [nginx access log](https://nginx.org/en/docs/http/ngx_http_log_module.html)
- [Apache HTTP Server logs](https://httpd.apache.org/docs/current/logs.html)
- [H2O access-log directives](https://h2o.examp1e.net/configure/access_log_directives.html)
- [Envoy access logging](https://www.envoyproxy.io/docs/envoy/latest/configuration/observability/access_log/usage.html)
- [Caddy logging](https://caddyserver.com/docs/logging)
- [HAProxy configuration](https://docs.haproxy.org/3.2/configuration.html)
- [Traefik access logs](https://doc.traefik.io/traefik/observe/logs-and-access-logs/)
- [lighttpd mod_accesslog](https://redmine.lighttpd.net/projects/lighttpd/wiki/Mod_accesslog)
- [varnishncsa](https://varnish-cache.org/docs/7.2/reference/varnishncsa.html)
- [Apache Traffic Server logging.yaml](https://docs.trafficserver.apache.org/en/latest/admin-guide/files/logging.yaml.en.html)
- [IIS W3C logging fields](https://learn.microsoft.com/en-us/iis/configuration/system.applicationhost/sites/sitedefaults/logfile/)
- [OpenLiteSpeed logs](https://docs.openlitespeed.org/config/logs/)
- [Squid logformat](https://www.squid-cache.org/Doc/config/logformat/)
