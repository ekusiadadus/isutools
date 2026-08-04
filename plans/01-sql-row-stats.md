# 01: SQL 行効率計測(rows examined / rows sent)

対象リリース: v1.2.0 / 新規パッケージ: `sqlrows`

## 背景

ISUCON13 優勝チームはインデックス追加の判断を slp の
「Rows examined / Rows sent 比が悪いクエリ」(目標: 5 倍以下)で行っている。
ISUCON10 予選も filesort・フルスキャンの検出が主要な導線。

isutools の SQL 計測はドライバプロキシ経由の時間・回数・p95 のみ
(`internal/agg.Entry`: Count/Total/Avg/Max/P95)で、**スキャン行数が取れない**。
ドライバインターフェースからは原理的に取得できないため、MySQL の
`performance_schema` をサーバ側から読む。

## ゴール

- ベンチ区間(reset→snapshot)の digest 別
  rows examined / rows sent / 回数 / 総時間をダッシュボードに表示する
- examined/sent 比・フルスキャン回数を advisor の warn として提示する
- performance_schema が使えない環境では黙って skip(fail-open)

## 非ゴール

- アプリ側正規化キー(sqlstats)と server digest の突合(v1 では別テーブル
  として表示。正規化方式が異なり fuzzy match は誤対応リスクが高い)
- PostgreSQL 対応(`pg_stat_statements` は別計画で扱う)
- EXPLAIN の実行(→ 計画 05)

## データソース

`performance_schema.events_statements_summary_by_digest` から取得:

```sql
SELECT DIGEST, DIGEST_TEXT, COUNT_STAR, SUM_TIMER_WAIT,
       SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_ROWS_AFFECTED,
       SUM_CREATED_TMP_TABLES, SUM_CREATED_TMP_DISK_TABLES,
       SUM_SORT_MERGE_PASSES, SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED
FROM performance_schema.events_statements_summary_by_digest
WHERE SCHEMA_NAME = DATABASE()
ORDER BY SUM_TIMER_WAIT DESC
LIMIT 200
```

前提と注意:

- MySQL 5.7/8.x は performance_schema=ON かつ statements_digest 有効が既定。
  **MariaDB は performance_schema が既定 OFF** → クエリ失敗で skip に劣化
- `SUM_TIMER_WAIT` はピコ秒。表示は ms へ変換
- digest テーブルは容量上限(`performance_schema_digests_size`)で追い出しが
  起きる。カウンタ後退を検出したら該当 digest を「新規」として扱い、
  スナップショットに `partial: true` を立てる
- TRUNCATE はしない(破壊的・要権限)。**baseline+delta 方式**で非破壊に取る

## 設計

### collector(新パッケージ `sqlrows`)

```go
type Collector struct { /* driverName, dsn, baseline map[digest]row, mu */ }

func New(driverName, dsn string) *Collector
func (c *Collector) Reset(ctx context.Context) error      // baseline 取得
func (c *Collector) Snapshot(ctx context.Context) *Snapshot // delta 計算

type Snapshot struct {
    Source  string  `json:"source"`   // "performance_schema"
    Partial bool    `json:"partial,omitempty"` // digest 追い出し検出
    Rows    []Row   `json:"rows"`
    Health  string  `json:"health,omitempty"`  // skip 理由
}

type Row struct {
    Digest       string        `json:"digest"`
    Query        string        `json:"query"`     // DIGEST_TEXT(先頭 512B に制限)
    Count        int64         `json:"count"`
    Total        time.Duration `json:"total_ns"`
    RowsExamined int64         `json:"rows_examined"`
    RowsSent     int64         `json:"rows_sent"`
    ExaminedPerSent float64    `json:"examined_per_sent"` // sent=0 は examined をそのまま
    NoIndexUsed  int64         `json:"no_index_used"`
    SortMergePasses int64      `json:"sort_merge_passes"`
    TmpDiskTables int64        `json:"tmp_disk_tables"`
}
```

- 接続は advisor と同じく `sqlstats.FirstConn()` の DSN で
  `sql.Open` + `SetMaxOpenConns(1)`、呼び出し毎に短命に使う
- baseline は Reset 時に map[DIGEST]Row で保持。Snapshot 時に差分を取り、
  COUNT_STAR が後退した digest は baseline を破棄して全量を採用 + Partial
- 上位 200 digest に制限(スナップショットサイズ抑制)。切り捨て時は
  Health に "top 200 に制限" を記録(silent cap 禁止)

### web 配線

- `web.Provider` に `SQLRows func(ctx context.Context) *sqlrows.Snapshot` を追加
  (nil skip)。snapshot 時と `POST /reset` 時の両方で呼ぶ
  (reset 側は collector.Reset を包む closure)
- `Snapshot` 構造体に `SQLRows *sqlrows.Snapshot \`json:"sqlrows,omitempty"\``
- template: SQL セクションの直後に「SQL row efficiency (server digest)」表。
  列: query / count / total / rows examined / rows sent / 比 / no-index /
  sort-merge。比が 5 超の行を強調表示
- diff ビュー: v1 では対象外(digest キーで後続対応可能な構造にしておく)

### advisor 統合

`advisor.WithSQLRows(checks []Check, snap *sqlrows.Snapshot) []Check`
(WithCacheTelemetry と同型、snapshot 時差し替え):

- `sql-rows-ratio`: examined/sent 比 > 5 の digest が上位(総時間順)にある
  → warn。「比 5 以下が目標(ISUCON13 優勝チームの基準)。WHERE/ORDER BY に
  合わせた複合インデックスを検討」
- `sql-rows-no-index`: SUM_NO_INDEX_USED > 0 の digest → warn
  (フルスキャン)。SUM_NO_GOOD_INDEX_USED も detail に併記
- `sql-rows-filesort`: SUM_SORT_MERGE_PASSES > 0 または
  SUM_CREATED_TMP_DISK_TABLES > 0 → warn(ソート/一時表がディスクへ)
- snapshot なし → 全て skip

### isutools.go 配線

- `sqlstats.FirstConn()` が MySQL 系のときだけ collector を生成
- 環境変数 `ISUTOOLS_SQLROWS=off` で無効化(既定は有効。読み取り 1 回
  /reset + 1 回/snapshot のみでランタイムコストはない)

## 実装ステップ(TDD)

1. `sqlrows` パッケージ: delta 計算・カウンタ後退・上限切り捨てを
   sqlmock(または in-package の rows スタブ)でテスト先行
2. advisor `WithSQLRows` + 閾値テスト
3. web Provider / Snapshot / template 配線 + レンダリングテスト
4. isutools.go 配線 + 環境変数テスト
5. docs(README 環境変数表・INTEGRATION.md「§ SQL 行効率」・
   IMPLEMENTATION_STATUS)
6. private-isu で実測 → フィールド検証結果を IMPLEMENTATION_STATUS に記録

## テスト計画

- unit: delta 正常系 / baseline なし(Reset 未実行)で全量 + Partial /
  COUNT_STAR 後退 / LIMIT 切り捨て Health / DIGEST_TEXT 512B 制限
- unit: advisor 閾値(比 5.0 境界、sent=0、no_index_used)
- integration: web snapshot JSON に sqlrows が出る / nil Provider で消える
- 実 DB テストは CI 対象外(ローカル手動 + private-isu 検証で担保)

## リスク

| リスク | 対策 |
|---|---|
| MariaDB / performance_schema OFF | 初回クエリ失敗で以後 skip(Health に理由) |
| digest 追い出しで比が不正確 | Partial フラグ + 後退 digest は全量扱い |
| DIGEST_TEXT に注意情報 | リテラルは digest 化済み(`?` 置換)で PII なし |
| 権限不足(SELECT on performance_schema) | エラーで skip。INTEGRATION.md に GRANT 例を記載 |
| スナップショット肥大 | 上位 200 + テキスト 512B 制限(32MiB キャップ内) |

## 見積もり

collector 1 日 + advisor/web 配線 1 日 + docs/検証 0.5 日。
