# 01: DB target registry(FirstConn の置換)— v6

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `sqlstats`, `dbinspect`, `advisor`, `isutools.go`

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] 論理 target と接続 credential の分離**。v5 の
   「1 TargetID = 1 DSN」前提は**撤回する**。この前提では 09 が要求する
   最小権限 EXPLAIN ユーザーを登録できない(同一 ID での再登録は重複
   エラー、別 ID にすると 04 の digest と結合できない)。
   → **`Purpose`(app / stats / explain)を型付き enum で導入**し、
   `RegisterDBInspector(targetID, purpose, driverName, dsn)` で
   **同一 targetID に複数の credential を紐づける**。
   `Inspect` は `Inspect(ctx, targetID, purpose, fn)` に変更する
   (v5 の `Inspect(ctx, id, fn)` は撤回)。
   1 targetID 配下の全 purpose は**同一の論理 identity** を共有し、
   04/06/09/10 は従来どおり targetID だけで結合する(§Purpose)
2. **[CRITICAL] 計測区間汚染の防止(接続衛生の registry 側の担当分)**。
   v5 は stats 用接続の DSN をそのまま使う前提だったため、
   collector 自身のクエリが**アプリ schema の digest として
   performance_schema に記録され、04 の計測区間を汚染していた**。この
   v5 の暗黙前提は撤回する。
   → **`PurposeStats` / `PurposeExplain` の接続は既定データベースを
   持たない DSN(`DBName=""`)へ正規化してから開く**。あわせて registry は
   アプリ schema 名を `TargetInfo.Schema`(非 secret)として保持し、
   呼び出し側が `WHERE SCHEMA_NAME = ?` の**バインド引数**として使えるように
   する(§接続衛生)
3. **[MEDIUM] 自動 TargetID の hash 長と canonical tuple の曖昧さ**。
   v5 の `hash8 = sha256(...)[:8]` は**撤回する**(「8 hex 文字 = 32bit」
   とも「8 バイト = 64bit」とも読め、前者なら本書が §パース不能 DSN で
   自ら否定した 32bit 公開 hash に逆戻りする)。また canonical tuple が
   **net(tcp/unix)を欠いていた**。
   → tuple を `driver + net + canonical addr + database` と定義し、
   hash は **sha256 の先頭 16 バイト(128bit)を base32(RFC 4648・小文字・
   padding なし)で 26 文字**に固定する。明示 ID にも**長さ・文字種の
   制約**を課し、**ID 比較は byte 単位の完全一致**であることを明記する
   (§TargetID)
4. **[MINOR] ヘッダ版数の陳腐化**。v3 のままだったので v6 に更新
   (本書は第5回レビュー差し戻し対応版)
5. **(06 からの参照要求)** 「自動導出 ID を手書きしてはならない」規則と
   lookup API(`TargetIDForDSN` / `Targets`)を明文化し、06 の
   `WatchDBPool` 使用例が引用できる形にした(§ID の受理規則)

## v3 での変更点(レビュー差し戻し対応)

1. **[HIGH] 接続順 `dbN` 命名の廃止**。並行・lazy 接続する 4 shard では
   再起動ごとに名前が入れ替わり、06 の pool 名とも対応しない。
   → **安定した TargetID を第一 API** にする:
   - 既定 ID は接続順ではなく **DSN の構造化パースから決定的に導出**
     (driver + net + host:port + database)。再起動・接続順に依存しない
   - 明示命名 API `isutools.RegisterDBTarget(id, driverName, dsn)` を提供し、
     衝突(同一 endpoint+db の別用途)や短い別名が必要な場合に使う。
     自動導出は fallback
   - sqlrows(04)・dbinspect・queryplan(09)・DB pool(06)・
     agent(10 の targets.json)は**同じ TargetID 名前空間**で結合する
2. **[MEDIUM] Inspect の接続所有権**。呼ぶたびに `sql.Open` する v2 案は
   04/09 のファンアウトで接続プールを大量生成し、callback が
   `*sql.DB` を保持できてしまう。
   → registry が **(target, purpose) ごとに MaxOpenConns(1) の inspector を
   1 つ所有**して再利用し、callback には raw `*sql.DB` ではなく
   **制限付き query interface** を渡す
3. **[MEDIUM] redaction の構造化**。文字列規則では escaped userinfo や
   driver 固有構文で漏れる。→ **driver 公式 parser(mysql.ParseDSN 等)で
   構造化し、allowlist 項目のみから表示文字列を再構築**。未知パラメータは
   原則非表示
4. `HasDSNParam(name, param) bool` の 3 値問題(false / 未設定 /
   解析失敗が区別不能)→ typed struct + `(value, known)` 形式へ変更
5. wrapper が `DriverContext` / `OpenConnector` を維持することを要件化

## ゴール

1. 全 DSN を**安定した TargetID** で登録・列挙できる(shard 対応)
2. DSN 文字列(credential)を registry の外に出さない
3. 04/06/09/10 が同一 TargetID で結合できる
4. 用途別 credential(app / stats / explain)を**同一 TargetID に**
   紐づけ、09 の最小権限ユーザーを結合を壊さずに導入できる
5. collector 自身のクエリを計測対象 schema の digest に混入させない
6. 既存の FirstConn 利用箇所を移行する

## 設計

### TargetID

```go
// canonical tuple(v6 で確定。credential は含めない):
//   tuple = driverName "\x00" net "\x00" canonicalAddr "\x00" database
//     driverName    : sql.Open に渡す実 driver 名(proxy 名ではなく "mysql")
//     net           : "tcp" | "unix"(未指定は driver 既定の "tcp")
//     canonicalAddr : tcp  → lowercase host + ":" + port(省略時は driver 既定
//                            3306 を補完。IPv6 は net.ParseIP().String() の
//                            正規形を角括弧なしで使う)
//                     unix → filepath.Clean した絶対 socket path
//     database      : DBName をそのまま(MySQL の識別子は OS により
//                     大小区別があるため正規化しない)
//
// 自動 ID(接続順非依存): 表示名と内部 ID を分離する。
//   alias : slug(driver "-" addrPart "-" database) を 32 文字に切り詰め
//           (許容文字 [a-z0-9._-]、それ以外は "_"、連続は 1 つに畳む)
//           例: "mysql-db1_3306-isuconp"
//   ID    : alias + "-" + hash26
//   hash26: base32(RFC 4648・小文字・padding なし, sha256(tuple)[:16])
//           = 128bit / 26 文字(v6 修正: v5 の "hash8" は撤回。
//             8 hex 文字なら 32bit で、本書が §パース不能 DSN で自ら
//             否定した強度に逆戻りするため。128bit なら 16 target 規模で
//             衝突は現実的に起こらない)
//   → 自動 ID の全長は 32 + 1 + 26 = 最大 59 バイト(明示 ID の上限 64 以内)
//
// 同一 canonical tuple → 同一 target として dedup。
// **異なる canonical tuple が同一 ID になった場合は dedup せずエラー**
// (04/06/09/10 の集計を壊すため。RegisterDBTarget の明示 ID どうしの
//  衝突も同様にエラー)。

// 明示命名(第一 API)。SQLDriverName より前に呼ぶ。
// driverName は必須引数(v4 修正: MySQL DSN に driver 種別は含まれず、
// driver 名は sql.Open の別引数であり DSN から特定できないため)。
func RegisterDBTarget(id, driverName, dsn string) error
// **論理 target を新規作成し、その PurposeApp credential を登録する**
// 唯一の API(用途別 credential の追加は RegisterDBInspector。
// v6 明確化: 両者は役割が異なり、片方が他方の糖衣ではない)。
// id 重複・空・不正文字・未知 driver はエラー。
```

**明示 ID の制約(v6 追加)**

| 項目 | 規則 | 理由 |
|---|---|---|
| 長さ | 1〜64 バイト | JSON key / agent 設定 / HTML の見出しに載るため |
| 文字種 | `[A-Za-z0-9._-]` のみ(ASCII) | 非 ASCII を禁じることで NFC/NFD 正規化の曖昧さが原理的に発生しない |
| 比較 | **byte 単位の完全一致**(`==`)。大小同一視・Unicode 正規化・前後空白の trim を一切行わない | 「見た目が同じで別 ID」を許さないより、「バイトが違えば別 ID」と単純化するほうが 04/06/09/10 の結合を検証しやすい |
| 検証失敗 | `ErrInvalidTargetID`(登録時に即エラー。fail-fast) | 起動時に気付ける |

**パース不能 DSN の扱い(v4 修正・v6 で維持)**: v3 の
`unparsed-<sha256(dsn)[:8]>` 自動 ID は廃止する
(credential 変更だけで ID が変わり安定でない・32bit 公開 hash は
衝突しやすく、既知 DSN 形式への弱い credential のオフライン照合器にも
なり得るため)。パース不能な DSN は**自動登録しない**:
`RegisterDBTarget` による明示登録を必須とし、未登録のまま観測された
場合は health に「unparsed DSN — RegisterDBTarget が必要」を記録して
当該接続を target 化しない(fail-open)。

### ID の受理規則(06 が引用する規則・v6 追加)

- **既存 ID のみを受理する消費側 API**: `WatchDBPool`(06)/
  `RegisterDBInspector` / `Inspect`。未登録 ID は `ErrUnknownTarget` を返す
- **target を新規作成するのは登録側 API だけ**: `RegisterDBTarget`、
  proxy driver による DSN 観測、10 の targets.json の各エントリ
  (= `RegisterDBTarget` 相当)の 3 経路
- **人間が選んだ短い ID を使いたい場合は、先に
  `RegisterDBTarget(id, driverName, dsn)` を呼ぶ**。以降その canonical
  tuple の自動導出は行われず、明示 ID が正となる
- **自動導出 ID を手書きしてはならない**(末尾に 26 文字の hash が付き、
  人間が書ける形ではない)。取得には次を使う:

```go
// この DSN の canonical tuple に registry が現在割り当てている ID を返す
// (明示登録済みならその明示 ID、proxy 観測済みなら自動導出 ID)。
// 未登録・未観測、または DSN がパース不能なら ok=false。
// **副作用として登録は行わない**(lookup 専用)。
func TargetIDForDSN(driverName, dsn string) (id string, ok bool)
func Targets() []TargetInfo
func Target(id string) (TargetInfo, bool)
```

- 06 の使用例は「`RegisterDBTarget("db1", "mysql", dsn)` →
  `WatchDBPool("db1", db.DB)`」の順で書けば ID を手書きせずに済む
  (v5 の 06 例が書いていた `"mysql-db1_3306-isuconp"` は hash suffix を
  欠くため、上記の規則では未登録 ID となりエラーになる)

### Purpose(v6 追加 — 論理 target と credential の分離)

```go
type Purpose string

const (
    // アプリ自身のトラフィック接続。schema 名・Display・Features の供給元。
    // proxy driver の観測または RegisterDBTarget で必ず 1 つ存在する。
    PurposeApp     Purpose = "app"
    // dbinspect(SHOW STATUS/VARIABLES)と 04(performance_schema digest)用。
    PurposeStats   Purpose = "stats"
    // 09 の最小権限 EXPLAIN 用(対象 schema と performance_schema への
    // SELECT のみ。DML なし・stored function の EXECUTE なし)。
    PurposeExplain Purpose = "explain"
)

// 既存 targetID に用途別 credential を追加する。
// targetID は既に registry に存在すること(§ID の受理規則)。
// purpose は PurposeStats / PurposeExplain のみ受理する。
func RegisterDBInspector(targetID string, purpose Purpose, driverName, dsn string) error
// 未知 targetID          → ErrUnknownTarget
// 未知 purpose / PurposeApp → ErrInvalidPurpose
//   (PurposeApp は target の identity そのものなので RegisterDBTarget
//    または proxy 観測でのみ設定される。後付けの差し替えを許すと
//    Display / Schema / canonical tuple が run 中に変わるため)
// 同一 (targetID, purpose) の再登録 → ErrDuplicatePurpose
```

**identity の不変条件**: `targetID` は**論理 target**を指し、purpose は
その中の**接続 credential**を指す。purpose を追加しても TargetID・alias・
Display・Schema は変化しない。したがって 04(digest)・06(pool)・
09(EXPLAIN)・10(agent 集約)は従来どおり targetID だけで結合できる。
canonical tuple は userinfo を含まないため、explain 用 DSN が別ユーザーでも
同一 tuple → 同一 target に解決される。

**purpose 未登録時の Inspect の挙動(どちらか一方に決める)**

| purpose | 未登録時 | 根拠 |
|---|---|---|
| `PurposeStats` | **`PurposeApp` の credential に fallback する**。ただし DSN は §接続衛生の正規化(既定 DB 除去等)を必ず適用する | 「1 行統合」で 04/dbinspect が動くことが製品の前提であり、10 の agent の targets.json も DSN を 1 本しか宣言しない。fallback しないと既定構成で 04 が丸ごと skip になる |
| `PurposeExplain` | **fallback しない**。`ErrPurposeNotRegistered` を返し、09 は当該 target の EXPLAIN を skip して health に理由を記録する | 09 の最小権限要件はセキュリティ制御であり、app credential(DML 権限あり)へ暗黙に降格させると `EXPLAIN SELECT` が stored function 経由で副作用を起こし得るという 09 の CRITICAL 指摘に逆戻りする。**セキュリティ制御は暗黙 fallback しない** |
| `PurposeApp` | 発生しない(全 target が必ず持つ不変条件) | — |

**explain credential の登録経路**

- 複数 target: `RegisterDBInspector(id, PurposeExplain, "mysql", dsn)` を
  target ごとに呼ぶ(唯一の正式経路)
- 単一 target のときのみの簡便形: 環境変数
  `ISUTOOLS_EXPLAIN_DSN`(+ `ISUTOOLS_EXPLAIN_DRIVER`、既定 `mysql`)を
  **登録済み target がちょうど 1 つのときだけ**その target に適用する。
  0 個または 2 個以上のときは起動を止めず、health に
  「ISUTOOLS_EXPLAIN_DSN は target が 1 つのときのみ有効」を記録して
  EXPLAIN を skip する(fail-open)

### 接続衛生(v6 追加 — 計測区間の非汚染)

`PurposeStats` / `PurposeExplain` の接続は、登録された DSN をそのまま
使わず、**開く前に必ず次の正規化を適用する**(mysql の場合は
`mysql.ParseDSN` → 構造体を書き換え → `FormatDSN`):

| 項目 | 正規化後 | 理由 |
|---|---|---|
| `DBName` | **空("")** | **これが本項の主目的**。既定 DB を持つ接続で実行した文は performance_schema の digest 行に `SCHEMA_NAME = <そのアプリ schema>` として記録され、04 が計測している schema の digest を collector 自身が汚染する。既定 DB を持たない接続の文は `SCHEMA_NAME IS NULL`(DIGEST は非 NULL)の行に集計され、04 の `WHERE SCHEMA_NAME = ? OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL)` から**構造的に外れる**。また DIGEST が非 NULL なので、04 が overflow 検出に使う `SCHEMA_NAME IS NULL AND DIGEST IS NULL` の特殊行とも混同されない |
| `MultiStatements` | **false** | 09 の要件。元 DSN から引き継がない(v3 からの契約を維持) |
| `InterpolateParams` | **false** | schema 名を `?` のバインド引数として渡す前提を固定する(クライアント側補間の有無で挙動が変わらないようにする) |
| `ParseTime` / `Loc` | **true / UTC** | 04 の `UTC_TIMESTAMP(6)`、09 の `QUERY_SAMPLE_SEEN` を `time.Time` で受けるため |
| `Timeout` / `ReadTimeout` / `WriteTimeout` | 1s / 2s / 2s | ベンチ区間中に inspector が詰まらないようにする |

- **schema 名の公開**: registry は `PurposeApp` の DSN の `DBName` を
  `TargetInfo.Schema` として保持する。これは credential ではないので
  外部へ公開してよい。04/09 は `DATABASE()` ではなく
  **`WHERE SCHEMA_NAME = ?` に `TargetInfo.Schema` をバインドする**
  (stats 接続には既定 DB が無いため `DATABASE()` は NULL を返す)
- `Schema` は **`PurposeApp` からのみ**決まる。`PurposeStats` /
  `PurposeExplain` の DSN に DBName が書かれていても `Schema` には
  反映せず、接続時に必ず除去する
- `PurposeApp` の DSN に database が無い(`Schema == ""`)場合、04/09 は
  当該 target を skip し、health に
  「no default schema — RegisterDBTarget の DSN に database を含めること」
  を記録する
- DBName を省略できない driver(pgx 等)は schema 分離ができないため、
  health に info を記録する。v1 では 04/09 の対象が MySQL のみなので
  実害はない(04 §非ゴール)

### registry API

```go
type TargetInfo struct {
    ID      string // 安定 TargetID
    Driver  string // PurposeApp の driver 名
    Display string // allowlist 再構築の表示文字列(下記)
    Schema  string // アプリ schema 名(PurposeApp の DBName)。非 secret。
                   // 04/09 が WHERE SCHEMA_NAME = ? に渡す(v6 追加)
    Purposes []Purpose // 登録済み purpose(app は必ず含む)
}
func Targets() []TargetInfo
func Target(id string) (TargetInfo, bool)

// Features は公式 parser による **PurposeApp の DSN** の typed 属性
// (advisor の interpolateParams 判定はアプリのトラフィックが対象のため。
//  v6 明確化: stats/explain の DSN 属性は返さない)。
// known=false は「パースできず不明」を表す(false と区別される)。
type DSNFeatures struct {
    InterpolateParams bool
    ParseTime         bool
    MultiStatements   bool
}
func Features(id string) (f DSNFeatures, known bool)

// Inspect: (target, purpose) 所有の inspector で fn を実行(v6: purpose 引数を追加。
// v5 の Inspect(ctx, id, fn) は撤回)。
// v5 から維持: MaxOpenConns(1) の *sql.DB は「同一セッション」の保証には
// ならない(接続断・再接続で session state(SET time_zone 等)が失われ、
// 後続クエリが別セッションになり得る。connection-local state には
// database/sql の Conn が必要)。
// → Inspect は呼び出しごとに db.Conn(ctx) で専用 *sql.Conn を取得し、
//   それを制限付き Querier で包んで fn に渡し、callback 終了時に必ず
//   Close する。session 初期化は registry がこの Conn 上で毎回行う:
//     SET time_zone = '+00:00'   (09 の UTC 固定要件を基盤側で保証)
//   この初期化文は 04 の query budget に「Inspect 1 回につき 1 文」として
//   計上する(既定 DB を持たない接続なので SCHEMA_NAME IS NULL 側に入る)。
// v4 から維持: 素の *sql.Rows は返さない。Rows は追跡 wrapper で返し、
// Inspect の return 時に未 Close の Rows を強制 Close する。
type Querier interface {
    QueryContext(ctx context.Context, q string, args ...any) (Rows, error) // 追跡 wrapper
    QueryRowContext(ctx context.Context, q string, args ...any) Row
    // v6 監査反映: 09 の要求を受理して追加する。09 は pinned connection 上で
    // `SET ROLE NONE` / `SET time_zone = '+00:00'` を実行する必要があり、
    // Query 系では実行できない(結果セットを返さない文のため)。
    // 実行可能な文は Inspect の purpose ごとに制限する(下記)。
    ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}
// **ExecContext の使用制限**: session 設定文(`SET` 系)にのみ使う。
// registry は purpose ごとに allowlist(先頭トークンが `SET`)を適用し、
// 違反は `ErrExecNotAllowed` を返す。DML/DDL は allowlist されない。
func Inspect(ctx context.Context, id string, purpose Purpose, fn func(context.Context, Querier) error) error
```

- inspector 接続は idle timeout(30s)で閉じ、次回 Inspect で再接続
  (ベンチ区間中に常時接続を残さない)
- inspector は (target, purpose) ごとに 1 本。上限は
  **16 target × 3 purpose = 48 handle**、各 `MaxOpenConns(1)`
- 09 用に「inspector の接続構成は元 DSN から multiStatements を
  引き継がない」ことを registry 側の契約にする(§接続衛生で
  正規化として実装)
- 登録上限 16 target(超過は health 記録)

### Display(redaction)の構造化

- 対象は **`PurposeApp` の DSN**。`mysql.ParseDSN` で構造化 →
  **allowlist(Net, Addr, DBName)のみ**から `tcp(db1:3306)/isuconp`
  形式を再構築。credential・未知パラメータは一切含めない
  (「credential 系を除外」ではなく「allowlist 以外非表示」)
- URL 形式(pgx 等)は url.Parse → host/path のみ。パース失敗は
  `"(unparsed dsn)"`
- **stats / explain の DSN は Display に一切現れない**(用途別 credential を
  増やしても露出面を増やさない)
- テストで Display に user/password/クエリパラメータが**構造的に
  含まれ得ない**ことを保証(allowlist 再構築なので文字列検査は補助)

### driver wrapper の維持要件

- 既存の proxy 登録は `driver.DriverContext` / `OpenConnector` を
  実装するドライバでその経路を維持する(go-sql-proxy の対応確認を
  受け入れ条件に含める)。DSN の観測点(OpenConnector/Open)で
  TargetID 導出・登録(`PurposeApp`)を行う
- inspector(stats / explain)は **proxy を経由しない素の driver** で開く
  (計測へ混入させないため)

### 利用箇所の移行

| 現状 | 移行後 |
|---|---|
| `dbinspect.Collect(ctx, name, dsn)` | `Inspect(ctx, id, PurposeStats, ...)` + Querier 版 Collect |
| 04 の `WHERE SCHEMA_NAME = DATABASE()` | `WHERE SCHEMA_NAME = ?` に `TargetInfo.Schema` をバインド(04 側で対応) |
| 09 の EXPLAIN 接続 | `Inspect(ctx, id, PurposeExplain, ...)`。未登録なら skip |
| 10 の targets.json の 1 エントリ | `RegisterDBTarget(id, driver, dsn)` と等価(= `PurposeApp`)|
| advisor interpolateParams check | `Features(id)` の typed 値を Options に事前評価 |
| advisor MySQL check の `Options.DB` | isutools.go 側で Inspect callback 内から実行 |
| `FirstConn` | Deprecated(先頭 target 相当を返す互換 shim)。v1.3.0 で削除 |

## 実装ステップ(TDD)

1. canonical tuple と TargetID 導出(mysql 形式 / URL 形式 / tcp と unix /
   パース不能 / dedup / RegisterDBTarget 優先 / ID 文字種・長さ検証)の
   テスト先行
2. Display の allowlist 再構築(credential 非含有の構造的保証)
3. Purpose 登録(`RegisterDBInspector`)と Inspect の purpose 解決
   (fallback 表のとおり)
4. 接続衛生の DSN 正規化(FormatDSN 結果の検査)と session 初期化
5. Features(known 3 値)・Inspect(inspector 再利用・接続数 1 の検証・
   idle close)
6. wrapper の DriverContext/OpenConnector 維持確認
7. dbinspect / advisor / isutools.go の移行(既存テスト回帰)
8. docs: INTEGRATION.md「DSN と TargetID と Purpose」節
   (shard 構成 + EXPLAIN 用 GRANT の例。GRANT 文の実体は 09 が記載)

## テスト計画

- unit: 同一 endpoint の並行初回接続 → 単一 target
- unit: RegisterDBTarget 済み DSN の自動導出スキップ
  (`TargetIDForDSN` が明示 ID を返す)
- unit: canonical tuple に net が含まれること —
  `tcp(127.0.0.1:3306)/isuconp` と `unix(/tmp/mysql.sock)/isuconp` が
  **異なる ID** になる
- unit: hash26 が 26 文字・`[a-z2-7]` のみ・同一 tuple で決定的
- unit: ID 比較の byte 完全一致 — `"db1"` と `"DB1"` は別 target、
  非 ASCII / 65 バイト / `"a b"` は `ErrInvalidTargetID`
- unit: 同一 targetID に app/stats/explain を登録しても
  `TargetInfo.ID` / `Display` / `Schema` が不変
- unit: `Inspect(..., PurposeExplain, ...)` は explain 未登録時に
  `ErrPurposeNotRegistered` を返し、**app credential を使わない**
  (fake driver に渡った DSN の user を検証)
- unit: `Inspect(..., PurposeStats, ...)` は stats 未登録時に app credential へ
  fallback し、渡る DSN が `DBName=""` / `multiStatements=false` /
  `interpolateParams=false` / `parseTime=true` / `loc=UTC` であること
- unit: `Features(id)` は PurposeApp の DSN の属性を返す
  (explain 用 DSN に interpolateParams を付けても結果が変わらない)
- unit: Inspect 並行呼び出しで (target, purpose) ごとの接続が
  1 本を超えないこと
- unit: Features: interpolateParams あり / なし / パース不能の 3 値
- integration(MySQL fixture): stats 接続で `SELECT DATABASE()` が NULL。
  Inspect を N 回実行後、
  `events_statements_summary_by_digest` の
  `SCHEMA_NAME = <app schema>` 側に collector 由来の digest が
  **1 件も増えない**こと(v6 の CRITICAL の受け入れ条件)
- integration: 4 DSN(shard 想定)で TargetID が再起動相当
  (registry 再構築)後も一致

## リスク

| リスク | 対策 |
|---|---|
| 同一 endpoint+db を用途別に分けたい構成 | RegisterDBTarget の明示命名で解決 |
| 用途別 credential の設定漏れ | stats は app へ fallback、explain は skip + health(§Purpose の表)|
| driver 差(pgx の DSN 形式) | driver ごとの parser 分岐。未対応 driver は明示登録必須(自動登録しない — 安全側) |
| inspector 経由のクエリが計測に混入 | inspector は素の接続(プロキシ非経由)+ 既定 DB を持たない DSN(§接続衛生) |
| 自動 ID が長い(最大 59 バイト) | 表示は alias/Display のみ。ID は機械用で手書き禁止(§ID の受理規則) |

## 見積もり

2.5 日(v5 の 2 日から増。Purpose 分離・DSN 正規化・schema 非汚染の
integration テストを含む)。
