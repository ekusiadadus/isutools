# isutools 設計書

`github.com/ekusiadadus/isutools` — ISUCON 向けオールインワン計測モジュール

- Status: Draft v1 (2026-08-03)
- Author: ekusiadadus (with Claude)

---

## 1. 背景と目的

private-isu のチューニングで `hirosuzuki/go-sql-logger` を評価した結果、
「環境変数トグル + ドライバプロキシ」という組み込みパターンは優秀だが、
以下が不足していると判断した(詳細は分析ログ参照):

- 集計・ソート済みのブラウザ表示がない(生 TSV のみ)
- SQL 以外(HTTP・アクセスログ・プロセスリソース)を扱えない
- ライセンスなし・更新停止のためフォーク不可

そこで **一つのモジュールで計測系すべてをまかなう** 自作パッケージを新規に作る。
ISUCON 本番に「`go get` + 数行」で持ち込めることをゴールとする。

## 2. 設計原則

1. **最小組み込み** — アプリ側の変更は最大3行。それ以上要求する機能は作らない
2. **低オーバーヘッド** — 計測オンでもスコア影響 < 1〜2%。ホットパスは
   「時刻取得2回 + マップ加算」以外に何もしない。計測オフ時はゼロコスト(素通し)
3. **依存最小・自己完結** — UI は html/template + 埋め込みCSSのみ。
   Prometheus/Grafana 等の外部サービス不要。外部依存は
   `shogo82148/go-sql-proxy`(MIT)と `quic-go`(HTTP/3、optional)程度に留める
4. **安定性最優先** — 計測層の panic はすべて recover しアプリに波及させない。
   メモリは上限付き(集計キー数上限)。ログファイル肥大なし(メモリ内集計)
5. **TDD** — 全パッケージ RED→GREEN→REFACTOR。カバレッジ 80% 以上。
   オーバーヘッド予算はベンチマークテストで CI 検証する
6. **シンプルさ > 機能** — takonomura 氏の言う「自分が全部理解できて、
   その場で書き換えられる道具」であること。柔軟性のために機能を削る

## 3. スコープ

### Phase 1(最重要・必須)

| 対象 | 手段 |
|---|---|
| MySQL / MariaDB | `database/sql` ドライバプロキシ(両者同一プロトコル・同一ドライバ) |
| PostgreSQL | 同上(`pgx stdlib` / `lib/pq` — ドライバ非依存実装のため自動対応) |
| HTTP/1.1, HTTP/2 | `http.Handler` ミドルウェア(net/http が両対応。`r.Proto` をラベル化) |
| HTTP/3 / QUIC | 同じミドルウェアを `quic-go/http3.Server{Handler: ...}` に渡すだけ |
| GraphQL | gqlgen extension(operation 単位)+ 汎用 body-sniff ミドルウェア |
| WebSocket | 接続レベル計測(httpstats が Upgrade を判別)+ フレームレベルは opt-in ラッパー(5.2.1) |
| nginx | ltsv / combined(+`$request_time`)パーサ + alp 相当の集計(バイト数・キャッシュ・upstream 分離。5.4) |
| Apache | combined + `%D` パーサ(設定スニペット同梱) |
| git コミットハッシュ | `debug.ReadBuildInfo()` の vcs.revision / vcs.modified(dirty)表示 |
| プロセス CPU/メモリ | `/proc` 直読みで CPU% / RSS 上位 N プロセスを表示 |
| ブラウザ表示 | 単一ハンドラがサーバ側ソート済み HTML を返す(初期表示から降順) |

### 非スコープ(v1)

- 生 QUIC ストリーム(HTTP/3 以外の QUIC 利用)— quic-go の qlog/Tracer 統合は Phase 2 候補
- pgx ネイティブ API(`pgxpool` 直接利用)— `database/sql` 経由のみ対応。制約として明記
- 分散トレーシング・時系列保存・外部ストレージ

## 4. アーキテクチャ

```
isutools/
├── isutools.go      // ファサード: RegisterSQL() / HTTP() / Handler()
├── sqlstats/        // SQL: ドライバプロキシ + メモリ内集計
├── httpstats/       // HTTP in: ミドルウェア集計 (h1/h2/h3, パス正規化)
├── gqlstats/        // GraphQL: operation 単位集計
├── accesslog/       // nginx/Apache ログの pull 型集計 (alp 相当)
├── procstats/       // /proc スキャン: プロセス別 CPU/RSS top-N
├── buildinfo/       // git hash + dirty 検出
├── internal/agg/    // 共通集計コア (sharded map, log2 バケットヒストグラム)
└── web/             // レポート UI + POST /reset + GET /json
```

### 組み込み例(private-isu の場合、計3行)

```go
import "github.com/ekusiadadus/isutools"

isutools.RegisterSQL("mysql")            // ① "mysql:isutools" ドライバを登録
db, _ = sqlx.Open("mysql"+os.Getenv("ISUTOOLS_SQL_POSTFIX"), dsn) // ②(既存行の書き換え)
r.Mount("/debug/isutools", isutools.Handler())  // ③ UI
// HTTP 集計もするなら: http.ListenAndServe(":8080", isutools.HTTP(r))
```

- `ISUTOOLS_SQL_POSTFIX=:isutools` で SQL 計測オン、未設定なら素のドライバ(ゼロコスト)
- `ISUTOOLS=off` で全機能を素通し化

## 5. コンポーネント設計

### 5.1 sqlstats

- `shogo82148/go-sql-proxy`(最新版・MIT)で `driver.Driver` をラップし
  `<name>:isutools` として登録。**ドライバ非依存**なので MySQL / MariaDB
  (go-sql-driver/mysql)、PostgreSQL(pgx stdlib, lib/pq)すべて同一コード
- 集計: 正規化クエリ(空白圧縮・1000字切詰・`/* tag */` 抽出)をキーに
  `count / total / max / log2バケットヒストグラム(≒p95)` を加算
- ホットパス最適化: 同一クエリ文字列ポインタ→正規化済みキーの `sync.Map`
  キャッシュ(prepared statement は同一文字列なので実質1回だけ正規化が走る)
- オーバーヘッド予算: **1クエリあたり追加 500ns 未満**(Benchmark で CI 検証)

### 5.2 httpstats

- `func Middleware(next http.Handler) http.Handler`。記録: メソッド、
  正規化パス、`r.Proto`(HTTP/1.1・HTTP/2.0・HTTP/3.0)、status、時間、bytes
- パス正規化: デフォルトで数値・UUID・拡張子前の ID セグメントを `*` に置換
  (`/image/123.jpg` → `/image/*.jpg`)。`WithPathRules([]Rule)` で上書き可能
- HTTP/2: net/http 標準(TLS)/ h2c どちらも同一ミドルウェアで計測可能
- HTTP/3/QUIC: `http3.Server{Handler: isutools.HTTP(mux)}` に渡すだけ。
  QUIC は HTTP/3 のトランスポートとして自動的にカバーされる

### 5.2.1 WebSocket / 長寿命コネクション(SSE 含む)

**素朴に計測するとレイテンシ統計が壊れる**(接続が数分生きるため、通常リクエストの
p95/avg に巨大な外れ値として混入する)。そのため httpstats は以下の扱いにする:

- リクエストの `Upgrade: websocket` ヘッダ / レスポンス 101 を検知したら、
  **通常のレイテンシテーブルから除外**し、専用の「Connections」セクションで集計:
  - 累計接続数 / **現在のアクティブ接続数(ゲージ)** / 接続持続時間の分布
  - SSE(`Content-Type: text/event-stream`)も同じ扱い(mazrean 氏の SSE 化・
    worker_connections 枯渇の事例に対応。当初 Phase 2 だった接続数ゲージを
    Phase 1 に昇格)
- フレーム/メッセージレベルの計測はライブラリ依存になるため **opt-in の1行ラッパー**
  で提供: `conn = isutools.WrapWSConn(conn)`(gorilla/websocket 等の
  `net.Conn` / `io.ReadWriter` を包み、送受フレーム数・バイト数を集計)。
  ハンドラ内の処理時間を測りたい場合は汎用カウンタ(Phase 2)を使う
- 制約: WebSocket over HTTP/2 (RFC 8441) / HTTP/3 (RFC 9220) は Go 標準の
  サーバ側サポートが限定的なため v1 は HTTP/1.1 Upgrade を対象とする(明記)

### 5.3 gqlstats

- HTTP レベルでは `POST /graphql` に潰れてしまうため operation 単位で集計する
- gqlgen 利用時: `graphql.HandlerExtension`(InterceptResponse)を提供 — 1行
- 非 gqlgen: リクエストボディ先頭を覗いて `operationName` を抽出する
  opt-in ミドルウェア(ボディコピーのコストがあるため明示指定時のみ)

### 5.4 accesslog(nginx / Apache)

- **pull 型**: アプリのホットパスには一切関与せず、レポート表示時(または
  `POST /collect`)にログファイルを差分読みして集計する。オフセットと inode を
  記憶し、ローテーションを検知したら先頭から読み直す
- フォーマット: ltsv(推奨・下記スニペット)、combined+`$request_time`、
  Apache combined+`%D`。自動判別
- Docker 構成ではログの volume 共有が必要(compose 例を同梱。5.8 参照)

#### 5.4.1 nginx ltsv フォーマット仕様(同梱スニペット)

画像配信チューニング(ISUCON 頻出: DB 内画像 → 静的配信 + キャッシュ)の
効果測定に必要なフィールドまで含めて確定する:

```nginx
log_format isutools ltsv escape=json
  "time:$time_iso8601"
  "\tmethod:$request_method"
  "\turi:$request_uri"
  "\tstatus:$status"
  "\treqtime:$request_time"          # クライアント視点の総時間
  "\tupstime:$upstream_response_time" # アプリ処理時間("-" = nginx が直接応答)
  "\tbytes:$body_bytes_sent"          # 転送バイト数(画像サイズ分析の主役)
  "\tcache:$upstream_cache_status"    # proxy_cache の HIT/MISS/BYPASS
  "\tctype:$sent_http_content_type";  # MIME 別集計用
```

#### 5.4.2 集計軸

| 軸 | 分かること |
|---|---|
| count / sum / avg / p95 / max(reqtime) | alp 相当の基本レイテンシ分析 |
| **bytes: パス別 合計/平均転送量・上位パス** | どの画像・静的パスが帯域を食っているか(合計時間だけでは見えない) |
| **reqtime − upstime の乖離** | nginx 側の滞留(worker/バッファ/帯域)とアプリの遅さを切り分け |
| **upstime = "-" の比率(nginx 直接応答率)** | 静的化オフロードがどれだけ効いたか |
| **status 304 比率** | Conditional GET / ETag・expires 設定の効き目 |
| **cache HIT/MISS 率** | proxy_cache 導入の効き目 |
| ctype 別集計 | 画像/CSS/JS/HTML の帯域内訳 |
| status 101 / 499 / 5xx | WebSocket 接続はレイテンシ集計から分離、499・reset は警告表示(接続枯渇の兆候) |

- Apache 側は `%D`(µs)+ `%B` で reqtime / bytes を同等に取る
  (upstream 分離・キャッシュ状態は nginx のみの対応と明記)
- WebSocket(101)はアクセスログ上も接続クローズ時に長大な reqtime で
  記録されるため、パーサが status=101 をレイテンシ集計から自動除外する
  (5.2.1 と整合)

### 5.5 procstats

- `/proc/[pid]/stat`(utime+stime)を 500ms 間隔で2回サンプリングして CPU% を
  算出、`/proc/[pid]/statm` から RSS。CPU / RSS それぞれ上位 N(既定10)を表示
- 外部依存なし(gopsutil 不使用)。Linux 専用で良い(ISUCON は Linux)
- テスト容易性のため proc ルートは差し替え可能(`testdata/proc` фикスチャ)

### 5.6 buildinfo

- 第一選択: `debug.ReadBuildInfo()` の `vcs.revision` / `vcs.modified`。
  Go 1.18+ はビルド時に自動埋め込みされるため **コード・フラグ不要**
- 表示例: `f4fdb31 (dirty)` / `f4fdb31`
- フォールバック(優先順): ① ldflags `-X` 注入(Makefile スニペット同梱)
  ② 環境変数 `ISUTOOLS_GIT_HASH` / `ISUTOOLS_GIT_DIRTY`
- 制約: Docker ビルドのコンテキストに `.git` が無いと vcs 情報が取れない
  (private-isu の golang/ コンテキストがまさにこれ)→ compose の build.args で
  ハッシュを渡す例を同梱

### 5.7 web(レポート UI)— snapshot-first アーキテクチャ

**主ワークフローは「リモートで計測 → スナップショットファイルをダウンロード →
手元の PC のブラウザで閲覧」とする。** 集計(コア)と閲覧(レンダラ)を
完全に分離し、すべてのレンダラは同一のスナップショット構造体から描画する。

```
collection core ──▶ Snapshot (構造体: meta + sql + http + gql + accesslog + proc)
                      ├─ GET /            … ライブ表示(現在のスナップショットを描画)
                      ├─ GET /snapshot.html … 自己完結 HTML エクスポート(主役)
                      ├─ GET /json          … 機械可読(差分・通知連携用)
                      └─ POST /reset        … 集計リセット(1世代前を保存)
```

- **`GET /snapshot.html`** — データ埋め込みの完全自己完結 HTML(外部リソース
  ゼロ・インライン CSS/最小限の JS)。ダウンロードして**ダブルクリックで開くだけ**
  で閲覧できる。初期表示は合計時間降順ソート済み、列クリックで再ソート
- meta にはベンチ結果(score)・git hash・dirty・計測時刻を含め、ファイル名も
  `20260803-2130_f4fdb31-dirty_score929.html` 形式で自動命名する
  (buildinfo 要件がここで効く)
- ライブ表示(`GET /`)も同一コアの別レンダラとして残す(実装コストほぼゼロ)。
  チューニング中に SSH トンネル越しでさっと見る用途と使い分ける
- **専用ローカルビューアアプリは作らない**(シンプルさ原則)。自己完結 HTML が
  ビューアそのものであり、履歴閲覧は snapshots/ ディレクトリとブラウザで足りる

この方式の利点:
1. **履歴が資産になる** — ベンチ毎のスナップショットが手元に蓄積し、
   Phase 2 の「スナップショット差分」はローカルの JSON 2つの比較に単純化される
2. **本番でサーバーが消えても感想戦ができる** — ISUCON 本番は競技後に
   サーバーが破棄されるため、手元に全計測が残ることは決定的に重要
3. **閲覧が計測対象に負荷をかけない** — ベンチ直後にエクスポート1回だけ
4. アプリが落ちた後・コンテナ再起動後も直前の計測を失わない

### 5.7.1 転送の自動化

takonomura 氏の「短いコマンド一発」原則に従い、手元 PC 側に `bench` 一発
コマンドを用意する(examples/ に同梱):

```
ssh 対象 'bench.sh'                       # ベンチ実行(前段で POST /reset)
curl … /snapshot.html+json → snapshots/   # リモート側で保存
rsync remote:snapshots/ ./snapshots/      # 手元へ回収
open snapshots/<latest>.html              # ブラウザで自動オープン
```

### 5.8 インフラ側の設計課題と解決

| 課題 | 解決 |
|---|---|
| nginx ログはアプリコンテナから見えない | compose で共有 volume(`nginx_logs`)を両方にマウント。設定例を `examples/` に同梱 |
| Apache も同様 | 同上 |
| Docker ビルドに `.git` が無い | build.args でハッシュ注入(5.6 フォールバック) |
| コンテナ内 `/proc` は自コンテナの PID namespace のみ | 単一台構成なら `pid: "host"`(compose)で全プロセス可視化。Docker Desktop は VM 内プロセスになる旨を注記 |
| 集計メモリの際限ない成長 | キー数上限(SQL 10k / HTTP 10k)。超過分は `(other)` に合算し、UI に警告表示(silent cap にしない) |

## 6. オーバーヘッドと安定性の設計

- 計測オフ: ドライバ未登録・ミドルウェアは `next` をそのまま返す(ゼロコスト)
- 計測オン: ホットパスは `time.Now()` 2回 + sharded map への加算のみ。
  文字列処理はキャッシュ済み正規化キーで回避
- すべての集計呼び出しは `defer recover()` でガードし、計測の失敗が
  アプリの応答に影響しない
- goroutine は起動しない(procstats のサンプリングもリクエスト駆動)。
  常駐コスト・グレースフルシャットダウン考慮が不要になる

## 7. TDD・テスト戦略

開発は全機能 RED → GREEN → REFACTOR で進める。実装 PR には必ず先行するテストを含める。

| パッケージ | テスト |
|---|---|
| internal/agg | 表駆動ユニット + `-race` 並行加算テスト + Benchmark(予算: 加算1回 < 100ns) |
| sqlstats | fake driver による proxy 経由の集計検証 / 正規化の表駆動 / Benchmark(< 500ns/query 追加) |
| httpstats | httptest(h1)+ `EnableHTTP2`(h2)/ h3 はプロトコルラベル単体テスト + quic-go 統合テスト(build tag `integration`) |
| gqlstats | gqlgen テストサーバ / operationName 抽出の表駆動 |
| accesslog | golden file(ltsv・combined・Apache %D)/ 差分読み・ローテ検知(inode 変更)テスト |
| procstats | `testdata/proc` フィクスチャで CPU%・RSS 算出検証 |
| buildinfo | ldflags 注入テスト / vcs settings パースの表駆動 |
| web | ソート順のアサーション + HTML golden snapshot |

- CI(GitHub Actions): `go vet` / `go test -race -cover`(80% ゲート)/
  ベンチマーク回帰チェック(予算超過で fail)
- 統合テスト: private-isu 環境で計測オン/オフのベンチスコア比較を記録し、
  オーバーヘッド < 2% を確認してからリリースタグを打つ

## 8. マイルストーン(Phase 1 内の実装順)

1. **M1**: internal/agg + sqlstats + web + buildinfo — private-isu に組み込み、
   go-sql-logger を置き換える(現行3行 → isutools 3行)
2. **M2**: httpstats(h1/h2)+ procstats
3. **M3**: accesslog(nginx ltsv/combined → Apache)
4. **M4**: gqlstats + HTTP/3(quic-go)統合テスト

## 9. Phase 2 候補 — 感想戦・登壇記事の分析から

出典: takonomura「ソロISUCONの戦い方」(ISUCON夏祭り2023)、
mazrean「ISUCON14感想戦で40万点超えました」(traP blog 2024-12)。

### 高優先(次に作るべきもの)

1. **汎用カウンタ/ゲージ API**(mazrean: キャッシュ hit/miss・状態分布を
   Grafana で可視化していた)
   `isutools.Count("ride_status_cache_hit")` の1行でレポートに集計表示。
   オンメモリキャッシュ導入後の効果測定は終盤で必ず必要になる。
   Prometheus/Grafana なしで同じ意思決定ができるのが本モジュールの流儀
2. **スナップショット差分**(takonomura: 「スコアが変わらなくても
   ボトルネックが移動しただけ」を計測で確認して判断する)
   `POST /reset` 時に直前の集計を1世代保存し、レポートに前回比
   (クエリ毎 total の増減)を表示。改善が効いたか・移動しただけかが一目で分かる
3. **pprof 統合**(mazrean: 終盤は DB ネック→アプリ CPU ネックに移行し、
   関数レベルの分析が主戦場になる)
   `/debug/isutools/pprof/` に net/http/pprof をマウント + ベンチ連動の
   自動30秒 CPU キャプチャ → `google/pprof` で top 関数を同じ UI に表示。
   procstats(プロセスレベル)の一段深い階層として自然に接続する
4. **GET /json → Markdown フォーマッタ**(takonomura: 計測結果を短い
   コマンド一発で GitHub コメント投稿)
   既存の bench.sh(Discord 通知)に SQL top5・HTTP top5 を含められる。
   実装は小さく効果が大きい

### 中優先

5. ~~アクティブ接続数ゲージ~~ → **Phase 1 に昇格済み**(5.2.1 の WebSocket/SSE
   接続計測に統合)
6. **異常検知の警告表示**(mazrean: `connection reset` で fail)
   accesslog の status 499 / 5xx 急増・reset をレポート上部に警告として出す
7. **ログローテ追随の堅牢化**(takonomura: ローテ自動化)— accesslog の
   inode 監視を「ローテしても集計が途切れない」保証まで引き上げる

### 低優先・スコープ外と判断したもの

8. **AST 一括変換**(takonomura: DB 呼び出しに Context を一括で渡すツール)
   → 価値はあるが計測モジュールとは責務が別。作るなら `isutools-rewrite` として別リポジトリ
9. **PGO プロファイル書き出し**(mazrean: Go 1.24 化・PGO)
   → pprof 統合(候補3)の副産物として `GET /pprof/default.pgo` を出すのは容易。優先度は低
10. **badger・イベントバス・SSE 実装テンプレ**(mazrean)
    → アプリ実装の話であり計測ツールの責務外。スニペット集として別管理が適切

### 記事から取り込んだ設計上の教訓(機能ではなく原則)

- 「機能が多いものより、自分が理解できてその場で書き換えられるもの」(takonomura)
  → 依存ゼロ UI・単一モジュール・全コード 2,000 行以内を目安にする
- 「無駄な変更をしない。判断材料になる計測結果を常に持つ」(takonomura)
  → スナップショット差分(候補2)を UI の一等市民にする
- 「終盤はネックが DB → アプリ CPU → GC/map へ移動する」(mazrean)
  → SQL だけでなく procstats / pprof を最初から同居させる本設計の妥当性を裏付け

## 10. 決定事項ログ

- 2026-08-03: 閲覧方式は **snapshot-first**(リモートで計測 → 自己完結 HTML を
  ダウンロード → 手元 PC で閲覧)に決定。ライブ表示は補助として残す(5.7)

## 11. 未決事項

- [ ] リポジトリ公開設定: Docker ビルド内の `go get` を素直に通すには public + MIT が最善
  (private のままなら vendoring か build secret で GOPRIVATE 認証が必要)
- [ ] モジュール名の確定: `isutools` / `isuprof` など
- [ ] パス正規化ルールの注入方法(コード / 環境変数 / 設定ファイル)
- [ ] `(other)` 合算の上限値のデフォルト
- [ ] snapshot.html の JS 許容範囲(ソート切替のみ最小限で入れるか、完全 JS なしか)
