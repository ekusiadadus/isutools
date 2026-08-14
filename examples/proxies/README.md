# Proxy access-log examples

These files are minimal logging fragments, not complete production server
configurations. Merge one into the version already installed by the contest or
deployment, validate it with that product's native configuration checker, and
point `ISUTOOLS_ACCESS_LOG` at the resulting file.

| Product | Example | `ISUTOOLS_ACCESS_LOG_FORMAT` |
|---|---|---|
| nginx / OpenResty | `nginx.conf` | `isutools-ltsv` |
| Apache httpd / OpenLiteSpeed | `apache.conf` | `isutools-json-v1` |
| H2O | `h2o.conf` | `isutools-json-v1` |
| Envoy 1.34+ | `envoy.yaml` | `isutools-json-v1` |
| Envoy <= 1.33 | `envoy-legacy.yaml` | `isutools-json-v1` |
| Caddy | `Caddyfile` | `caddy-json` |
| HAProxy | `haproxy.cfg` | `isutools-json-v1` |
| Traefik Proxy | `traefik.yaml` | `traefik-json` |
| lighttpd | `lighttpd.conf` | `isutools-json-v1` |
| Varnish | `varnishncsa.service` | `isutools-json-v1` |
| Apache Traffic Server | `trafficserver-logging.yaml` | `isutools-json-v1` |
| IIS | `iis-logging.ps1` | `iis-w3c` |
| Squid | `squid.conf` | `isutools-json-v1` |
| nginx stream/TCP | `nginx-stream.conf` | separate L4 evidence; not parsed as HTTP |

The modern examples deliberately omit query strings and client request
headers. Envoy <= 1.33 cannot remove the query with its built-in formatter;
`envoy-legacy.yaml` is decoded safely, but its on-disk log can contain the
query. Flow/scenario analysis defaults to application middleware, so it works
with every listed proxy without copying a public cookie into a log.

These files are integration fragments. Merge one into the installed product's
complete configuration and run that product's native config validator before
reload. Parser fixtures do not replace version-specific native validation.

CI runs `integration/test-proxy-configs.sh` against pinned nginx 1.28.0,
Apache httpd 2.4.65, H2O 2.2.5, Envoy 1.34.13, legacy Envoy 1.33.11,
and Caddy 2.10.2.
The remaining products are schema-compatible fixtures, not claims about an
unseen deployment.
