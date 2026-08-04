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
