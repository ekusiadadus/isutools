# 09: EXPLAIN 自動化 — 再設計版(QUERY_SAMPLE_TEXT 経路)

種別: 機能 / 対象リリース: v1.3.0 / 依存: 01(registry)、04(digest delta) / 新規パッケージ: `queryplan`

## 旧計画(旧05)からの変更点

レビューの CRITICAL 指摘により、**raw exemplar 方式を廃止**する:

1. **前提が hook 実装と不一致だった**: go-sql-proxy の hook が受け取るのは
   query 文字列と引数の**別々の値**であり(現行 hook は
   `[]driver.NamedValue` を破棄している — sqlstats/sqlstats.go:94 で確認)、
   interpolateParams=true でも hook 時点の文字列にリテラルが埋まるわけでは
   ない。「interpolate 済みの生 SQL を保持」は成立しない
2. 4KiB 切り詰めは構文途中になり EXPLAIN 不能
3. raw SQL/args の 2MiB 保持は PII・credential 面を増やす
4. reset/snapshot 競合で SQL 世代と exemplar 世代がずれる
5. **GET /json は毎回 snapshot を生成する**(web/web.go:644 で確認)ため、
   旧設計では画面閲覧のたびに最大 20 件の EXPLAIN が DB へ飛ぶ

代替として MySQL 8 が公式にこの用途向けに提供する
`events_statements_summary_by_digest.QUERY_SAMPLE_TEXT`
(dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-summary-tables.html)
を使う。

## ゴール

1. 04 の区間 delta 上位 digest について、サーバ保持のサンプル SQL で
   EXPLAIN を実行し、type / key / possible_keys / rows / Extra
   (Using filesort / Using temporary)を表示する
2. サンプル SQL(リテラル入り)は**短命利用のみ**:
   メモリに保持せず、snapshot / HTML / JSON へ一切出力しない
3. EXPLAIN の実行は **POST /collect または POST /save の時に 1 回だけ**。
   GET はキャッシュ済み結果を描画する

## 対応範囲(2 系統)

| 環境 | 対応 |
|---|---|
| MySQL 8.0.17+(QUERY_SAMPLE_TEXT あり) | v1 で対応 |
| MySQL 5.7 / MariaDB | **v1 非対応**(capability probe で skip)。将来、アプリが安全な query+args を明示登録する API(`isutools.ExplainTarget(...)`)を別計画で検討 |

## 設計

### capability probe

- 04 の probe に追加: `QUERY_SAMPLE_TEXT` 列の存在を
  information_schema.COLUMNS で確認。無ければ skip(health に記録)
- `performance_schema_max_sql_text_length`(既定 1024)を取得し、
  サンプルが上限長に達している場合は「切り詰めの可能性」として
  当該 digest の EXPLAIN をスキップ(構文途中の EXPLAIN を試みない)

### 実行フロー(collect/save 時のみ)

1. 04 の delta 結果から SUM_TIMER_WAIT 降順で上位
   `ISUTOOLS_EXPLAIN_TOP`(既定 10)の SELECT digest を選ぶ
2. 各 digest について registry(01)の `Inspect` で:
   - inspector session は `SET time_zone = '+00:00'` で **UTC に固定**する
     (v4 修正: TIMESTAMP 取得値は session time zone の影響を受ける)
   - `SELECT QUERY_SAMPLE_TEXT, QUERY_SAMPLE_SEEN FROM ...
     WHERE SCHEMA_NAME = DATABASE() AND DIGEST = ?` で取得
     (v4 修正: digest table の主キーは (SCHEMA_NAME, DIGEST)。
     DIGEST 単独条件では別 schema のサンプルを取り得る)
   - **鮮度判定は DB 側時計だけで行う**(v4 修正: アプリプロセスの
     BoundaryAt と比較しない。DB 専用ホストでは時計が異なる)。
     判定材料は **04 が保存する DB 側 UTC 区間**(v5 修正: 04 の
     データモデルに、baseline 採取と final 採取それぞれの前後で取得した
     `UTC_TIMESTAMP(6)` の before/after ペアが含まれる — 04 §区間の
     妥当性判定を参照)。`QUERY_SAMPLE_SEEN` が
     **[baseline 区間の after, collect 区間の before]** に入る場合のみ
     fresh とする(点時刻比較ではなく保守的な区間判定)。
     区間外は過去 run のサンプルであり、リテラル値で実行計画が変わり得る
     ため **advisor 判定から除外**し、表示は `stale`(取得時刻付き)として
     グレー表示する
   - 区間内なら `EXPLAIN <sample>` を実行。
     **sample 文字列はこの関数スコープ限りで破棄**(構造体へ保存しない)
   - **エラー整形**: driver エラーを Plan.Err に入れる際、エラー文字列に
     sample が埋まっていないことを検査し、digest とエラー種別のみに
     置き換える(raw sample のエラー経由漏洩防止 — v3 追加)
3. 結果(digest キー + プラン行 + SampleSeen/Stale)を run 単位で
   キャッシュし、snapshot / GET はキャッシュを描画する
4. タイムアウト: 1 digest 250ms・全体 2 秒。超過分は Err 記録でスキップ
5. 接続は 01 の registry 契約により **multiStatements を引き継がない**
   (基盤側で保証。本計画では受け入れテストのみ)
6. EXPLAIN 結果の `key` / `possible_keys` 等は **NULL になり得る**。
   `sql.NullString` で受け、表示は空欄(パース失敗にしない)

機能全体の master flag は `ISUTOOLS_EXPLAIN=1`(**明示 opt-in・既定 off**。
EXPLAIN は DB への追加クエリを伴うため。機能単位 ABBA の対象)。

```go
type Plan struct {
    Digest     string    `json:"digest"`
    Query      string    `json:"query"` // 04 と同じ DIGEST_TEXT(正規化済・512B)のみ
    SampleSeen time.Time `json:"sample_seen"`      // QUERY_SAMPLE_SEEN(v3 追加)
    Stale      bool      `json:"stale,omitempty"`  // run 区間外サンプル(advisor 対象外)
    Rows       []PlanRow `json:"rows"`
    Err        string    `json:"err,omitempty"`    // digest とエラー種別のみ(sample 非含有)
}
type PlanRow struct { // v5: MySQL EXPLAIN 出力仕様上、実質全列が NULL になり得る。
                      // scan は sql.Null* で受け、JSON は omitempty の pointer にする
    SelectType   *string `json:"select_type,omitempty"`
    Table        *string `json:"table,omitempty"`
    Type         *string `json:"type,omitempty"`
    Key          *string `json:"key,omitempty"`
    PossibleKeys *string `json:"possible_keys,omitempty"`
    Rows         *int64  `json:"rows,omitempty"`
    Extra        *string `json:"extra,omitempty"`
}
```

### 安全性

- **最小権限 inspector ユーザーを必須にする(v5・CRITICAL 対応)**。
  「SELECT のみ対象だから EXPLAIN は実行を伴わない」は安全保証に
  ならない: MySQL 公式資料のとおり、外側の問い合わせがテーブルへ
  アクセスし内側でデータを変更する stored function を呼ぶ場合、
  `EXPLAIN SELECT` でも**副作用が発生し得る**。対応:
  - EXPLAIN 用接続は専用の最小権限ユーザー(対象 schema への SELECT と
    performance_schema への SELECT のみ。**DML なし・stored function の
    EXECUTE なし**)を必須とし、`ISUTOOLS_EXPLAIN_DSN` 相当の
    別 credential として 01 の registry に登録する
  - 起動時に `SHOW GRANTS` で権限を検証し、**安全な権限を確認できない
    target は EXPLAIN を skip**(health に理由を記録)
  - **副作用 fixture テスト**: データを変更する stored function を含む
    SELECT に対し EXPLAIN を実行し、テーブルが変更されないこと
    (権限エラーで拒否されること)を検証する
  - INTEGRATION.md に GRANT 文の実例を記載する
- 接続は registry 管理の短命接続(multiStatements を付与しない。
  DSN は registry の外に出ない)
- サンプル非保存の保証はテストで固定する
  (snapshot JSON/HTML にリテラルが出ないことの文字列検査。
  検査用にリテラルへ既知マーカーを仕込んだ fixture を使う)

### 表示と advisor

- 「Query plans」セクション: digest(正規化文)ごとにプラン行、
  `type=ALL`・`Using filesort`・`Using temporary` をハイライト
- `advisor.WithQueryPlans`(閾値 provisional、04 と同じ確定手順):
  - `plan-full-scan`: type=ALL かつ rows ≥ 1000 → warn
  - `plan-filesort` / `plan-temporary`: 該当 Extra → warn
- 04 の統計系 warn(no_index_used 等)との関係:
  04 = 網羅的な兆候検出、09 = 根因(使われた index)の提示。
  ID を分離し、重複時は 09 の detail に 04 の数値を併記

## 実装ステップ(TDD)

1. probe(列なし・max_sql_text_length 取得)のテスト先行
2. queryplan: EXPLAIN 行パース・タイムアウト・上限長スキップ・
   sample 非保持(スコープ検査はレビューで担保、出力検査はテスト)
3. collect/save への組み込み(GET で EXPLAIN が走らないことのテスト —
   fake DB への呼び出し回数を計測)
4. advisor / template
5. docs + private-isu 検証(index 削除 → filesort 検出)

## リスク

| リスク | 対策 |
|---|---|
| sample が古い(過去の実行例) | SampleSeen の区間判定で advisor 対象から除外 + stale 表示(データモデルに組み込み済み) |
| 8.0.17 未満・MariaDB | probe で skip(v1 スコープ外を明示) |
| EXPLAIN 負荷 | 上位 10・collect/save 時のみ・250ms/2s 上限 |
| リテラル漏洩 | サンプル文字列の非保存 + 出力の文字列検査テスト |

## 見積もり

probe+queryplan 1.5 日、最小権限ユーザー検証(SHOW GRANTS + 副作用
fixture)0.5 日、配線+docs+検証 1 日。計 3 日。
