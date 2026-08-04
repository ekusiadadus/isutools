# 01: DB target registry(FirstConn の置換)— v3

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `sqlstats`, `dbinspect`, `advisor`, `isutools.go`

## v3 での変更点(レビュー差し戻し対応)

1. **[HIGH] 接続順 `dbN` 命名の廃止**。並行・lazy 接続する 4 shard では
   再起動ごとに名前が入れ替わり、06 の pool 名とも対応しない。
   → **安定した TargetID を第一 API** にする:
   - 既定 ID は接続順ではなく **DSN の構造化パースから決定的に導出**
     (driver + host:port + database。例: `mysql-db1_3306-isuconp`)。
     再起動・接続順に依存しない
   - 明示命名 API `isutools.RegisterDBTarget(id, dsn)` を提供し、
     衝突(同一 endpoint+db の別用途)や短い別名が必要な場合に使う。
     自動導出は fallback
   - sqlrows(04)・dbinspect・queryplan(09)・DB pool(06)・
     agent(10 の `ISUTOOLS_AGENT_TARGETS="name=dsn;..."`)は
     **同じ TargetID 名前空間**で結合する
2. **[MEDIUM] Inspect の接続所有権**。呼ぶたびに `sql.Open` する v2 案は
   04/09 のファンアウトで接続プールを大量生成し、callback が
   `*sql.DB` を保持できてしまう。
   → registry が **target ごとに MaxOpenConns(1) の inspector を 1 つ所有**
   して再利用し、callback には raw `*sql.DB` ではなく**制限付き query
   interface** を渡す
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
4. 既存の FirstConn 利用箇所を移行する

## 設計

### TargetID

```go
// 既定: 構造化 DSN から決定的に導出(接続順非依存)
//   mysql tcp(db1:3306)/isuconp  →  "mysql-db1_3306-isuconp"
// 同一 ID になる DSN は同一 target として dedup。

// 明示命名(第一 API)。SQLDriverName より前に呼ぶ。
// driverName は必須引数(v4 修正: MySQL DSN に driver 種別は含まれず、
// driver 名は sql.Open の別引数であり DSN から特定できないため)。
func RegisterDBTarget(id, driverName, dsn string) error   // id 重複・空・未知 driver はエラー
```

**パース不能 DSN の扱い(v4 修正)**: v3 の
`unparsed-<sha256(dsn)[:8]>` 自動 ID は廃止する
(credential 変更だけで ID が変わり安定でない・32bit 公開 hash は
衝突しやすく、既知 DSN 形式への弱い credential のオフライン照合器にも
なり得るため)。パース不能な DSN は**自動登録しない**:
`RegisterDBTarget` による明示登録を必須とし、未登録のまま観測された
場合は health に「unparsed DSN — RegisterDBTarget が必要」を記録して
当該接続を target 化しない(fail-open)。

### registry API

```go
type TargetInfo struct {
    ID       string // 安定 TargetID
    Driver   string
    Display  string // allowlist 再構築の表示文字列(下記)
}
func Targets() []TargetInfo

// Features は公式 parser による typed な DSN 属性。known=false は
// 「パースできず不明」を表す(false と区別される)。
type DSNFeatures struct {
    InterpolateParams bool
    ParseTime         bool
    MultiStatements   bool
}
func Features(id string) (f DSNFeatures, known bool)

// Inspect: target 所有の inspector(MaxOpenConns(1)、再利用)で fn を実行。
// fn には制限付き interface を渡す。*sql.DB は渡さない。
// v4 修正: 素の *sql.Rows を返すと callback 外へ保持でき、唯一の接続を
// 恒久的に pin できてしまう。Rows は追跡 wrapper で返し、Inspect の
// return 時に未 Close の Rows を強制 Close する(以後の操作はエラー)。
type Querier interface {
    QueryContext(ctx context.Context, q string, args ...any) (Rows, error) // 追跡 wrapper
    QueryRowContext(ctx context.Context, q string, args ...any) Row
}
func Inspect(ctx context.Context, id string, fn func(context.Context, Querier) error) error
```

- inspector 接続は idle timeout(30s)で閉じ、次回 Inspect で再接続
  (ベンチ区間中に常時接続を残さない)
- 09 用に「inspector の接続構成は元 DSN から multiStatements を
  引き継がない」ことを registry 側の契約にする(09 の要件を基盤で保証)
- 登録上限 16 target(超過は health 記録)

### Display(redaction)の構造化

- `mysql.ParseDSN` で構造化 → **allowlist(Net, Addr, DBName)のみ**から
  `tcp(db1:3306)/isuconp` 形式を再構築。credential・未知パラメータは
  一切含めない(「credential 系を除外」ではなく「allowlist 以外非表示」)
- URL 形式(pgx 等)は url.Parse → host/path のみ。パース失敗は
  `"(unparsed dsn)"`
- テストで Display に user/password/クエリパラメータが**構造的に
  含まれ得ない**ことを保証(allowlist 再構築なので文字列検査は補助)

### driver wrapper の維持要件

- 既存の proxy 登録は `driver.DriverContext` / `OpenConnector` を
  実装するドライバでその経路を維持する(go-sql-proxy の対応確認を
  受け入れ条件に含める)。DSN の観測点(OpenConnector/Open)で
  TargetID 導出・登録を行う

### 利用箇所の移行

| 現状 | 移行後 |
|---|---|
| `dbinspect.Collect(ctx, name, dsn)` | `Inspect(ctx, id, ...)` + Querier 版 Collect |
| advisor interpolateParams check | `Features(id)` の typed 値を Options に事前評価 |
| advisor MySQL check の `Options.DB` | isutools.go 側で Inspect callback 内から実行 |
| `FirstConn` | Deprecated(先頭 target 相当を返す互換 shim)。v1.3.0 で削除 |

## 実装ステップ(TDD)

1. TargetID 導出(mysql 形式 / URL 形式 / パース不能 / dedup /
   RegisterDBTarget 優先)のテスト先行
2. Display の allowlist 再構築(credential 非含有の構造的保証)
3. Features(known 3 値)・Inspect(inspector 再利用・接続数 1 の検証・
   idle close)
4. wrapper の DriverContext/OpenConnector 維持確認
5. dbinspect / advisor / isutools.go の移行(既存テスト回帰)
6. docs: INTEGRATION.md「DSN と TargetID」節(shard 構成の例)

## テスト計画

- unit: 同一 endpoint の並行初回接続 → 単一 target
- unit: RegisterDBTarget 済み DSN の自動導出スキップ
- unit: Inspect 並行呼び出しで接続が 1 本を超えないこと
- unit: Features: interpolateParams あり / なし / パース不能の 3 値
- integration: 4 DSN(shard 想定)で TargetID が再起動相当
  (registry 再構築)後も一致

## リスク

| リスク | 対策 |
|---|---|
| 同一 endpoint+db を用途別に分けたい構成 | RegisterDBTarget の明示命名で解決 |
| driver 差(pgx の DSN 形式) | driver ごとの parser 分岐。未対応 driver は明示登録必須(自動登録しない — 安全側) |
| inspector 経由のクエリが計測に混入 | inspector は素の接続(プロキシ非経由)を使用 |

## 見積もり

2 日(v2 の 1.5 日から増。TargetID 導出と Querier 化を含む)。
