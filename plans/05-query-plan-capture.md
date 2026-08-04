# 05: 代表クエリの EXPLAIN 自動化

対象リリース: v1.3.0 / 変更箇所: `sqlstats`(exemplar)、新規 `queryplan`、`advisor`

## 背景

ISUCON10 予選作問記事のボトルネック導線は EXPLAIN の `Using filesort`
(MySQL 5.7 の降順インデックス非対応が根因)。計画 01 の
performance_schema 統計(`SUM_SORT_MERGE_PASSES` / `SUM_NO_INDEX_USED`)は
「filesort が起きている」ことまでは示すが、**どのインデックスが使われ、
何が使われなかったか**は EXPLAIN でしか分からない。

前提として EXPLAIN には正規化前の実行可能な SQL が必要だが、isutools の
sqlstats はリテラルをマスクした正規化キーしか保持していない。
→ **exemplar(代表実 SQL)の bounded 保持**が本計画の中核。

## ゴール

1. 遅いクエリ上位 N 件について EXPLAIN を自動実行し、
   type / key / rows / Extra(Using filesort・Using temporary 等)を表示
2. フルスキャン・filesort を advisor の warn として提示
3. **保存スナップショットに生 SQL(リテラル入り)を残さない**

## 非ゴール

- `EXPLAIN ANALYZE`(実行を伴い危険。対象外)
- PostgreSQL(`EXPLAIN` 構文差。将来対応)
- 01 との digest 突合(独立に動作する)

## 設計

### 5a. exemplar 保持(sqlstats 拡張)

- 正規化キーごとに**実 SQL を 1 つだけ**保持する bounded ストア:
  - 対象: 実行時間が `ISUTOOLS_EXPLAIN_MIN_MS`(既定 10ms)以上のクエリのみ
  - 容量: 最大 512 キー、SQL は 4KiB に切り詰め(メモリ上限 ~2MiB)
  - 更新: 同一キーは「より遅かった実行」の SQL で置き換え
    (最悪ケースのプランを見たい)
  - 世代: reset で全消去
- 既定 **off**(`ISUTOOLS_EXPLAIN=1` で有効化)。off 時は分岐 1 回で
  オーバーヘッドゼロを維持
- **exemplar はプロセス内のみ**。snapshot JSON / HTML には一切出力しない
  (保存スナップショットは共有される前提。リテラルには PII があり得る)

### 5b. EXPLAIN 実行(新パッケージ `queryplan`)

snapshot 時(reset 時ではない)に実行:

1. agg スナップショットから Total 降順で上位 `ISUTOOLS_EXPLAIN_TOP`
   (既定 20)キーを選ぶ
2. exemplar があるキーだけ、raw 接続(`sqlstats.FirstConn` の DSN、
   MaxOpenConns=1)で `EXPLAIN <sql>` を実行
3. 1 クエリあたり 250ms・全体 2 秒の context タイムアウト
   (snapshot をブロックしない)

```go
type Plan struct {
    Key      string   `json:"key"`       // 正規化キー(表示はこれのみ)
    Rows     []PlanRow `json:"rows"`     // EXPLAIN の各行
    Err      string   `json:"err,omitempty"`
}
type PlanRow struct {
    SelectType string `json:"select_type"`
    Table      string `json:"table"`
    Type       string `json:"type"`      // ALL / index / range / ref ...
    Key        string `json:"key"`       // 使われたインデックス
    PossibleKeys string `json:"possible_keys"`
    Rows       int64  `json:"rows"`
    Extra      string `json:"extra"`     // Using filesort / temporary ...
}
```

- 対象文種: SELECT / UPDATE / DELETE / INSERT ... SELECT
  (EXPLAIN は実行しないため安全。それ以外はスキップ)
- 同一正規化キーは snapshot 間でキャッシュしない(インデックス追加後の
  プラン変化を毎回反映するのが目的のため)

### 5c. 表示と advisor

- template: SQL セクションの各行に展開式のプラン表示(または独立
  「Query plans」セクション)。`type=ALL`・`Using filesort`・
  `Using temporary` をハイライト
- `advisor.WithQueryPlans(checks, plans)`:
  - `plan-full-scan`: type=ALL かつ rows > 1000 → warn
    「インデックス未使用。possible_keys=%s」
  - `plan-filesort`: Using filesort → warn
    「ORDER BY に合う複合/降順インデックスを検討(ISUCON10 の定番)」
  - `plan-temporary`: Using temporary → warn
- 01(sqlrows)と両方有効な場合は相互補完(01 は網羅・05 は根因)。
  advisor ID を分けてあるので重複 warn は許容し、詳細度で使い分ける

## 実装ステップ(TDD)

1. sqlstats exemplar ストア(容量上限・4KiB 切り詰め・遅い方優先・
   reset 消去・off 時ゼロコスト)をテスト先行
2. queryplan: EXPLAIN 行パース(MySQL 8 の列構成)・タイムアウト・
   文種フィルタを sqlmock でテスト
3. web/advisor 配線(exemplar 非出力の検証テストを必ず含める)
4. docs: README・INTEGRATION.md(「EXPLAIN 自動化」節: 有効化手順、
   PII が snapshot に出ない設計の明記、権限)
5. private-isu 検証(意図的にインデックスを消して warn が出ること)

## テスト計画

- unit: exemplar 境界(513 キー目・4097 バイト目・同キー高速実行での
  非置換)
- unit: EXPLAIN 失敗(構文非対応・権限不足)→ Plan.Err に記録し継続
- integration: snapshot JSON に key と plan は出るが exemplar SQL が
  出ないこと(文字列検索で保証)
- ABBA: `ISUTOOLS_EXPLAIN=1` 状態でのオーバーヘッド計測
  (exemplar 書き込みは 10ms 超クエリのみなので理論上無視可能だが実測)

## リスク

| リスク | 対策 |
|---|---|
| 生 SQL の漏洩 | プロセス内限定・snapshot 非出力をテストで固定 |
| EXPLAIN の DB 負荷 | 上位 20 件・250ms/2s タイムアウト・snapshot 時のみ |
| プリペアド構文で EXPLAIN 不可 | exemplar は interpolate 済みの生 SQL を保持するため回避(interpolateParams 無効時は args を埋めず Err 記録) |
| マルチステートメント/コメント注入 | exemplar は観測した SQL そのもの。EXPLAIN プレフィックス付与のみで加工しない + multiStatements は既定無効 |

## 見積もり

exemplar 1 日、queryplan 1 日、配線+docs+検証 1 日。
