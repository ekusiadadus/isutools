# 09: EXPLAIN 自動化 — 再設計版(QUERY_SAMPLE_TEXT 経路)— v6

種別: 機能 / 対象リリース: v1.3.0 / 依存: 01(registry v6)、02(runctl v6 — 実行契機と予算)、04(digest delta v6) / 新規パッケージ: `queryplan`

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] 最小権限 EXPLAIN credential を 01 v6 の purpose 分離で表現し直す**。
   v5 の「`ISUTOOLS_EXPLAIN_DSN` 相当の別 credential として 01 の registry に
   登録する」という記述は**撤回する**。v5 時点の registry は
   「1 TargetID = 1 DSN」だったため、同一 target に 2 本目の credential を
   登録する手段が存在せず(同一 ID の再登録は重複エラー、別 ID にすると
   04 の digest と結合できない)、この計画の CRITICAL 対応は
   **実装不能な記述だった**。
   → 01 v6 の `RegisterDBInspector(targetID, PurposeExplain, driverName, dsn)` と
   `Inspect(ctx, targetID, PurposeExplain, fn)` を**そのまま使う**。
   `PurposeExplain` 未登録時は `ErrPurposeNotRegistered` を受けて
   **当該 target の EXPLAIN を skip** し、health に理由を記録する。
   **`PurposeApp` にも `PurposeStats` にも絶対に fallback しない**
   (§EXPLAIN 用 credential)
2. **[HIGH] `SHOW GRANTS` 単独では role 経由の権限を検出できない**。
   v5 の「起動時に `SHOW GRANTS` で権限を検証する」は**撤回する**。
   `SHOW GRANTS` は account に**直接**付与された権限と
   `GRANT <role> TO <user>` の行しか列挙せず、role が保持する
   INSERT/UPDATE/DELETE/EXECUTE は展開されない。したがって
   role 経由で DML / EXECUTE を持つ account を「安全」と誤判定し得た。
   また検証を「起動時」に別セッションで行う点も誤りで、
   `SET ROLE` はセッション局所であるため EXPLAIN を実行する
   セッションでの検証にならなかった。
   → **同一 pinned connection 上で `SET ROLE NONE` を主対策**とし、
   `CURRENT_ROLE()` で無効化を検証、さらに
   **granted role を `SHOW GRANTS ... USING` で展開して allowlist 判定**する。
   `SET ROLE NONE` が使えない場合の fallback も規定する(§権限検証)
3. **[MEDIUM] driver エラーの「sample が埋まっていたら置換」は撤回**。
   v5 の「エラー文字列に sample が埋まっていないことを検査して
   digest とエラー種別に置き換える」は、**完全一致しか検出できず、
   切り詰め・エスケープ・部分文字列としてのリテラル片が残る**。
   → **raw driver エラーは一切保存しない**。allowlist された分類 enum と
   driver エラーコード(errno / SQLSTATE)だけを持つ
   `PlanError` 構造体に写像する。自由文字列フィールドを型として持たない
   (§エラーの構造化)
4. **(04 v6 追随)鮮度判定を 04 の DB 側 UTC 区間に完全に合わせる**。
   04 v6 が target ごとに保存する `DBClock` を唯一の判定材料とし、
   04 v6 で追加された**時計順序の異常(clock anomaly)時は鮮度判定を
   行わず stale 扱い**の規則を取り込む。`Stale bool` は撤回し、
   3 値の `Freshness`(fresh / stale / unknown)+ 閉じた理由 enum にする
   (§鮮度判定)
5. **(01 v6 追随)セッション設定を pinned connection 上で行う**。
   v5 の「registry 管理の短命接続」という曖昧な表現は撤回する。
   01 v6 の `Inspect` は呼び出しごとに `db.Conn(ctx)` で専用 `*sql.Conn` を
   pin し、その上で `SET time_zone = '+00:00'` を毎回実行する。09 は
   **1 target あたり 1 回の `Inspect` の中で、権限検証・`SET ROLE NONE`・
   サンプル取得・全 digest の EXPLAIN をすべて完了させる**。
   digest ごとに `Inspect` を分割してはならない(再接続で session state が
   失われるため)(§セッション確立)
6. **(自己点検)`WHERE SCHEMA_NAME = DATABASE()` の撤回と `USE` の追加**。
   v5 の sample 取得クエリは `SCHEMA_NAME = DATABASE()` を使っていたが、
   01 v6 の§接続衛生により inspector 接続は `DBName=""` に正規化されるため
   `DATABASE()` は NULL を返し、**このクエリは常に 0 行になる**。
   → `WHERE SCHEMA_NAME = ?` に `TargetInfo.Schema` をバインドする。
   あわせて「既定 DB が無い接続では非修飾テーブル名の EXPLAIN が
   `1046 No database selected` になる」問題を明示的に扱う
   (§既定 DB を持たない接続で EXPLAIN する)

### v6 監査反映(第6回レビュー・cross-file 整合)

- **[C1] EXPLAIN の実行契機を `POST /collect` から切り離した**。
  02 v6 の `POST /collect` は buffered accesslog の**非終端 flush** で、
  run を終了させず・世代を進めず・snapshot を作らず、04 の `Collect` も
  未実行なので**区間 delta が存在しない**。「collect/save の時に 1 回だけ」は
  **撤回**し、唯一の契機を **`FinishRun` 後の background worker
  (02 §FinishRun 手順 7 の `EnrichBudget` 内)** に一本化した
  (§ゴール 3 / §実行フロー / §クエリ数 / §テスト計画)
- **[C2] 独自タイムアウトを撤回し 02 の予算階層に従属させた**。
  「session 確立 500ms・EXPLAIN 全体 2 秒・`Inspect` context 2.5 秒」は
  `PerTargetBudget`(1s)と `EnrichBudget`(2s)を N target で即座に破っており
  **撤回**。`Inspect` の ctx は
  `min(PerTargetBudget, 残 EnrichBudget)`、「全体 2 秒」は **run 全体**の
  `EnrichBudget` と読み替え、「上位 10 × 250ms」を再計算したうえで
  target / digest の drop 記録規則を追加した(§予算と target ファンアウト)
- **[C3] 04 v6 の `DBClock` 型に合わせた**。入れ子表現
  `db_clock: {baseline: [...], final: [...]}` と 09 側での順序比較の
  再実装は**撤回**し、フラットな
  `DBClock{BaselineBefore, BaselineAfter, FinalBefore, FinalAfter,
  Monotonic, Anomaly}` の**フィールド名をそのまま使い**、
  `Monotonic` / `Anomaly` を**消費する**形にした(§鮮度判定)
- **[C4] 解決済み依存を削除**。「04 は `db_clock` を targetID ごとに保存する
  こと」は 04 v6 で充足済みのため §未解決の依存から削除。残る未解決は
  **01 の `Querier` に `ExecContext` が無い**件のみ(§未解決の依存)

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
   メモリに保持せず、snapshot / HTML / JSON へ一切出力しない。
   **driver エラー経由の間接漏洩も型で塞ぐ**(v6)
3. EXPLAIN の実行は **`FinishRun` 後の background worker
   (02 §FinishRun 手順 7 の `EnrichBudget` 内)で 1 回だけ**。
   GET はキャッシュ済み結果を描画する。
   **v6 監査反映: 「`POST /collect` の時にも実行する」は撤回する。**
   02 v6 の§HTTP により `POST /collect` は buffered accesslog の
   **非終端 flush** であり、run を終了させず・世代を進めず・snapshot も
   作らない。さらにこの時点では 04 の `Collect` が未実行で
   **区間 delta が存在しない**ため、上位 digest を選ぶ入力自体が無い
4. EXPLAIN は**専用の最小権限 credential でのみ**実行し、
   その credential が無い target では**実行しない**(v6)
5. EXPLAIN セッション自身のクエリを 04 の計測区間へ混入させない(v6)

## 対応範囲(2 系統)

| 環境 | 対応 |
|---|---|
| MySQL 8.0.17+(QUERY_SAMPLE_TEXT あり) | v1 で対応 |
| MySQL 5.7 / MariaDB | **v1 非対応**(capability probe で skip)。将来、アプリが安全な query+args を明示登録する API(`isutools.ExplainTarget(...)`)を別計画で検討 |

role 関連の文(`SET ROLE NONE` / `CURRENT_ROLE()` / `SHOW GRANTS ... USING`)は
MySQL 8.0 以降にのみ存在するが、本計画の対応環境が 8.0.17+ なので前提を満たす。

## 依存契約(他計画から受け取る名前)

本計画は以下の**正確な名前**に依存する。名前を言い換えない。

| 提供元 | 名前 | 用途 |
|---|---|---|
| 01 v6 | `Purpose` / `PurposeExplain` | EXPLAIN 用 credential の purpose |
| 01 v6 | `RegisterDBInspector(targetID string, purpose Purpose, driverName, dsn string) error` | EXPLAIN credential の登録 |
| 01 v6 | `Inspect(ctx, id string, purpose Purpose, fn func(context.Context, Querier) error) error` | pinned connection の取得 |
| 01 v6 | `Querier`(`QueryContext` / `QueryRowContext`。`Rows` は追跡 wrapper) | 文の実行 |
| 01 v6 | `ErrPurposeNotRegistered` / `ErrUnknownTarget` | skip 判定 |
| 01 v6 | `TargetInfo.Schema`(`PurposeApp` の DBName。非 secret) | `WHERE SCHEMA_NAME = ?` / `USE` |
| 01 v6 | §接続衛生(`DBName=""` / `MultiStatements=false` / `InterpolateParams=false` / `ParseTime=true` / `Loc=UTC`) | 前提 |
| 01 v6 | §Purpose の「`PurposeExplain` は fallback しない」表 | セキュリティ契約 |
| 01 v6 | 環境変数 `ISUTOOLS_EXPLAIN_DSN` / `ISUTOOLS_EXPLAIN_DRIVER`(単一 target 時のみ有効) | 簡便な登録経路(所有は 01) |
| 02 v6 | `FinishRun` 手順 7 の background worker(freeze 後の付加取得フェーズ) | EXPLAIN の**唯一の実行契機**(v6 監査反映) |
| 02 v6 | `runctl.EnrichBudget`(**2s**。run 全体の付加取得フェーズ) | enrich フェーズ全体の上限 |
| 02 v6 | `runctl.PerTargetBudget`(**1s**。01 の `Inspect` 1 回) | 1 target の `Inspect` に張る子 ctx |
| 02 v6 | `runctl.BaselineConcurrency`(**8**) | target ファンアウトの並列度 |
| 04 v6 | `DBClock{BaselineBefore, BaselineAfter, FinalBefore, FinalAfter, Monotonic, Anomaly}`(JSON: `baseline_before` / `baseline_after` / `final_before` / `final_after` / `monotonic` / `anomaly`。時刻はいずれも DB 側 `UTC_TIMESTAMP(6)`) | 鮮度判定 |
| 04 v6 | `DBClock.Monotonic`(bool)/ `DBClock.Anomaly`(安定コード。`clock-missing` / `clock-backwards-baseline` / `clock-backwards-final` / `clock-backwards-interval`) | 鮮度判定の無効化条件。**09 は順序比較を再実装せず、この 2 つを消費する** |
| 04 v6 | 区間 partial 判定(`runctl.ValidityPartial`) | 鮮度判定の無効化条件 |
| 04 v6 | 上位 digest の `DIGEST`・`DIGEST_TEXT`(512B 切り詰め)・delta `SUM_TIMER_WAIT` | 対象選定と表示 |

**04 への要求(充足済み)**: `db_clock` は **targetID ごとに**保存されている
必要がある(DB ホストごとに時計が異なるため、run 全体で 1 組では複数 target
構成で誤判定になる)。**04 v6 §DB 側時計の順序検証で「04 は target ごとに
`DBClock` を snapshot へ載せる」と規定済みであり、この要求は解決している**
(v6 監査反映。§未解決の依存から削除した)。

## 設計

### capability probe(pinned connection 上で実施)

判定は 2 種類に分ける。**セッション状態に依存する判定はキャッシュしない**
(v5 は権限検証を起動時 1 回にしていたため、別セッションの状態を
根拠に EXPLAIN していた。これを撤回する)。

**A. run 単位でキャッシュしてよい(サーバ構成に依存・run 初回の 1 回だけ実行)**

| probe | 判定 | skip 時の health reason |
|---|---|---|
| `PurposeExplain` の登録 | `Inspect` が `ErrPurposeNotRegistered` を返さないこと | `explain-purpose-unregistered` |
| `TargetInfo.Schema != ""` | 01 v6 は `PurposeApp` の DSN に database が無いと `Schema` が空 | `explain-no-schema` |
| `QUERY_SAMPLE_TEXT` 列の存在 | `information_schema.COLUMNS`(TABLE_SCHEMA='performance_schema', TABLE_NAME='events_statements_summary_by_digest', COLUMN_NAME='QUERY_SAMPLE_TEXT') | `explain-unsupported` |
| `performance_schema_max_sql_text_length`(既定 1024) | 取得してキャッシュ | — |

**B. `Inspect` のたびに必ず再実行する(セッション局所・キャッシュ禁止)**

| probe | 判定 | skip 時の health reason |
|---|---|---|
| role の無効化 | §権限検証(手順 1〜2) | `explain-roles-active` |
| 権限の allowlist 判定 | §権限検証(手順 3〜4) | `explain-grants-too-broad` / `explain-grants-unverifiable` |
| セッションの非計装化 | §既定 DB を持たない接続で EXPLAIN する(手順 5〜6) | `explain-session-instrumented` |

サンプル長が `performance_schema_max_sql_text_length` に達している digest は
**構文途中の可能性がある**ため EXPLAIN せず、
`PlanError{Class: "sample_possibly_truncated"}` を記録する。

### EXPLAIN 用 credential(01 v6 の purpose 分離)

**v5 の記述(「`ISUTOOLS_EXPLAIN_DSN` 相当の別 credential として registry に
登録する」)は撤回する。** 正式な経路は 01 v6 の API だけである。

```go
// 登録(アプリ起動時。target ごとに 1 回)
isutools.RegisterDBTarget("db1", "mysql", appDSN)                       // PurposeApp
isutools.RegisterDBInspector("db1", isutools.PurposeExplain, "mysql", explainDSN)

// 使用(FinishRun 後の background worker 内。target ごとに 1 回だけ Inspect する)
err := isutools.Inspect(ctx, "db1", isutools.PurposeExplain,
    func(ctx context.Context, q isutools.Querier) error { /* §セッション確立 */ })
```

- 単一 target 構成に限り、01 v6 が環境変数
  `ISUTOOLS_EXPLAIN_DSN`(+ `ISUTOOLS_EXPLAIN_DRIVER`、既定 `mysql`)を
  読んで `RegisterDBInspector(..., PurposeExplain, ...)` 相当を行う。
  **この解釈と「target が 0 個または 2 個以上なら適用しない」規則は
  01 の所有**であり、09 は結果として得られる登録状態しか見ない
- **未登録時の挙動(規定)**: `Inspect(ctx, id, PurposeExplain, fn)` は
  01 v6 の§Purpose の表のとおり `ErrPurposeNotRegistered` を返す。
  09 はこれを受けて:
  1. 当該 target の EXPLAIN を **skip**(Plan を 1 件も生成しない)
  2. health に
     `queryplan[db1]: skip (explain credential not registered — RegisterDBInspector(id, PurposeExplain, ...) が必要)`
     を reason ID `explain-purpose-unregistered` で記録
  3. 他 target の処理は継続する(fail-open。run 全体は失敗させない)
- **`PurposeApp` / `PurposeStats` への fallback は行わない**。
  これはセキュリティ制御であり、DML 権限を持つ app credential へ暗黙に
  降格させると `EXPLAIN SELECT` が stored function 経由で副作用を起こし得る
  という本計画の CRITICAL 指摘に逆戻りする。
  09 のコードは `PurposeApp` / `PurposeStats` を**参照しない**
  (grep で `PurposeApp` が `queryplan` パッケージに出現しないことを
  受け入れ条件にする)

### セッション確立(pinned connection 上の手順)

01 v6 の `Inspect` は呼び出しごとに専用 `*sql.Conn` を pin し、registry 自身が
その上で `SET time_zone = '+00:00'` を実行する(TIMESTAMP を UTC で受けるため)。
09 は**同一 callback 内**で以下を上から順に実行する。途中で失敗したら
その target を skip して return する(部分実行しない)。

| # | 文 | 目的 | 失敗時 |
|---|---|---|---|
| 0 | (registry) `SET time_zone = '+00:00'` | UTC 固定 | Inspect がエラー → skip |
| 1 | `SET ROLE NONE` | active role をすべて無効化 | fallback(§権限検証) |
| 2 | `SELECT CURRENT_ROLE()` | 1 の効果を**検証**(`NONE` であること) | fallback(§権限検証) |
| 3 | `SHOW GRANTS FOR CURRENT_USER()` | 直接付与権限 + granted role の列挙 | skip `explain-grants-unverifiable` |
| 4 | `SHOW GRANTS FOR CURRENT_USER() USING <roles>`(role がある時のみ) | role 権限の展開 | skip `explain-grants-unverifiable` |
| 5 | `UPDATE performance_schema.threads SET INSTRUMENTED='NO' WHERE PROCESSLIST_ID = CONNECTION_ID()` | 自セッションの非計装化(エラーは無視) | 6 で判定 |
| 6 | `SELECT INSTRUMENTED FROM performance_schema.threads WHERE PROCESSLIST_ID = CONNECTION_ID()` | 5 の効果を**検証** | `'YES'` なら skip `explain-session-instrumented` |
| 7 | probe(§capability probe。run 初回のみ 2 文) | 対応判定 | skip |
| 8 | ``USE `<schema>` `` | EXPLAIN のための既定 DB | skip `explain-no-schema` |
| 9 | サンプル一括取得(1 文) | §実行フロー | 空なら Plan なし |
| 10 | `EXPLAIN <sample>` × N | §実行フロー | digest 単位で `PlanError` |

- **1〜8 を毎回 `Inspect` の先頭で実行する**。「起動時に 1 回検証して
  以後は信じる」という v5 の設計は撤回する。session state
  (`SET ROLE NONE` / `INSTRUMENTED='NO'` / `USE`)は接続の寿命と一致するので、
  再接続で失われた状態を検出せずに EXPLAIN するのが v5 の穴だった
- **順序の要件**: 5〜6(非計装化)は 8(`USE`)より**前**に行う。
  非計装化が失敗して skip する場合でも、それまでの文は既定 DB が NULL の
  ままなので `SCHEMA_NAME IS NULL` 側に集計され、04 の計測対象
  (`SCHEMA_NAME = <app schema>`)を汚さない
- 1〜8 のタイムアウトは合計 **300ms**(§予算と target ファンアウト。
  v6 監査反映: v6 初出の
  「500ms」は**撤回する**。`PerTargetBudget = 1s` の内側で手順 9〜10 に
  残余を確保できないため)。超過は skip
- 01 v6 の `Querier` は `QueryContext` / `QueryRowContext` のみを持つ。
  `SET` / `USE` / `UPDATE` は MySQL の text protocol では結果セット 0 列を
  返すため `QueryContext` で実行できる。返る `Rows` は必ず `Close` する
  (01 の追跡 wrapper が `Inspect` 復帰時に強制 Close するが、
  明示 Close をテストで固定する)。**01 に `ExecContext` が追加された場合は
  そちらへ移行する**(§未解決の依存)

### 権限検証(role 展開を含む)

**主対策**: 手順 1〜2。`SET ROLE NONE` により、そのセッションでは
**直接付与された権限だけが有効**になる。`SELECT CURRENT_ROLE()` が
`NONE` を返すことで実際に無効化されたことを検証する
(`mandatory_roles` / `activate_all_roles_on_login` の影響を仮定せず、
**返り値で確認**する)。

**検出**: 主対策は「無効化」であって「検出」ではない。専用ユーザーに
DML/EXECUTE role が付いている構成は設定ミスであり、次回以降
`SET ROLE NONE` が効かない経路(接続 pool の差し替え、proxy の介在)で
危険になり得る。したがって**無効化とは独立に、granted role を展開して
判定する**:

1. 手順 3 の出力から `` GRANT `r_x`@`%` TO `u`@`%` `` 形の行を抽出し、
   role 名を集める(サーバ出力なのでユーザー入力ではない。
   backtick を二重化して再引用する)
2. role が 1 つ以上あれば手順 4 を実行する:

```sql
SHOW GRANTS FOR CURRENT_USER() USING `r_x`@`%`, `r_y`@`%`;
```

3. **3 の出力と 4 の出力の和集合**を allowlist で判定する

**allowlist(これ以外が 1 つでもあれば skip)**

| 許可 | 例 |
|---|---|
| `USAGE ON *.*` | 実権限なし |
| SELECT ON `<app schema>`.* または `<app schema>`.`<table>` | `GRANT SELECT ON isuconp.* TO ...` |
| SELECT ON `performance_schema`.* または `performance_schema`.`events_statements_summary_by_digest` / `.threads` | — |
| UPDATE ON `performance_schema`.`threads`(任意。手順 5 用) | アプリデータには到達できない |
| `GRANT <role> TO <user>` の行そのもの | role 自体は権限を持たない。role の中身は 2 で展開して同じ allowlist にかける |

- **`SELECT ON *.*` は拒否する**(`mysql.user` 等が読めるため)。
  reason `explain-grants-too-broad`
- INSERT / UPDATE(threads 以外)/ DELETE / CREATE / DROP / ALTER / INDEX /
  REFERENCES / TRIGGER / CREATE ROUTINE / ALTER ROUTINE / EVENT /
  LOCK TABLES / CREATE TEMPORARY TABLES / **EXECUTE** / SUPER / FILE /
  `WITH GRANT OPTION` / `ALL PRIVILEGES` は**すべて拒否**。
  reason `explain-grants-too-broad`
- **EXECUTE が最重要**: EXECUTE が無ければ、`SQL SECURITY DEFINER` の
  stored function を EXPLAIN 経由で呼ぶこと自体ができない
- **パースできない grant 行が 1 行でもあれば skip**
  (allowlist 方式なので未知構文は危険側に倒す)。
  reason `explain-grants-unverifiable`

**fallback(`SET ROLE NONE` が使えない場合)**

手順 1 がエラーになる、または手順 2 が `NONE` 以外を返した場合:

1. 手順 2 の `CURRENT_ROLE()` の返り値(`` `r_x`@`%`,`r_y`@`%` `` 形式)を
   role リストとしてパースする
2. `SHOW GRANTS FOR CURRENT_USER() USING <active roles>` を実行し、
   その結果も allowlist 判定に含める
3. 判定を通れば EXPLAIN を続行する(active role があっても、その権限が
   allowlist 内なら安全)
4. 1 のパースまたは 2 の実行が失敗したら **skip**。
   reason `explain-roles-active`

**INTEGRATION.md への運用要件**: EXPLAIN 用ユーザーの `SELECT` は
**role 経由ではなく直接 GRANT する**こと。role 経由だと `SET ROLE NONE`
後に SELECT すら失えず、EXPLAIN が毎回 `permission_denied` になる。

### 既定 DB を持たない接続で EXPLAIN する

01 v6 の§接続衛生により `PurposeExplain` の接続は `DBName=""` で開かれる。
これは 04 の計測区間を汚さないための必須の正規化だが、そのままだと:

- サンプル取得の `WHERE SCHEMA_NAME = DATABASE()` が NULL 比較になり
  **常に 0 行**(v5 の記述の誤り。撤回する)
- 非修飾テーブル名を含むサンプルの EXPLAIN が
  `1046 No database selected` で失敗する

対応:

- サンプル取得は `WHERE SCHEMA_NAME = ? AND DIGEST IN (?, ...)` とし、
  第 1 引数に **`TargetInfo.Schema`(01 v6)をバインド**する
- EXPLAIN の直前に手順 8 の ``USE `<schema>` `` を発行する。
  識別子は bind parameter にできないため、`TargetInfo.Schema` を
  `^[0-9A-Za-z_$]{1,64}$` で検証してから backtick 二重化して埋め込む。
  検証に失敗したら skip(reason `explain-no-schema`)
- `USE` 以降の文は既定 DB を持つので、**もし計装されていれば**
  `SCHEMA_NAME = <app schema>` の digest 行として記録され 04 を汚す。
  これを構造的に防ぐため、**手順 6 でセッションが `INSTRUMENTED='NO'` で
  あることを確認できた場合にのみ手順 8 以降へ進む**。
  確認できない場合は EXPLAIN 全体を skip する(reason
  `explain-session-instrumented`)
- 非計装化の手段は 2 通り。どちらでも手順 6 の検証が通れば同じ扱い:
  1. 運用側(推奨・追加権限不要): `performance_schema.setup_actors` に
     EXPLAIN 用ユーザーの `ENABLED='NO'` 行を DBA が投入しておく
  2. 実行時: 手順 5 の `UPDATE performance_schema.threads`
     (`UPDATE ON performance_schema.threads` の GRANT が必要。
     §権限検証の allowlist に含めてある)
- INTEGRATION.md に 1 と 2 の両方の SQL を記載する

### 実行フロー(`FinishRun` 後の background worker 内でのみ)

**実行契機(v6 監査反映)**: 09 の EXPLAIN は 02 v6 の `FinishRun` 手順 7 の
background worker が `Collect` を終えた**後**、immutable snapshot を構築する
**前**の付加取得フェーズ(`EnrichBudget`)でのみ走る。
**v5/v6 初出の「`POST /collect` の時に実行する」は撤回する** — 02 v6 §HTTP の
とおり `/collect` は非終端 flush で run を終了させず・世代を進めず・
snapshot を作らず、04 の `Collect` も未実行なので**区間 delta が存在しない**。
`POST /save` / `POST /finish` は `FinishRun` を呼ぶだけであり、EXPLAIN は
その同期応答の**外側**(background worker)で走る。GET と `POST /collect` は
EXPLAIN を 1 回も発行しない。

1. 04 の delta 結果から SUM_TIMER_WAIT 降順で上位
   `ISUTOOLS_EXPLAIN_TOP`(既定 10)の SELECT digest を選ぶ。
   `DIGEST_TEXT`(正規化済・512B)は 04 の結果から受け取り、
   09 は再取得しない
2. target ごとに **1 回だけ** `Inspect(ctx, id, PurposeExplain, fn)` を呼び、
   §セッション確立の手順 1〜10 を実行する
3. サンプル一括取得(1 文):

```sql
SELECT DIGEST, QUERY_SAMPLE_TEXT, QUERY_SAMPLE_SEEN
FROM performance_schema.events_statements_summary_by_digest
WHERE SCHEMA_NAME = ? AND DIGEST IN (?, ?, ...)
```

   - 第 1 引数は `TargetInfo.Schema`(v6 修正: `DATABASE()` は撤回)
   - 主キーは (SCHEMA_NAME, DIGEST) なので、DIGEST 単独条件で
     別 schema のサンプルを取り得るという v4 の指摘は引き続き有効
   - `QUERY_SAMPLE_SEEN` は 01 v6 の `ParseTime=true` / `Loc=UTC` と
     手順 0 の `SET time_zone = '+00:00'` により UTC の `time.Time` で受かる
4. 各 digest について:
   - §鮮度判定を行う。`fresh` 以外は EXPLAIN を実行せず、
     Plan を `Freshness` 付きで記録する(表示はグレー、advisor 対象外)
   - サンプル長が `performance_schema_max_sql_text_length` 以上なら
     `sample_possibly_truncated` で EXPLAIN しない
   - `fresh` なら `EXPLAIN <sample>` を実行。
     **sample 文字列はこの関数スコープ限りで破棄**(構造体へ保存しない)
   - エラーは §エラーの構造化 に従って `PlanError` へ写像する
     (**raw driver エラーは保存も log 出力もしない**)
5. 結果(digest キー + プラン行 + SampleSeen / Freshness)を run 単位で
   キャッシュし、snapshot / GET はキャッシュを描画する
6. タイムアウト: **§予算と target ファンアウト**に従う
   (v6 監査反映: 「session 確立 500ms・EXPLAIN 全体 2 秒・`Inspect` に渡す
   context は 2.5 秒」は**撤回する**。02 が予算階層の唯一の権威であり、
   `Inspect` 1 回に 2.5s を渡すのは `PerTargetBudget = 1s` の違反、
   「全体 2 秒」を target ごとに解釈するのは `EnrichBudget = 2s` の違反)
7. 接続は 01 v6 の§接続衛生により **multiStatements を引き継がない**
   (基盤側で保証。本計画では受け入れテストのみ)
8. EXPLAIN 結果の `key` / `possible_keys` 等は **NULL になり得る**。
   `sql.NullString` で受け、表示は空欄(パース失敗にしない)

機能全体の master flag は `ISUTOOLS_EXPLAIN=1`(**明示 opt-in・既定 off**。
EXPLAIN は DB への追加クエリを伴うため。機能単位 ABBA の対象)。

**クエリ数(target あたり・enrich フェーズ 1 回あたり)**:
registry 1(SET time_zone)+ session 確立 5〜6(SET ROLE NONE /
CURRENT_ROLE / SHOW GRANTS / USING は role 保有時のみ / UPDATE threads /
SELECT threads)+ probe 2(run 初回のみ)+ USE 1 + サンプル取得 1 +
EXPLAIN ≤ 10 = **run 初回 ≤ 21 文、以降 ≤ 19 文**。
手順 5 以降の文は非計装なので digest table には現れない
(1 run = enrich フェーズ 1 回なので、v6 初出の「collect/save 1 回あたり」
という数え方は撤回する)。

### 予算と target ファンアウト(02 §予算モデルの引用)

**09 は 02 の予算階層に新しい親子予算を定義しない**。`PerTargetBudget` の
**内側の配分**(session 確立 / digest 単位など)は 09 が定める
(02 §予算モデルも「per-digest 250ms は 09 の値」と明記している)
(v6 監査反映: v6 初出の「session 確立 500ms・1 digest 250ms・
EXPLAIN 全体 2 秒・`Inspect` context 2.5 秒」という**独自予算は撤回する**。
02 §予算モデルは「下流計画は定数名と数値を引用し、独自値を定義しない」と
規定しており、旧記述は N target で親予算を即座に破っていた)。

| 02 の定数 | 値 | 09 での適用 |
|---|---|---|
| `runctl.EnrichBudget` | **2s** | **run 全体**の enrich フェーズ。全 target・全 digest の合計。「EXPLAIN 全体 2 秒」とはこれのことで、**target ごとの 2 秒ではない** |
| `runctl.PerTargetBudget` | **1s** | 1 target の `Inspect` 1 回 |
| `runctl.BaselineConcurrency` | **8** | target ファンアウトの並列度 |

**`Inspect` に渡す ctx**:

```
perTargetCtx = context.WithTimeout(enrichCtx,
                   min(runctl.PerTargetBudget, deadline(enrichCtx) − now))
```

**1 target 1s の内訳(合計 1s を超えない)**

| 区間 | 上限 | 備考 |
|---|---|---|
| 手順 1〜8(session 確立 + probe + `USE`) | 300ms | v6 初出の 500ms から短縮 |
| 手順 9(サンプル一括取得 1 文) | 100ms | |
| 手順 10(EXPLAIN ループ) | 残余 ≥ 600ms | 1 digest あたりの**上限**は 250ms |

**「上位 10 digest × 250ms」の再計算**: 10 × 250ms = 2.5s は
`PerTargetBudget`(1s)にも `EnrichBudget`(2s)にも**入らない**。
したがって 250ms は「1 digest がこれ以上かかったら打ち切る上限」であって
**予約枠ではない**。実測の EXPLAIN は数 ms 級なので通常は 600ms に 10 件が
収まる。最悪ケース(全件が上限に張り付く)では **2 件で残余を使い切る**。
`ISUTOOLS_EXPLAIN_TOP`(既定 10)は**選定の上限であって実行の保証ではない**。

**収まらない場合に何を落とすか(黙って切り詰めない)**

1. target は 04 の delta `SUM_TIMER_WAIT` 合計の降順、同値は `TargetID` 昇順で
   波に割り当てる(処理順は決定的)。並列度は `runctl.BaselineConcurrency`(8)
2. 波を開始する前に `deadline(enrichCtx) − now < 400ms`
   (= 手順 1〜8 の 300ms + サンプル取得 100ms)なら**その波を開始しない**。
   接続も開かない
3. 未処理 target は Plan を 1 件も生成せず、health
   `queryplan[db3,db4]: skip (enrich budget exhausted)` を
   reason ID `explain-budget-exhausted` で記録する
4. digest ループは 1 件ごとに残余を見て、`残余 < 250ms` になったら**打ち切る**。
   打ち切った digest は `PlanError{Class: "budget_exhausted"}` の Plan として
   **記録する**(結果 map から消さない)
5. `EnrichBudget` 超過は run を失敗させない(fail-open)。09 は 02 の
   collector 契約上 `Required:false` 相当であり、run の `Validity` を
   下げるのは 04 の責務である

**充足の根拠**: 16 target ÷ `BaselineConcurrency`(8)= 2 波、
最悪 2 × `PerTargetBudget`(1s)= **2s ≤ `EnrichBudget`(2s)**。
9 target 以上では最終波が手順 2 の事前チェックで落ちうるため、
その分は必ず `explain-budget-exhausted` として記録される。

### データモデル

```go
type Plan struct {
    Digest     string         `json:"digest"`
    Query      string         `json:"query"`      // 04 と同じ DIGEST_TEXT(正規化済・512B)のみ
    SampleSeen time.Time      `json:"sample_seen"` // QUERY_SAMPLE_SEEN(DB 側 UTC)
    Freshness  FreshnessState `json:"freshness"`   // v6: v5 の Stale bool は撤回(3 値が必要)
    FreshReason FreshReason   `json:"fresh_reason,omitempty"` // 閉じた enum。自由文字列ではない
    Rows       []PlanRow      `json:"rows"`
    Err        *PlanError     `json:"err,omitempty"` // v6: v5 の Err string は撤回
}

// v6: 3 値。clock anomaly のとき「stale と断定」もできないため unknown が要る。
type FreshnessState string
const (
    FreshnessFresh   FreshnessState = "fresh"   // advisor 対象
    FreshnessStale   FreshnessState = "stale"   // 区間外と判定できた
    FreshnessUnknown FreshnessState = "unknown" // 判定材料が信用できない
)

type FreshReason string
const (
    FreshInInterval     FreshReason = "in_interval"
    FreshBeforeInterval FreshReason = "before_interval"
    FreshAfterInterval  FreshReason = "after_interval"
    FreshClockAnomaly   FreshReason = "db_clock_anomaly"   // 04 v6 の新規則
    FreshClockMissing   FreshReason = "db_clock_missing"
    FreshRunPartial     FreshReason = "run_partial"
    FreshIntervalShort  FreshReason = "interval_too_short"
)

type PlanRow struct { // v5 から維持: MySQL EXPLAIN 出力仕様上、実質全列が NULL になり得る。
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

`Plan` に**サンプル由来の文字列を格納できるフィールドが 1 つも無い**ことが
v6 の構造的保証である(`Query` は 04 由来の正規化文、`Err` は §エラーの構造化)。

### 鮮度判定(04 v6 の DB 側 UTC 区間)

**鮮度判定は DB 側時計だけで行う**(v4 から維持: アプリプロセスの
`BoundaryAt` と比較しない。DB 専用ホストでは時計が異なる)。
判定材料は 04 v6 が **targetID ごとに** snapshot へ載せる
`DBClock` **1 つだけ**である(04 v6 §DB 側時計の順序検証):

```go
// 04 v6 が所有する型。09 は再定義せず、この形のまま受け取る。
type DBClock struct {
    BaselineBefore time.Time `json:"baseline_before"`
    BaselineAfter  time.Time `json:"baseline_after"`
    FinalBefore    time.Time `json:"final_before"`
    FinalAfter     time.Time `json:"final_after"`
    Monotonic      bool      `json:"monotonic"`
    Anomaly        string    `json:"anomaly,omitempty"`
}
```

**v6 監査反映**: v6 初出の入れ子表現
`db_clock: {baseline: [before, after], final: [before, after]}` は
**撤回する**。04 v6 の実体は上の**フラットな構造体**であり、配列でも
入れ子でもない。あわせて、**09 が
`baseline.before ≤ baseline.after ≤ final.before ≤ final.after` を
自前で再計算する記述も撤回する**。順序検証は 04 が済ませており、
09 は `Monotonic` / `Anomaly` を**消費するだけ**である
(比較を二重実装すると 04 の異常コード分類と食い違い得る)。

**判定不能条件(EXPLAIN の実行より前に評価する。
1 つでも該当したら EXPLAIN を実行しない)**

| 条件 | Freshness | FreshReason |
|---|---|---|
| 当該 target の `DBClock` が snapshot に無い(04 が当該 target を skip) | `unknown` | `db_clock_missing` |
| `DBClock.Monotonic == false`(04 v6 が順序検証済み。**09 は比較を再実装しない**。`DBClock.Anomaly` が `clock-missing` / `clock-backwards-baseline` / `clock-backwards-final` / `clock-backwards-interval` のいずれか) | `unknown` | `db_clock_anomaly` |
| 04 が当該 run を partial と判定(server_uuid 変化 / Uptime 減少 / baseline keyset 消失 / counter 後退) | `unknown` | `run_partial` |
| 後述の fresh 窓が空(`lo > hi`) | `unknown` | `interval_too_short` |

v5 は「区間外なら stale」の 2 値しか持たず、**時計が壊れている場合に
`SampleSeen` を区間と比較して fresh と誤判定し得た**。この 2 値設計は撤回する。
clock anomaly 時は「stale と断定する」のではなく **判定そのものを行わず**、
`unknown` として advisor 対象外・グレー表示にする(04 v6 の
「`Monotonic == false` ⇒ 09 は鮮度判定を一切行わない」を、表示・advisor の
扱いは stale と同じまま、理由が識別できる形で実装したもの)。
このとき **EXPLAIN も実行しない**(04 v6 §09 への契約の表と一致)。
表示に出す理由コードは 09 で作り直さず、`DBClock.Anomaly` の値をそのまま
日本語ラベルへ写像する。

**fresh の定義(判定可能な場合)**

```
前提: DBClock.Monotonic == true(04 が検証済み。09 は再検証しない)

lo = ceil_sec(DBClock.BaselineAfter) + 1s
hi = floor_sec(DBClock.FinalBefore)  - 1s
fresh  ⟺  lo ≤ QUERY_SAMPLE_SEEN ≤ hi
```

- 区間は **[`DBClock.BaselineAfter`, `DBClock.FinalBefore`]**。
  点時刻比較ではなく保守的な区間判定である(v5 から維持)。
  `BaselineBefore` / `FinalAfter` は 04 の順序検証用であり、
  09 の窓計算には**使わない**
- 両端を 1 秒ずつ**内側へ**狭めるのは、`QUERY_SAMPLE_SEEN` の
  小数秒桁数がサーバ設定に依存し、秒に切り捨てられた値が最大 1 秒
  古く見えるため。**false-fresh を作らない方向にのみ丸める**
- `QUERY_SAMPLE_SEEN < lo` → `stale` / `before_interval`
- `QUERY_SAMPLE_SEEN > hi` → `stale` / `after_interval`
- `stale` / `unknown` は過去 run または判定不能のサンプルであり、
  リテラル値で実行計画が変わり得るため **advisor 判定から除外**し、
  表示は取得時刻と理由付きでグレー表示する

### エラーの構造化(raw driver error を保存しない)

**v5 の「エラー文字列に sample が埋まっていないことを検査して置き換える」は
撤回する。** 完全一致検査は、(a) driver が sample を切り詰めて埋め込む場合、
(b) 引用符・改行がエスケープされる場合、(c) `near '...'` のように
**部分文字列**だけを含む場合(MySQL の 1064 が典型)を検出できない。

**v6: raw driver エラーは `Plan` にも snapshot にも log にも一切出さない。**
`Inspect` の callback スコープ内で分類に写像し、元の `error` はその場で捨てる。

```go
type PlanError struct {
    Class    PlanErrorClass `json:"class"`              // 閉じた enum
    Errno    uint16         `json:"errno,omitempty"`    // MySQL エラー番号(数値)
    SQLState string         `json:"sqlstate,omitempty"` // ^[0-9A-Z]{5}$ を満たす時のみ
}

type PlanErrorClass string
const (
    PlanErrTimeout          PlanErrorClass = "timeout"
    PlanErrBudgetExhausted  PlanErrorClass = "budget_exhausted" // v6 監査反映: 実行前に打ち切った digest を記録する
    PlanErrPermission       PlanErrorClass = "permission_denied"
    PlanErrSyntax           PlanErrorClass = "syntax_or_truncated"
    PlanErrObjectMissing    PlanErrorClass = "object_missing"
    PlanErrSampleUnavail    PlanErrorClass = "sample_unavailable"
    PlanErrSampleTruncated  PlanErrorClass = "sample_possibly_truncated"
    PlanErrConnection       PlanErrorClass = "connection_error"
    PlanErrOther            PlanErrorClass = "other"
)
```

**写像表(固定。errno が表に無ければ `other` + errno のみ保存)**

| 判定元 | Class |
|---|---|
| `context.DeadlineExceeded` / 1317 / 3024 | `timeout` |
| `PerTargetBudget` / `EnrichBudget` の残余不足で**実行しなかった** | `budget_exhausted` |
| 1044 / 1045 / 1142 / 1143 / 1370 | `permission_denied` |
| 1064 / 1149 | `syntax_or_truncated` |
| 1046(No database selected) | `object_missing`(手順 8 の不具合。health にも記録) |
| 1051 / 1054 / 1109 / 1146 | `object_missing` |
| `driver.ErrBadConn` / 2006 / 2013 | `connection_error` |
| サンプル行が無い | `sample_unavailable` |
| サンプル長 ≥ max_sql_text_length | `sample_possibly_truncated` |
| 上記以外 | `other` |

**漏洩不能性の担保(型と検査の二段)**

1. **型**: `PlanError` の文字列フィールドは `Class`(閉じた定数集合)と
   `SQLState`(正規表現検証済み)だけ。自由文字列フィールドを持たない。
   `reflect` で `PlanError` の全フィールドを走査し、
   **`Class` / `SQLState` 以外の string 型フィールドが追加されたら失敗する**
   テストを置く(将来の回帰防止)
2. **値**: `Errno` は `uint16`、`SQLState` は書き込み前に
   `^[0-9A-Z]{5}$` を検査し、外れたら空文字にする
   (driver が細工された SQLSTATE を返しても素通りしない)

### 安全性

- **最小権限 inspector ユーザーを必須にする(v5 の CRITICAL 対応を維持)**。
  「SELECT のみ対象だから EXPLAIN は実行を伴わない」は安全保証に
  ならない: MySQL 公式資料のとおり、外側の問い合わせがテーブルへ
  アクセスし内側でデータを変更する stored function を呼ぶ場合、
  `EXPLAIN SELECT` でも**副作用が発生し得る**。対応:
  - EXPLAIN 用接続は専用の最小権限ユーザーとし、01 v6 の
    `RegisterDBInspector(id, PurposeExplain, ...)` で登録する
    (§EXPLAIN 用 credential。v5 の登録方法の記述は撤回済み)
  - 権限検証は **EXPLAIN と同一の pinned connection 上**で、
    `SET ROLE NONE` + role 展開 + allowlist で行う(§権限検証)
  - **安全な権限を確認できない target は EXPLAIN を skip**
    (health に reason ID 付きで記録)
  - INTEGRATION.md に GRANT 文・setup_actors 行・
    「SELECT は role 経由にしない」注意を記載する
- 接続は 01 v6 の registry 管理(§接続衛生: `DBName=""` /
  `MultiStatements=false` / `InterpolateParams=false` / `ParseTime=true` /
  `Loc=UTC`)。DSN は registry の外に出ない
- EXPLAIN セッションは非計装(§既定 DB を持たない接続で EXPLAIN する)。
  09 のクエリが 04 の digest delta に現れない
- サンプル非保存の保証はテストで固定する
  (snapshot JSON/HTML にリテラルが出ないことの文字列検査。
  検査用にリテラルへ既知マーカーを仕込んだ fixture を使う)

### 表示と advisor

- 「Query plans」セクション: digest(正規化文)ごとにプラン行、
  `type=ALL`・`Using filesort`・`Using temporary` をハイライト
- `Freshness != fresh` の行はグレー表示し、`FreshReason` を
  日本語ラベルに写像して併記(例: `db_clock_anomaly` → 「DB 時計異常のため判定不能」)
- `advisor.WithQueryPlans`(閾値 provisional、04 と同じ確定手順)。
  **入力は `Freshness == fresh` の Plan のみ**:
  - `plan-full-scan`: type=ALL かつ rows ≥ 1000 → warn
  - `plan-filesort` / `plan-temporary`: 該当 Extra → warn
- 04 の統計系 warn(no_index_used 等)との関係:
  04 = 網羅的な兆候検出、09 = 根因(使われた index)の提示。
  ID を分離し、重複時は 09 の detail に 04 の数値を併記

## 実装ステップ(TDD)

1. probe(列なし・max_sql_text_length 取得・Schema 空)のテスト先行
2. 権限検証: `SHOW GRANTS` パーサ(直接権限 / role 行 / USING 展開 /
   未知構文)と allowlist 判定のテスト先行
3. セッション確立の手順順序(非計装化 → `USE`)のテスト
4. 鮮度判定: fresh / before / after / `Monotonic == false` / `DBClock` 欠落 /
   run partial / 窓が空 のテーブル駆動テスト(04 の `DBClock` を注入し、
   09 側で順序比較を再実装していないことを固定する)
5. `PlanError` 写像と reflect による自由文字列フィールド禁止テスト
6. queryplan: EXPLAIN 行パース・タイムアウト・上限長スキップ・
   sample 非保持(スコープ検査はレビューで担保、出力検査はテスト)
7. 02 の `FinishRun` background worker(手順 7 の `EnrichBudget` 区間)への
   組み込み(GET と **`POST /collect`** で EXPLAIN が走らないことのテスト —
   fake DB への呼び出し回数を計測)。あわせて `EnrichBudget` /
   `PerTargetBudget` の残余に基づく target / digest の drop 記録
8. advisor / template
9. docs + private-isu 検証(index 削除 → filesort 検出)

## テスト計画

- unit: `PurposeExplain` 未登録 → `ErrPurposeNotRegistered` を受けて
  **Plan を 1 件も生成せず**、health に `explain-purpose-unregistered` を記録。
  fake driver に渡った DSN の user が **app / stats の user ではない**こと
  (= fallback していないこと)を検証
- unit: `queryplan` パッケージのソースに `PurposeApp` / `PurposeStats` が
  出現しないこと(grep ベースの受け入れ条件)
- **integration(MySQL fixture)[HIGH 対応の必須テスト]**:
  `CREATE ROLE r_dml; GRANT INSERT,UPDATE,DELETE ON isuconp.* TO r_dml;
   GRANT r_dml TO isutools_explain;` の状態(`activate_all_roles_on_login=ON`)で
  09 を実行し、**EXPLAIN が 1 件も実行されず**、health に
  `explain-grants-too-broad` が記録されること。
  `SET ROLE NONE` が成功して `CURRENT_ROLE()` が `NONE` を返す場合でも
  **skip すること**(role 展開による検出が効いている証明)
- integration: `GRANT EXECUTE ON isuconp.* TO r_exec; GRANT r_exec TO ...` でも
  同様に skip
- integration: 正しい最小権限(`SELECT ON isuconp.*` +
  `SELECT ON performance_schema.*` + 任意で
  `UPDATE ON performance_schema.threads`、role なし)で EXPLAIN が実行されること
- integration: `SELECT ON *.*` は `explain-grants-too-broad` で skip
- **副作用 fixture テスト(v5 から維持)**: データを変更する
  stored function を含む SELECT に対し EXPLAIN を実行し、
  テーブルが変更されないこと(EXECUTE 権限が無く拒否されること)を検証
- **integration: 非計装化**: 09 を実行した前後で
  `events_statements_summary_by_digest` の `SCHEMA_NAME = 'isuconp'` 側に
  09 由来の digest(`EXPLAIN ...` / `USE ...`)が **1 件も増えない**こと
- integration: `UPDATE performance_schema.threads` の GRANT を外し
  setup_actors も設定しない状態では、`explain-session-instrumented` で
  **EXPLAIN が skip される**こと
- unit: 鮮度判定テーブル駆動(入力は 04 v6 の `DBClock` 構造体)—
  `BaselineAfter` の 0.5 秒前 → `stale/before_interval`、
  `FinalAfter` ではなく `FinalBefore` の 0.5 秒後 → `stale/after_interval`、
  区間中央 → `fresh/in_interval`、
  `Monotonic=false, Anomaly="clock-backwards-interval"` →
  `unknown/db_clock_anomaly`(EXPLAIN 不実行)、
  `Monotonic=false, Anomaly="clock-missing"` → 同上、
  当該 target の `DBClock` 自体が無い → `unknown/db_clock_missing`、
  04 partial → `unknown/run_partial`、
  区間幅 1 秒 → `unknown/interval_too_short`
- unit: `Monotonic=false` の入力に対し、09 が `BaselineBefore` /
  `FinalAfter` を**一度も読まない**こと(順序比較の再実装が無いことの固定)
- unit: `unknown` / `stale` の Plan が `advisor.WithQueryPlans` の入力から
  除外されること
- **[MEDIUM 対応の必須テスト] 漏洩検査**: リテラルに既知マーカー
  `ZZ_ISUTOOLS_MARKER_ZZ` を含む fixture で、
  permission_denied / syntax(1064。MySQL は `near '...'` で
  **サンプルの部分文字列**をエラーに含める)/ object_missing / timeout の
  4 系統を発生させ、
  (a) `Plan` の JSON、(b) snapshot JSON、(c) HTML の 3 出力すべてに
  マーカーが現れないこと、さらに
  (d) **サンプル文字列の長さ 4 以上の全部分文字列**が出力に現れないこと
  を検証する
- unit: reflect で `PlanError` に `Class` / `SQLState` 以外の string
  フィールドが無いこと
- unit: driver が `SQLSTATE` に `'ZZ_MARKER'` のような値を返しても
  `SQLState` が空になること
- unit: GET /json を 10 回叩いても EXPLAIN の実行回数が 0 のままであること
- **unit: `POST /collect` を 10 回叩いても EXPLAIN の実行回数が 0 のまま**で
  あること(v6 監査反映。`/collect` は非終端で 04 の delta も存在しない)
- unit: 1 target あたり `Inspect` の呼び出しが **1 run の enrich フェーズ
  1 回につき 1 回だけ**であること(digest ごとに分割していないこと)
- unit: `Inspect` に渡された ctx の deadline が
  `min(runctl.PerTargetBudget, 残 EnrichBudget)` を超えないこと
  (fake clock で `EnrichBudget` の残余を 400ms に設定し、
  子 ctx が 1s ではなく 400ms 以下になることを固定)
- unit: enrich 残余不足で開始しなかった target が health
  `explain-budget-exhausted` に**記録される**こと(結果から黙って
  消えないこと)。同様に打ち切った digest が
  `PlanError{Class:"budget_exhausted"}` として残ること

## リスク

| リスク | 対策 |
|---|---|
| explain credential の設定漏れ | `ErrPurposeNotRegistered` → skip + health(fallback しない。01 v6 §Purpose) |
| role 経由の危険権限 | `SET ROLE NONE`(無効化)+ `SHOW GRANTS ... USING` 展開(検出)の二段。未知構文は skip |
| driver エラー経由のリテラル漏洩 | raw error を保存しない構造化 `PlanError` + reflect テスト + 部分文字列検査 |
| sample が古い(過去の実行例) | `DBClock` の `[BaselineAfter, FinalBefore]` 区間判定で advisor 対象から除外 + グレー表示 |
| DB 時計の異常 | 04 v6 の `DBClock.Monotonic == false` を**そのまま消費**し `unknown/db_clock_anomaly` として判定放棄(比較は 09 で再実装しない) |
| 09 のクエリが 04 の計測を汚す | セッション非計装化を**検証してから** `USE` する。検証できなければ skip |
| 8.0.17 未満・MariaDB | probe で skip(v1 スコープ外を明示) |
| EXPLAIN 負荷 | 上位 10・**enrich フェーズのみ**(GET / `POST /collect` では 0 回)・`PerTargetBudget`(1s)/ `EnrichBudget`(2s)の内側・target あたり Inspect 1 回 |
| enrich 予算に全 target が収まらない | 決定的順序で処理し、未処理は `explain-budget-exhausted`、未実行 digest は `PlanError{Class:"budget_exhausted"}` として**記録**(黙って切り詰めない) |

## 他計画への依存(v6 監査で全て解決済み)

- ~~**01**: `Querier` に `ExecContext` が無い~~ —
  **v6 監査反映: 01 v6 が受理して追加済み**。01 §registry API の `Querier` に
  `ExecContext(ctx, q string, args ...any) (sql.Result, error)` が入り、
  使用は session 設定文(先頭トークン `SET`)に allowlist され、
  違反は `ErrExecNotAllowed` になる。09 は pin 済み `*sql.Conn` 上で
  `SET ROLE NONE` / `SET time_zone` をこれで発行する。
  **`USE \`<schema>\`` と `UPDATE performance_schema.threads ...` は
  allowlist 外**なので、前者は §既定 DB を持たない接続で EXPLAIN する の
  スキーマ修飾方式で回避し、後者は §非計装セッション の方式に従う
  (どちらも `ExecContext` を必要としない形に確定済み)
- (以下は経緯の記録)01 v6 の `Querier` は
  `QueryContext(ctx, q string, args ...any) (Rows, error)` と
  `QueryRowContext(...) Row` の 2 メソッドしか持たず、**`ExecContext` が無い**。
  09 は結果セットを返さない文を 4 種類発行する必要がある:
  `SET ROLE NONE` / `USE \`<schema>\`` / `SET time_zone`(registry 側)/
  `UPDATE performance_schema.threads SET INSTRUMENTED='NO' ...`。
  現状はこれらを `QueryContext` で発行し、返る `Rows` を即 `Close` している
  (MySQL の text protocol では動作するが意図が不明瞭で、`Rows` の
  取り回しが無駄)。
  **01 に追加してほしいもの(正確なシグネチャ)**:

  ```go
  type Querier interface {
      QueryContext(ctx context.Context, q string, args ...any) (Rows, error)
      QueryRowContext(ctx context.Context, q string, args ...any) Row
      ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) // 09 が要求
  }
  ```

  条件: pin 済みの同一 `*sql.Conn` 上で実行されること(session 局所の
  `SET ROLE NONE` / `USE` が EXPLAIN と同じセッションに効く必要がある)。
  → **01 v6 で追加済み**(上記のとおり allowlist 付き)
- ~~**04**: `db_clock` を targetID ごとに保存すること~~ —
  **v6 監査反映: この依存は解決済みなので削除する**。04 v6
  §DB 側時計の順序検証で `DBClock` 型が定義され、
  「04 は target ごとに `DBClock` を snapshot へ載せる」と明記されている。
  clock anomaly / run partial も `DBClock.Monotonic` / `DBClock.Anomaly` /
  `runctl.ValidityPartial` として 09 から参照可能になっている

## 見積もり

probe + queryplan 1.5 日、権限検証(`SET ROLE NONE` + role 展開 +
allowlist パーサ + fixture)1 日、非計装セッション + `USE` 対応 0.5 日、
構造化エラーと漏洩検査テスト 0.5 日、配線 + docs + 検証 1 日。
**計 4.5 日**(v5 の 3 日から増。role 展開・非計装化・エラー構造化の
3 件が新規)。
