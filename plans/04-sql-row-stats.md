# 04: SQL 行効率計測(rows examined / rows sent)— v6

種別: 機能 / 対象リリース: v1.2.0 / 依存: 01(registry)、02(run lifecycle)/ 新規パッケージ: `sqlrows`

**本ファイルは自前の予算値を定義しない**。時間予算・collector 契約・validity は
02(唯一の正)を引用し、接続・schema 名・purpose は 01 を引用する。

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] collector 自身のクエリによる計測区間の汚染を解消**。
   v5 は「stats 用接続が対象 DB を既定データベースとして持つ」前提で
   `WHERE SCHEMA_NAME = DATABASE()` を使っていた。performance_schema は
   **実行時点の既定データベース**を文に帰属させるため、baseline 採取文
   そのものが baseline スナップショット確定の**後**にアプリ schema 側へ
   計上され、**必ず最終 delta に現れる**。
   → 統計用接続は **既定データベースを持たない**(01 v6 §接続衛生の
   `DBName=""`)。対象 schema は registry の `TargetInfo.Schema` を
   **バインド引数**として渡す(`WHERE SCHEMA_NAME = ?`)。
   本書の SQL を全文書き換えた(§SQL 全文)。
   collector 自身の digest が delta に現れないことを**必須の
   integration テスト**にした(§自己汚染テスト)
2. **[HIGH] 時間予算の二重定義を解消**。v5 の「per-target 1 秒・全体 3 秒」は
   02 v6 の `PhaseStartBaselineBudget = 5s` /
   `PerCollectorBaselineBudget = 3.5s` と衝突していた。
   → 02 §予算モデルの定数名と数値を**引用のみ**にし、16 target が
   収まる根拠と、収まらない場合の**drop 対象と記録方法**を定義した
   (§予算と target ファンアウト)
3. **[MEDIUM] 実行文数の列挙が不正確**。v5 で追加した
   `UTC_TIMESTAMP(6)` 4 回と、01 が Inspect ごとに実行する
   `SET time_zone = '+00:00'` を数え落としていた。
   → probe 文と定常 run 文を分けた**境界別の完全な列挙**(表 A〜D)を置き、
   ABBA の実測条件をこの数へ更新した(§文数の正確な列挙)
4. **[MEDIUM] DB 側時計の逆行(NTP 等)を扱っていなかった**。
   `final.UTCBefore < baseline.UTCAfter` になると 09 の鮮度判定区間が
   逆転し、全サンプルが誤って `stale` になる。
   → 保存済み DB-UTC 4 点の**順序検証**を定義し、異常時は run を partial に
   して **09 に鮮度判定を一切させない**契約にした(§DB 側時計の順序検証)
5. **[MEDIUM] 01 v6 / 02 v6 の API 名へ追随**。
   `Inspect(ctx, id, purpose, fn)`、`isutools.PurposeStats`、
   `TargetInfo.Schema`、`runctl.BaselineCollector`
   (`CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release`)
   に合わせ、v5 の「Reset 時 / Snapshot 時」という phase 名も
   02 の `start-baseline` / `finish-final` へ置換した
6. **v6 監査反映**(他計画との突き合わせ結果を反映)。
   - **09 は改訂済み**: 09 v6 が `Freshness`(fresh / stale / unknown)+
     `FreshReason` を既に採用しているため、§09 への契約の
     「09 側の改訂が必要(`Plan.Stale bool` は 2 値)」を**撤回**した
   - **README は再算定済み**: plans/README.md v6 の再算定表と本書の
     **3.5 日**が一致していることを確認し、§見積もりの
     「README 側の再算定が必要」を**撤回**した
   - **実測を追記**: MySQL 9.7.2 実機で自己汚染修正の有効性を検証し
     (§実測による裏付け)、`SCHEMA_NAME IS NULL` 行が overflow 行だけでない
     ことを踏まえ overflow 判定を
     **`SCHEMA_NAME IS NULL AND DIGEST IS NULL`(両方 NULL)**に明確化。
     `TestOverflowRequiresBothNull` を追加した

## v5 から撤回する主張

| v5 の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| `WHERE SCHEMA_NAME = DATABASE()` | 01 v6 の統計用接続は既定 DB を持たないので `DATABASE()` は NULL を返し 0 行になる。かつ既定 DB を持つ接続で実行すると collector 自身の文がアプリ schema の digest として記録され、**baseline 採取文が baseline 確定後に計上されて必ず delta へ入る** | `WHERE SCHEMA_NAME = ? OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL)` に `TargetInfo.Schema` をバインド(§SQL 全文) |
| 「per-target 1 秒・全体 3 秒の context」 | 02 v6 §予算モデルが唯一の権威であり、全体 3s は `PerCollectorBaselineBudget = 3.5s` と別数値の二重定義になる | 02 の定数を引用のみ(§予算と target ファンアウト) |
| 「Reset 時 = probe 2 + server_uuid/Uptime 1 + digest 全件 1、Snapshot 時 = 3 クエリ」 | v5 自身が追加した `UTC_TIMESTAMP(6)` 4 回と、01 の `SET time_zone` を計上していない | 表 A〜D の完全列挙(§文数の正確な列挙) |
| `SHOW GLOBAL STATUS LIKE 'Uptime'` を無条件に使う | SHOW 文は `UTC_TIMESTAMP(6)` と 1 文に畳み込めず、境界あたりの文数が増える | 既定は `performance_schema.global_status`。probe P4 が失敗した target のみ SHOW 経路(§SQL 全文) |
| 「Reset 時 / Snapshot 時」という phase 名 | 02 v6 で `/collect` は**非終端**になり境界を張らない。`Snapshot 時` に対応する境界は存在しない | `CaptureBaseline`(start-baseline phase)/ `CaptureFinal`(finish-final phase) |
| 01 の `Inspect(ctx, id, fn)` | 01 v6 で `Inspect(ctx, id, purpose, fn)` に変更(01 §registry API) | `Inspect(ctx, id, isutools.PurposeStats, fn)` |
| DB-UTC の before/after を「保存するだけ」 | 順序が壊れた場合の扱いが無く、09 が空区間/逆区間で全 sample を stale と誤判定する | §DB 側時計の順序検証(異常時は 09 が鮮度判定を行わない) |

## 旧計画(旧01)からの変更点(v3 以前・維持)

レビューの CRITICAL 指摘 3 件を反映:

1. **「上位 200 件だけの baseline」を廃止** — baseline 時に 201 位以下だった
   digest がベンチ後に上位へ入ると、過去の累積値を区間値として誤計上する。
   baseline / final とも**対象 schema の全 digest を取得**し、差分計算後に
   表示上位 200 へ切る
2. **「COUNT_STAR 後退 = 追い出し」の誤りを修正** — MySQL の digest table は
   満杯時に行を追い出すのではなく、未登録 digest を
   `SCHEMA_NAME=NULL, DIGEST=NULL` の**特殊行へ集約**する(公式仕様:
   dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-digests.html)。
   overflow は NULL 行のデルタで検出し、独立 health として扱う。
   counter 後退は外部 TRUNCATE・DB 再起動・instrument reset を意味するので
   当該 target の区間を無効化する(§区間の妥当性判定)
3. **RowsSent=0 の比率定義を修正** — 比率 = RowsExamined とせず **N/A**。
   SELECT と DML を分離して表示する

## ゴール

- ベンチ区間(start-baseline → finish-final)の digest 別
  rows examined / rows sent / 回数 / 総時間を registry の **target ごと**に表示
- SELECT の examined/sent 比、フルスキャン、filesort 兆候を advisor に提示
  (閾値は provisional と明記し、private-isu 実測後に確定)
- capability probe により非対応環境では黙って skip
- **collector 自身のクエリを計測区間へ 1 件も混入させない**

## 非ゴール

- アプリ側正規化キーとの突合(誤対応リスク。将来課題)
- PostgreSQL(`pg_stat_statements` は別計画)
- EXPLAIN(→ 09)

## 01 / 02 から引用する契約(唯一の出所)

| 出所 | 名前 | 本計画での使い方 |
|---|---|---|
| 01 | `isutools.PurposeStats` | `Inspect` の purpose。未登録なら app credential へ fallback(接続衛生の正規化は必ず適用される) |
| 01 | `func Inspect(ctx context.Context, id string, purpose Purpose, fn func(context.Context, Querier) error) error` | target ごとの専用 `*sql.Conn` を取得する唯一の経路 |
| 01 | `TargetInfo.Schema` | `WHERE SCHEMA_NAME = ?` のバインド値。`""` なら当該 target を skip |
| 01 | `TargetInfo.ID` / `Targets()` / `Target(id)` | target の列挙と結合キー |
| 01 | §接続衛生(`DBName=""` / `InterpolateParams=false` / `ParseTime=true` / `Loc=UTC` / `MultiStatements=false` / Timeout 1s・R/W 2s) | 既定 DB 無し・`?` バインド・`UTC_TIMESTAMP(6)` の `time.Time` 受けの前提 |
| 01 | Inspect ごとの session 初期化 `SET time_zone = '+00:00'` | 04 の文数に「Inspect 1 回につき 1 文」として計上(01 が明記) |
| 01 | `ErrUnknownTarget` | 未登録 ID を受けたときのエラー |
| 02 | `runctl.BaselineCollector` | sqlrows はこのインタフェースで登録する |
| 02 | `runctl.SampleResult` / `BaselineHandle` / `Epoch` / `ErrStaleEpoch` | 境界戻り値と fencing |
| 02 | `func (h runctl.BaselineHandle) Sample() any` | **`Collect` の唯一の入力経路**。`base.Sample().(*Sample)` / `final.Sample().(*Sample)` で採取値を復元する(02 §`BaselineHandle.Sample()`)。unexported な `sample` フィールドには到達できない |
| 02 | `runctl.Registration{Name:"sqlrows", Required:false}` | optional collector(02 §登録の既定に従う) |
| 02 | `PerCollectorBaselineBudget` / `PerTargetBudget` / `BaselineConcurrency` | 予算(§予算と target ファンアウト) |
| 02 | `Validity`(`ValidityValid` / `ValidityPartial` / `ValidityInvalid`) | 区間妥当性の表現。04 は独自の validity 型を作らない |
| 02 | 結果表 6 / 12(optional baseline の失敗) | sqlrows の失敗は run を **partial** にする(invalid にはしない) |

## collector 契約への適合(02 §collector 契約)

```go
package sqlrows

type Collector struct {
    // (runID, epoch) → CaptureBaseline が採った Sample。
    // CaptureFinal が DIGEST_TEXT 取得対象を決めるためだけに読む。
    // **Collect は pending を読まない**(handle だけから区間値を作る)。
    pending map[runKey]*Sample
    // (runID, epoch) → 確定済み SampleResult(冪等再送用)
    results map[runKey]runctl.SampleResult
    probes  map[string]probeResult // key: TargetID
}

func (c *Collector) Name() string { return "sqlrows" }
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) // → *Section
func (c *Collector) Release(h runctl.BaselineHandle)                        // 冪等。pending / results から削除
```

**`Committed` の決め方**(02 §Committed セマンティクスへの適合):

| 状況 | err | Committed |
|---|---|---|
| 1 つ以上の target を採取できた(一部 drop あり) | nil | **true** |
| 全 target が probe skip(performance_schema=OFF 等) | nil | **true**(空 `Sample`。「対象なし」は失敗ではない) |
| 全 target が採取失敗(接続不能・権限エラー・budget 枯渇) | `ErrNoTargetCaptured` | **false** |

- `nil` + `Committed=false` は 02 の契約違反なので**発生させない**。
  上表を `TestCommittedMatrix` で固定する
- 同一 (runID, epoch) の再呼び出しは `results` から**同一値**(`At` も同一)を返す。
  異なる epoch は `runctl.ErrStaleEpoch` を返す
- ctx 期限超過時も `Committed` を正しく設定して返す(02 の要求)

### 不変 sample のデータモデル

```go
type Sample struct {                     // BaselineHandle.sample の実体(構築後は不変)
    Targets map[string]*TargetSample     // key: 01 の TargetID
}

type TargetSample struct {
    TargetID   string
    Schema     string               // 01 の TargetInfo.Schema(WHERE SCHEMA_NAME = ? のバインド値)
    ServerUUID string               // @@server_uuid
    UptimeSec  int64                // performance_schema.global_status / SHOW GLOBAL STATUS
    UTCBefore  time.Time            // 境界の digest 全件 SELECT の**直前**の UTC_TIMESTAMP(6)
    UTCAfter   time.Time            // 同 **直後** の UTC_TIMESTAMP(6)
    Digests    map[string]DigestRow // key: DIGEST(hex)。SCHEMA_NAME = Schema の行のみ
    Overflow   DigestRow            // SCHEMA_NAME IS NULL AND DIGEST IS NULL の 1 行
    Texts      map[string]string    // final のみ。DIGEST → DIGEST_TEXT(512B 切り詰め)
    Captured   bool
    Code       string               // "" | "probe-skip" | "no-schema" | "budget-exhausted" | "query-error"
    Err        string
}

type DigestRow struct {
    CountStar, TimerWait                        uint64
    RowsExamined, RowsSent, RowsAffected        uint64
    CreatedTmpDiskTables, SortMergePasses       uint64
    NoIndexUsed, NoGoodIndexUsed                uint64
}
```

`Collect(base, final)` は **`base.Sample().(*Sample)` / `final.Sample().(*Sample)` で
復元した 2 つの `Sample` だけ**(02 §`BaselineHandle.Sample()` が唯一の入口)から
`*Section` を作り、
DB・registry・`pending` に一切触れない
(02 の `TestBaselineCollect_UsesFrozenSamplesOnly` 相当を
`TestSqlrowsCollect_NoIO` として実装する)。

## SQL 全文(v6 で全書き換え — 既定 DB 無し + バインド schema)

**大前提**: 01 v6 §接続衛生により `PurposeStats` の接続は
`DBName=""` で開かれる。したがって

- `SELECT DATABASE()` は **NULL** を返す。`DATABASE()` は**一切使わない**
- collector が実行した文は digest table の
  **`SCHEMA_NAME IS NULL` かつ `DIGEST` 非 NULL** の行に集計される。
  これは `WHERE SCHEMA_NAME = ?` から**構造的に外れ**、
  overflow 判定行(`SCHEMA_NAME IS NULL AND DIGEST IS NULL`)とも別行である

### probe 文(target ごと・プロセス内 1 回。初回の `CaptureBaseline` と同一 `Inspect` 内で実行)

```sql
-- P1
SELECT @@performance_schema;                       -- 0 なら当該 target を skip
-- P2
SELECT ENABLED FROM performance_schema.setup_consumers
 WHERE NAME = 'statements_digest';                 -- 'NO' なら skip
-- P3(列差異 = MariaDB 検出、および 09 の QUERY_SAMPLE_TEXT 有無)
SELECT COLUMN_NAME FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = 'performance_schema'
   AND TABLE_NAME   = 'events_statements_summary_by_digest';
-- P4(Uptime 取得経路の決定。エラーなら SHOW 経路へフォールバック)
SELECT VARIABLE_VALUE FROM performance_schema.global_status
 WHERE VARIABLE_NAME = 'Uptime';
```

probe 結果は target ごとに `probes` へキャッシュし、health へ記録する
(`sqlrows[db1]: skip (performance_schema=OFF)`)。
`ServerUUID` が変化した target は probe を再実行する。

### 境界文(`CaptureBaseline` / `CaptureFinal` 共通の 3 文)

```sql
-- B1 / F1: メタ + 区間の「前」時刻。pfs 経路では 1 文に畳み込む。
SELECT @@server_uuid AS server_uuid,
       (SELECT VARIABLE_VALUE FROM performance_schema.global_status
         WHERE VARIABLE_NAME = 'Uptime') AS uptime_sec,
       UTC_TIMESTAMP(6) AS db_utc_before;

-- B2 / F2: digest 全件。schema 名は **バインド引数**(? = TargetInfo.Schema)。
SELECT DIGEST, COUNT_STAR, SUM_TIMER_WAIT,
       SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_ROWS_AFFECTED,
       SUM_CREATED_TMP_DISK_TABLES, SUM_SORT_MERGE_PASSES,
       SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED
  FROM performance_schema.events_statements_summary_by_digest
 WHERE SCHEMA_NAME = ? OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL);

-- B3 / F3: 区間の「後」時刻。
SELECT UTC_TIMESTAMP(6) AS db_utc_after;
```

**SHOW 経路(P4 が失敗した target のみ)**: B1 を次の 2 文に分割する。

```sql
SELECT @@server_uuid AS server_uuid, UTC_TIMESTAMP(6) AS db_utc_before;
SHOW GLOBAL STATUS LIKE 'Uptime';
```

### DIGEST_TEXT 取得(F4 — `CaptureFinal` の中だけで実行する)

```sql
SELECT DIGEST, LEFT(DIGEST_TEXT, 512) AS digest_text
  FROM performance_schema.events_statements_summary_by_digest
 WHERE SCHEMA_NAME = ? AND DIGEST IN (?, ?, /* … 最大 DigestTextFetchLimit=200 */);
```

- **なぜ `CaptureFinal` の中か**: 02 の契約により `Collect(base, final)` は
  I/O を行えない。一方 DIGEST_TEXT の対象は delta 上位で決まる。
  → `CaptureFinal` が F2 の結果と `pending[runKey]` の baseline から
  **暫定 delta** を計算し、`SUM_TIMER_WAIT` 降順で上位 200 の DIGEST を選び、
  F4 でテキストを取得して `final` の `Sample.Texts` に埋め込む。
  `Collect` は同じ 2 handle から**同じ delta を再計算**し、`Texts` を join するだけ
- 主キーは `(SCHEMA_NAME, DIGEST)` なので **`SCHEMA_NAME = ?` を必ず併記**する
  (v5 の修正を維持。複数 schema fixture をテストに残す)
- `pending[runKey]` が無い(baseline 失敗・preempt 後)場合は F4 を実行せず
  `Texts` を空にする。`Collect` は該当 digest の `Query` 列を
  `"(digest text unavailable)"` にする
- `IN` リスト長は上位件数によって変わる。MySQL 8.0.19+ は digest 上
  `IN (...)` に畳まれて 1 digest、それ未満では長さごとに別 digest になり得るが、
  **いずれも `SCHEMA_NAME IS NULL` 側**なので delta には影響しない

### collector 自身が作る digest 行

上記の全文(`SET time_zone` / P1〜P4 / B1〜B3 / F1〜F4)は
`SCHEMA_NAME IS NULL` 側に高々 **12 digest 行**を作る。
`performance_schema_digests_size`(既定 10000)に対して無視できる量であり、
overflow 判定行とは別行である。

## 自己汚染テスト(CRITICAL の受け入れ条件・必須)

`sqlrows/contamination_integration_test.go`(`//go:build integration`、
MySQL 8.4 fixture、CI の integration job で必ず実行する)。

`TestNoSelfContamination`:

1. `TRUNCATE performance_schema.events_statements_summary_by_digest`
2. アプリ schema `isuconp` へ既知のクエリを N 回流す(別セッション・既定 DB あり)
3. `CaptureBaseline` → 追加負荷 → `CaptureFinal` → `Collect` を実行
4. **collector 文の digest 集合 `C` を実測で作る**: 別の既定 DB 無しセッションで
   §SQL 全文の全文(`SET time_zone`・P1〜P4・B1〜B3・B1 の SHOW 経路 2 文・
   F1〜F4)を 1 回ずつ実行し、`SCHEMA_NAME IS NULL AND DIGEST IS NOT NULL` の
   DIGEST を集める(文字列一致ではなく **digest 一致**で判定する)
5. 検証(すべて必須):
   - `delta.Digests` のキー集合 ∩ `C` == **空**
   - `SCHEMA_NAME = 'isuconp'` 側の行数増分に collector 由来の行が **0 件**
   - `Inspect(..., PurposeStats, ...)` 内の `SELECT DATABASE()` が **NULL**
     (01 の接続衛生が効いていることの直接確認)
   - digest 全件 SELECT・Uptime 取得(pfs 経路と SHOW 経路の両方)・
     `UTC_TIMESTAMP(6)` の各 digest が **delta のどこにも現れない**
     (overflow 行・`Texts`・advisor 入力を含む全経路を走査する)
6. SHOW 経路も同一テストで検証する(P4 を強制失敗させる fake で分岐を通す)

`TestNoSelfContamination_MultiSchema`: 2 つの schema(`isuconp` / `other`)を
持つ fixture で、`SCHEMA_NAME = ?` のバインドが効き
**別 schema の同一 digest を拾わない**ことを検証する。

### 実測による裏付け(MySQL 9.7.2)

private-isu の実 MySQL **9.7.2**(`setup_consumers.statements_digest` 有効)に対して
実機で検証し、v6 の自己汚染修正が意図どおり効くことを確認した。

**バージョン整合(v6 監査反映)**: 検証環境は 9.7.2、CI の integration fixture は
**8.4** である。ここで根拠にしている 2 点 —(a)digest テーブルの主キーが
`(SCHEMA_NAME, DIGEST)` であること、(b)既定 DB を持たないセッションの文が
`SCHEMA_NAME IS NULL` に集計されること — は 8.0 以降の performance_schema 仕様で
共通であり、両バージョンで同一に成立する。09 の対応範囲(8.0.17+)とも矛盾しない。
CI では 8.4 fixture で同じ表明をテストし(`TestOverflowRequiresBothNull` 等)、
9.x は**手動検証環境**として記録する(サポート表に 9.x を追加するものではない)。

| # | 検証条件 | 実測結果 |
|---|---|---|
| 1 | 既定データベース = アプリ schema のまま collector の統計クエリを実行 | そのクエリ自身が `SCHEMA_NAME='isuconp'` / `COUNT_STAR=1` として計上された。すなわち `WHERE SCHEMA_NAME = ?` に**必ず一致し、常に delta へ入る**(v5 方式が壊れていたことの直接確認) |
| 2 | 既定データベース**無し**(01 §接続衛生の `DBName=""`)で同一クエリを実行 | 同じ文が `SCHEMA_NAME = NULL` に計上され、アプリ schema に対する `WHERE SCHEMA_NAME = ?` に**一度も一致しない**。→ **修正が実機で有効であることを検証済み** |
| 3 | 同一 DIGEST を異なる既定 DB から実行 | `SCHEMA_NAME` ごとに**別行**として現れた。F4(DIGEST_TEXT 取得)が `SCHEMA_NAME = ?` を必ず併記する根拠である主キー `(SCHEMA_NAME, DIGEST)` を実測で裏付けた |

**明示すべき含意 — overflow 判定は両方 NULL でなければならない**:
検証 2 が示すとおり、`SCHEMA_NAME IS NULL` の行は overflow 集約行**だけではない**。
既定データベースを持たずに実行された正規の文(まさに本 collector 自身の文)も
同じ `SCHEMA_NAME IS NULL` 側に載る。したがって

- overflow 行の同定条件は
  **`SCHEMA_NAME IS NULL AND DIGEST IS NULL`(2 列とも NULL)**に固定する。
  `SCHEMA_NAME IS NULL` のみで判定してはならない。
  §SQL 全文 B2 / F2 の `OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL)` と
  `TargetSample.Overflow` のコメントは既にこの条件であり、**変更は不要**
  (本監査で条件を再確認し、曖昧さが無いことを確定した)
- `Sample` 構築時のパースも同条件で行う。`DIGEST` が非 NULL の
  NULL-schema 行は `Digests`(schema 一致行のみ)にも `Overflow` にも入れず
  **破棄**する。V7(`sqlrows-overflow`)の判定は `Overflow` の delta のみを見る
- **追加テスト `TestOverflowRequiresBothNull`**: `SCHEMA_NAME IS NULL` かつ
  **`DIGEST` が非 NULL** の行(= 既定 DB 無しで実行された collector 自身の文に相当)
  を含む fixture を与え、その行が `Overflow` に**計上されない**こと・
  `sqlrows-overflow` health を**発火させない**こと・
  `SCHEMA_NAME IS NULL AND DIGEST IS NULL` の行だけが `Overflow` になることを固定する

## 予算と target ファンアウト(02 §予算モデルの引用)

04 は秒数を定義しない。使うのは 02 の定数だけである。

| 02 の定数 | 値 | 04 での適用 |
|---|---|---|
| `runctl.PhaseStartBaselineBudget` / `PhaseFinishFinalBudget` | 5s | 親(02 が全 baseline collector をまとめて切る)。04 は直接使わない |
| `runctl.PerCollectorBaselineBudget` | **3.5s** | 02 が sqlrows の 1 境界に渡す ctx の期限 |
| `runctl.PerTargetBudget` | **1s** | 1 target の 1 回の `Inspect`(表 B または表 C の全文)に張る子 ctx |
| `runctl.BaselineConcurrency` | **8** | target ファンアウトの並列度 |

**充足の根拠**(02 §予算モデル「下流計画への指示」と同一):
16 target ÷ `BaselineConcurrency`(8) = **2 波**、最悪
2 × `PerTargetBudget`(1s) = **2s ≤ `PerCollectorBaselineBudget`(3.5s)**。

### 収まらない場合に何を落とすか(黙って切り詰めない)

1. target は **`TargetID` の昇順**で波に割り当てる(処理順は決定的)
2. 波を開始する前に
   `deadline(collectorCtx) − now < runctl.PerTargetBudget` なら
   **その波を開始しない**
3. 未採取 target は `TargetSample{Captured:false, Code:"budget-exhausted"}` として
   `Sample` に**記録**する(map から消さない)
4. health `sqlrows-target-dropped: db3, db4 (budget-exhausted)` を出す
5. sqlrows は `Required:false` なので、02 の結果表 6 / 12 により
   run の `Validity` は **`ValidityPartial`**(`ValidityInvalid` にはしない)
6. `CollectorBoundary.Code` には 02 の定義済み値 `"not-captured"` を入れる
   (04 独自の値を wire に出さない。`"budget-exhausted"` は 04 内部の詳細理由)

### 不対境界の drop

`Collect(base, final)` は **両 handle に `Captured:true` で存在する target のみ**
セクション化する。片側にしか無い target は区間値を作れないため
`Code:"unpaired-boundary"` として drop し(数値を出さない)、
health `sqlrows-target-dropped` に理由付きで載せる。

### テスト

- `TestTargetDropIsDeterministic`: 20 target を登録し budget を人工的に絞る →
  drop されるのが常に `TargetID` 昇順の末尾側であること(`-count=50` で不変)
- `TestTargetDropIsRecorded`: drop された target が `Sample` と health の
  両方に現れ、`Validity == ValidityPartial` になること
- `TestUnpairedTargetDropped`: baseline のみ採取できた target が
  セクションに出ないこと

## 文数の正確な列挙(MEDIUM 対応)

`SET time_zone = '+00:00'` は 01 が `Inspect` ごとに実行する 1 文で、
01 §registry API の指示どおり **04 の文数に計上する**。

### 表 A: probe 文(target ごと・プロセス内 1 回)

| # | 文 | 数 |
|---|---|---|
| P1 | `SELECT @@performance_schema` | 1 |
| P2 | `setup_consumers` の `statements_digest` | 1 |
| P3 | `information_schema.COLUMNS` の列一覧 | 1 |
| P4 | `performance_schema.global_status` の Uptime(経路決定) | 1 |
| | **probe 計** | **4** |

### 表 B: 開始境界 `CaptureBaseline`(1 target)

| # | 文 | pfs 経路 | SHOW 経路 |
|---|---|---|---|
| S0 | `SET time_zone = '+00:00'`(01 の Inspect が実行) | 1 | 1 |
| B1 | server_uuid + Uptime + `UTC_TIMESTAMP(6)`(畳み込み 1 文) | 1 | — |
| B1a | server_uuid + `UTC_TIMESTAMP(6)` | — | 1 |
| B1b | `SHOW GLOBAL STATUS LIKE 'Uptime'` | — | 1 |
| B2 | digest 全件 SELECT | 1 | 1 |
| B3 | `SELECT UTC_TIMESTAMP(6)` | 1 | 1 |
| | **定常計** | **4** | **5** |
| | **初回 run のみ(+表 A)** | **8** | **9** |

### 表 C: 終了境界 `CaptureFinal`(1 target)

| # | 文 | pfs 経路 | SHOW 経路 |
|---|---|---|---|
| S0 | `SET time_zone = '+00:00'` | 1 | 1 |
| F1 / F1a+F1b | B1 / B1a+B1b と同形 | 1 | 2 |
| F2 | digest 全件 SELECT | 1 | 1 |
| F3 | `SELECT UTC_TIMESTAMP(6)` | 1 | 1 |
| F4 | 上位 200 の DIGEST_TEXT 取得(上位 0 件 または baseline 不対なら実行しない) | 0〜1 | 0〜1 |
| | **定常計** | **4〜5** | **5〜6** |

### 表 D: run 全体の合計

| 条件 | 1 target | 16 target |
|---|---|---|
| 初回 run・pfs 経路・F4 あり | 8 + 5 = **13** | **208** |
| 2 回目以降・pfs 経路・F4 あり | 4 + 5 = **9** | **144** |
| 2 回目以降・pfs 経路・F4 なし | 4 + 4 = **8** | 128 |
| 2 回目以降・SHOW 経路・F4 あり | 5 + 6 = **11** | 176 |

- **`UTC_TIMESTAMP(6)` の評価回数は常に 4 回 / target / run**(B1・B3・F1・F3)。
  うち 2 回(B1・F1)は独立文ではなくメタ取得文に畳み込まれている
- **ベンチ区間中(境界と境界の間)の追加文は 0**。04 は境界でしか DB を叩かない
- `TestStatementCount`: fake `Querier` に渡った文を数え、表 B / 表 C の
  数と**完全一致**することを検証する(pfs 経路 / SHOW 経路 / F4 有無 /
  初回・2 回目の 8 通りを table-driven)

### ABBA 測定条件(表 D を反映)

`ISUTOOLS_SQLROWS=off ↔ on` の単独 ABBA では、
**境界時にのみ**次の文が増えることを実測条件とする:

- 2 回目以降の run: **1 target あたり 9 文**(pfs 経路・F4 あり)。
  16 target 構成で **144 文 / run**
- プロセス内の初回 run のみ: 上記 + probe 4 文 / target(16 target で +64 文)
- ベンチ区間中の増分は **0 文 / 0 接続**
  (inspector 接続は 01 の idle timeout 30s で閉じる)

v5 の「境界時のこれらのクエリ差」という曖昧な記述は撤回し、
`examples/abba.sh` の判定に上の具体数を書き込む。

## 区間の妥当性判定(v6 で統合・整理)

判定は**すべて `Collect(base, final)` の中**で、2 つの `Sample` だけから行う
(I/O しない)。target ごとに独立に判定する。

| # | 検出条件 | delta の扱い | run validity | health | 09 への指示 |
|---|---|---|---|---|---|
| V1 | `base.ServerUUID != final.ServerUUID` | 数値を出さない(理由だけ表示) | partial | `sqlrows-db-restart` | 当該 target の EXPLAIN を skip |
| V2 | uuid 不変 かつ `final.UptimeSec < base.UptimeSec` かつ V6 なし | 数値を出さない | partial | `sqlrows-db-restart` | skip |
| V3 | uuid 不変 かつ `final.UptimeSec < base.UptimeSec` かつ V6 あり | **保持**(Uptime 減少は時計逆行の副作用) | partial | `sqlrows-clock-anomaly` | **鮮度判定を行わない** |
| V4 | baseline keyset の消失(`base.Digests` のいずれかの DIGEST が `final.Digests` に無い) | 数値を出さない | partial | `sqlrows-counter-reset` | skip |
| V5 | いずれかの digest で counter 後退(`final.CountStar < base.CountStar` 等、全 SUM 列を検査) | 数値を出さない | partial | `sqlrows-counter-reset` | skip |
| V6 | DB-UTC 4 点の順序違反(§DB 側時計の順序検証) | **保持**(counter delta は壁時計に依存しない) | partial | `sqlrows-clock-anomaly` | **鮮度判定を一切行わない** |
| V7 | overflow 行の delta > 0 | 保持 | 変化なし | `sqlrows-overflow` | 影響なし |

- V2 と V3 の分岐がある理由: `Uptime` は「サーバ開始時刻と現在時刻の差」なので、
  NTP による**時計の後退**でも減少し得る。V6 を伴わない Uptime 減少だけを
  DB 再起動と断定する(server_uuid は `auto.cnf` に永続するため再起動でも不変)
- **検出限界の契約明記**(v5 から維持): 「TRUNCATE 後に全 baseline digest が
  同数以上再実行された」ケースは原理的に検出できない。
  INTEGRATION.md に『run 中に performance_schema を外部から TRUNCATE しない』を
  運用前提として明記し、判定は best-effort であることを契約に含める
- **NULL digest overflow 行は instance-global** なので、同一 DB instance 上の
  複数 schema / 複数 target に重複表示しない(**`ServerUUID` 単位で 1 件に dedup**。
  v3 修正を維持)。表示は「digests_size 不足により一部の文が集約行へ落ちた
  (カバレッジ不完全)」
- 04 は独自の validity 型を持たない。上表の partial は
  02 の `runctl.ValidityPartial` を指す(02 結果表 6 / 12 の optional baseline 失敗)

### DB 側時計の順序検証(v6 追加 — MEDIUM 対応)

09 の鮮度判定は 04 が保存する DB-UTC 区間に依存する。
NTP・仮想化ホストの時刻同期・手動 `date` で DB 側の壁時計が**後退**すると、
区間が空または逆転し、09 は全サンプルを誤って `stale` と判定する。

```go
type DBClock struct {
    BaselineBefore time.Time `json:"baseline_before"` // base.UTCBefore
    BaselineAfter  time.Time `json:"baseline_after"`  // base.UTCAfter
    FinalBefore    time.Time `json:"final_before"`    // final.UTCBefore
    FinalAfter     time.Time `json:"final_after"`     // final.UTCAfter
    Monotonic      bool      `json:"monotonic"`       // 下の 4 条件をすべて満たす
    Anomaly        string    `json:"anomaly,omitempty"` // 安定コード(下表)
}
```

要求する順序(すべて DB 側 UTC。等号を許す = 単調非減少):

```
BaselineBefore <= BaselineAfter <= FinalBefore <= FinalAfter
```

| 違反 | `Anomaly` の値 |
|---|---|
| 4 点のいずれかが zero(未採取) | `"clock-missing"` |
| `BaselineAfter < BaselineBefore` | `"clock-backwards-baseline"` |
| `FinalAfter < FinalBefore` | `"clock-backwards-final"` |
| `FinalBefore < BaselineAfter` | `"clock-backwards-interval"` |

- 複数該当時は上表の**上から最初に一致したもの**を採用する(決定的)
- 異常時: `Monotonic = false`、`Anomaly` を設定、
  health `sqlrows-clock-anomaly: db1 (clock-backwards-interval, -1.8s)` を記録、
  run の validity を `ValidityPartial` にする。
  **counter delta は破棄しない**(壁時計に依存しないため)
- 正常時: `Monotonic = true`、`Anomaly = ""`
- テスト:
  - `TestDBClockOrdering`(上表 4 行 + 正常の 5 ケースを table-driven。
    fake `Querier` が返す `UTC_TIMESTAMP(6)` を注入)
  - `TestDBClockAnomaly_KeepsDeltaMarksPartial`(異常時に delta の数値が
    残り、`Validity == ValidityPartial` になること)

### 09 への契約(鮮度判定の入力)

04 は target ごとに `DBClock` を snapshot へ載せる。09 の判定規則:

| `Monotonic` | 09 の挙動 |
|---|---|
| `true` | `QUERY_SAMPLE_SEEN ∈ [BaselineAfter, FinalBefore]` のときだけ fresh。区間外は stale(advisor 対象外・グレー表示) |
| `false` | **鮮度判定を一切行わない**。fresh とも stale とも判定せず、当該 target の EXPLAIN を実行せず、`freshness = "unknown"` として advisor 対象外にする |

- **09 v6 は対応済み(v6 監査で確認)**: 「09 側の改訂が必要」「現行 09 の
  `Plan.Stale bool` は 2 値なので `unknown` を表現できない」という
  本書 v6 初版の記述は**撤回する**。09 v6 §データモデルは `Plan.Stale bool` を
  既に撤回し、`Plan.Freshness FreshnessState`(`FreshnessFresh` = `"fresh"` /
  `FreshnessStale` = `"stale"` / `FreshnessUnknown` = `"unknown"`)と、
  閉じた理由 enum `Plan.FreshReason FreshReason`
  (`FreshClockAnomaly` = `"db_clock_anomaly"` /
  `FreshClockMissing` = `"db_clock_missing"` /
  `FreshRunPartial` = `"run_partial"` ほか)を**採用済み**である。
  04 は `bool` を前提にした出力を持たない(04 が出すのは
  `DBClock.Monotonic` / `DBClock.Anomaly` だけで、3 値化は 09 側の責務)
- V1 / V2 / V4 / V5(区間そのものが無効)の target については、
  `Monotonic` の値によらず 09 は当該 target の EXPLAIN を **skip** する

## 文種分離と比率

- DIGEST_TEXT の先頭トークンで SELECT / DML(INSERT・UPDATE・DELETE・
  REPLACE)/ その他に分類する。ただし **`WITH ... SELECT`(CTE)は
  SELECT として扱う**(v4 修正: 先頭トークンのみでは Other に落ちる。
  WITH 開始の場合は本体の文種まで読み進める。CTE fixture をテストに含める)
- DIGEST_TEXT が無い digest(F4 未実行・上位 200 圏外)は
  `"(digest text unavailable)"` として **Other** に分類し、比率は N/A にする
- 比率列(`examined_per_sent`)は **SELECT かつ RowsSent > 0 のみ**算出。
  それ以外は N/A 表示。DML は RowsAffected 列を主表示にする

## 表示

- delta の `SUM_TIMER_WAIT` 降順で上位 200(`DigestTextFetchLimit` と同値)。
  **切り捨ては全件数と表示件数の両方を明示**(`shown=200 / total=1234`)
- target ごとの表。drop / 無効区間の target は数値の代わりに
  理由コード(`budget-exhausted` / `unpaired-boundary` / `db-restart` /
  `counter-reset`)を表示する
- `db_clock` の `Anomaly` が非空の target はヘッダに注記を出す

### メモリ

- `LIMIT なし`(全 digest)。行数上限は
  `performance_schema_digests_size`(既定 ~1 万)。
  メモリ見積もりは「1 行 100B」のような楽観値を仮定しない(v4 修正:
  Go の map bucket・string・object overhead を含むと数倍になり、
  16 target では GC 影響も無視できない)
- baseline には DIGEST_TEXT を**含めない**。final も上位 200 のみ
  (`Texts` は最大 200 × 512B / target)
- **allocation benchmark(`-benchmem`)で 1 万 digest × 16 target の
  実測上限を受け入れ条件にし、実測値を docs に記録する**
  (`BenchmarkSampleAlloc` / `BenchmarkCollectAlloc`。
  `Texts` を含む final 側も別途測る)

## advisor(WithSQLRows、閾値は provisional)

- `sql-rows-ratio`: SELECT・sent>0・比 > 5・総時間上位 → warn
  (ISUCON13 の「5 倍以下」基準を出典として明記)
- `sql-rows-no-index`: SUM_NO_INDEX_USED デルタ > 0 → warn
- `sql-rows-filesort`: SUM_SORT_MERGE_PASSES または
  SUM_CREATED_TMP_DISK_TABLES のデルタ > 0 → warn
- `sql-rows-overflow`: overflow 検出 → info(digests_size 増加を提案)
- **区間が無効な target(V1 / V2 / V4 / V5)は advisor の入力に入れない**。
  V3 / V6(時計異常のみ)の target は counter delta が有効なので入力に入れる
- **閾値確定の手順**: private-isu で意図的に index を外し、
  誤警報ゼロ・検出漏れゼロを確認してから既定有効。それまで
  Recommendation に (provisional) を付す

## feature flag

`ISUTOOLS_SQLROWS=off` で無効(既定 on)。実測条件は §ABBA 測定条件。

## health キー(本計画が追加するもの・7 つに固定)

`sqlrows-skip`(probe による skip)/ `sqlrows-no-schema`
(`TargetInfo.Schema == ""`。01 の指示どおり target を skip)/
`sqlrows-overflow` / `sqlrows-db-restart` / `sqlrows-counter-reset` /
`sqlrows-clock-anomaly` / `sqlrows-target-dropped`。

## 実装ステップ(TDD)

1. `Sample` / `DigestRow` / `DBClock` の型と、`Collect(base, final)` の
   delta 計算テスト先行(初登場 digest / counter 後退 / keyset 消失 /
   NULL overflow 行の分離(`TestOverflowRequiresBothNull`)/ 切り捨て表示 /
   `TestSqlrowsCollect_NoIO`)
2. `TestDBClockOrdering` + `TestDBClockAnomaly_KeepsDeltaMarksPartial`
3. probe(performance_schema OFF / consumers NO / 権限エラー /
   P4 失敗 → SHOW 経路)のテスト
4. 01 の `Inspect(ctx, id, PurposeStats, fn)` 経由の複数 target 取得。
   `TestStatementCount`(表 B / 表 C の 8 通り)、
   `TestTargetDropIsDeterministic` / `TestTargetDropIsRecorded` /
   `TestUnpairedTargetDropped`
5. 02 の `BaselineCollector` 実装(`TestCommittedMatrix`・冪等再送・
   `ErrStaleEpoch`)と `Registration{Required:false}` での登録
6. 文種分類・比率 N/A のテスト
7. **integration: `TestNoSelfContamination` / `TestNoSelfContamination_MultiSchema`**
   (CRITICAL の受け入れ条件。CI の integration job で必須)
8. advisor WithSQLRows + web 配線・template
9. `BenchmarkSampleAlloc` / `BenchmarkCollectAlloc` の実測値を docs に記録
10. docs + private-isu 検証(閾値確定)、`examples/abba.sh` に表 D の数を反映

## テスト計画(受け入れ条件)

- unit カバレッジ 80%(共通契約 5)
- **必須 integration**: `TestNoSelfContamination`(§自己汚染テスト)。
  これが落ちたら CRITICAL の未解決としてリリースを止める
- `TestStatementCount` が表 B / 表 C と**完全一致**すること
- 02 の conformance: `Collect` 中に I/O が起きないこと・
  `Committed` が上表どおりであること・同一 (runID, epoch) の再送が同値であること
- 01 の contract: `Inspect` に渡る DSN が `DBName=""` /
  `interpolateParams=false` / `parseTime=true` / `loc=UTC` であること
  (01 側の unit と重複するが、04 でも回帰として固定する)

## リスク

| リスク | 対策 |
|---|---|
| 全 digest 取得のコスト | digest table 上限 ~1 万行・境界 2 回のみ。probe 失敗で以後停止 |
| 複数 schema を跨ぐアプリ | v1 は `TargetInfo.Schema` 1 つのみ(`WHERE SCHEMA_NAME = ?`)。表示にその旨明記 |
| MariaDB の列差異 | probe P3 で列存在を確認し、欠落時 skip。Uptime は SHOW 経路へ |
| baseline と final の間の TRUNCATE | server_uuid + Uptime + baseline keyset 消失 + counter 後退の**複合判定** → 当該 target を無効化(§区間の妥当性判定) |
| collector 自身のクエリの混入 | 既定 DB 無し接続(01 §接続衛生)+ `SCHEMA_NAME = ?` バインド + 必須 integration テスト(§自己汚染テスト) |
| DB 側時計の逆行 | 4 点の順序検証。異常時は delta を残しつつ 09 の鮮度判定を停止(§DB 側時計の順序検証) |
| 16 target が予算に収まらない | 決定的な drop + 記録 + partial(§予算と target ファンアウト)。黙って切り詰めない |

## 見積もり

**3.5 日**(v5 の 2.5 日 + 1.0 日):

| 追加項目 | 増分 |
|---|---|
| 既定 DB 無し接続への SQL 全書き換え + 自己汚染 integration テスト(fixture 込み) | +0.5 日 |
| `BaselineCollector` 適合(handle / `Committed` / 冪等 / `Collect` 純粋化・F4 の CaptureFinal 移動) | +0.25 日 |
| 予算引用・決定的 drop・不対境界 drop とそのテスト | +0.125 日 |
| `DBClock` 順序検証 + 09 契約 + テスト | +0.125 日 |

**README v6 との一致を確認済み(v6 監査)**: 「README 側の再算定が必要」という
本書 v6 初版の注記は**撤回する**。plans/README.md §リリース対応(見積もり v6 再算定)
は既に全数値を再導出しており、
`v1.2.0 = 01(2.5)+ 02(9.5)+ 04(3.5) = 実装 raw 15.5 日 → +30% で 20 日`、
v5 からの増分表も `04: 2.5 → 3.5(+1.0)` となっている。
本節の **3.5 日**はこの表と同値であり、再算定の残作業は無い。
