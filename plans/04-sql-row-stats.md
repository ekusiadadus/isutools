# 04: SQL 行効率計測(rows examined / rows sent)— 再設計版

種別: 機能 / 対象リリース: v1.2.0 / 依存: 01(registry) / 新規パッケージ: `sqlrows`

## 旧計画(旧01)からの変更点

レビューの CRITICAL 指摘 3 件を反映:

1. **「上位 200 件だけの baseline」を廃止** — baseline 時に 201 位以下だった
   digest がベンチ後に上位へ入ると、過去の累積値を区間値として誤計上する。
   baseline / current とも**対象 schema の全 digest を取得**し、差分計算後に
   表示上位 200 へ切る
2. **「COUNT_STAR 後退 = 追い出し」の誤りを修正** — MySQL の digest table は
   満杯時に行を追い出すのではなく、未登録 digest を
   `SCHEMA_NAME=NULL, DIGEST=NULL` の**特殊行へ集約**する(公式仕様:
   dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-digests.html)。
   overflow は NULL 行のデルタで検出し、独立 health として扱う。
   counter 後退は外部 TRUNCATE・DB 再起動・instrument reset を意味するので
   **run 全体を partial** にする
3. **RowsSent=0 の比率定義を修正** — 比率 = RowsExamined とせず **N/A**。
   SELECT と DML を分離して表示する

## ゴール

- ベンチ区間(reset→snapshot)の digest 別
  rows examined / rows sent / 回数 / 総時間を registry の **target ごと**に表示
- SELECT の examined/sent 比、フルスキャン、filesort 兆候を advisor に提示
  (閾値は provisional と明記し、private-isu 実測後に確定)
- capability probe により非対応環境では黙って skip

## 非ゴール

- アプリ側正規化キーとの突合(誤対応リスク。将来課題)
- PostgreSQL(`pg_stat_statements` は別計画)
- EXPLAIN(→ 09)

## 設計

### capability probe(Reset 時に 1 回)

```sql
SELECT @@performance_schema;                          -- 0 なら skip(MariaDB 既定 OFF)
SELECT ENABLED FROM performance_schema.setup_consumers
 WHERE NAME = 'statements_digest';                    -- NO なら skip
```

結果は target ごとに health へ記録(`sqlrows[db1]: skip (performance_schema=OFF)`)。

### baseline / delta(全 digest 方式)

- Reset 時・Snapshot 時とも:

```sql
SELECT DIGEST, COUNT_STAR, SUM_TIMER_WAIT,
       SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_ROWS_AFFECTED,
       SUM_CREATED_TMP_DISK_TABLES, SUM_SORT_MERGE_PASSES,
       SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED
FROM performance_schema.events_statements_summary_by_digest
WHERE SCHEMA_NAME = DATABASE() OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL)
```

  - **LIMIT なし**(全 digest)。1 行 ≈ 100B、上限は
    `performance_schema_digests_size`(既定 ~1 万)なので
    baseline のメモリは高々 ~1MiB/target
  - baseline には DIGEST_TEXT を**含めない**(メモリ節約)。
    表示対象に決まった上位 200 についてのみ、Snapshot 時に
    `WHERE DIGEST IN (...)` で DIGEST_TEXT(512B に切り詰め)を追加取得
- delta 計算: current − baseline。baseline に無い digest は
  「区間中に初登場」なので current 値をそのまま採用(全件取得している
  ため、これは正しい区間値)
- 表示: delta の SUM_TIMER_WAIT 降順で上位 200。
  **切り捨ては全件数と表示件数の両方を明示**(`shown=200 / total=1234`)
- **NULL digest overflow 行**: デルタ > 0 なら
  health `sqlrows-overflow` を独立に記録し、「digests_size 不足により
  一部の文が集約行へ落ちた(カバレッジ不完全)」と表示
- **counter 後退**(いずれかの digest で current < baseline):
  外部 TRUNCATE / 再起動 / instrument reset とみなし、
  当該 target の区間を **partial** にする(値は current をそのまま表示し、
  「絶対値は区間外を含む可能性」を明記)

### 文種分離と比率

- DIGEST_TEXT 先頭トークンで SELECT / DML(INSERT・UPDATE・DELETE・
  REPLACE)/ その他に分類
- 比率列(`examined_per_sent`)は **SELECT かつ RowsSent > 0 のみ**算出。
  それ以外は N/A 表示。DML は RowsAffected 列を主表示にする

### 複数 target(01 依存)

- registry の全 target(上限 16)に対して並行に probe/取得
  (per-target 1 秒・全体 3 秒の context)。表示は target 名ごとの表

### advisor(WithSQLRows、閾値は provisional)

- `sql-rows-ratio`: SELECT・sent>0・比 > 5・総時間上位 → warn
  (ISUCON13 の「5 倍以下」基準を出典として明記)
- `sql-rows-no-index`: SUM_NO_INDEX_USED デルタ > 0 → warn
- `sql-rows-filesort`: SUM_SORT_MERGE_PASSES または
  SUM_CREATED_TMP_DISK_TABLES のデルタ > 0 → warn
- `sql-rows-overflow`: overflow 検出 → info(digests_size 増加を提案)
- **閾値確定の手順**: private-isu で意図的に index を外し、
  誤警報ゼロ・検出漏れゼロを確認してから既定有効。それまで
  Recommendation に (provisional) を付す

### feature flag

`ISUTOOLS_SQLROWS=off` で無効(既定 on)。単独 ABBA:
flag off ↔ on で Reset/Snapshot 時の 2 クエリのみの差であることを実測。

## 実装ステップ(TDD)

1. delta 計算のテスト先行: 初登場 digest / counter 後退 → partial /
   NULL overflow 行の分離 / 切り捨て表示
2. 文種分類・比率 N/A のテスト
3. probe(performance_schema OFF / consumers NO / 権限エラー)のテスト
4. registry 経由の複数 target 取得(01 の Inspect を使用)
5. advisor WithSQLRows + web 配線・template
6. docs + private-isu 検証(閾値確定)

## リスク

| リスク | 対策 |
|---|---|
| 全 digest 取得のコスト | digest table 上限 ~1 万行・2 回/世代のみ。probe 失敗で以後停止 |
| 複数 schema を跨ぐアプリ | v1 は接続先 schema のみ(WHERE 条件)。表示にその旨明記 |
| MariaDB の列差異 | probe で列存在も確認し、欠落時 skip |
| baseline と snapshot の間の TRUNCATE | counter 後退検出 → partial(仕様どおり) |

## 見積もり

2.5 日(旧 2.5 日と同等。全件 baseline 化はコスト増だが top-N 突合ロジックが消えるため相殺)。
