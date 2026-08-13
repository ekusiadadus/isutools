# Advisor rule provenance

Advisor output is deterministic diagnostic evidence, not an automatically
generated tuning score. Every row states its source, freshness, scope, formula
and limitation in the snapshot. The anchors below are stable rule identifiers.

| rule | category | predicate / evidence boundary |
|---|---|---|
| <a id="dsn-interpolate-params"></a>`dsn-interpolate-params` | config | registered MySQL DSN metadata; not server runtime proof |
| <a id="mysql-max-connections"></a>`mysql-max-connections` | measured DB | current MySQL variable; compare with app pool × host count |
| <a id="mysql-buffer-pool"></a>`mysql-buffer-pool` | measured DB | buffer pool vs information_schema data+index bytes |
| <a id="mysql-slow-log"></a>`mysql-slow-log` | measured DB | current slow_query_log state; write cost is workload-dependent |
| <a id="nginx-gzip"></a>`nginx-gzip` | config | bounded nginx config contains `gzip on` |
| <a id="nginx-keepalive"></a>`nginx-keepalive` | config | upstream keepalive directive is present |
| <a id="nginx-worker-connections"></a>`nginx-worker-connections` | config | configured value; static config is not active-master proof |
| <a id="nginx-sendfile"></a>`nginx-sendfile` | config | bounded nginx config contains `sendfile on` |
| <a id="nginx-expires"></a>`nginx-expires` | config | static-cache directive present; correctness is app-specific |
| <a id="nginx-proxy-cache"></a>`nginx-proxy-cache` | config | proxy cache opportunity, not a universal requirement |
| <a id="nginx-proxy-cache-lock"></a>`nginx-proxy-cache-lock` | config | lock checked only when proxy cache is enabled |
| <a id="nginx-proxy-cache-set-cookie"></a>`nginx-proxy-cache-set-cookie` | security config | warns when Set-Cookie cache protection is bypassed |
| <a id="cache-app-telemetry"></a>`cache-app-telemetry` | measured metric | explicit interval hits/misses/evictions; HTTP cannot infer it |
| <a id="os-somaxconn"></a>`os-somaxconn` | host metric | procfs value; must be read with server listen backlog |
| <a id="os-port-range"></a>`os-port-range` | host metric | local ephemeral range; does not prove exhaustion |
| <a id="os-nofile"></a>`os-nofile` | host metric | process soft limit; current descriptor demand is separate |
| <a id="go-gomaxprocs"></a>`go-gomaxprocs` | runtime metric | GOMAXPROCS compared with visible cgroup cpu.max |
| <a id="nginx-upstream-uds"></a>`nginx-upstream-uds` | opportunity | only static same-host plain-HTTP upstreams qualify |
| <a id="nginx-listen-backlog"></a>`nginx-listen-backlog` | config+host | explicit backlog compared with somaxconn |
| <a id="go-pgo"></a>`go-pgo` | build provenance | Go build settings; no performance gain is predicted |
| <a id="plan-full-scan"></a>`plan-full-scan` | measured plan | fresh interval plan contains type=ALL |
| <a id="plan-filesort"></a>`plan-filesort` | measured plan | fresh interval plan contains Using filesort |
| <a id="plan-temporary"></a>`plan-temporary` | measured plan | fresh interval plan contains Using temporary |
| <a id="http3-server"></a>`http3-server` | protocol config | proxy declares QUIC/HTTP3 capability |
| <a id="http3-tls"></a>`http3-tls` | protocol config | TLS 1.3/certificate configuration only |
| <a id="http3-advertisement"></a>`http3-advertisement` | protocol config | Alt-Svc present; not client reachability proof |
| <a id="http3-fallback"></a>`http3-fallback` | protocol config | h2/h1 fallback present |
| <a id="http3-udp-listener"></a>`http3-udp-listener` | local socket | local UDP/443 evidence only |
| <a id="http3-network-path"></a>`http3-network-path` | external evidence | explicit reachable/blocked result; never guessed locally |
| <a id="http3-edge"></a>`http3-edge` | external evidence | operator-declared CDN/LB termination state |
| <a id="http3-quic-health"></a>`http3-quic-health` | measured metric | supplied QUIC retransmit/drop counters |
| <a id="http3-traffic"></a>`http3-traffic` | measured metric | access-log protocol distribution; scoped to log vantage point |
| <a id="ech-config"></a>`ech-config` | config | nginx ssl_ech_file only; DNS HTTPS record remains external |
| <a id="ech-key-rotation"></a>`ech-key-rotation` | config | number/order of ECH key files |
| <a id="ech-logging"></a>`ech-logging` | config | ssl_ech_status logging present; does not prove success traffic |

Unknown or future rules still receive conservative generated provenance and a
stable docs anchor. Adding a rule should add its anchor to this table in the
same change.
