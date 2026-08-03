# isutools 設計書

`github.com/ekusiadadus/isutools` — ISUCON 向けオールインワン計測モジュール

- Status: Implementation Candidate v3 (2026-08-03、M0 core/M2をlocal実装済み。
  private-isuへのv0.2再統合、ABBA性能gate、collector横断atomic swapは未完)
- Runtime: Go 1.24+
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

1. **最小組み込み** — SQL + loopback管理UIの基本導入は1行、HTTP計測を含めても2行を目標とする。
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
├── isutools.go      // SQLDriverName() / HTTP() / Handler() + loopback admin
├── sqlstats/        // SQL: ドライバプロキシ + メモリ内集計
├── httpstats/       // HTTP in: ミドルウェア集計 (h1/h2, パス正規化)
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
既定値ではアプリのルーター・nginx を経由せず外部にもbindしない(P0-6 の安全境界に合致)。
loopbackはtokenなしで即閲覧できる。Docker内の `0.0.0.0` bind + host側
`127.0.0.1` publishでは `ISUTOOLS_ALLOW_UNAUTHENTICATED=1` を明示した場合だけ
tokenなしを許可する。`ISUTOOLS_TOKEN` は外部公開時の保護であり、設定時は
Bearer認証を全endpointへ適用する。
Docker ではhost側も `127.0.0.1` に限定したport mappingで到達性を制御する。`Handler()` も公開して
おり、アプリと同一ポートに載せたい場合は従来どおり任意のルーターに Mount できる。

#### 管理サーバの認証ポリシー(確定・v0.2.1、2026-08-03 ユーザー決定)

**前提とする閲覧経路は「SSH トンネル経由のみ」である。** 管理ポートは
インターネットにも LAN にも直接公開しない。この前提を崩す構成にする場合は
必ず `ISUTOOLS_TOKEN` を設定すること。

認証の全モード(実装と1対1対応):

| bind 先 | ISUTOOLS_TOKEN | ISUTOOLS_ALLOW_UNAUTHENTICATED | 挙動 |
|---|---|---|---|
| loopback(既定 `127.0.0.1:19191`) | 不要 | 不要 | **認証なし**で提供。到達性が OS により loopback に限定されているため |
| 非 loopback(`:19191` 等) | 設定あり | — | 全 endpoint に Bearer 認証(SHA-256 定数時間比較)。ブラウザは `?token=` 1回 → HttpOnly Cookie |
| 非 loopback | 未設定 | `=1` を明示 | **認証なし**で提供 + 起動ログに警告 + collector health を `degraded` に固定(ダッシュボード上でも常時見える) |
| 非 loopback | 未設定 | 未設定 | **fail closed**: 管理サーバを起動しない(黙って無防備公開しない) |

`ALLOW_UNAUTHENTICATED=1` を正当化できるのは、**プロセスの外側で到達性が
制限されている場合だけ**である。具体的には本プロジェクトの private-isu 構成:
コンテナ内 bind は `0.0.0.0:19191` だが、(1) Docker の port publish が
`127.0.0.1:19191` に限定され、(2) そこへは SSH トンネルでしか到達できない。
この2段の制限が「アプリ内 token と同等以上」の防護であるため、token を
外して UX(素の `http://localhost:19191/` で閲覧)を優先する — これが
本決定の全根拠である。**Docker なしで NIC に直接 bind する本番 ISUCON
サーバでは (1)(2) が存在しないため、この env を設定してはならない。**

経緯: v0.2.0 は「非 loopback = token 必須」で fail closed のみだった。
SSH トンネル前提の実運用でブラウザ閲覧の摩擦が問題となり、ユーザー決定で
「明示 opt-in の無認証モード」を追加した(token 機構自体は廃止していない)。
なお、認証を緩める変更のコミットは Claude 側の権限ゲートで拒否されるため、
ユーザー自身の手でコミットされた(0eaa475)。

**on/off 契約**(レビュー P0-1 反映): 有効判定は `SQLDriverName()` の1箇所に集約する。
`ISUTOOLS=off` ならラップせず素のドライバ名を返す(ゼロコスト)。登録失敗時も
素の名前を返す **fail-open** で、計測がアプリの起動を壊すことは決してない。
判定は起動時に1回で、動的切替はしない。旧案の `ISUTOOLS_SQL_POSTFIX` 環境変数は
「off なのにプロキシ名で接続して起動失敗する」矛盾があったため廃止した。

- SQL登録・admin bindがfail-openした場合はstructured warningを必ずlogし、
  Handlerへ到達できる場合はcollector healthとHTML/JSONの `partial` にも残す
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
- M2実装のフォーマットは下記の**明示nginx LTSVのみ**。combined+`$request_time` と
  Apache combined+`%D` はM3へ送る。自動判別で曖昧なログを推測して読まない
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
                    └─▶ immutable Snapshot schema v3
                          (meta + health + sql + http + gql + accesslog + proc)
                            ├─ GET /              … ライブ表示
                            ├─ GET /snapshot.html … 自己完結HTMLエクスポート
                            ├─ GET /json          … 機械可読
                            └─ POST /reset        … 次generationへ切替
```

- **`GET /snapshot.html`** — データ埋め込みの完全自己完結 HTML(外部リソース
  ゼロ・インライン CSS/最小限の JS)。ダウンロードして**ダブルクリックで開くだけ**
  で閲覧できる。初期表示は合計時間降順ソート済み、列クリックで再ソート
- meta には `schema_version`、tool version、generation、計測開始/終了、
  git hash、dirty state、host、各collector healthを含める。scoreはアプリから
  知り得ないため、bench scriptが結果取得後にmetaへ付与するかファイル名だけに含める
- ライブ表示(`GET /`)も同じSnapshotを入力にする別rendererとして残す。
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

**実装上の現在境界**: SQLとHTTPはそれぞれ開始時generationをpinし、同時resetを
直列化して旧in-flight完了まで待つ。admin handlerはcollectorごとのresetを逐次実行するため、
collector横断のswapは1命令のatomic operationではない。通常のbench運用では
`POST /reset` の応答後に負荷を開始することで同じ区間になる。継続traffic中の厳密な
横断境界が必要なら、全collector共有のbarrierを導入することをv0.2 release gateに残す。

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

- 自動管理serverは既定で `127.0.0.1:19191` にbindし、アプリrouter/reverse proxyから
  分離する。SSH tunnelまたは対象host上のbench scriptから利用する
- loopbackはtoken不要。Docker内で非loopback bindしてもhost側port mappingを
  `127.0.0.1` に限定し、`ISUTOOLS_ALLOW_UNAUTHENTICATED=1` を明示した場合はtokenなしでよい。
  この明示opt-in時はwarningとhealth degradedで外部露出リスクを表示する。
  tokenもopt-inもない非loopback bindはlisten前にfail-closedとする
- `ISUTOOLS_TOKEN` 設定時は `Authorization: Bearer <token>` をSHA-256後に定数時間比較。
  ブラウザ向けには初回 `/?token=<token>` でHttpOnly cookieを発行し、以後は通常URLで閲覧できる
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
| debug endpoint の外部露出 | 独立loopback admin server。外部公開時はBearer token、Docker host-loopbackは明示的なunauthenticated opt-in。collectにsize/time/concurrency上限 |

## 6. オーバーヘッドと安定性の設計

- 計測オフ: `SQLDriverName` は元driver名を返し、HTTP middlewareは起動時に
  `next` をそのまま返す。リクエスト/クエリ単位の追加分をゼロにする
- 計測オン: 時刻取得、bounded正規化cache、集計、必要なResponseWriterラップを行う。
  「時刻2回 + map加算だけ」とは過小表現せず、各コストをbenchmarkで個別表示する
- sharded mapは初期実装として採用するが、hot key競合を並行benchmarkで測る。
  予算を超えた場合だけ既存keyのatomic加算等へ変更する
- recoverはisutools自身の処理だけに限定する。`next.ServeHTTP`、実driver、resolverの
  panicは元のスタック/意味を保って透過する
- 常駐goroutineはloopback管理serverのserve loopだけに限定する。collectorは常駐させず、
  procstatsはreset/snapshotの2点差分、accesslogはcollect要求駆動とし、
  必要なlog flush待ちは期限付きpollで行う

## 7. TDD・テスト戦略

開発は全機能 RED → GREEN → REFACTOR で進める。実装 PR には必ず先行するテストを含める。

| パッケージ | テスト |
|---|---|
| internal/agg | 表駆動 + `-race` + hot key並行benchmark + cap直前/超過 + reset/snapshot競合 |
| sqlstats | fake driver proxy / Exec・Query error / 正規化privacy / cache上限 / 3ドライバcompile smoke |
| httpstats | h1/h2 / optional interface透過 / panic再伝播 / streaming / h3 integration(build tag) |
| gqlstats | operation/subscription / unnamed / body上限・復元 / unsupported transport health |
| accesslog | golden / 複数upstream / inode rotate / copytruncate / buffered flush / malformed・巨大行fuzz |
| procstats | start/end差分 / PID再利用 / process出現・消滅 / permission failure / RSS |
| buildinfo | VCSあり・なし / dirty・clean・unknown / ldflags・env優先順位 |
| web | schema互換 / sort / HTML escape / method・loopback・token / 同時reset・snapshot |

- 機能CI: `go vet` / `go test -race` / 集約cover profile 80% / parser fuzz seed /
  optional adapterごとのcompile・integration job
- 共有GitHub-hosted runnerのbenchmarkはinformational。ns単位のhard failは行わない
- 性能ゲート: 同一binary・同一host・同一初期データで A=off / B=on のABBAを
  複数組実施し、score/throughput/p95/error rateを保存する。ウォームアップと外れ値規則を
  先に固定し、スコア影響 < 2% を信頼区間込みで確認してからrelease tagを打つ

## 8. マイルストーンと受け入れ条件

0. **M0(設計契約・release gate、local core実装済み)**: on/off、health、generation、
   Snapshot schema v3、method固定、nonloopback Bearer/明示unauthenticated opt-inをcontract testで固定。
   collector横断barrierとremote性能gateは未完
1. **M1(v0.1.0、実装・private-isu基本統合済み)**: internal/agg + sqlstats + web +
   buildinfo + sysinfo + loopback admin server。private-isu ABBA比較を残す
2. **M2(v0.2、local candidate実装済み)**: httpstats(h1/h2) + nginx LTSV accesslog +
   区間差分procstats + dashboard/DB schema。private-isu再統合とABBAを残す
3. **M3(v0.3)**: Apache + gqlstats/gqlgen adapter
4. **M4(v1.0)**: WebSocket/SSE接続計測 + HTTP/3 optional integration + 全体ABBA gate

各milestoneは次をすべて満たすまで「完了」としない:

- 文書の公開API例がcompileし、実装との不一致がない
- 欠損・上限超過・未対応入力をsilent successにせずhealth/partialへ出す
- `go test -race`、coverage gate、該当integration testが通る
- 新collectorがSnapshot schemaとreset generation契約に従う
- remote/deployed/private-isuで未検証の結果を、ローカルtest成功から推測して完了扱いしない

## 9. Phase 2 候補 — 感想戦・登壇記事の分析から

出典: takonomura「ソロISUCONの戦い方」(ISUCON夏祭り2023)、
mazrean「ISUCON14感想戦で40万点超えました」(traP blog 2024-12)。

### 高優先(次に作るべきもの)

1. **汎用カウンタ/ゲージ API**(mazrean: キャッシュ hit/miss・状態分布を
   Grafana で可視化していた)
   `isutools.Count("ride_status_cache_hit")` の1行でレポートに集計表示。
   オンメモリキャッシュ導入後の効果測定は終盤で必ず必要になる。
   Prometheus/Grafana なしで同じ意思決定ができるのが本モジュールの流儀
2. **複数世代のスナップショット差分UI**(takonomura: 「スコアが変わらなくても
   ボトルネックが移動しただけ」を計測で確認して判断する)
   v1の「直前1世代を保持」を拡張し、ローカルJSON間でクエリ毎totalの増減を表示。
   改善が効いたか・ボトルネックが移動しただけかを判定する
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

5. **WebSocketライブラリ別メッセージ計測adapter** — gorilla/websocket等を
   型安全に包み、message type/count/payload bytesを取得。generic net.Conn方式は採らない
6. **異常検知の警告表示**(mazrean: `connection reset` で fail)
   accesslog の status 499 / 5xx 急増・reset をレポート上部に警告として出す

### マルチホスト対応【将来的な実装予定・未着手】

**status: 未実装。分割するかどうか・どう分割するか(役割別か、DB だけ
別出しか等)は現時点で未決定**であり、どのトポロジになっても成立するよう
方針だけを固めておく。実装はサーバー分割の方針が決まった時点で行う。

1. **ホスト単位で自己完結**(トポロジ非依存の基本原則): 計測プロセスが
   動くホストごとに admin server を1つ持ち、snapshot は `meta.host.hostname`
   で自己記述する(これは実装済みの性質であり、分割しても壊れない)
2. **bench スクリプトが全ホストへ fan-out**: `/reset` を全台 → ベンチ →
   `/save?score=` を全台。ファイル名にホスト名を含める(`_host<name>`)
3. **アプリの載らないホスト**(DB 専用・nginx 専用などになった場合):
   procstats + accesslog + admin だけの軽量単体バイナリ `isutools-agent` を
   用意する。なお dbinspect は DSN 経由なので、DB がどのホストにあっても
   アプリ側からリモート検査でき変更不要
4. **閲覧**: まずは「ホスト毎の snapshot を並べて見る」で開始(実装ゼロ)。
   その後 run index がホスト別ファイルを同一実行としてグルーピング表示 →
   最終的に merged view(SQL は全台合算・HTTP はホスト別列)を検討

### 低優先・スコープ外と判断したもの

7. **AST 一括変換**(takonomura: DB 呼び出しに Context を一括で渡すツール)
   → 価値はあるが計測モジュールとは責務が別。作るなら `isutools-rewrite` として別リポジトリ
8. **PGO プロファイル書き出し**(mazrean: Go 1.24 化・PGO)
   → pprof 統合(候補3)の副産物として `GET /pprof/default.pgo` を出すのは容易。優先度は低
9. **badger・イベントバス・SSE 実装テンプレ**(mazrean)
    → アプリ実装の話であり計測ツールの責務外。スニペット集として別管理が適切

### 記事から取り込んだ設計上の教訓(機能ではなく原則)

- 「機能が多いものより、自分が理解できてその場で書き換えられるもの」(takonomura)
  → 依存ゼロ UI・小さなcore・optional adapter分離を優先する。総行数2,000のhard capで
  テストや安全性を削らず、package責務とcyclomatic complexityで管理する
- 「無駄な変更をしない。判断材料になる計測結果を常に持つ」(takonomura)
  → スナップショット差分(候補2)を UI の一等市民にする
- 「終盤はネックが DB → アプリ CPU → GC/map へ移動する」(mazrean)
  → SQL だけでなく procstats / pprof を最初から同居させる本設計の妥当性を裏付け

## 10. 設計レビュー反映状況(2026-08-03)

「設計へ採用」と「実装済み」を混同しない。各項目は該当milestoneの受け入れ条件で
再検証する。

| 指摘 | 設計上の決定 | 現在の実装状況 |
|---|---|---|
| SQL on/off契約 | `SQLDriverName`へ一本化、旧suffix環境変数を廃止 | M1実装済み |
| SQL時間にRows読取を含まない | query dispatch durationと明記 | M1実装・UI脚注あり |
| SQLリテラル/PIIとcache増大 | literal mask、引数非保存、値キーcache 50k、4KB超非cache | M1実装済み、dialect fuzzは未完 |
| p95がlog2近似 | bucket上限値としてUI/JSONに表示 | M1 UI実装済み |
| buildinfo欠落をcleanと誤認 | dirty / clean / unknownの3値 | 設計反映、M1実装conformance未完 |
| fail-openが欠損を隠す | collector health / partialをSnapshotへ追加 | schema v3/HTML/JSONへ実装済み |
| resetと同時計測の境界 | generationをatomic swapし旧世代を凍結 | SQL/HTTP単体と同時reset直列化は実装済み、collector横断barrier未完 |
| WebSocket汎用frame wrapper不成立 | v1はconnection/wire bytes、messageはPhase 2 adapter | M4予定 |
| procがベンチ後を測る | reset/snapshotの区間差分 | M2 local実装済み |
| upstream複数値、Apache `%B`誤解 | 5.4のraw/合計/試行数、`%O`区別 | nginx LTSVはM2 local実装済み、ApacheはM3予定 |
| gqlgen operation単位 | `InterceptOperation`、subscription重複回避 | M3予定 |
| optional dependency | gqlgen/quic-goをcoreから分離 | package/module境界は実装前に確定 |
| 共有CIのns hard gate | informational + 固定host ABBA | 手順反映、remote検証未実施 |
| recoverがアプリpanicを隠す | isutools内部だけrecover | SQL hookとHTTP再panicを実装・test済み |
| debug endpoint露出 | 独立loopback admin、外部公開時はBearer認証 | localhost無認証、Docker明示opt-in、token/header/query-cookie、method固定を実装済み。collect resource上限は未完 |

## 10.5 v1.0 ギャップと優先順位(2026-08-04 整理)

現況: v0.5.0。実装済み = SQL(3DB・リテラルマスク・世代ストア)/ HTTP(h1/h2)/
nginx ログ(LTSV+JSON+alp キー・ローテ追随)/ procstats+CPU total / dbinspect(MySQL)/
pprof(endpoint+ベンチ連動自動採取)/ snapshot-first UI(実行一覧・日時ID詳細・live)/
JST / score-in-meta / health・generation / 管理サーバ認証3モード / CI(12pkg・cover 86%+)。
private-isu 実戦で 0→299,668 を計測しながら達成(dogfooding 済み)。

### v1.0 必須(このギャップを埋めたら 1.0 を切る)

| # | 項目 | 理由・内容 |
|---|---|---|
| 1 | **パス正規化ルール注入**(`ISUTOOLS_PATH_RULES`) | httpstats で `/@user` が個別集計される実害が出ている。nginx map 相当の regex→置換を env/Option で注入 |
| 2 | **スナップショット差分ビュー** | prev はデータとして保持済みで UI 未実装。「改善が効いたか・ボトルネックが移動しただけか」の比較こそチューニングループの核(takonomura 原則) |
| 3 | **汎用カウンタ/ゲージ API** | 次の定石=オンメモリキャッシュ導入時に hit/miss 計測が必須になる(`isutools.Count("x")` 1行) |
| 4 | **WebSocket/SSE 接続分離**(5.2.1 実装) | 設計のみで未実装。WS を使う問題でレイテンシ統計が壊れるのを防ぐ+アクティブ接続ゲージ |
| 5 | **collect/save の資源上限** | レビュー P0-6 残項目(size/time/concurrency 上限)。リリース硬化 |
| 6 | **on/off ABBA オーバーヘッド検証の定型化** | リリースゲート(§7)を手順化して 1.0 の根拠にする(現状: 個別ベンチのみ) |

### v1.0 追加要望(2026-08-04 ユーザー要望で追加)

| # | 項目 | 設計方針 |
|---|---|---|
| 7 | **設定アドバイザ(advisor)** | 「ISUCON 必須級だが未設定」を検出してレポートに常時表示する新コレクター。検査対象: **DSN**(`interpolateParams` = プリペアドステートメント往復の削減)、**MySQL 変数**(`max_connections` / `innodb_buffer_pool_size` vs データ量 / `slow_query_log`)、**nginx conf**(gzip / upstream keepalive / worker_connections / sendfile / expires — `ISUTOOLS_NGINX_CONF` でconfを読み取り)、**OS**(`somaxconn` / `ip_local_port_range` / nofile 上限)、**Go**(GOMAXPROCS vs cgroup CPU クォータ)。各チェックは fail-open、ok/missing/warn/info の4値 |
| 8 | **シナリオ負荷試験の可視化(k6 連携 + 行動フロー)** | 負荷生成は **k6 を外部ツールとして採用**(再実装しない。書籍4章の方針)。isutools 側は (a) private-isu 用のシナリオ例を examples/ に同梱、(b) **行動フロー可視化**: nginx ログにセッション識別子(セッション Cookie の短縮ハッシュ `sess:`)を追加し、セッション単位の遷移(`/login → / → /posts/N`)を集計して「ユーザーがどうアプリを使っているか」の上位パスをレポート表示、(c) 将来: k6 の JSON 出力(`--out json`)の取り込み。**Cookie 値そのものは記録しない**(ハッシュのみ、プライバシー/流出対策) |

### v1.0 に入れない(1.x 以降)

- pprof top 関数のレポート内表示(現状のファイル DL + `go tool pprof -http` で実用十分)
- JSON→Markdown フォーマッタ(Discord/GitHub 連携強化)
- Apache ログ(`%D`/`%O`)対応・gqlstats(必要になった時点で)
- HTTP/3 統合テスト(quic-go 環境依存)
- クロスコレクター共有 generation gate(IMPLEMENTATION_STATUS 記載の硬化項目。
  ベンチ自動化が「reset 完了後に負荷開始」を守る限り実害なし)
- マルチホスト対応(トポロジ未決定・「将来的な実装予定」節を参照)
- isutools-agent 単体バイナリ(マルチホストとセット)

## 11. 決定事項ログ

- 2026-08-03 (v0.2.1): **管理サーバの無認証モードを明示 opt-in で追加**
  (`ISUTOOLS_ALLOW_UNAUTHENTICATED=1`)。前提は「SSH トンネル + Docker の
  `127.0.0.1` 限定 publish」の2段制限であり、**token 機構は廃止していない**
  (外部公開時の必須保護として維持、未設定かつ opt-in なしの非 loopback は
  fail closed のまま)。詳細は 4章「管理サーバの認証ポリシー」参照
  (ユーザー決定・ユーザー自身のコミット 0eaa475)
- 2026-08-03: 閲覧方式は **snapshot-first**(リモートで計測 → 自己完結 HTML を
  ダウンロード → 手元 PC で閲覧)に決定。ライブ表示は補助として残す(5.7)
- 2026-08-03: リポジトリは **public + MIT** で確定(Docker ビルド内 `go get` 対応)
- 2026-08-03: snapshot.html には**最小限の JS を許容**(列ソート切替のみ。
  外部リソース読み込みは引き続き禁止、自己完結性は維持)
- 2026-08-03: モジュール名は `isutools` で確定
- 2026-08-03: SQL有効判定は `SQLDriverName` に一本化し、`ISUTOOLS_SQL_POSTFIX` は廃止
- 2026-08-03: 管理UIは既定 `127.0.0.1:19191` の独立server。基本導入は1行
- 2026-08-03: Snapshotはschema versionとgenerationを持ち、1世代前をv1で保持。
  複数世代diff UIはPhase 2
- 2026-08-03: WebSocket v1は接続レベルまで。generic frame wrapperは採用しない
- 2026-08-03: procstatsはreset/snapshot区間差分。表示時500ms sample案は廃止
- 2026-08-03: SQL/HTTPの既定キー上限は各10,000、正規化cacheは50,000。
  超過・欠損はhealthへ表示する
- 2026-08-03: localhostは設定なしで閲覧可能。Docker host-loopback publishのtoken省略は
  `ISUTOOLS_ALLOW_UNAUTHENTICATED=1` の明示opt-inを要求し、health warningを出す
- 2026-08-03: DB schema/health/M2 collectorsを加えたSnapshotをschema v3とする。
  SQL/HTTP単体のgeneration swapは実装済み、collector横断barrierは別gateとして残す

## 12. Architecture Decision Records

### ADR-001: 起動時設定と管理チャネル

- **Context**: suffix環境変数とglobal offが競合し、アプリ起動失敗または意図しないproxy利用が起きる
- **Decision**: `SQLDriverName`が起動時にraw/proxyを選び、成功時だけ独立loopback adminを起動する
- **Consequences**: 基本導入は1行。fail-openはアプリを守る一方、health実装なしでは計測欠損を見逃す
- **Status**: Implemented。localhost無認証、Docker明示opt-in、non-loopback Bearer、health表示を検証済み

### ADR-002: GenerationベースSnapshot

- **Context**: resetと進行中計測が競合すると、比較対象のベンチ区間が曖昧になる
- **Decision**: resetで新generationへatomic swapし、旧generationをimmutable Snapshotとして凍結する
- **Consequences**: collectorは開始時generationへ最後まで記録する必要がある。実装は複雑になるが比較可能性を優先する
- **Status**: Partially implemented。SQL/HTTP単体conformanceはrace test済み、collector横断barrierは未完

### ADR-003: 長寿命接続の分離

- **Context**: WebSocket/SSEを通常HTTPへ混ぜるとavg/p95が壊れ、generic frame wrapperも型安全に作れない
- **Decision**: v1は接続数・active・duration・wire bytesだけを専用tableへ記録する
- **Consequences**: message単位の判断にはPhase 2 adapterが必要
- **Status**: Accepted、M4予定

### ADR-004: Fail-openと安全境界

- **Context**: 計測器がアプリを止めてはならないが、silent failureと外部公開も危険
- **Decision**: isutools内部だけrecoverし、欠損はhealth/partialへ出す。adminはloopbackを既定とする
- **Consequences**: 外部公開にはtokenまたは外部認証を推奨。Docker host-loopback publishは
  明示opt-inによりtokenなしで使える。opt-inなしの非loopbackはfail-closedとなる
- **Status**: Implemented for core/admin。accesslog/proc固有healthもsnapshotへ保持

## 13. 未決事項

- [ ] パス正規化ルールの注入方法(コード / 環境変数 / 設定ファイル)
- [ ] gqlgen / quic-go adapterをnested moduleにするか、別repository/moduleにするか
- [ ] collector横断generation barrierをv0.2で必須にするか、reset応答後にbench開始する運用契約で十分か
- [ ] scoreをSnapshot JSONへ後付けするCLI/API、またはファイル名だけに限定するか
- [ ] 背景分析ログの保存先と一次資料リンク
