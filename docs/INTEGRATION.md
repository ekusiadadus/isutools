# Integration guide

この文書は、isutools を既存アプリ・DB ドライバ・Web サーバへ接続するための
実運用手順です。最小構成は SQL の1行ですが、アクセスログ、プロセス計測、
CPU profile はそれぞれ明示的な事前準備が必要です。

## 1. 必須・任意の全体像

| 機能 | 追加ライブラリ | 外部ツール・OS条件 | 必要な設定 |
|---|---|---|---|
| SQL 集計 | isutools + 利用中の `database/sql` driver | なし | `SQLDriverName` より前に driver を登録 |
| HTTP 集計 | なし | なし | 最上位 Handler を `isutools.HTTP` で包む |
| 管理 UI / JSON / pprof endpoint | pprof は Go 標準ライブラリ | ブラウザまたは curl | `SQLDriverName` 成功時に loopback で自動起動 |
| proxyアクセスログ | なし | nginx/Caddy/Apache/Envoy、共有ログファイル | 対応フォーマットと `ISUTOOLS_ACCESS_LOG` |
| HTTP/3 readiness Advisor | なし | HTTP/3対応proxy。外部疎通clientは任意 | proxy設定。client protocol実測にはログの`proto` |
| procstats | なし | Linux `/proc` | 対象 PID namespace と `/proc` の読み取り権限 |
| CPU profile 自動保存 | なし | `go tool pprof` は解析時のみ | writable な `ISUTOOLS_DATA_DIR` と採取秒数 |
| k6 シナリオ | Go ライブラリ不要 | k6 実行ファイル | 負荷試験を行う場合だけ |
| SQL 行効率 (sqlrows) | なし | `performance_schema` と `statements_digest` consumer | 既定 DB を持たない stats 接続(§10)。`ISUTOOLS_SQLROWS` |
| Host 資源 (hoststats) | なし | Linux procfs / sysfs、cgroup 上限は v2 のみ | `ISUTOOLS_HOSTSTATS`。cgroup は `ISUTOOLS_CGROUP_PATH` / `_SCOPE` |
| Network (netstats) | なし | Linux `/proc/net`、`/sys/class/net` | `ISUTOOLS_NETSTATS` |
| DB Pool (dbpool) | なし | なし | 登録済み TargetID で `isutools.WatchDBPool` を呼ぶ |
| Query Plans (EXPLAIN) | なし | MySQL 8.0.17+(`QUERY_SAMPLE_TEXT`) | `ISUTOOLS_EXPLAIN=1` と EXPLAIN 専用の最小権限ユーザー(§11) |

Prometheus、Grafana、エージェントデーモンは不要です。`go-sql-proxy` は
isutools 自身の Go module 依存として取得されます。

## 2. DB ドライバへの接続

### 共通ルール

isutools は driver 実装を同梱しません。利用アプリが従来どおり driver を import し、
`database/sql` へ登録した後、その登録名を `isutools.SQLDriverName` に渡します。

```go
driverName := isutools.SQLDriverName("mysql")
db, err := sql.Open(driverName, dsn) // sqlx.Open でも同じ
```

登録に失敗した場合はアプリを止めず、元の driver 名を返します。厳格な起動確認が
必要な CI では `isutools.RegisterSQL("mysql")` の error を確認してください。

### MySQL / MariaDB

```bash
go get github.com/go-sql-driver/mysql
go get github.com/ekusiadadus/isutools
```

```go
import (
    "database/sql"
    _ "github.com/go-sql-driver/mysql"
    "github.com/ekusiadadus/isutools"
)

db, err := sql.Open(isutools.SQLDriverName("mysql"), dsn)
```

MariaDB も `go-sql-driver/mysql` を使う場合は登録名が `mysql` です。
Advisor と DB Schema の自動検査は MySQL / MariaDB が対象です。

### PostgreSQL: pgx stdlib

```bash
go get github.com/jackc/pgx/v5/stdlib
```

```go
import (
    "database/sql"
    _ "github.com/jackc/pgx/v5/stdlib"
    "github.com/ekusiadadus/isutools"
)

db, err := sql.Open(isutools.SQLDriverName("pgx"),
    "postgres://user:pass@127.0.0.1/dbname?sslmode=disable")
```

`pgxpool.Pool` を直接使う native API は `database/sql` driver ではないため対象外です。
`pgx/v5/stdlib` 経由にしてください。SQL 集計は動作しますが、v1 の DB Schema 自動検査は
MySQL / MariaDB のみで、PostgreSQL では未対応状態を表示します。

### PostgreSQL: lib/pq

```bash
go get github.com/lib/pq
```

```go
import _ "github.com/lib/pq"

db, err := sql.Open(isutools.SQLDriverName("postgres"), dsn)
```

### その他の `database/sql` driver

SQLite 等も、driver が `database/sql` へ登録する名前を渡せば SQL 集計部分は利用できます。
ただし個別 driver の DSN 検査と schema inspector は自動対応しません。driver 名は
`sql.Drivers()` で確認できます。

## 3. HTTP Handler

ルーターの外側を一度だけ包みます。query string は保存せず、数値・UUID のパス要素は
既定で `*` に正規化されます。

```go
http.ListenAndServe(":8080", isutools.HTTP(router))
```

WebSocket は handshake が実際に 101 / Hijack へ成功した時だけ、SSE は
`Content-Type: text/event-stream` が確定した時だけ Connections へ分類されます。
拒否された Upgrade は通常 HTTP として記録します。

## 4. nginx アクセスログ

### JSON は必須か

必須ではありません。推奨の LTSV と JSON の両方に対応し、`ParseLine` は行頭の最初の
非空白文字が `{` なら JSON、それ以外なら LTSV と判定します。標準 combined log を
曖昧に推測することはありません。

1. [nginx-isutools.conf](../examples/nginx-isutools.conf) を `http {}` 内で include
2. 計測対象の `server {}` に次を追加
3. アプリプロセスから同じファイルを読めるようにする

```nginx
access_log /var/log/nginx/isutools.log isutools;
```

```bash
export ISUTOOLS_ACCESS_LOG=/var/log/nginx/isutools.log
```

Docker では nginx とアプリの両コンテナへ同じ log volume を mount します。アプリの
実行ユーザーに読み取り権限が必要です。buffered log は `POST /collect` が期限付きで
安定するまで回収します。一回の回収は既定 64 MiB に制限されます。

同梱LTSVの`proto:$server_protocol`は、HTTP/1.1・2・3のclient-facing protocol比率、
protocol別5xx、p95をAdvisorへ渡します。`proto`は任意フィールドなので古いログも読めますが、
reverse proxyがHTTP/3を終端する構成では、これがないとアプリ側`r.Proto`はupstream protocol
しか表さず、HTTP/3利用率を判定できません。

nginx が静的ファイルを直接返す場合、`$upstream_response_time` は `-` だけでなく空文字に
なる構成があります。どちらも正常な「upstream timing なし」として数え、accesslog を partial
にはしません。不正な数値や文字列だけを parse failure として partial にします。

### User Flow の `sess`

同梱設定は `X-Isutools-Session` を `sess:` として記録します。これは認証 Cookie ではなく、
128 byte 以下の短い疑似 ID にしてください。k6 例は VU 番号とiteration由来のテスト ID を送ります。
実ユーザーではアプリ、njs、Lua 等で Cookie を HMAC/SHA-256 化してから header へ渡します。
nginx core だけでは安全な暗号学的 hash を作れないため、生 Cookie、session token、email を
直接 log_format に入れてはいけません。安全な ID を作れない場合は `sess` を省略できます。

### Scenario Stories（最小のファネル/フロー基盤）

`sess`に加えて、アプリまたは負荷生成側が短い非秘密ラベルを送ります。

```text
X-Isutools-Session: k6-vu-3-iter-12
X-Isutools-Scenario: login_and_browse
```

同梱nginx設定はそれぞれ`sess:`と`scenario:`へ出力します。isutoolsは
`scenario + sess`ごとに`METHOD URI`の実測列を作り、同一journeyをまとめて
`sessions / requests / observed journey`の上位20件をHTML/JSONへ表示します。
1 journeyは32 step、追跡sessionと共有page辞書は各10,000件に制限され、超過は
health/partialに残ります。同一page文字列はsession間で共有し、追跡メモリを抑えます。

これは明示ラベル別の「実際に通ったflow」の最小実装です。URLからlogin/purchase等を
勝手に推測せず、必須stepや順序、conversion/drop-offを定義するGA4風funnel DSLは
まだ実装しません。最初は例えば`anonymous_browse`、`login_and_browse`、`author_post`を
別ラベルにし、k6のiterationごとに別の疑似`sess`を付けて比較してください。

`/posts/123`のような動的IDを同じstoryへまとめる場合は、nginx `map`等でログの`uri`を
`/posts/*`へ正規化します。isutoolsは意味を壊す可能性があるため、proxyログのURIを
scenario用に勝手には書き換えません。

`scenario`は64 byte以下、`sess`は128 byte以下の英数字と`._~-`だけを受理します。
生Cookie、session token、Authorization、email、user IDをそのまま入れてはいけません。
これらのheaderは計測ラベルであり、認証・認可・監査ログには使えません。public clientからは
偽装できるため、実ユーザー計測では外部から来た同名headerをedgeで削除し、trusted app/proxyが
HMAC等から作った値で上書きしてください。同梱k6例は閉じたベンチ環境用です。

## 5. Apache HTTP Server

v1 は Apache の標準 combined log を自動判定しません。`%D` が microseconds であるなど
nginx と単位が異なるため、推測すると誤集計になるからです。明示 JSON を使います。

```apache
LogFormat "{\"method\":\"%m\",\"uri\":\"%U\",\"status\":%>s,\"reqtime_us\":%D,\"bytes\":%B,\"proto\":\"%H\",\"sess\":\"%{X-Isutools-Session}i\",\"scenario\":\"%{X-Isutools-Scenario}i\"}" isutools_json
CustomLog /var/log/apache2/isutools.json isutools_json
```

そのファイルを `ISUTOOLS_ACCESS_LOG` に指定します。旧 `ISUTOOLS_NGINX_LOG` も
後方互換で利用でき、両方ある場合は汎用名 `ISUTOOLS_ACCESS_LOG` を優先します。
`reqtime_us` は parser が seconds へ変換します。`%B` は response body bytes です。
`mod_logio` の `%O` は header を含む wire bytes で意味が異なるため、この例では使いません。
Apache core のこの例からは upstream time / cache status を取得できないため、該当欄は空です。

JSON の文字列へ任意 header や生 Cookie を追加するときは、利用する Apache module が
JSON escape を保証するか確認してください。保証できない値は追加しないでください。

## 6. HTTP/3 / QUIC readiness Advisor

### 診断するものと、診断しないもの

Advisorは次を独立したcheckとして表示します。

- `http3-server`: nginxの`listen ... quic`、Caddyのprotocol設定、EnvoyのUDP listener・
  `quic_options`・QUIC transport
- `http3-tls`: TLS 1.3と証明書設定の存在。証明書の期限・名前・trustは実接続で別途検証
- `http3-advertisement`: `Alt-Svc: h3=...`。Caddyはh3 defaultと明示的なheader削除を検査
- `http3-fallback`: 同じendpointのHTTP/2またはHTTP/1.1 TCP fallback
- `http3-udp-listener`: Linux `/proc/net/udp`・`udp6`のローカルUDP/443
- `http3-network-path`: 外部clientからUDP/443へ到達できたという明示evidence
- `http3-edge`: LB/CDNでのHTTP/3終端の明示evidence
- `http3-traffic`: protocol別件数・5xx率・p95上限。proxy logをアプリ計測より優先
- `http3-quic-health`: QUIC packet再送率とUDP datagram drop

設定ファイルの検査はreadinessであり、実行中binaryのbuild option、証明書の有効性、
container port publish、security group、NAT、LB/CDNの管理画面を証明しません。これらを
自動推測して`ok`にすることはありません。

### 共通設定

```bash
export ISUTOOLS_PROXY_CONF=/etc/nginx       # fileまたはnginxのconf directory
export ISUTOOLS_PROXY_KIND=nginx            # nginx / caddy / envoy
```

`_KIND`を省略すると、明確な設定signatureまたはファイル名からだけ判別します。
曖昧なら`skip`です。旧`ISUTOOLS_NGINX_CONF`は後方互換で、汎用変数がない場合だけ使います。

nginx は directory より実際の entrypoint file(`/etc/nginx/nginx.conf` など)を指定してください。
entrypoint mode は `include` の glob を再帰的に解決し、symlink target と cycle を一度だけ読みます
(上限 256 files / 4 MiB / depth 32)。directory mode は全 `*.conf` を探索するため、symlink
重複は除けても include されていない inactive fragment を実設定と区別できません。

nginxは少なくともTCP fallback、QUIC listener、証明書、Alt-Svcを同じserverで用意します。

```nginx
listen 443 ssl;
listen 443 quic reuseport;
http2 on;
ssl_protocols TLSv1.2 TLSv1.3;
ssl_certificate /path/fullchain.pem;
ssl_certificate_key /path/privkey.pem;
add_header Alt-Svc 'h3=":443"' always;
```

Caddyは通常h1/h2/h3とautomatic HTTPSが既定です。明示する場合はCaddyfileを指定します。

```bash
export ISUTOOLS_PROXY_CONF=/etc/caddy/Caddyfile
export ISUTOOLS_PROXY_KIND=caddy
```

Caddy native JSON access logの`request.method` / `request.uri` / `request.proto`、
`duration` / `size` / `status`に加え、検証済みの`X-Isutools-Session`・
`X-Isutools-Scenario`だけを読みます。ログファイルを
`ISUTOOLS_ACCESS_LOG`へ指定するとclient-facing protocolを集計します。

ただし、HTTP/3を外部LB/CDNで終端する場合、origin側のaccess logはedgeからoriginまでの
protocolしか表しません。`ISUTOOLS_HTTP3_EDGE`を設定した構成では、Advisorもlocal logを
client-facing HTTP/3の証拠として扱いません。edge側analyticsまたは外部client実測を併用します。

EnvoyはUDP listenerの`quic_options`、`QuicDownstreamTransport`、downstream TLS、
TCP listenerの`codec_type: AUTO`、TCP応答のAlt-Svcを検査します。アクセスログを
flat JSONにし、`proto`へ`%PROTOCOL%`、必要なら安全な`sess`と`scenario`を出力してください。

### 外部経路・edge・QUIC telemetry

外部疎通後、その結果だけを明示します。

```bash
export ISUTOOLS_HTTP3_UDP443=reachable       # または blocked
export ISUTOOLS_HTTP3_EDGE=cloud-edge        # LB/CDNを使う場合だけ任意の識別名
export ISUTOOLS_HTTP3_EDGE_ENABLED=true      # 管理画面+外部実測で確認した値
```

ローカルUDP listenerがあっても、DockerはTCPと別に`443:443/udp`のpublishが必要です。
疎通試験はHTTP/3対応clientを別hostから実行し、応答protocolと証明書を確認してください。
同じhostのUDP socket確認だけではfirewall/NAT/LB通過の証拠になりません。

再送・dropはGo HTTP middlewareから取得できません。Envoy QUIC/UDP stats等から
`POST /reset`からベンチ終了までの差分を作り、次のJSONを更新します。

```bash
export ISUTOOLS_HTTP3_QUIC_METRICS=/run/isutools/quic.json
```

```json
{
  "packets_sent": 100000,
  "packets_retransmitted": 500,
  "udp_datagrams_dropped": 0
}
```

ファイルはsnapshot/`POST /save`時に毎回読み直され、64 KiBに制限されます。counterの
開始点が異なる場合、再送率は比較不能です。Advisorは2%以上の再送率、1件以上のUDP drop、
または不整合counterを`warn`にします。閾値は障害探索の初期値であり、移行効果の証明では
ありません。

### 移行判断

HTTP/3が観測されても自動的に「高速化した」とは判定しません。同一binary・同一workloadで
fallbackとHTTP/3をA/Bし、score、protocol別p95、5xx率、QUIC再送・dropを比較してください。
通信が低遅延・低損失のLAN内だけなら、HTTP/3の利得がないことも正常です。

### キャッシュ戦略 / ECH check

同じ設定入力(`ISUTOOLS_NGINX_CONF` / `ISUTOOLS_PROXY_CONF`)から、次のcheckも表示します。

- `nginx-proxy-cache`: `proxy_pass`があるのに`proxy_cache`がない場合のadvisory(`info`)。
  キャッシュは「必要なら使う」ものなのでdefect扱いにしません
- `nginx-proxy-cache-lock`: `proxy_cache`有効時に`proxy_cache_lock`がないと`warn`
  (キャッシュ失効の瞬間に再生成が多重実行されるthundering herd)
- `nginx-proxy-cache-set-cookie`: `proxy_ignore_headers`が`Set-Cookie`を無視していると
  `warn`(セッション入り応答が共有キャッシュ経由で他クライアントへ配られるリスク)
- `ech-config` / `ech-key-rotation` / `ech-logging`: nginxの`ssl_ech_file`の有無、鍵切替時の
  新旧鍵並行受け入れ(先頭のファイルだけがretry_configs対象、2件目以降は復号専用)、
  `$ssl_ech_status`のログ出力。ECHはnginx設定のみ検査し、DNS HTTPSレコード(`ech=`)は
  プロセス内から検証しません

アプリ側キャッシュ(memcached / Redis / Valkey)のヒット率とexpire前evictionは
HTTP middlewareから観測できないため、`cache-app-telemetry`は明示入力です。

```bash
export ISUTOOLS_CACHE_METRICS=/run/isutools/cache.json
```

```json
{
  "hits": 90000,
  "misses": 10000,
  "evictions": 0
}
```

QUIC telemetryと同じくsnapshot / `POST /save`時に毎回読み直し、64 KiBに制限されます。
counterは`POST /reset`からベンチ終了までの差分で作ります。`evictions`(expire前の削除)が
1件以上なら容量不足として`warn`、ヒット率50%未満も`warn`です。

## 7. pprof、procstats、負荷生成ツール

### pprof

計測側の追加ライブラリは不要です。`net/http/pprof` は Go 標準ライブラリで、管理 UI の
`/pprof/` 以下へ登録済みです。

```bash
# 30秒 CPU profile を取得
curl -o cpu.pprof 'http://127.0.0.1:19191/pprof/profile?seconds=30'
go tool pprof -http=:0 ./app cpu.pprof
```

`POST /reset` 後の自動採取を使う場合:

```bash
export ISUTOOLS_DATA_DIR=/var/lib/isutools   # アプリ実行ユーザーが書けること
export ISUTOOLS_PPROF_SECONDS=30
```

解析にはローカルの `go tool pprof` が必要です。グラフ表示には Graphviz があると便利ですが、
計測・top 表示の必須条件ではありません。profile は対象実行ファイルと近い Go toolchain で
解析し、シンボルを失わない build artifact を保持してください。

### procstats

追加ライブラリは不要ですが Linux 専用です。コンテナ内ではその PID namespace のプロセス
だけが見えます。ホスト全体を測りたい場合は、意図を確認したうえで host PID namespace と
読み取り可能な `/proc` を与えてください。macOS では disabled と表示されます。

ホスト全体の `/proc` では短命な補助プロセスの終了は通常の churn なので、それだけでは
snapshot を partial にしません。自動配線はアプリ自身の PID を明示追跡し、その identity が
失われた場合は partial にします。`procstats` を直接組み込む場合は
`procstats.WithTrackedPIDs(...)` で同じ契約を追加できます。

自動配線した procstats は run coordinator の baseline collector です。`POST /reset` と
アプリ内の `ResetNow` のどちらで開始しても、その開始境界で baseline、`POST /finish` / save
の終了境界で final を凍結します。Handler 構築時からの経過や、終了後に dashboard を開くまでの
時間は CPU 差分に入りません。

### k6、curl、jq

- k6: 負荷生成を行う場合だけ必要。isutools の Go dependency ではありません
- curl: reset / collect / save の制御例で使用
- jq: JSON の手動確認と ABBA gate script で使用
- Graphviz: pprof のグラフ描画だけに任意

ABBA release gate は [abba.sh](../examples/abba.sh) を使います。最低3ブロック、固定 warm-up、
実行バイナリ fingerprint、score / p95 / error rate、paired 95% CI が必須です。

## 8. 管理ポートと権限

既定 `127.0.0.1:19191` は loopback のため無認証です。遠隔からは SSH tunnel を使います。

```bash
ssh -L 19191:127.0.0.1:19191 user@server
```

application-level token 認証はありません。非 loopback bind は既定で fail closed で、
SSH / firewall / host loopback publish により外部到達性を制限した構成だけ
`ISUTOOLS_ALLOW_UNAUTHENTICATED=1` で明示許可します。
自動管理サーバは loopback `Host` と同一originだけを受け入れ、cross-site browser
requestを403にします。これはSSH境界を補強しますが、外部公開を安全にする認証の代替ではありません。
管理 endpoint、保存ディレクトリ、アクセスログにはベンチ結果や環境情報が含まれるため、
インターネットへ直接公開しないでください。

## 9. 新しい collector の有効化

v1.2 で追加された collector は、いずれも run の**両端でだけ**データを取ります
(`POST /reset` が置く開始境界と、`POST /finish` / `POST /save` が置く終了境界)。
ベンチ実行中の常時サンプリングはありません。EXPLAIN 取得(§11)だけは例外で、
終了境界より後の enrich phase に 1 回だけ走ります。

### フラグと既定値

| 表示 | section key | フラグ | 既定 | 無効化になる値 |
|---|---|---|---|---|
| SQL 行効率 | `sqlrows` | `ISUTOOLS_SQLROWS` | on | `off` `0` `false` `no` `disabled` |
| Host | `hoststats` | `ISUTOOLS_HOSTSTATS` | on | `off` `0` `false` `no` |
| Network | `network` | `ISUTOOLS_NETSTATS` | on | `off` `0` `false` `no` `disabled` |
| DB Pool | `dbpool` | `ISUTOOLS_DBPOOL` | on | `off` `0` `false` `no` `disabled` |
| Query Plans | `queryplan` | `ISUTOOLS_EXPLAIN` | **off** | 有効化する値は `1` `on` `true` `yes` `enabled` |

`ISUTOOLS_HOSTSTATS` だけは `disabled` を無効化として解釈しません(残り3つは解釈します)。
collector を登録するかどうかの判定はプロセス起動時に一度だけ行われるため、
実行中に環境変数を書き換えても切り替わりません。

off にした collector と、前提を満たさない collector は run coordinator へ
**登録されません**。登録されない collector は phase 予算を消費せず境界記録にも現れないため、
「その collector を外して測った実行」との比較(ABBA)がそのまま成立します。
理由は必ず Collector Health に残ります。

### SQL 行効率(sqlrows)

`performance_schema.events_statements_summary_by_digest` を run の両端で読み、
その差分を rows examined / rows sent として出します。前提は3つです。

1. `performance_schema = ON`。MySQL の起動時オプションなので `my.cnf` で指定します
2. `statements_digest` consumer が有効

   ```sql
   UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME = 'statements_digest';
   ```

3. 計測用接続(purpose `stats`)が**既定 DB を持たない**こと。これは仮定ではなく、
   target ごとに `performance_schema.threads.PROCESSLIST_DB` を読んで確認します(§10)

前提が欠けた target は「測らない」ではなく「skip した理由付きで表示する」に倒れます。
Collector Health の `sqlrows` 行に、コードが出す文字列がそのまま並びます。

| Collector Health の行 | 原因と対処 |
|---|---|
| `sqlrows-skip: <target> (performance_schema is OFF)` | `performance_schema = ON` で再起動 |
| `sqlrows-skip: <target> (the statements_digest consumer is disabled)` | 上の `setup_consumers` を有効化 |
| `sqlrows-skip: <target> (setup_consumers has no statements_digest row)` | この consumer を持たないサーバ |
| `sqlrows-skip: <target> (digest table is missing <列名>)` | digest 表に必須列がない(MariaDB や古い MySQL) |
| `sqlrows-skip: <target> (the inspection connection has default database "<db>": ...)` | 接続衛生が効いていない。§10 の DSN 形式を確認 |
| `sqlrows-skip: <target> (performance_schema.threads has no row for this connection, ...)` | 既定 DB の有無を確認できないので skip(fail-closed) |
| `sqlrows-target-dropped: <target> (capability probe failed: ...)` | 権限不足などで問い合わせ自体が失敗 |

sqlrows は optional collector なので、skip も失敗も run を `partial` にするだけで
`invalid` にはしません。1 target あたりの表示行は 200 件で、差分計算は全 digest に対して
行ってから切り詰めます。

**sqlrows を off にすると EXPLAIN(§11)も動きません。** EXPLAIN は sqlrows が publish した
区間統計を入力にして digest を順位付けするため、区間が無ければ health 通知も出さずに
何もしません。

### Host 資源(hoststats)

Linux の procfs / sysfs / cgroup v2 だけを読みます。追加の外部プロセスはありません。

- 非 Linux、あるいは `/proc/meminfo` を stat できないホストでは collector が
  **登録されず**、Collector Health の `hoststats` が disabled になります。
  理由は `hoststats: unsupported OS or missing procfs: GOOS=darwin` の形です
- フラグで切った場合の理由は `ISUTOOLS_HOSTSTATS is off`
- 読めなかったソースは1つずつ落ちます。`not-captured:psi` などの code が付き、
  health には `hoststats-source-skipped: sources not captured: psi` が出ます
  (PSI は kernel が `/proc/pressure` を出す構成でのみ取れます)
- ディスク使用量の statfs 対象は `/` だけです。v1.2 の配線ではデータディレクトリを
  第二の statfs 対象として渡していないため、DB のデータ volume だけが埋まる事象は
  この欄からは見えません
- block device の区間カウンタがすべて不変なら disk 表から省略します。名前で
  `loop*` / `ram*` を除外しないため、実際に動いた仮想 backing device は残ります。
  区間中に出現した device と counter rewind のある device も診断証拠として残ります
- cgroup 上限は **v2 のみ**です。v1 のホストでは
  `hoststats-cgroup-v1: cgroup v2 is not available; cgroup limits were skipped`

cgroup は「どの cgroup を読んだか」が値と同じくらい重要なので、scope を必ず併記します。

```bash
export ISUTOOLS_CGROUP_PATH=system.slice/mysql.service  # cgroup2 マウント root からの相対パス
export ISUTOOLS_CGROUP_SCOPE=host                       # 初期 cgroup namespace にいるという宣言
export ISUTOOLS_ROLE=db                                 # 表示用の自由記述ラベル
```

`ISUTOOLS_CGROUP_PATH` を指定して解決に失敗した場合、agent 自身の cgroup へ
fallback せず cgroup 全体を skip します(fail-closed)。理由は
`hoststats-cgroup-path-rejected: ISUTOOLS_CGROUP_PATH rejected: <code>` で、code は
`absolute` / `dotdot` / `invalid` / `not-found` / `eval-failed` / `escapes-mount` /
`unreadable` / `no-mount` のいずれかです。scope は
`configured-cgroup` / `host` / `visible-root` / `agent-cgroup` の4種で、
`host` は `ISUTOOLS_CGROUP_SCOPE=host` を明示したときにしか付きません
(cgroup namespace の中からは自分がホストかどうか判定できないため、推測しません)。

`ISUTOOLS_ROLE` は identity 欄に表示されるだけのラベルで、これで分岐する処理は
ありません。**複数ホストの集約(計画10)は未実装です。** v1.2 は単一ホスト構成のみで、
peer プロトコルも hub もありません。ラベルは、ホストごとに別々の管理サーバを開いて
見比べるときの目印として使ってください。

### Network(netstats)

section key は `network` です(パッケージ名は `netstats`)。Linux 専用で、
非 Linux では disabled になり、理由は `/proc/net is only available on Linux` です。
フラグで切った場合は `ISUTOOLS_NETSTATS is off`。

- TCP 要約(`/proc/net/sockstat`、`sockstat6`)は終了境界の**点観測**です。
  gauge なので差分は取りません。TIME_WAIT は方向もローカルポート所有も区別しないので、
  これ単体で ephemeral port 枯渇の証拠にはなりません
- NIC の bytes / packets / errors / drops は**区間デルタ**。loopback(`lo`)は常に除外します
  (プロセス間通信なので、表を占有するだけでネットワークを説明しません)
- speed と MTU は `/sys/class/net/<if>/` の値をそのまま表示します。判定も推奨文言も
  付けません(経路全体が同じ MTU に合意していることは1枚の NIC からは分かりません)
- health key は `netstats-sysfs-unreadable` / `netstats-link-changed` /
  `netstats-counter-rewind` / `netstats-proc-unreadable` の4つ。ファイルが「無い」ことは
  報告しません(仮想 NIC では正常なため)

netstats の値を閾値にする advisor check は意図的にありません。

### DB Pool(dbpool)

`database/sql` のプール統計(`(*sql.DB).Stats`)を run の両端で取ります。
唯一の前提は「**登録済みの TargetID で `WatchDBPool` を呼ぶこと**」です。

```go
db, err := sql.Open(isutools.SQLDriverName("mysql"), dsn)
if err != nil {
    return err
}
if err := isutools.WatchDBPool("app", db); err != nil {
    log.Printf("isutools: db pool not watched: %v", err) // 計測欠損。アプリは止めない
}
```

ID は byte 単位で比較され、大文字小文字の畳み込みも trim も正規化もしません。
`WatchDBPool` は target を作りません(typo で作られた第二の target が、
1つの DB をレポート上の2行に割ってしまうため)。未登録 ID を渡すと
`isutools: unknown target id: "..." — register it with RegisterDBTarget, or look the id up with sqlstats.TargetIDForDSN`
が返ります。自動採番の ID は末尾26文字が hash なので手では書けません。
`sqlstats.TargetIDForDSN(driverName, dsn)` で引くか、`RegisterDBTarget` で明示的に
名前を付けてください(§10)。

watch できるプールは 16 個(`sqlstats.MaxTargets` と同数)までです。
引数の検査は `ISUTOOLS=off` でも実行されるので、配線ミスは本番構成でも表面化します。

一度も watch していない run では DB Pool section 自体が作られません。その理由は
Collector Health の `dbpool` 行に info として出ます。

```text
dbpool-not-registered: WatchDBPool was not called before the run started
```

run の途中で watch したプールは**次の run から**計測されます(開始境界より後の baseline を
渡すと、区間の一部を区間全体として報告してしまうため)。この場合は
`dbpool-registered-mid-run`、run 中に `UnwatchDBPool` した場合は最後のサンプルを取ってから
外し `dbpool-unwatched-mid-run` を残します。どちらも degraded として扱われるため、
レポートの `meta.partial` が立ちます(未 watch を告げる `dbpool-not-registered` だけは
info 扱いで、`partial` にはしません)。

数値のしきい値判定は v1.2 では意図的に入れていません(`WaitDuration` は全 goroutine の
待ち時間の総和なので、区間の実時間と比べても意味を持たないため)。

## 10. DB target と用途別 credential

### TargetID が何を揃えているか

TargetID は「論理的な1つのデータベース」に付く安定 ID で、SQL 集計・行効率・
プール統計・実行計画がすべてこのキーで join します。ID は自動採番
(読みやすい alias + 26文字の base32 hash)か、`RegisterDBTarget` による明示指定です。
ID の規則は 1〜64 byte、`[A-Za-z0-9._-]` のみ、byte 単位比較。登録上限は 16 target です。

1 target は複数の credential(purpose)を持てます。

| purpose | 用途 | 未登録時 |
|---|---|---|
| `app` | アプリ自身のトラフィック接続。`Display` / `Schema` / DSN 属性の唯一の出所 | 必ず存在する(明示登録か、proxy driver が観測して自動登録) |
| `stats` | `SHOW STATUS` / `SHOW VARIABLES` / performance_schema の読み取り | `app` credential へ fallback(接続衛生は同じ規則で適用) |
| `explain` | EXPLAIN 専用の最小権限接続 | **fallback しない**。`ErrPurposeNotRegistered` で skip |

`explain` が fallback しないのは仕様です。DML 権限を持つ credential へ暗黙に降格したら、
最小権限ユーザーを用意した意味がなくなります(§11)。

### なぜ stats / explain 接続は既定 DB を持たないのか

performance_schema は isutools 自身の文も同じように記録します。既定 DB を持つ接続から
発行した文は計測対象 schema の digest として記録され、次の run の区間に混ざります。
そこで registry は stats / explain 用の DSN を**既定 DB を外して組み直して**から開きます。
既定 DB のない接続の文は `SCHEMA_NAME IS NULL` に落ちるため、計測対象 schema の行とは
構造的に一致しません。schema 名は `WHERE SCHEMA_NAME = ?` の bind 値として渡します
(`DATABASE()` は使いません。使えば NULL になるうえ、上の保証が崩れます)。

同時に次のパラメータも上書きされます。

| パラメータ | 値 | 理由 |
|---|---|---|
| (既定 DB) | 削除 | 上記 |
| `multiStatements` | `false` | 検査接続に文のバッチを載せない |
| `interpolateParams` | `false` | schema 名は bind 値のまま渡す |
| `parseTime` | `true` | サーバ側 timestamp を `time.Time` で受ける |
| `loc` | `UTC` | セッションは `SET time_zone = '+00:00'` で UTC に固定 |
| `timeout` | `1s` | 検査がベンチを止めない |
| `readTimeout` / `writeTimeout` | `2s` | 同上 |

検査用接続は (target, purpose) ごとに1本だけ開き、30秒アイドルで閉じます。

この組み直しは **go-sql-driver/mysql 形式の DSN**
(`user:password@tcp(host:port)/dbname?params`)でしか行えません。URL 形式
(`postgres://...`)の DSN はそのまま driver へ渡るため既定 DB を外せず、
`RegisterDBInspector` は登録時点で
`isutools: dsn form does not support inspector connection hygiene` を返して拒否します。
「何時間か後に静かに汚染された計測」より「起動時に見えるエラー」の方がましだからです。
`app` credential はこの制約を受けません(アプリの DSN はアプリが決めるものです)。

### 登録順序

```go
import (
    "database/sql"
    "log"
    "os"

    _ "github.com/go-sql-driver/mysql"
    "github.com/ekusiadadus/isutools"
    "github.com/ekusiadadus/isutools/sqlstats"
)

const appDSN = "isuconp:isuconp@tcp(127.0.0.1:3306)/isuconp?parseTime=true"

func setup() (*sql.DB, error) {
    // 1. driver は database/sql へ登録済みであること(blank import)
    // 2. RegisterDBTarget は SQLDriverName より前に呼ぶ
    if err := isutools.RegisterDBTarget("app", "mysql", appDSN); err != nil {
        log.Printf("isutools: db target: %v", err)
    }

    // 3. 用途別 credential(任意)。app と同じ driver / network / アドレスであること
    if err := isutools.RegisterDBInspector("app", sqlstats.PurposeStats, "mysql",
        "isutools_stats:"+os.Getenv("STATS_PASSWORD")+"@tcp(127.0.0.1:3306)/"); err != nil {
        log.Printf("isutools: stats credential: %v", err)
    }

    // 4. アプリ接続
    db, err := sql.Open(isutools.SQLDriverName("mysql"), appDSN)
    if err != nil {
        return nil, err
    }
    // 5. プール統計
    if err := isutools.WatchDBPool("app", db); err != nil {
        log.Printf("isutools: db pool: %v", err)
    }
    return db, nil
}
```

順序の理由:

- **`RegisterDBTarget` は `SQLDriverName` より前**。proxy driver は最初の接続で DSN を
  観測して自動採番 ID で登録するので、後から明示登録しようとすると
  `isutools: target id already in use: this database was already auto-registered as "<id>"; call RegisterDBTarget before SQLDriverName`
  で失敗します
- **driver は `RegisterDBTarget` より前**。`database/sql` に未登録の driver 名を渡すと
  `isutools: driver is not registered` になります。driver 名は素の名前
  (`mysql`)でも計測用の `mysql:isutools` でも構いません(suffix は剥がされます)
- **`RegisterDBInspector` は対象 target の登録後**。未登録 ID なら
  `isutools: unknown target id`、既に同じ purpose があれば
  `isutools: purpose already registered`、driver / network / アドレスが `app` credential と
  違えば `isutools: inspector credential points at a different target` です
  (既定 DB の違いは許容されます。検査接続はそれを外すので)

`ISUTOOLS=off` のときは `RegisterDBTarget` / `RegisterDBInspector` は何もせず nil を返します。
これらの関数は登録に失敗しても error を返すだけでアプリを止めません。log には残し、
計測の欠損として Collector Health を確認してください。

登録済み target は `sqlstats.Targets()`(ID・driver・Display・schema・purpose 一覧)、
`sqlstats.Target(id)`、`sqlstats.TargetIDForDSN(driver, dsn)` で確認できます。
`Display` は DSN の allowlist されたフィールドから組み直した文字列なので、
パスワードや未知のパラメータは決して含みません。

## 11. EXPLAIN 取得(計画09)

**この節は安全性の話です。既定は off で、専用の最小権限 MySQL ユーザーを必須にしています。**

### なぜ「専用ユーザー推奨」ではなく必須なのか

`EXPLAIN SELECT ...` は読み取りだけの操作に見えますが、そうとは限りません。
対象の SELECT が `SQL SECURITY DEFINER` のストアド関数を呼んでいれば、
EXPLAIN 経由でその関数に到達し、定義者権限で書き込みが起きる可能性があります。
これを構造的に潰す方法は1つだけで、**EXECUTE も DML も持たない credential で実行すること**です。

そこで isutools は、EXPLAIN を実行する当の接続の上で実効権限を確認し、
「持っていないことを積極的に確認できた」場合にだけ EXPLAIN を発行します。
確認できなければその target は skip します(fail-closed)。
アプリの credential や stats credential へ fallback することは一切ありません。

### 有効化

```bash
export ISUTOOLS_EXPLAIN=1          # 既定 off。1 / on / true / yes / enabled で有効
export ISUTOOLS_EXPLAIN_TOP=10     # 1 target あたりの digest 数。既定 10、上限 200
```

credential の登録は2通りです。

```go
// 汎用: target ごとに登録する
isutools.RegisterDBInspector("app", sqlstats.PurposeExplain, "mysql",
    "isutools_explain:"+os.Getenv("EXPLAIN_PASSWORD")+"@tcp(127.0.0.1:3306)/")
```

```bash
# 単一 target 限定のショートカット
export ISUTOOLS_EXPLAIN_DSN='isutools_explain:...@tcp(127.0.0.1:3306)/'
export ISUTOOLS_EXPLAIN_DRIVER=mysql   # 省略時 mysql
```

`ISUTOOLS_EXPLAIN_DSN` は**登録済み target がちょうど1つのときだけ**有効です。
2つ以上あるとどちらの DB の credential か決められないため、推測せずに拒否し、
Collector Health の `queryplan-credential` に次を残します。

```text
ISUTOOLS_EXPLAIN_DSN は登録済み target がちょうど 1 つのときだけ有効です(現在 2 個)。target ごとに RegisterDBInspector(id, PurposeExplain, ...) を呼んでください
```

### GRANT(role 経由では動きません)

計測対象 schema を `isuconp` とした場合の最小構成です。**必ず直接 GRANT してください。**

```sql
CREATE USER 'isutools_explain'@'127.0.0.1' IDENTIFIED BY '<password>';

GRANT SELECT ON `isuconp`.* TO 'isutools_explain'@'127.0.0.1';
GRANT SELECT ON `performance_schema`.* TO 'isutools_explain'@'127.0.0.1';
GRANT UPDATE ON `performance_schema`.`threads` TO 'isutools_explain'@'127.0.0.1';
```

3行目は、EXPLAIN セッション自身を非計装にするためのものです。これを与えたくない場合は
`setup_actors` でこのユーザーの新規セッションを計装対象から外しても構いません。
どちらの方法でも、isutools は
`SELECT INSTRUMENTED FROM performance_schema.threads WHERE PROCESSLIST_ID = CONNECTION_ID()`
が `NO` を返すことを毎回確認し、確認できなければ target を skip します
(計装されたままだと、EXPLAIN や `USE` が計測対象 schema の digest として次の run に混ざります)。

**role で与えた権限は使えません。** セッションは冒頭で `SET ROLE NONE` を実行し、
`SELECT CURRENT_ROLE()` を読み返して literal `NONE` が返ることを確認します。
つまり role 経由の権限はセッション中ずっと非活性です。この状態で
performance_schema すら読めなければ session 確立段階で target ごと skip され
(`explain-session-instrumented` あるいは `explain-query-error`)、対象 schema の
テーブルだけが role 経由なら digest ごとに `permission_denied`(errno 1142 / 1143)になります。
どちらにせよ実行計画は1件も得られません。

role を付けたまま放置することも見逃されません。付与されている role は
`SHOW GRANTS FOR CURRENT_USER() USING ...` で展開され、同じ allowlist で判定されます。
現在非活性でも、DML や EXECUTE を持つ role が付いていれば target は skip です
(接続プールの設定が1つ変われば明日には活性になるため)。role の入れ子は展開が
閉じるまで最大4周し、閉じなければ「検証できない」として skip します。

allowlist は閉じています。

| 許可 | 不許可 |
|---|---|
| `USAGE` | `INSERT` / `DELETE` / `EXECUTE` / `ALL PRIVILEGES` / 動的 `*_ADMIN` / `PROXY` |
| 計測対象 schema への `SELECT` | `*.*` への `SELECT`。データベース名に `%` を含む GRANT も同じ扱い |
| `performance_schema` への `SELECT` | 上記2つ以外の schema への `SELECT`(`information_schema` への明示 GRANT も該当) |
| `performance_schema`.`threads` への `UPDATE` | それ以外の `UPDATE` |
| role のメンバーシップ行(権限は展開して判定) | `WITH GRANT OPTION` |
| — | `FUNCTION` / `PROCEDURE` への GRANT |

`SHOW GRANTS` が1行も返さない場合も skip します。MySQL の全アカウントは最低でも
`GRANT USAGE ON *.*` を持つので、空の出力は「権限が無い」ではなく「読めなかった」だからです。
解釈できない行(`REVOKE` を含む部分剥奪など)が1行でもあれば、その時点で検証不能として扱います。

### skip されたときに出る文字列

target が skip されると、Collector Health の `queryplan` 行に安定 ID 付きで理由が出ます。

| code | 表示される理由 |
|---|---|
| `explain-purpose-unregistered` | EXPLAIN 専用 credential が未登録です — RegisterDBInspector(id, PurposeExplain, ...) で登録してください(app / stats の credential へは fallback しません) |
| `explain-grants-too-broad` | EXPLAIN 用ユーザーの実効権限が広すぎます(対象 schema と performance_schema への SELECT 以外を持っています) |
| `explain-grants-unverifiable` | SHOW GRANTS を読めない、または解釈できない行があったため権限を検証できませんでした |
| `explain-roles-active` | SET ROLE NONE が効かず、有効な role を特定できませんでした |
| `explain-session-instrumented` | セッションを非計装にできませんでした(performance_schema.threads の UPDATE 権限か setup_actors の設定が必要です) |
| `explain-unsupported` | QUERY_SAMPLE_TEXT 列がありません — EXPLAIN 自動化は MySQL 8.0.17 以降のみ対応です |
| `explain-no-schema` | schema 名が空、または識別子として使えない文字を含みます |
| `explain-unknown-target` | この target ID は registry に登録されていません |
| `explain-budget-exhausted` | enrich の予算が尽きたため、この target は実行しませんでした |
| `explain-target-timeout` | enrich の予算内に EXPLAIN セッションが返らなかったため、この target の待機を打ち切りました(driver が context を無視している可能性があります) |
| `explain-query-error` | EXPLAIN セッションの文が失敗しました |
| `explain-no-default-database` | 既定 DB が設定されておらず EXPLAIN が「No database selected」で失敗しました(USE が効いていません) |

`explain-no-interval`(区間統計が使えない)と `explain-no-digests`(区間内に対象 SELECT が無い)は
health 通知を出しません。前者は sqlrows 側で既に報告済み、後者は正常な状態だからです。

### サーバ要件

`performance_schema.events_statements_summary_by_digest.QUERY_SAMPLE_TEXT` が必須です。
これは **MySQL 8.0.17 以降**にしかありません。MySQL 5.7 と MariaDB は
`explain-unsupported` として skip されます。proxy driver は文と引数を分けて受け取るため
isutools 側に生 SQL は存在せず、EXPLAIN にかけられるのはサーバが記録したこのサンプルだけです。

サンプル文字列はリテラルを含むため、読み出したコールバックの外へ出しません。
`Plan.Query` は sqlrows が publish した正規化済みの `DIGEST_TEXT` で、失敗した EXPLAIN は
分類(`permission_denied`、`syntax_or_truncated` など)と errno / SQLSTATE だけに落とされます。
driver のメッセージは保存もログ出力もしません(MySQL の 1064 は文の断片を引用するため)。
長さが `performance_schema_max_sql_text_length` に達しているサンプルは、途中で切れている
可能性があるためサーバへ送らず `sample_possibly_truncated` として残します。

### いつ実行されるか

EXPLAIN は **run につき1回、終了境界の後の enrich phase** で実行されます。
ダッシュボードを開いても再実行はしません。`POST /collect` でも実行しません
(非終端の flush には順位付けの元になる区間がないため)。
`ISUTOOLS_EXPLAIN` はプロセス起動時に一度だけ読まれるので、
有効化はアプリを起動する前に行ってください。

対象は区間内の **SELECT digest だけ**です。DML の EXPLAIN は MySQL 8 では文法上通りますが、
この credential はアプリ schema への DML 権限を持たないので必ず `permission_denied` になり、
最初から選ばれません。

予算は enrich 全体で 2 秒、1 target あたり 1 秒、その内訳が
session 確立 300ms / サンプル一括読み出し 100ms / EXPLAIN 1本あたり 250ms です。
target は区間合計時間の大きい順に処理し、予算に収まらない target は黙って落とさず
`explain-budget-exhausted` として記録します。

判定に使うのは**計測区間の中で実行されたサンプルだけ**です。区間外のサンプルは
リテラルが違えば実行計画も変わるため、`stale` として灰色表示にし、
advisor の `plan-full-scan` / `plan-filesort` / `plan-temporary` の入力にもしません。

### 運用上の注意

- パスワードはアプリの設定ファイルではなく、EXPLAIN 専用に払い出してください。
  接続元ホストも `'user'@'127.0.0.1'` のように絞るのが安全です
- DSN がスナップショットに出ることはありません。表示名は allowlist から組み直され、
  driver の error も credential の断片を含むと判定された場合は落とされます
- ベンチ本番で有効にする前に、1回空の run を回して Collector Health の `queryplan` 行が
  skip 理由を出していないことを確認してください。権限周りの失敗はすべてここに出ます

## 12. ベンチスクリプトとの統合

### run 境界の契約

| endpoint | 効果 | 応答 |
|---|---|---|
| `POST /reset` | 世代を回して**新しい run を開始**する。実行中の run があれば preempt して破棄する | `204`、`X-Isutools-Run-Id` header。run を開けなければ `409`(fail-closed) |
| `POST /collect` | buffered アクセスログの**非終端 flush**。境界は動かさない | `204` |
| `POST /finish` | 現 run の**終了境界を固定**する。drain も snapshot 構築も待たない | `202` + 境界 JSON、`X-Isutools-Run-Id` |
| `POST /save?score=N` | 終了境界を固定し、immutable snapshot を待って ack し、html + json を保存する | `200` + `{"file": "..."}`。`ISUTOOLS_DATA_DIR` 未設定なら `400` |
| `POST /abort` | 現 run を破棄する。snapshot は作らない。冪等 | `204` |

`POST /collect` は**終端ではありません**。終了境界が既に固定された後に呼ぶと `409` と
`Retry-After: 1` を返します。

```text
the run's boundary is already fixed; POST /reset before collecting again
```

境界の後に flush すると、次の世代に属するバイト列を測り終えた区間へ足し込むことになるため、
拒否は意図的な fail-closed です。**flush は必ず `/finish` / `/save` より前に**行ってください。

`/reset` `/collect` `/finish` `/abort` `/save` は同時に1つしか走りません。
競合したときの応答は `409` と次の本文です。

```text
another reset, collect, or save is already running
```

典型的な1ブロックはこうなります。

```bash
ADMIN=http://127.0.0.1:19191

run_id=$(curl -fsS -X POST -D - -o /dev/null "$ADMIN/reset" \
  | awk -F': ' 'tolower($1) == "x-isutools-run-id" {print $2}' | tr -d '\r')
echo "isutools run: $run_id"              # 計測結果に付ける run のラベル

score=$(./bench.sh)                       # ベンチ本体
curl -fsS -X POST "$ADMIN/collect"        # 終了境界の前に flush する
curl -fsS -X POST "$ADMIN/save?score=$score"
```

計測結果を捨ててよい試走なら `/abort`、レポートは要らないが停止時刻を厳密に決めたい場合は
`/finish` を使います。`/reset` は**前の run を破棄する**ので、結果が必要な run は必ず
`/finish` か `/save` で閉じてください。

### initialize handler での `ResetNow`

ベンチマーカーが `POST /initialize` を叩く構成では、境界はアプリ側で置くのが正確です。
`ResetNow` は実行中の run を preempt するので、**最後の initialize が決定的に勝ちます**。

```go
func postInitialize(w http.ResponseWriter, r *http.Request) {
    err := isutools.SerializeInitialize(r.Context(), func(ctx context.Context) error {
        if err := rebuildDatabase(ctx); err != nil {
            return err
        }
        // 応答を返す前に呼ぶ。ベンチは応答を見た瞬間に負荷をかけ始める
        start, err := isutools.ResetNow(ctx)
        if err != nil {
            return err
        }
        if start.Validity == isutools.ValidityInvalid {
            return fmt.Errorf("isutools: run %s is invalid", start.RunID)
        }
        return nil
    })
    if err != nil {
        http.Error(w, "initialize failed", http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusOK)
}
```

守るべき点は3つです。

1. **応答を書く前に呼ぶ。** 応答の後に境界を置くと、run の最初の数秒が黙って欠けます
2. **失敗は失敗として扱う。** error か `ValidityInvalid` が返ったら 500 を返してください。
   汚染されていると分かっている run を、それらしい数字として出す方が有害です
3. **handler 全体を `SerializeInitialize` で包む。** 境界を置くだけでは、
   その後に走る2回目の initialize が DB を作り直して run を汚すのを防げません

`SerializeInitialize` の外で initialize 用の reset を取ると、run は成立しますが
Collector Health に degraded として次が残ります。

```text
initialize reset taken outside SerializeInitialize; a concurrent rebuild may have polluted this run
```

guard の獲得は 30 秒で諦め、`isutools.ErrInitializeBusy` を返します。guard は
プロセス内でのみ有効で、プロセスやホストをまたいだ直列化はできません。
`SerializeInitialize` は `ISUTOOLS=off` でも動き続けます(計測フラグを切ったせいで
アプリの直列化まで消えるのは事故なので)。

initialize がリトライされる構成では `ResetNowWithNonce(ctx, nonce)` を使います。
同じ nonce の再呼び出しは新しい run を開かず、最初の `StartResult` をそのまま返します。

`ISUTOOLS=off` のとき `ResetNow` はゼロ値の `StartResult` と nil error を返すので、
initialize handler 側に build tag も分岐も要りません。

### ベンチスクリプトと initialize の併用

`POST /reset` とアプリ側 `ResetNow` は併用できます。どちらも実行中の run を preempt するので、
**後に呼ばれた方が勝ちます**。スクリプトが `/reset` を叩いた後にベンチが `POST /initialize` を
送る構成なら、最終的な境界は `ResetNow` が置いたものになり、`/reset` が開いた run は
破棄されます。どちらか一方に統一するのが分かりやすいですが、併用しても run が
二重に残ることはありません。

run 境界はこのプロセスの中だけの概念です。**複数ホストの同時境界(計画10)は未実装**なので、
アプリと DB が別ホストの構成では、ホストごとに `/reset` を叩き、
ホストごとのレポートを見比べてください。
