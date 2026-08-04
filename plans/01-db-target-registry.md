# 01: DB target registry(FirstConn の置換)

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `sqlstats`, `dbinspect`, `advisor`, `isutools.go`

## 背景(レビュー指摘)

`sqlstats.FirstConn`(sqlstats/sqlstats.go:26 付近)は**最初に開かれた
1 本の DSN をグローバル保持**し、生の DSN 文字列を呼び出し側へ返す。

- ISUCON12 の user-shard 4 DB のような複数 DSN 構成を扱えない
  (2 本目以降は観測から漏れる)
- credential 入りの DSN が advisor / dbinspect へ渡り、今後
  04(sqlrows)・09(explain)・10(agent)が増えるたびに保持箇所が増える

04/09/10 がすべてこの API に依存するため、先行して置き換える。

## ゴール

1. 観測された**全ての** DSN を named target として登録・列挙できる
2. DSN 文字列を registry の外に出さずに、raw 接続を使う検査を実行できる
3. 既存の `FirstConn` 利用箇所(dbinspect / advisor / isutools.go)を移行する

## 非ゴール

- 複数 target への検査ファンアウトそのもの(04 で実施)
- PostgreSQL 対応の拡張(registry は driver 名を保持するだけで中立)

## 設計

### registry(sqlstats 内に追加)

```go
// TargetInfo は credential を含まない公開情報。
type TargetInfo struct {
    Name     string // "db1", "db2", ... 登録順の自動命名
    Driver   string // "mysql" など(base driver 名)
    Redacted string // user/password を除去した DSN(host/db/主要パラメータのみ)
}

func Targets() []TargetInfo

// Inspect は名前で指定した target への短命 raw 接続を開いて fn に渡す。
// DSN は返さない。fn 完了後に必ず Close する。MaxOpenConns(1)。
func Inspect(ctx context.Context, name string, fn func(context.Context, *sql.DB) error) error

// HasDSNParam は DSN 文字列を晒さずにパラメータ有無だけを答える
// (advisor の interpolateParams check 用)。
func HasDSNParam(name, param string) bool
```

- 登録: 既存の Open hook(接続確立時)で DSN を正規化キーに dedup し、
  未知なら `dbN` として登録。上限 16 target(超過は health に記録)
- 明示命名(任意): `isutools.NameDB(dsnSubstring, "shard1")` は
  **導入しない**。自動名 + Redacted 表示で判別可能であり、API を増やさない
  (必要になったら別途検討)
- redact 規則: `user:pass@` の除去、DSN パラメータのうち既知の
  credential 系(password 等)の除去。driver ごとの形式差は
  go-sql-driver 形式と URL 形式の 2 種をサポートし、パース不能なら
  `"(unparsed dsn)"` として**全体を伏せる**(安全側)

### 利用箇所の移行

| 現状 | 移行後 |
|---|---|
| `dbinspect.Collect(ctx, name, dsn)` | `sqlstats.Inspect(ctx, target, func(ctx, db) { schema = collect(db) })` |
| advisor `Options.DSN` の interpolateParams check | `HasDSNParam(target, "interpolateParams=true")` の結果を `Options` に事前評価して渡す |
| advisor `Options.DB`(raw 接続) | `Inspect` の callback 内で `Collect` を呼ぶ形に isutools.go 側を変更(advisor パッケージの `Options.DB *sql.DB` 自体は維持し、所有権コメントを更新) |
| `FirstConn` | Deprecated として 1 リリース維持(先頭 target を返す)。v1.3.0 で削除 |

### snapshot 表示

- DB Schema セクションのヘッダに target 名と Redacted DSN を表示
  (複数 target 時の判別)。v1.2.0 では検査対象は先頭 target のまま、
  複数 target への拡大は 04 と同時に行う

## 実装ステップ(TDD)

1. redact のテスト先行(go-sql-driver 形式 / URL 形式 / パース不能 /
   password パラメータ)
2. registry(dedup・自動命名・上限・並行登録)のテスト
3. `Inspect` / `HasDSNParam` + FirstConn の deprecated 化
4. dbinspect / advisor / isutools.go の移行(既存テストが回帰検知)
5. docs: INTEGRATION.md の「DSN の扱い」節(credential が snapshot に
   出ないことの明記)

## テスト計画

- unit: 同一 DSN の再接続で target が増えないこと
- unit: 17 本目の DSN で health 記録 + 16 本までは全登録
- unit: Redacted に user/password が含まれないこと(文字列検査)
- integration: 既存の advisor / dbinspect のテストが移行後も green

## リスク

| リスク | 対策 |
|---|---|
| redact 漏れ(未知 DSN 形式) | パース不能は全伏せ。既知形式のみ部分表示 |
| FirstConn 利用の外部コード | Deprecated 期間を 1 リリース確保 |
| 登録経路(接続 hook)の競合 | 既存 connMu の粒度を registry に流用 |

## 見積もり

1.5 日(移行含む)。
