# isutools 設計書

`github.com/ekusiadadus/isutools` — ISUCON 向けオールインワン計測モジュール

- Status: Revised Draft v2 (2026-08-03、設計レビュー反映・M1 実装中)
- Author: ekusiadadus (with Claude)

---

## 1. 背景と目的

private-isu のチューニングで `hirosuzuki/go-sql-logger` を評価した結果、
「環境変数トグル + ドライバプロキシ」という組み込みパターンは優秀だが、
以下が不足していると判断した(リリース前に比較対象・検証日・再現手順を
`docs/` 配下の分析ログとしてリンクする):

- 集計・ソート済みのブラウザ表示がない(生 TSV のみ)
- SQL 以外(HTTP・アクセスログ・プロセスリソース)を扱えない
- ライセンスなし・更新停止のためフォーク不可

そこで **一つのモジュールで計測系すべてをまかなう** 自作パッケージを新規に作る。
ISUCON 本番に「`go get` + 数行」で持ち込めることをゴールとする。

## 2. 設計原則

1. **最小組み込み** — SQL + UI の基本導入は2行、HTTP計測を含めても3行を目標とする。
   accesslog・procstats 等のインフラ設定は行数に含めず、追加作業を機能別に明記する
2. **低オーバーヘッド** — 計測オンでも実アプリのスコア影響 < 1〜2% を目標とする。
   ホットパスの処理(時刻取得、正規化済みキー取得、集計加算、ResponseWriterラップ)は
   個別ベンチと実環境の両方で測る。計測オフ時は起動時に素のドライバ/Handlerを選び、
   リクエスト・クエリ単位の追加処理を行わない
3. **依存最小・自己完結** — UI は html/template + 埋め込みCSSのみ。
   Prometheus/Grafana 等の外部サービス不要。core の実行時依存は
   `shogo82148/go-sql-proxy`(MIT、バージョン固定)に留め、gqlgen・quic-go は
   アダプタ/統合テスト側だけの optional dependency とする
4. **安定性最優先** — isutools 自身の正規化・集計・描画処理の panic だけを recover し、
   アプリHandler、DBドライバ、resolverのpanicは握り潰さない。
   メモリは上限付き(集計キー数上限)。ログファイル肥大なし(メモリ内集計)
5. **TDD** — 全パッケージ RED→GREEN→REFACTOR。カバレッジ 80% 以上。
   機能CIと性能ゲートを分離し、性能は固定環境と実アプリABBA比較で検証する
6. **シンプルさ > 機能** — takonomura 氏の言う「自分が全部理解できて、
   その場で書き換えられる道具」であること。柔軟性のために機能を削る
7. **安全・観測可能な縮退** — 計測失敗はアプリを止めないが、黙って欠損させない。
   snapshot に collector health / dropped count / partial を残し、デバッグEndpointは
   既定で外部公開しない

## 3. スコープ

### v1 対象(マイルストーン単位で段階リリース)

| Milestone | 対象 | 手段 |
|---|---|---|
| M1 / v0.1 | MySQL / MariaDB / PostgreSQL(database/sql) | 登録済み `driver.Driver` をプロキシ化。pgx native API は対象外 |
| M1 / v0.1 | buildinfo + Snapshot + HTML/JSON | schema version付きSnapshotから全rendererを生成 |
| M2 / v0.2 | HTTP/1.1, HTTP/2 + nginx + procstats | Handler middleware、差分ログ、ベンチ区間の `/proc` 差分 |
| M3 / v0.3 | Apache + gqlgen | Apache明示format、gqlgen operation adapter |
| M4 / v1.0 | WebSocket/SSE接続 + HTTP/3互換性 | 接続レベル計測とoptional統合テスト。フレーム計測は除外 |

### 非スコープ(v1)

- 生 QUIC ストリーム(HTTP/3 以外の QUIC 利用)— quic-go の qlog/Tracer 統合は Phase 2 候補
- pgx ネイティブ API(`pgxpool` 直接利用)— `database/sql` 経由のみ対応。制約として明記
- 分散トレーシング・時系列保存・外部ストレージ
- WebSocket のフレーム/メッセージ数—ライブラリ別アダプタが必要なため Phase 2

## 4. アーキテクチャ

```
isutools/
├── isutools.go      // ファサード: RegisterSQL() / HTTP() / Handler()
├── sqlstats/        // SQL: ドライバプロキシ + メモリ内集計
├── httpstats/       // HTTP in: ミドルウェア集計 (h1/h2/h3, パス正規化)
├── gqlstats/        // GraphQL: 共通operation集計
│   └── gqlgen/      // optional: gqlgen HandlerExtension adapter
├── accesslog/       // nginx/Apache ログの pull 型集計 (alp 相当)
├── procstats/       // /proc スキャン: プロセス別 CPU/RSS top-N
├── buildinfo/       // git hash + dirty 検出
├── internal/agg/    // 共通集計コア (bounded map, log2 バケットヒストグラム)
├── internal/health/ // collectorの失敗・drop・partial状態
└── web/             // レポート UI + POST /reset + GET /json
```

### 組み込み例(private-isu の場合、基本1行・HTTP込み2行)

```go
import "github.com/ekusiadadus/isutools"

db, _ = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // ①(既存行の書き換えのみ)
// HTTP 集計もするなら(M2): http.ListenAndServe(":8080", isutools.HTTP(r))
```

**管理サーバ方式**: snapshot-first ではブラウザ閲覧はダウンロードしたファイルで
行うため、アプリのルーターにレポート UI を載せる必要はない。ただし
「ベンチ前 reset」「スナップショット取得」の**制御チャネル**は必要なので、
`SQLDriverName()` が成功時に**別ポートの管理サーバ**を1度だけ起動する
(既定 `127.0.0.1:19191`、`ISUTOOLS_ADDR` で変更、`ISUTOOLS_ADDR=off` で無効)。
アプリのルーター・nginx を経由しないため外部に露出しない(P0-6 の安全境界に合致)。
Docker では compose の env + ports で到達性を制御する。`Handler()` も公開して
おり、アプリと同一ポートに載せたい場合は従来どおり任意のルーターに Mount できる。

**on/off 契約**(レビュー P0-1 反映): 有効判定は `SQLDriverName()` の1箇所に集約する。
`ISUTOOLS=off` ならラップせず素のドライバ名を返す(ゼロコスト)。登録失敗時も
素の名前を返す **fail-open** で、計測がアプリの起動を壊すことは決してない。
判定は起動時に1回で、動的切替はしない。旧案の `ISUTOOLS_SQL_POSTFIX` 環境変数は
「off なのにプロキシ名で接続して起動失敗する」矛盾があったため廃止した。

- fail-openした場合は collector health に警告を残し、HTML/JSONの `partial` をtrueにする
- 厳格に失敗させたいCI/運用では `RegisterSQL` のerrorを確認する
- 元ドライバは呼び出し前にblank import等で `database/sql` へ登録済みでなければならない
- `ISUTOOLS=off` は起動時の不変設定とし、実行中の環境変数変更は保証しない

## 5. コンポーネント設計

### 5.1 sqlstats

- `shogo82148/go-sql-proxy`(MIT、v0.7.3に固定)で `driver.Driver` をラップし
  `<name>:isutools` として登録。**ドライバ非依存**なので MySQL / MariaDB
  (go-sql-driver/mysql)、PostgreSQL(pgx stdlib, lib/pq)すべて同一コード
- 登録は `sql.Open(name, "").Driver()` で元driverを取得し、重複登録はno-op、
  未登録名はerrorとする
- 集計時間はExec/Query開始からdriverが結果/`driver.Rows`を返すまで。
  `Rows.Next`〜`Rows.Close`の読み切り時間は含まないため、UIにも
  `query dispatch duration` と明記する
- 集計キーはSQLリテラル/コメント由来の機密値を残さない正規化を行い、空白圧縮、
  placeholder化、1000字切詰、許可文字だけの `/* tag */` を適用する。引数値は保存しない
- prepared statementはPrepare時に正規化する。非prepared query用キャッシュを置く場合は
  文字列ポインタに依存せず、集計キー上限とは別の明示上限を持たせる
- `count / error_count / total / max / log2 bucket` を加算し、p95は
  **該当bucket上限値**であることをUI/JSONに明記する
- microbenchmarkの目標は **1クエリあたり追加 500ns未満**。共有CIのhard gateにはせず、
  固定runnerとprivate-isu ABBA比較でリリース判定する

### 5.2 httpstats

- `func Middleware(next http.Handler) http.Handler`。記録: メソッド、
  正規化パス、`r.Proto`(HTTP/1.1・HTTP/2.0・HTTP/3.0)、status、時間、bytes
- パス正規化: デフォルトで数値・UUID・拡張子前の ID セグメントを `*` に置換
  (`/image/123.jpg` → `/image/*.jpg`)。`WithPathRules([]Rule)` で上書き可能
- 集計キーには `r.URL.Path` を使い、query stringは保存しない
- ResponseWriter wrapper はstatus/bytesを記録しつつ、`Unwrap`、`Flusher`、
  `Hijacker`、`io.ReaderFrom` 等の元writerの能力を壊さない。アプリHandlerのpanicは再panicする
- HTTP/2: net/http 標準(TLS)/ h2c どちらも同一ミドルウェアで計測可能
- HTTP/3/QUIC: `http3.Server{Handler: isutools.HTTP(mux)}` に渡すだけ。
  core はquic-goをimportせず、HTTP/3互換性はbuild tag付き統合テストで保証する

### 5.2.1 WebSocket / 長寿命コネクション(SSE 含む)

**素朴に計測するとレイテンシ統計が壊れる**(接続が数分生きるため、通常リクエストの
p95/avg に巨大な外れ値として混入する)。そのため httpstats は以下の扱いにする:

- HTTP/1.1の `Upgrade: websocket` を検知し、middlewareの `Hijack()` が返す
  `net.Conn` をClose追跡付きで包む。Closeは一度だけ計上し、
  **通常のレイテンシテーブルから除外**して専用の「Connections」セクションで集計:
  - 累計接続数 / **現在のアクティブ接続数(ゲージ)** / 接続持続時間の分布
  - SSE(`Content-Type: text/event-stream`)も同じ扱い(mazrean 氏の SSE 化・
    worker_connections 枯渇の事例に対応。当初 Phase 2 だった接続数ゲージを
    Phase 1 に昇格)
- フレーム/メッセージ数の計測はレビュー P0-2 を受けて **Phase 2 のライブラリ別
  アダプタに後送**する(`*websocket.Conn` は `net.Conn` ではなく、基底コネクションの
  直接読み書きは接続を壊すため、汎用ラッパーは成立しない)。Phase 1 は
  接続数・アクティブゲージ・持続時間・**wire バイト数**(Hijack 後の `net.Conn` を
  Close 追跡付きで包んで計測)までとする
- ResponseWriter ラッパーは `Hijacker` / `Flusher` / `ReaderFrom` / `Unwrap` を
  必ず透過する(消すと WebSocket/SSE 自体が壊れる)
- SSEは`Content-Type: text/event-stream`確定時からHandler終了/Context cancelまでを
  activeとして扱い、接続時間は通常HTTPレイテンシへ混ぜない
- 制約: WebSocket over HTTP/2 (RFC 8441) / HTTP/3 (RFC 9220) は Go 標準の
  サーバ側サポートが限定的なため v1 は HTTP/1.1 Upgrade を対象とする(明記)

### 5.3 gqlstats

- HTTP レベルでは `POST /graphql` に潰れてしまうため operation 単位で集計する
- gqlgen 利用時: optionalな `gqlstats/gqlgen` パッケージから
  `graphql.HandlerExtension`(`InterceptOperation`)を提供 — 1行。
  responseが複数回発生するsubscriptionでもoperation countを重複計上しない
- 非 gqlgen: GET / JSON POSTの`operationName`だけを対象にしたopt-in middlewareとし、
  Bodyサイズ上限、読み取り後のBody復元、匿名operationの安定hashを必須とする。
  batch/APQ/multipart/WebSocketを未対応のまま推測集計せず、healthにunsupportedを出す

### 5.4 accesslog(nginx / Apache)

- **pull 型**: アプリのホットパスには一切関与しない。`POST /reset` で現在の
  inode・offsetを世代の開始点として記録し、snapshot/`POST /collect` で
  その世代の差分だけを読み込む
- ローテーションは inode 変更前の旧ファイル末尾を可能な限りdrainしてから新ファイルへ
  移り、copytruncate(同じinodeでsize < offset)はoffset=0へ戻す。欠損・重複の可能性は
  collector healthへ表示する
- フォーマット: ltsv(推奨・下記スニペット)、combined+`$request_time`、
  Apache combined+`%D`。設定でformatを明示する方式を第一選択とし、自動判別は
  失敗時に黙って誤読しないbest-effort fallbackとする
- Docker 構成ではログの volume 共有が必要(compose 例を同梱。5.8 参照)

#### 5.4.1 nginx ltsv フォーマット仕様(同梱スニペット)

画像配信チューニング(ISUCON 頻出: DB 内画像 → 静的配信 + キャッシュ)の
効果測定に必要なフィールドまで含めて確定する:

```nginx
log_format isutools ltsv escape=json
  "time:$time_iso8601"
  "\tmethod:$request_method"
  "\turi:$uri"                       # query stringを保存しない
  "\tstatus:$status"
  "\treqtime:$request_time"          # クライアント視点の総時間
  "\tupstime:$upstream_response_time" # upstream時間。retry時は複数値、"-"は計測値なし
  "\tbytes:$body_bytes_sent"          # 転送バイト数(画像サイズ分析の主役)
  "\tcache:$upstream_cache_status"    # proxy_cache の HIT/MISS/BYPASS
  "\tctype:$sent_http_content_type";  # MIME 別集計用
```

#### 5.4.2 集計軸

| 軸 | 分かること |
|---|---|
| count / sum / avg / p95 / max(reqtime) | alp 相当の基本レイテンシ分析 |
| **bytes: パス別 合計/平均転送量・上位パス** | どの画像・静的パスが帯域を食っているか(合計時間だけでは見えない) |
| **reqtime − upstream合計の残差** | client upload・nginx処理・buffer・downstream等を含む非upstream時間の手掛かり |
| **upstime = "-" の比率(no-upstream-timing率)** | upstream計測値がない割合。静的配信成功とは断定しない |
| **status 304 比率** | Conditional GET / ETag・expires 設定の効き目 |
| **cache HIT/MISS 率** | proxy_cache 導入の効き目 |
| ctype 別集計 | 画像/CSS/JS/HTML の帯域内訳 |
| status 101 / 499 / 5xx | WebSocket 接続はレイテンシ集計から分離、499・reset は警告表示(接続枯渇の兆候) |

- `$upstream_response_time` のカンマ/コロン区切り値は全試行をparseし、raw値、合計、
  試行回数を保持する。parse不能なら残差を出さずpartial警告にする
- Apache 側は `%D`(µs)でdurationを取る。`%B`はHTTP response body sizeであり
  実ネットワーク送信量ではないため、実送信量が必要なら `mod_logio` の `%O` を使い、
  UIでも `response_size` と `wire_bytes` を区別する
- WebSocket(101)はアクセスログ上も接続クローズ時に長大な reqtime で
  記録されるため、パーサが status=101 をレイテンシ集計から自動除外する
  (5.2.1 と整合)
- nginx/Apacheでbuffered logを使う場合はflush間隔を短く設定し、snapshot取得側は
  設定した猶予時間だけ新規行をpollしてから世代を凍結する

### 5.5 procstats

- **世代差分方式**(レビュー P0-3 反映): 表示時の瞬間サンプリングでは
  「ベンチ終了後のアイドル状態」を測ってしまう。`POST /reset` 時に
  `/proc/[pid]/stat`(utime+stime+starttime)と全体 jiffies のベースラインを
  記録し、snapshot 取得時に**ベンチ区間全体の差分**として CPU 時間を算出する
- PID 再利用は starttime の一致で検知。CPU% は「1コア=100%(top 互換)」と定義
- RSS は snapshot 時点の値(`/proc/[pid]/statm`)。CPU / RSS 上位 N(既定10)
- 外部依存なし(gopsutil 不使用)。Linux 専用で良い(ISUCON は Linux)
- テスト容易性のため proc ルートは差し替え可能(`testdata/proc` フィクスチャ)

### 5.5.1 ホスト基本情報(sysinfo)

すべてのレポート・スナップショットの meta に**必ず**表示する(どのハードで
測った数字かを常に明示する):

- CPU モデル名(`/proc/cpuinfo`、ARM は Hardware/Model フォールバック)
- コア数(`runtime.NumCPU`)/ メモリ総量 GB(`/proc/meminfo`)/ OS・arch / hostname
- 取得失敗時は "unknown" / "?" 表示(空欄にしない)。darwin は開発用ベストエフォート

### 5.6 buildinfo

- 第一選択: `debug.ReadBuildInfo()` の `vcs.revision` / `vcs.modified`。
  Go 1.18+ でも、main package・main module・build directoryが同じVCS repositoryにあり、
  `-buildvcs=false`でない等の条件を満たす場合にだけ自動埋め込みされる
- 状態は `dirty / clean / unknown` の3値で保持し、情報欠落をclean扱いしない。
  表示例: `f4fdb31 (dirty)` / `f4fdb31` / `unknown`
- フォールバック(優先順): ① ldflags `-X` 注入(Makefile スニペット同梱)
  ② 環境変数 `ISUTOOLS_GIT_HASH` / `ISUTOOLS_GIT_DIRTY`
- 制約: Docker ビルドのコンテキストに `.git` が無いと vcs 情報が取れない
  (private-isu の golang/ コンテキストがまさにこれ)→ compose `build.args`、
  Dockerfile `ARG`、`go build -ldflags '-X ...'` まで一続きの例を同梱する

### 5.7 web(レポート UI)— snapshot-first アーキテクチャ

**主ワークフローは「リモートで計測 → スナップショットファイルをダウンロード →
手元の PC のブラウザで閲覧」とする。** 集計(コア)と閲覧(レンダラ)を
完全に分離し、すべてのレンダラは同一のスナップショット構造体から描画する。

```
immutable startup config
        │
collectors ──▶ Generation N (resetでatomic swap、proc/logの開始点を保持)
                    │
                    └─▶ immutable Snapshot v1
                          (meta + health + sql + http + gql + accesslog + proc)
                      ├─ GET /            … ライブ表示(現在のスナップショットを描画)
                      ├─ GET /snapshot.html … 自己完結 HTML エクスポート(主役)
                      ├─ GET /json          … 機械可読(差分・通知連携用)
                      └─ POST /reset        … 集計リセット(1世代前を保存)
```

- **`GET /snapshot.html`** — データ埋め込みの完全自己完結 HTML(外部リソース
  ゼロ・インライン CSS/最小限の JS)。ダウンロードして**ダブルクリックで開くだけ**
  で閲覧できる。初期表示は合計時間降順ソート済み、列クリックで再ソート
- meta には `schema_version`、tool version、generation、計測開始/終了、
  git hash、dirty state、host、各collector healthを含める。scoreはアプリから
  知り得ないため、bench scriptが結果取得後にmetaへ付与するかファイル名だけに含める
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

**永続性の正確な範囲**(レビュー P0-4 反映): 集計はメモリ内のみであり、
残るのは**エクスポート済みのスナップショットだけ**。エクスポート前にプロセスが
落ちればその世代は失われる(ベンチ後に必ず取得する運用が前提)。
snapshot の meta には `schema_version` / `generation`(reset 毎に増加)/
取得時刻 / git revision+dirty / ホスト情報(5.5.1)を含める。score は
アプリからは知り得ないため、ベンチスクリプト側がファイル名に付与する。

**generation境界**: `POST /reset` は新しい空generationへatomic swapし、その後に
旧generationを不変Snapshotとして保存する。進行中の計測は開始時に取得したgenerationへ
最後まで加算し、reset前後へ二重計上しない。Snapshot生成中のreset、同時reset、
collector失敗をrace testで固定する。1世代前の保持はv1の基本機能とし、
UI上の差分計算・複数世代履歴はPhase 2とする。

### 5.7.1 転送の自動化

takonomura 氏の「短いコマンド一発」原則に従い、手元 PC 側に `bench` 一発
コマンドを用意する(examples/ に同梱):

```
ssh 対象 'bench.sh'                       # reset → ベンチ → HTML/JSONを.tmpへ取得
mv snapshots/<run>.tmp snapshots/<run>    # 取得成功後だけatomic rename
rsync remote:snapshots/ ./snapshots/      # 手元へ回収
open snapshots/<latest>.html              # ブラウザで自動オープン
```

### 5.7.2 Web endpoint の安全境界

- 既定ではloopbackからのアクセスだけを許可する。非loopbackで使う場合は
  `ISUTOOLS_TOKEN` 等の明示tokenを要求し、token未設定の外部公開を拒否する
- `GET /`、`GET /snapshot.html`、`GET /json`、`POST /collect`、`POST /reset` の
  methodを固定する。collect対象のfile size、Body、実行時間、同時実行数に上限を設ける
- SQL引数・query stringは保存しない。HTMLは `html/template` のcontextual escapingを使い、
  inline JSON中の `<`、`>`、`&`、`</script>` を安全にescapeする
- collector失敗はHTTP成功に見せかけず、Snapshotの `partial` とhealthへ残す。
  ただし計測失敗を理由にアプリ本体の処理を失敗させない

### 5.8 インフラ側の設計課題と解決

| 課題 | 解決 |
|---|---|
| nginx ログはアプリコンテナから見えない | compose で共有 volume(`nginx_logs`)を両方にマウント。設定例を `examples/` に同梱 |
| Apache も同様 | 同上 |
| Docker ビルドに `.git` が無い | build.args でハッシュ注入(5.6 フォールバック) |
| コンテナ内 `/proc` は自コンテナの PID namespace のみ | 単一台構成なら `pid: "host"`(compose)で全プロセス可視化。Docker Desktop は VM 内プロセスになる旨を注記 |
| 集計メモリの際限ない成長 | 全collectorと正規化cacheに個別上限。SQL/HTTPは各10k、超過分は `(other)` に合算しUI/healthへ警告 |
| debug endpoint の外部露出 | loopback既定、外部利用は明示token。collectにsize/time/concurrency上限 |

## 6. オーバーヘッドと安定性の設計

- 計測オフ: `SQLDriverName` は元driver名を返し、HTTP middlewareは起動時に
  `next` をそのまま返す。リクエスト/クエリ単位の追加分をゼロにする
- 計測オン: 時刻取得、bounded正規化cache、集計、必要なResponseWriterラップを行う。
  「時刻2回 + map加算だけ」とは過小表現せず、各コストをbenchmarkで個別表示する
- sharded mapは初期実装として採用するが、hot key競合を並行benchmarkで測る。
  予算を超えた場合だけ既存keyのatomic加算等へ変更する
- recoverはisutools自身の処理だけに限定する。`next.ServeHTTP`、実driver、resolverの
  panicは元のスタック/意味を保って透過する
- 常駐goroutineは原則起動しない。procstatsはreset/snapshotの2点差分、accesslogは
  collect要求駆動とし、必要なlog flush待ちは期限付きpollで行う

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

## 9.5 外部レビュー(2026-08-03)の反映

GPT-5.6 による設計レビューの指摘と対応。上記各節に反映済みの P0 のほか:

| 指摘 | 対応 |
|---|---|
| SQL 計測時間はクエリ発行〜応答開始まで(行読み取り除外) | 仕様として明記し、レポート脚注に表示。Rows 追跡は複雑化に見合わず v1 は採らない |
| SQL 正規化が弱い(リテラル高カーディナリティ・PII 残留) | **採用**: 文字列リテラル('' / \' エスケープ対応)と数値リテラルを ? にマスク |
| 正規化キャッシュの無制限成長 | 実装済み: 値キー・50,000 件上限・4KB 超は非キャッシュ |
| p95 は log2 バケット近似(最大約2倍幅) | レポートに「p95*(近似・上限側)」と明示 |
| `$upstream_response_time` はリトライ時カンマ区切り複数値 / Apache `%B` は実送信量でない(`%O` が必要)/ `upstime="-"` ≠ 静的配信成功 | M3 実装時の仕様に取り込み(5.4 注記) |
| gqlgen は InterceptResponse でなく **InterceptOperation** が operation 計測の中心(subscription は response 複数回) | M4 実装時に反映 |
| gqlgen / quic-go を本体依存にしない | アダプタは**ネストモジュール**(別 go.mod)として分離 |
| CI の ns 単位ハードゲートは共有ランナーで不安定 | ベンチは informational とし、リリース前に対象ホストで on/off ABBA 比較を実施 |
| recover の範囲がアプリの panic を握り潰す恐れ | 実装済み: recover は計測フック内部のみ。アプリ・ドライバの panic は透過 |
| /debug エンドポイントの保護 | ISUCON 用途では bench が /debug を叩かない前提とし、v1 は「外部公開しない」運用注記 + 将来 `ISUTOOLS_TOKEN` オプション |

## 10. 決定事項ログ

- 2026-08-03: 閲覧方式は **snapshot-first**(リモートで計測 → 自己完結 HTML を
  ダウンロード → 手元 PC で閲覧)に決定。ライブ表示は補助として残す(5.7)
- 2026-08-03: リポジトリは **public + MIT** で確定(Docker ビルド内 `go get` 対応)
- 2026-08-03: snapshot.html には**最小限の JS を許容**(列ソート切替のみ。
  外部リソース読み込みは引き続き禁止、自己完結性は維持)
- 2026-08-03: モジュール名は `isutools` で確定

## 11. 未決事項

- [ ] パス正規化ルールの注入方法(コード / 環境変数 / 設定ファイル)
- [ ] `(other)` 合算の上限値のデフォルト(暫定: SQL/HTTP 各 10,000 キー)
