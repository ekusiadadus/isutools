# isutools

[![Go Reference](https://pkg.go.dev/badge/github.com/ekusiadadus/isutools.svg)](https://pkg.go.dev/github.com/ekusiadadus/isutools)
[![CI](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml/badge.svg)](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)

ISUCON 向けオールインワン計測モジュール。**アプリの変更1行**で SQL / HTTP /
nginx アクセスログ / プロセス・CPU / DB スキーマ / pprof を計測し、
**ソート済みダッシュボード**と**自己完結スナップショット**で振り返る。

*All-in-one profiling for ISUCON-style tuning: add SQL / HTTP / access-log /
process / schema / pprof dashboards with per-benchmark history and diffs.
Includes a reproducible ABBA overhead gate.*

ベンチ毎の履歴がスコア・git rev 付きで並び、行クリックで当時の全計測が開く:

![isutools dashboard: per-benchmark run history with scores and git revisions](docs/images/dashboard-runs.png)

- 導入詳細: **[DB・nginx/Apache・pprof・事前準備](./docs/INTEGRATION.md)**
- 設計書: [DESIGN.md](./DESIGN.md) / 実装状況: [docs/IMPLEMENTATION_STATUS.md](./docs/IMPLEMENTATION_STATUS.md)
- License: MIT / Runtime: Go 1.24+
- 実績(dogfooding): private-isu を本モジュールの計測だけで1日チューニングし
  **score 0 → 541,650**(fail 0)。
  [全記録はブログ記事に](https://ekusiadadus.com/ja/blog/private-isu-500k-with-isutools)

## クイックスタート

```go
import "github.com/ekusiadadus/isutools"

db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // 既存の sqlx.Open を書き換えるだけ

// HTTP も測るなら既存 Handler を1回包む
http.ListenAndServe(":8080", isutools.HTTP(handler))
```

対象 driver はこの呼び出しより前に blank import 等で `database/sql` へ登録して
おく。MySQL / MariaDB / PostgreSQL(database/sql 経由)すべて同じ1行。
登録成功時に管理サーバが `127.0.0.1:19191` へ一度だけ起動する。
`SQLDriverName` は登録に失敗しても素の driver 名へ **fail-open** し、欠損は
`meta.partial` / `meta.health` に必ず残す(アプリの起動を壊さない)。
driver ごとの import・DSN・pgxpool の制約は
[統合ガイド §2](./docs/INTEGRATION.md#2-db-ドライバへの接続)を参照。

## エンドポイント(管理サーバ)

| ルート | 内容 |
|---|---|
| `GET /` | **実行一覧**(日時 JST・gen・rev・score)。行クリックで詳細へ |
| `GET /<run-id>` | 保存済み実行の詳細(旧秒精度 ID と衝突防止付き高精度 ID) |
| `GET /live` | 現在計測中のライブレポート(合計時間降順ソート済み) |
| `GET /snapshot.html` | 自己完結 HTML をダウンロード(手元でダブルクリック閲覧) |
| `GET /json` | 機械可読スナップショット(`prev` = 前世代付き) |
| `GET /pprof/` | net/http/pprof(アプリプロセスのプロファイル) |
| `GET /diff?a=<id>&b=<id>` | **2つの実行の差分**(total/count/avg。件数差がある行は改善と断定しない) |
| `POST /reset` | 世代リセット(ベンチ前に叩く)。CPU プロファイル自動採取も開始 |
| `POST /collect` | buffered nginx log を期限付きで flush 待ち・回収 |
| `POST /save?score=N` | 現世代を上限付きで html+json staging 保存(HTML は JSON 公開後に一覧へ出る) |
| `GET /files/<name>` | 保存済み html / json / pprof の取得 |

## レポートに出るもの

- **meta**: 時刻(**常に JST**)・git rev(+dirty)・世代番号・score・
  **ホスト情報(CPU モデル / コア数 / メモリ GB / OS)**
- **Collector Health**: 各コレクターの状態・欠損(`partial`)警告
- **DB Schema**: 世代開始時点のテーブル・行数・**インデックス一覧**(「実行前に何が
  貼ってあったか」の証跡)
- **SQL**: 正規化クエリ別 total/count/errors/avg/p95/max(文字列・数値リテラルは `?` にマスク)
- **HTTP**: アプリ視点のリクエスト別レイテンシ・バイト数
- **Proxy Access Log**: nginx/Caddy/明示JSON(reqtime/upstime 分離・bytes・cache・304 等)
- **Processes**: ベンチ区間のプロセス別 CPU/RSS(top 互換 1core=100%)+
  **CPU total: N% busy(user/sys/iowait/idle)** — ハードを使い切れているかが一目で分かる
- **User Flow**: セッション毎のページ遷移 上位20(proxy ログの `sess:` フィールドから。
  「ユーザーがどうアプリを使っているか」の実測。k6 シナリオの検証にも)
- **Scenario Stories**: 安全な`scenario`ラベル + 疑似`sess`ごとのrequest列を、
  シナリオ別の上位ユーザーストーリーとして集計(GA4風flowの最小基盤)
- **Counters**: `isutools.Count("cache_hit")` によるアプリ内カウンタ(世代毎リセット)
- **Advisor**: ISUCON 定石で未設定のものに加え、HTTP/3/QUIC移行readiness
  (server/TLS/Alt-Svc/fallback/UDP/edge/実測protocol/再送・drop evidence)
- **Snapshots / CPU Profiles**: 過去実行・プロファイルの一覧(ダッシュボードから選択、
  各行に前回実行との diff リンク)

## 環境変数

| 変数 | 既定 | 意味 |
|---|---|---|
| `ISUTOOLS` | (on) | `off` で全機能無効(素の driver 名を返す。query path 追加処理ゼロ) |
| `ISUTOOLS_ADDR` | `127.0.0.1:19191` | 管理サーバ bind。`off` で管理サーバのみ無効(SQL 集計は継続) |
| `ISUTOOLS_ALLOW_UNAUTHENTICATED` | — | `1` で非 loopback bind を明示許可。**SSH トンネル + `127.0.0.1` 限定 publish の Docker 構成専用**(security warningを表示。計測の`partial`とは分離) |
| `ISUTOOLS_DATA_DIR` | — | スナップショット / プロファイルの永続化先(実行一覧の実体) |
| `ISUTOOLS_ACCESS_LOG` | — | nginx/Caddy/Apache/Envoy ログのパス。**LTSV / JSON 行を自動判別**(対応形式は統合ガイド参照) |
| `ISUTOOLS_NGINX_LOG` | — | 旧変数名。`ACCESS_LOG` 未設定時だけ fallback |
| `ISUTOOLS_PPROF_SECONDS` | 0 | reset 後に CPU プロファイルを N 秒自動採取(ベンチ区間を丸ごと採る) |
| `ISUTOOLS_GIT_HASH` / `_DIRTY` | — | Docker ビルドで vcs 情報が埋まらない場合の rev 注入 |
| `ISUTOOLS_PATH_RULES` | — | HTTP パス正規化ルール(`regex=replacement;...` 各ペアは最後の `=` で分割) |
| `ISUTOOLS_NGINX_CONF` | — | advisor が検査する nginx conf(ファイル or ディレクトリ) |
| `ISUTOOLS_PROXY_CONF` / `_KIND` | — / auto | HTTP/3 advisor が読む nginx/Caddy/Envoy 設定。汎用名を優先、kind は `nginx` / `caddy` / `envoy` |
| `ISUTOOLS_HTTP3_UDP443` | — | 外部clientからの結果を `reachable` / `blocked` で明示。プロセス内からfirewall/NATを推測しない |
| `ISUTOOLS_HTTP3_EDGE` / `_EDGE_ENABLED` | — | LB/CDN名と、そのedgeでのHTTP/3有効状態(`true` / `false`)の明示evidence |
| `ISUTOOLS_HTTP3_QUIC_METRICS` | — | snapshot時に再読込するproxy QUIC counter JSON。再送率とUDP dropを診断 |
| `ISUTOOLS_CACHE_METRICS` | — | snapshot時に再読込するアプリ側キャッシュ counter JSON(`hits` / `misses` / `evictions`)。ヒット率とexpire前evictionを診断 |

## 追加ライブラリと事前準備

pprof は Go 標準ライブラリなので、計測側の追加 package は不要。自動 CPU profile には
writable な `ISUTOOLS_DATA_DIR`、解析時だけ `go tool pprof` が必要になる。procstats も
追加 package はないが Linux `/proc` と PID namespace の権限が必要。k6、curl、jq、
Graphviz は用途別の外部コマンドであり、isutools の runtime library ではない。
機能別の必須／任意一覧は [統合ガイド §1](./docs/INTEGRATION.md#1-必須任意の全体像)。

## nginx 設定(アクセスログ計測)

[examples/nginx-isutools.conf](./examples/nginx-isutools.conf) の LTSV 形式を推奨。
URI グルーピングは nginx の `map` で行い、集計キーを `uri:$uri_group`、生パスを
`rawuri:$uri` として両方残す方式が便利(パーサは未知キーを無視する)。
JSON 形式(`log_format ... escape=json '{...}'`)も同じキー名、または
**alp のデフォルトキー(`body_bytes` / `response_time`)**でそのまま読める。
JSON は必須ではなく、行頭 `{` なら JSON、それ以外は LTSV と自動判定する。
標準 combined log は推測しない。Apache は明示 JSON と `%D` の microseconds 変換が
必要になるため、設定例・権限・Docker volume・安全な `sess` の作り方を
[統合ガイド §4–5](./docs/INTEGRATION.md#4-nginx-アクセスログ)にまとめた。

## HTTP/3 / QUIC 移行Advisor

Advisorはnginx/Caddy/Envoy設定、LinuxのローカルUDP/443 listener、proxyアクセスログの
`proto`別件数・5xx・p95を検査する。reverse proxy終端時はアプリの`r.Proto`が
client protocolを表さないため、`ISUTOOLS_ACCESS_LOG`のclient-facing protocolを優先する。
ただし外部LB/CDN終端ではorigin logもclient-facingではないため、edge analyticsまたは
外部client実測が必要になる。
LB/CDN、外部firewall/NAT、QUIC再送counterは自動推測せず、明示evidenceがない場合は
`skip`になる。設定例、判定限界、Caddy native JSON、Envoy telemetryは
[統合ガイド §6](./docs/INTEGRATION.md#6-http3--quic-readiness-advisor)を参照。

この機能は移行可否の診断であり、isutools自身がHTTP/3 serverになる機能ではない。
`quic-go`等のruntime依存は追加されず、実listenerと外部clientでの接続試験は別途必要。

## ベンチスクリプトの型

```bash
ADMIN=http://localhost:19191
curl -X POST $ADMIN/reset                  # 世代開始(CPU プロファイル自動開始)
<ベンチ実行>
curl -X POST "$ADMIN/save?score=$score"    # 永続化 → ダッシュボード一覧に1行追加
curl $ADMIN/json | jq '.sql[:5]'           # その場で top5 確認
```

## スクリーンショット

**Advisor** — 「ISUCON の定石なのに未設定」を自動検出(プリペアドステートメント・
gzip・buffer pool・カーネルパラメータ・GOMAXPROCS)。直すと ok に変わるチェックリスト:

![isutools advisor: detects unconfigured ISUCON-critical settings](docs/images/report-advisor.png)

**diff ビュー** — 2つの実行間でクエリ/パス毎の合計時間の増減を表示。
「改善したのか、ボトルネックが移動しただけか」が一目で分かる:

![isutools diff view: per-query total-time deltas between two runs](docs/images/diff-view.png)

## セキュリティモデル(要約)

閲覧経路は **SSH トンネル前提**で、application-level token 機構は持たない。
loopback bind はそのまま利用でき、非 loopback は `ALLOW_UNAUTHENTICATED=1` を
明示し、Docker publish・firewall・SSHで到達性を制限した場合だけ許可する。
opt-inなしの非 loopback は fail closed。完全な契約と根拠は DESIGN.md 4章。
`isutools.Handler()` を自前 router に mount する場合のアクセス制御は呼び出し側の責任。

## シナリオ負荷試験(k6)との連携

負荷生成は [k6](https://k6.io) をそのまま使う(再実装しない)。
[examples/k6-private-isu.js](./examples/k6-private-isu.js) にログイン →
タイムライン → 投稿詳細 → 作者ページのシナリオ例がある。`POST /reset` →
k6 実行 → `POST /save` で、サーバ側から見たシナリオの SQL/HTTP/User Flow が
ダッシュボードに揃う。
同梱例は raw Cookie ではなく `X-Isutools-Session: k6-vu-N-iter-M` を送り、nginx 例が
その非秘密 ID だけを`sess:`に記録する。さらに
`X-Isutools-Scenario: login_and_browse`を`scenario:`へ記録すると、同じrequest列を
たどった疑似session数と上位journeyがScenario Storiesへ表示される。生Cookie、Bearer token、
email等を`sess`/`scenario`へ入れてはいけない。これらは偽装可能な計測ラベルであり、
認証・認可には使用しない。実ユーザー計測では外部headerを削除し、trusted app/proxyで
疑似IDを上書きする。詳細は
[統合ガイド §4](./docs/INTEGRATION.md#scenario-stories最小のファネルフロー基盤)。

## オーバーヘッド検証(ABBA)

[examples/abba.sh](./examples/abba.sh) が off→on→on→off の4連ベンチで
構成するブロックを最低3回実行する。同一 binary/image fingerprint、固定 warm-up、
score・p95・error rate、paired 95% CI を TSV と provenance に保存し、CI 上限で判定する。
必要な BENCH 出力形式はスクリプト先頭と[統合ガイド §7](./docs/INTEGRATION.md#7-pprofprocstats負荷生成ツール)を参照。

## v1.0 対応状況

実装済み: SQL(MySQL/MariaDB/PostgreSQL)/ HTTP(h1/h2)/ nginx ログ
(LTSV+JSON, ローテ追随)/ procstats + **CPU total** / dbinspect(MySQL)/
pprof(エンドポイント + 自動採取)/ snapshot-first ダッシュボード(実行一覧・
日時 ID 詳細・ライブ)/ JST 固定 / スコア記録 / collector health / 世代管理。

v1.0 のパス正規化、差分、counter、WebSocket/SSE 分離、User Flow、資源上限は実装済み。
Apache combined の推測 parser、gqlgen operation adapter、HTTP/3 実listener統合テストは
1.x 以降の明示的な非対応項目。現在のローカル検証範囲と、release tag 後に追加された
証跡を含む正確な状態は [実装状況](./docs/IMPLEMENTATION_STATUS.md)を参照。

---

「ISUCON」は、さくらインターネット株式会社の商標または登録商標です。本プロジェクトは [isucon.net](https://isucon.net/) の運営とは無関係の非公式ツールです。
