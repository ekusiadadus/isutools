# isutools

[![Go Reference](https://pkg.go.dev/badge/github.com/ekusiadadus/isutools.svg)](https://pkg.go.dev/github.com/ekusiadadus/isutools)
[![CI](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml/badge.svg)](https://github.com/ekusiadadus/isutools/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](./LICENSE)

ISUCON 向けオールインワン計測モジュール。**アプリの変更1行**で SQL / HTTP /
nginx アクセスログ / プロセス・CPU / DB スキーマ / pprof を計測し、
**ソート済みダッシュボード**と**自己完結スナップショット**で振り返る。

*All-in-one profiling for ISUCON-style tuning: change one line, get SQL /
HTTP / access-log / process / schema / pprof dashboards with per-benchmark
history and diffs. Zero measured overhead (ABBA-verified).*

ベンチ毎の履歴がスコア・git rev 付きで並び、行クリックで当時の全計測が開く:

![isutools dashboard: per-benchmark run history with scores and git revisions](docs/images/dashboard-runs.png)

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

## エンドポイント(管理サーバ)

| ルート | 内容 |
|---|---|
| `GET /` | **実行一覧**(日時 JST・gen・rev・score)。行クリックで詳細へ |
| `GET /<run-id>` | 保存済み実行の詳細(`20260804-002143` 形式の日時 ID) |
| `GET /live` | 現在計測中のライブレポート(合計時間降順ソート済み) |
| `GET /snapshot.html` | 自己完結 HTML をダウンロード(手元でダブルクリック閲覧) |
| `GET /json` | 機械可読スナップショット(`prev` = 前世代付き) |
| `GET /pprof/` | net/http/pprof(アプリプロセスのプロファイル) |
| `GET /diff?a=<id>&b=<id>` | **2つの実行の差分**(クエリ/パス毎の合計時間変化、改善/悪化を色分け) |
| `POST /reset` | 世代リセット(ベンチ前に叩く)。CPU プロファイル自動採取も開始 |
| `POST /collect` | buffered nginx log を期限付きで flush 待ち・回収 |
| `POST /save?score=N` | 現世代を html+json で atomic 保存(score は meta とファイル名に記録) |
| `GET /files/<name>` | 保存済み html / json / pprof の取得 |

## レポートに出るもの

- **meta**: 時刻(**常に JST**)・git rev(+dirty)・世代番号・score・
  **ホスト情報(CPU モデル / コア数 / メモリ GB / OS)**
- **Collector Health**: 各コレクターの状態・欠損(`partial`)警告
- **DB Schema**: 世代開始時点のテーブル・行数・**インデックス一覧**(「実行前に何が
  貼ってあったか」の証跡)
- **SQL**: 正規化クエリ別 total/count/errors/avg/p95/max(文字列・数値リテラルは `?` にマスク)
- **HTTP**: アプリ視点のリクエスト別レイテンシ・バイト数
- **nginx Access Log**: alp 相当(reqtime/upstime 分離・bytes・cache・304 等)
- **Processes**: ベンチ区間のプロセス別 CPU/RSS(top 互換 1core=100%)+
  **CPU total: N% busy(user/sys/iowait/idle)** — ハードを使い切れているかが一目で分かる
- **User Flow**: セッション毎のページ遷移 上位20(nginx ログの `sess:` フィールドから。
  「ユーザーがどうアプリを使っているか」の実測。k6 シナリオの検証にも)
- **Counters**: `isutools.Count("cache_hit")` によるアプリ内カウンタ(世代毎リセット)
- **Advisor**: ISUCON 定石で未設定のもの(プリペアドステートメント・gzip・buffer pool・
  カーネルパラメータ・GOMAXPROCS など)
- **Snapshots / CPU Profiles**: 過去実行・プロファイルの一覧(ダッシュボードから選択、
  各行に前回実行との diff リンク)

## 環境変数

| 変数 | 既定 | 意味 |
|---|---|---|
| `ISUTOOLS` | (on) | `off` で全機能無効(素の driver 名を返す。query path 追加処理ゼロ) |
| `ISUTOOLS_ADDR` | `127.0.0.1:19191` | 管理サーバ bind。`off` で管理サーバのみ無効(SQL 集計は継続) |
| `ISUTOOLS_TOKEN` | — | 非 loopback bind の Bearer / `?token=`+Cookie 認証(外部公開時は必須) |
| `ISUTOOLS_ALLOW_UNAUTHENTICATED` | — | `1` で非 loopback でも無認証。**SSH トンネル + `127.0.0.1` 限定 publish の Docker 構成専用**(warning + health degraded を常時表示) |
| `ISUTOOLS_DATA_DIR` | — | スナップショット / プロファイルの永続化先(実行一覧の実体) |
| `ISUTOOLS_NGINX_LOG` | — | nginx ログのパス。**LTSV / JSON 行を自動判別**(alp キー互換) |
| `ISUTOOLS_PPROF_SECONDS` | 0 | reset 後に CPU プロファイルを N 秒自動採取(ベンチ区間を丸ごと採る) |
| `ISUTOOLS_GIT_HASH` / `_DIRTY` | — | Docker ビルドで vcs 情報が埋まらない場合の rev 注入 |
| `ISUTOOLS_PATH_RULES` | — | HTTP パス正規化ルール(`regex=replacement;...` 各ペアは最後の `=` で分割) |
| `ISUTOOLS_NGINX_CONF` | — | advisor が検査する nginx conf(ファイル or ディレクトリ) |

## nginx 設定(アクセスログ計測)

[examples/nginx-isutools.conf](./examples/nginx-isutools.conf) の LTSV 形式を推奨。
URI グルーピングは nginx の `map` で行い、集計キーを `uri:$uri_group`、生パスを
`rawuri:$uri` として両方残す方式が便利(パーサは未知キーを無視する)。
JSON 形式(`log_format ... escape=json '{...}'`)も同じキー名、または
**alp のデフォルトキー(`body_bytes` / `response_time`)**でそのまま読める。

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

閲覧経路は **SSH トンネル前提**。loopback bind は無認証、非 loopback は
token 必須(fail closed)、`ALLOW_UNAUTHENTICATED=1` は「プロセスの外側で
到達性が制限されている構成」だけの明示 opt-in。生の ISUCON サーバでは必ず
`ISUTOOLS_TOKEN` を設定する。完全なマトリクスと根拠は DESIGN.md 4章。
`isutools.Handler()` を自前 router に mount する場合のアクセス制御は呼び出し側の責任。

## シナリオ負荷試験(k6)との連携

負荷生成は [k6](https://k6.io) をそのまま使う(再実装しない)。
[examples/k6-private-isu.js](./examples/k6-private-isu.js) にログイン →
タイムライン → 投稿詳細 → 作者ページのシナリオ例がある。`POST /reset` →
k6 実行 → `POST /save` で、サーバ側から見たシナリオの SQL/HTTP/User Flow が
ダッシュボードに揃う。

## オーバーヘッド検証(ABBA)

[examples/abba.sh](./examples/abba.sh) が off→on→on→off の4連ベンチで
計測オーバーヘッドを算出する(リリース基準 < 2%)。

## 対応状況と v1.0 ロードマップ

実装済み: SQL(MySQL/MariaDB/PostgreSQL)/ HTTP(h1/h2)/ nginx ログ
(LTSV+JSON, ローテ追随)/ procstats + **CPU total** / dbinspect(MySQL)/
pprof(エンドポイント + 自動採取)/ snapshot-first ダッシュボード(実行一覧・
日時 ID 詳細・ライブ)/ JST 固定 / スコア記録 / collector health / 世代管理。

v1.0 までのギャップ(パス正規化ルール注入・スナップショット差分ビュー・
汎用カウンタ・WebSocket/SSE 接続分離・gqlstats ほか)は
DESIGN.md「**v1.0 ギャップと優先順位**」を参照。
