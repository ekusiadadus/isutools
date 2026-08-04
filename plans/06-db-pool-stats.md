# 06: DB プール統計(表示のみ)— 旧02 から分離 — v6

種別: 機能 / 対象リリース: v1.2.x /
依存: 01(TargetID・ID の受理規則・`Target` / `TargetIDForDSN`)、
02(`BaselineCollector` 契約・予算モデル・`Validity`)/
新規パッケージ: `dbpool`

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[HIGH] 使用例が手書きの TargetID を渡していた問題**。v5 の例
   `WatchDBPool("mysql-db1_3306-isuconp", db.DB)` は**撤回する**。
   `"mysql-db1_3306-isuconp"` は 01 の自動 ID の **alias 部分でしかなく**、
   実際の自動 ID は `alias + "-" + hash26`(base32 26 文字)である
   (01 §TargetID)。したがってこの文字列は registry に存在せず、
   `WatchDBPool` は `ErrUnknownTarget` を返して pool が 1 つも計測されない。
   → 例を **`RegisterDBTarget(id, driverName, dsn)` で明示 ID を先に作り、
   同じ ID を `WatchDBPool` に渡す**形へ書き換えた。明示登録を使わない
   場合の代替として 01 の lookup API `TargetIDForDSN` を使う形も併記した。
   受理規則(**registry に存在する ID のみ受理**)と不一致ケースの
   テストを §API に明記(§API / §テスト計画)
2. **[HIGH] baseline / final 採取の契約が 02 v6 とずれていた**。v3 の
   「baseline は 02 coordinator の **reset hook** で取得」「区間デルタは
   **02 の世代境界**に従う」は**撤回する**。dbpool は 02 §登録の
   **baseline 型 collector** であり、世代(generation)型ではない。
   → `runctl.BaselineCollector`(`CaptureBaseline` / `CaptureFinal` /
   `Collect(base, final)` / `Release`)を実装する形に書き直し、
   `SampleResult.Committed`・(runID, epoch) 冪等・`ErrStaleEpoch`・
   「`Collect` は I/O を行わない」までを具体化した(§02 collector 契約の実装)
3. **[HIGH] 「run 途中の Watch/Unwatch は run 状態自体を partial にする」を
   撤回**。(a) 02 v6 は `RunState`(lifecycle)と `Validity`(妥当性)を
   分離し、v5 の `StatePartial` を撤回済みなので「run 状態を partial」と
   いう表現自体が 02 v6 に存在しない。(b) この規則は「run 開始より遅い
   baseline を作る」ことを前提にしていたが、v6 は **deferred activation**
   (run の watch set は `CaptureBaseline` 時点で確定)により遅れた baseline を
   作らない。(c) 仮に降格させるなら baseline collector が使える経路は
   `CaptureFinal` / `Collect` から err を返すことだけで、02 §結果表 11/12/15/16 行に
   より **DB Pool セクション全体が snapshot から除外**される。
   → run の `Validity` は降格させず、**entry 単位の `Partial` + `Code` +
   `BaselineAt` / `FinalAt` + 06 固有 health キー**で表現する
   (§ライフサイクル)
4. **[MEDIUM] web の live provider を撤回**。v3 の
   `Provider DBPools func() []dbpool.Entry` は、02 §FinishRun 手順 7 の
   「**固定値だけから** immutable snapshot を構築」に反する
   (snapshot 構築時に `db.Stats()` を読み直すことになる)。
   → `Collect(base, final)` の戻り値 `[]dbpool.Entry` を snapshot の
   `DBPools` フィールドへ格納し、web は immutable snapshot を描画する
   (§実装ステップ 4)
5. **[MEDIUM] v3 のテスト「baseline 未取得(登録が reset 後)→ 初回 snapshot は
   登録時点比」を撤回**。deferred activation により run 途中で登録された
   pool は**その run に一切現れない**ため、「登録時点比」という区間値は
   作らない。代替テストを 2 本用意した(§テスト計画)
6. **[MINOR] 「`Stats()` は atomic 読みのみ」という記述を撤回**。
   `(*sql.DB).Stats()` は `*sql.DB` の内部 mutex を 1 回取る実装であり
   atomic ではない。I/O を行わない点は変わらないため結論(オーバーヘッド
   ほぼゼロ)は維持し、根拠だけ正した(§実装ステップ 6)
7. **[MINOR] 予算・並列度・上限を 02 / 01 から引用する形に統一**。
   06 は独自の秒数・独自の上限値を定義しない(§02 collector 契約の実装)
8. **[MINOR] `Entry.Name` を `Entry.TargetID` へ改名**(JSON key も
   `name` → `target_id`)。「01 の TargetID と byte 完全一致で結合する値」で
   あることを型名で自明にするため。あわせて `Display`(01 の
   `TargetInfo.Display`)と区間端点 `BaselineAt` / `FinalAt`、
   安定コード `Code` を追加した(§データモデル)
9. **[MINOR] ヘッダ版数の陳腐化**。v3 のままだったので v6 に更新

## 旧計画(旧02)からの変更点

レビュー指摘を反映(v3 で確定・v6 でも維持):

1. **advisor 閾値を v1 から全廃**。旧計画の判定式は成立していなかった:
   - `WaitDuration` は**並列 wait の合計**であり、wall interval の 1% との
     単純比較は意味を持たない(64 並列なら interval の 64 倍まで積み上がる)
   - 「過去に wait した」ことと snapshot 時点の `Open == MaxOpen` は
     因果を結び付けられない
   - プール増加が常に正しいとは限らない(DB 側過負荷を悪化させ得る)
   - まず統計を表示し、advisor 閾値は private-isu 実測後に別 PR で導入
2. **API が error を返す**(旧計画は「登録エラーにする」と書きながら
   例に戻り値が無かった)
3. **`MaxIdleTimeClosed` を追加**(欠落していた。database/sql の
   DBStats 全フィールドを網羅)
4. **lifetime churn の「Count 比」廃止**(未定義だった)

## ゴール

アプリが 1 行で `*sql.DB` を登録すると、ベンチ区間のプール統計が
snapshot に表示される。判断は読者(+将来の advisor)に委ねる。

## API(v6 修正)

```go
// package isutools。第一引数は 01 の TargetID(同じ名前空間で
// sqlrows(04)/ dbinspect / queryplan(09)/ agent(10)と結合する)。
// 任意 interface は受けない(typed-nil / panic / blocking 実装の混入を
// 防ぐため *sql.DB に限定。sqlx 等は .DB を渡す)。
func WatchDBPool(targetID string, db *sql.DB) error
func UnwatchDBPool(targetID string) error

// sentinel error(errors.Is で判定可能)
var (
    ErrNilDB         = errors.New("isutools: WatchDBPool: db is nil")
    ErrDuplicatePool = errors.New("isutools: WatchDBPool: target already watched")
    ErrTooManyPools  = errors.New("isutools: WatchDBPool: too many pools (max 16)")
)
// 未登録 targetID は 01 の ErrUnknownTarget をそのまま返す(06 で別名を作らない)。
```

### targetID の受理規則(v6 で明文化)

| 規則 | 内容 | 出所 |
|---|---|---|
| 受理対象 | **01 の registry に既に存在する ID のみ**。判定は `isutools.Target(targetID)` が `ok == true` を返すこと | 01 §ID の受理規則(「既存 ID のみを受理する消費側 API: `WatchDBPool`(06)/ `RegisterDBInspector` / `Inspect`」) |
| 未登録時 | **`ErrUnknownTarget` を返し、watch は行わない**(`WatchDBPool` は target を新規作成しない) | 01 §ID の受理規則(target を新規作成するのは `RegisterDBTarget` / proxy 観測 / 10 の targets.json の 3 経路だけ) |
| 比較方法 | **byte 単位の完全一致**(`==`)。大小同一視・Unicode 正規化・前後空白 trim を一切行わない | 01 §明示 ID の制約 |
| 手書き禁止 | **自動導出 ID を手書きしてはならない**(末尾に base32 26 文字の hash が付くため人間が書けない)。取得は `TargetIDForDSN` を使う | 01 §TargetID / §ID の受理規則 |
| 重複 | 同一 targetID の二重 watch は `ErrDuplicatePool`。プール再作成時は `UnwatchDBPool` → `WatchDBPool` | 本書 §ライフサイクル |
| 上限 | 16(01 の「登録上限 16 target」と同一値。各 target につき pool は 1 つなので通常は先に 01 側で頭打ちになる) | 01 §registry API |
| nil | `db == nil` は `ErrNilDB` | 本書 |

`UnwatchDBPool` は**冪等**:未登録 targetID は `ErrUnknownTarget`、
登録済みだが watch していない targetID は no-op で `nil` を返す。

### 使用例(推奨形 — README の Minimal integration に追記)

```go
const dbTarget = "db1" // 人間が選ぶ安定 ID(01 §明示 ID の制約: [A-Za-z0-9._-] / 1〜64 バイト)

func main() {
    dsn := "isucon:isucon@tcp(127.0.0.1:3306)/isuconp?parseTime=true"

    // 1) 先に論理 target を明示登録する(01 §ID の受理規則)。
    //    SQLDriverName より前に呼ぶこと。以降この canonical tuple の
    //    自動 ID 導出は行われず、"db1" が正となる。
    if err := isutools.RegisterDBTarget(dbTarget, "mysql", dsn); err != nil {
        log.Fatal(err)
    }

    // 2) アプリの接続を開く(proxy driver 経由)
    db, err := sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
    if err != nil {
        log.Fatal(err)
    }

    // 3) 同じ ID で pool を登録する(sqlx は .DB を渡す)
    if err := isutools.WatchDBPool(dbTarget, db.DB); err != nil {
        log.Print(err) // 計測は継続する(fail-open)
    }
}
```

### 使用例(明示登録を使わない場合 — lookup 形)

```go
db, err := sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
if err != nil {
    log.Fatal(err)
}
// 自動導出 ID は alias + "-" + hash26 なので手書きできない。
// 必ず 01 の lookup API で取得する(副作用なし・登録は行わない)。
id, ok := isutools.TargetIDForDSN("mysql", dsn)
if !ok {
    // proxy driver がまだ DSN を観測していない、または DSN がパース不能。
    // → RegisterDBTarget による明示登録が必要(推奨形へ)
    log.Print("isutools: db target not registered; call RegisterDBTarget")
} else if err := isutools.WatchDBPool(id, db.DB); err != nil {
    log.Print(err)
}
```

- `driver.DriverContext` を実装する driver では `sql.Open` が即座に
  `OpenConnector(dsn)` を呼ぶため、**接続確立前でも** `TargetIDForDSN` が
  解決できる(01 §driver wrapper の維持要件)。実装していない driver では
  初回接続まで `ok == false` になり得るため、**推奨形(明示登録)を第一とする**
- **v5 の例の撤回(再掲)**: `WatchDBPool("mysql-db1_3306-isuconp", db.DB)` は
  誤りである。この文字列は alias 部分のみで hash26 を欠くため registry に
  存在せず、`ErrUnknownTarget` になる(01 §ID の受理規則の末尾で 01 側も
  同じ指摘を明記している)

## ライフサイクル(v6 再定義)

### run の watch set

- **run の watch set は `CaptureBaseline`(02 §StartRun 手順 4 の
  start-baseline phase)の時点で watch されていた targetID の集合で確定する**。
  `Collect(base, final)` はこの集合についてのみ entry を作る
- `dbpool.Collector` は watch map と run ごとのサンプルを 1 本の mutex で
  保護する。`WatchDBPool` / `UnwatchDBPool` と `CaptureBaseline` /
  `CaptureFinal` が並行しても watch set は原子的に決まる

### run 途中の Watch(deferred activation)

- `WatchDBPool` は常に即座に watch map へ追加し `nil` を返す。ただし
  **その時点で進行中の run の watch set には入らない**。当該 pool は
  **次の `CaptureBaseline` から**計測対象になる
- health `dbpool-registered-mid-run`(info)に targetID と
  「次の run(次の /reset)から計測されます」を記録する
- **run の `Validity` は変更しない**(v3 の「run 状態自体を partial にする」は
  §v6 での変更点 3 のとおり撤回)。遅れた baseline を作らないため、
  この run が報告する entry はいずれも run 区間の全長をカバーしている

### run 途中の Unwatch

- `UnwatchDBPool` は呼ばれた瞬間に `db.Stats()` を **farewell サンプル**として
  `unwatchedAt` と共に記録し、watch map から外して `*sql.DB` の参照を捨てる
  (アプリが `Close` した pool を掴み続けない)
- 当該 targetID が進行中の run の watch set に含まれる場合、`CaptureFinal` は
  farewell サンプルを final として採用し、`Collect` は
  `Partial = true` / `Code = "unwatched-mid-run"` / `FinalAt = unwatchedAt` の
  entry を出す(**区間が run より短いことを entry 自身が申告する**)
- health `dbpool-unwatched-mid-run`(info)に targetID と unwatchedAt を記録
- プール再作成は `UnwatchDBPool` → `WatchDBPool` の順。再 Watch した pool は
  deferred activation により次の run から新しい baseline で計測される

### その他

- `POST /collect` は非終端で、accesslog の flush だけを行う
  (02 §HTTP の互換性保証 2/3)。**dbpool は `/collect` で何もしない**
  — 世代を進めず、サンプルも採らない
- run が `aborted` になった場合、Controller が `Release` を呼び dbpool の
  handle 内サンプルは破棄される。次の `StartRun` の watch set は
  その時点の watch map から作り直される
- feature flag: `ISUTOOLS_DBPOOL=off` のとき `RegisterBaseline` を**行わず**、
  `WatchDBPool` は §受理規則の検証だけ行って(引数バグを隠さないため)
  no-op で `nil` を返す。health `dbpool-disabled`(info)。機能単位 ABBA 用の
  kill-switch。既定 on。ただし watch が 0 件なら DB Pool セクション自体を出さない

## 02 collector 契約の実装(v6 追加)

dbpool は 02 §登録の **baseline 型 collector**(`procstats / sqlrows(04) /
dbpool(06) / network(05) / hoststats(03)`)である。

```go
package dbpool

// Collector は runctl.BaselineCollector を実装する。
// package dbpool は package isutools を import しない(循環回避)。
// TargetID の受理検証と Display の取得は isutools.WatchDBPool が行い、
// 検証済みの値だけを Watch へ渡す。
type Collector struct { /* mu, watch map, run ごとのサンプル cache */ }

var Default = &Collector{} // isutools が所有する単一実体

// isutools.WatchDBPool / UnwatchDBPool から呼ばれる内部 API。
// display は 01 の TargetInfo.Display(redaction 済み)。
func (c *Collector) Watch(targetID, display string, db *sql.DB) error
func (c *Collector) Unwatch(targetID string) error // farewell サンプルを取ってから外す

// テスト用の内部フック(unexported)。Watch は内部で db.Stats を渡すだけなので、
// 公開 API を *sql.DB に限定したまま fake Statser でデルタ計算を検証できる。
func (c *Collector) watchStats(targetID, display string, stats func() sql.DBStats) error

func (c *Collector) Name() string { return "dbpool" }

// watch set 全件の db.Stats() を読み、deep copy した
// map[string]sql.DBStats(キー = TargetID)を BaselineHandle に内包させて返す。
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)

// 固定済み 2 サンプルだけから []Entry を作る。I/O も db.Stats() も呼ばない。
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) // → []Entry

// handle が内包するサンプル参照を落とす。冪等(二重 Release は no-op)。
func (c *Collector) Release(h runctl.BaselineHandle)
```

登録(isutools.go):

```go
ctrl.RegisterBaseline(runctl.Registration{
    Name:       "dbpool",
    Required:   false, // 02 §登録の既定(required は sqlstats / httpstats のみ)
    SerialOnly: false, // db.Stats() は I/O を伴わないため並列採取で安全
}, dbpool.Default)
```

| 契約項目 | dbpool の実装 | 出所 |
|---|---|---|
| `Committed` | 成功時は必ず `Committed = true` / `err = nil`。呼び出し冒頭で `ctx.Err() != nil` の場合のみ `Committed = false` + その err(zero value は返さない) | 02 §Committed セマンティクス |
| 冪等性 | (runID, epoch, phase) をキーにサンプルを cache し、再呼び出しは **`At` も含めて同一の `SampleResult`** を返す | 02 §Committed セマンティクス |
| 古い epoch | 保持中より古い epoch での呼び出しは `runctl.ErrStaleEpoch` | 02 §Committed セマンティクス |
| `Collect` の入力経路 | **`base.Sample().(map[string]sql.DBStats)` / `final.Sample().(map[string]sql.DBStats)` が唯一の入口**(02 §`BaselineHandle.Sample()`)。unexported な `sample` フィールドや collector 内の map には到達しない | 02 `func (h runctl.BaselineHandle) Sample() any` |
| `Collect` の純粋性 | 上記で復元した 2 つの map のみを読む。`db.Stats()`・DB・`/proc` へ一切アクセスしない | 02 `BaselineCollector.Collect` / `TestBaselineCollect_UsesFrozenSamplesOnly` |
| per-collector 予算 | `PerCollectorBaselineBudget`(**3.5s**)の内側。06 は独自の秒数を定義しない。実測は 16 pool で数十 µs(I/O なし) | 02 §予算モデル |
| per-target 予算 | **適用対象外**。dbpool は 01 の `Inspect` を呼ばないため `PerTargetBudget`(1s)を消費しない | 02 §予算モデル |
| 並列採取 | `BaselineConcurrency = 8` の errgroup で他の baseline collector と並列に呼ばれる。`SerialOnly = false` | 02 §並列採取 |
| 境界ウィンドウ | `SampleResult.At` が `StartResult.BoundaryWindow` / `FinishAccepted.BoundaryWindow` に入る。dbpool の寄与は 1ms 未満で `SpreadLimitBoundary`(1500ms)を押し上げない | 02 §境界ウィンドウ |
| 失敗時の run 評価 | optional なので、万一 capture が失敗しても run は `ValidityPartial` に留まり `ValidityInvalid` にはならない(02 §結果表 6 行 / 12 行) | 02 §結果表 |

## データモデル

```go
type Entry struct {
    TargetID string `json:"target_id"` // 01 の TargetID(byte 完全一致で結合)
    Display  string `json:"display"`   // 01 TargetInfo.Display(redaction 済み)。Watch 時に取得
    // 点観測(final サンプル時点 = 02 の finish-final phase)
    MaxOpen int `json:"max_open"`
    Open    int `json:"open"`
    InUse   int `json:"in_use"`
    Idle    int `json:"idle"`
    // 区間デルタ(final − baseline)
    WaitCount         int64         `json:"wait_count"`
    WaitDuration      time.Duration `json:"wait_duration_ns"` // 並列 wait の合計(表示に注記)
    MaxIdleClosed     int64         `json:"max_idle_closed"`
    MaxIdleTimeClosed int64         `json:"max_idle_time_closed"`
    MaxLifetimeClosed int64         `json:"max_lifetime_closed"`
    // 区間の実測端点(02 の BoundaryWindow と同一時刻軸)
    BaselineAt time.Time `json:"baseline_at"` // = base.SampledAt
    FinalAt    time.Time `json:"final_at"`    // = final.SampledAt(mid-run Unwatch 時は unwatchedAt)
    Partial bool `json:"partial,omitempty"`
    // Code は空 = 正常。06 が定義する安定コードのみ(runctl.CollectorBoundary.Code とは別物):
    //   "counter-rewind"    カウンタ後退(プール再作成等)を検出
    //   "unwatched-mid-run" run 中に UnwatchDBPool された(区間が run より短い)
    Code string `json:"code,omitempty"`
}
```

- サンプルは `Collector` が保持する `map[string]sql.DBStats`(キー = TargetID)。
  `Collect(base, final)` が両サンプルの差を取る
- **カウンタ後退の検出**: `WaitCount` / `WaitDuration` / `MaxIdleClosed` /
  `MaxIdleTimeClosed` / `MaxLifetimeClosed` のいずれかで `final < base` なら
  `Partial = true` / `Code = "counter-rewind"` とし、**デルタではなく final の
  current 値をそのまま表示**する(値の意味を表に注記)
- `WaitDuration` の表示ラベルに「並列 wait の合計(wall 時間ではない)」を
  固定注記。`WaitCount > 0` なら `平均 wait = WaitDuration / WaitCount` を
  併記(これは分布に依存しない安全な導出)
- entry の並び順は `TargetID` の昇順(snapshot の決定性のため)

## advisor(v1 では実装しない)

将来 PR の候補シグナルとして記録のみ(private-isu 実測で評価してから):

- 平均 wait(WaitDuration/WaitCount)が SQL p95 と同オーダー
- WaitCount / (WaitCount + 総クエリ数) の比
- MaxLifetimeClosed が大きい(SetConnMaxLifetime 過小)

いずれも「プールを増やせ」と短絡しない文面にする(DB 側飽和の可能性を
併記する)ことを要件にする。

## 実装ステップ(TDD)

1. dbpool: `watchStats` フックの fake Statser でデルタ・後退 Partial・
   登録上限・重複を、`isutools.WatchDBPool` 側で nil / 未登録 ID を
   テスト先行
2. dbpool: `runctl.BaselineCollector` の実装
   (`CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release`)。
   (runID, epoch) 冪等・`ErrStaleEpoch`・`Committed`・`Collect` の
   非 I/O を conformance test で固定(02 §collector 契約)
3. isutools.go: `WatchDBPool` / `UnwatchDBPool` 公開(01 の `Target` による
   受理検証を含む)+ `ctrl.RegisterBaseline` への登録
4. web: snapshot の `DBPools []dbpool.Entry` を Connections セクションの
   「DB Pool」表として描画(注記文言含む)。
   **live provider `func() []dbpool.Entry` は使わない**
   (02 §FinishRun 手順 7「固定値だけから snapshot を構築」)
5. docs: README(推奨形の 1 行統合例)+ INTEGRATION.md
   (「RegisterDBTarget → WatchDBPool」の順と、自動 ID を手書きしない理由)
6. 単独 ABBA。`(*sql.DB).Stats()` は `*sql.DB` の内部 mutex を 1 回取るだけで
   **I/O を伴わない**(v3 の「atomic 読み」は撤回。実装は mutex)。
   計測区間中は一切呼ばず、境界の 2 回(baseline / final)だけ呼ぶため
   理論影響はほぼゼロ。これを実測で確認する

## テスト計画

### targetID の受理(v6・第5回レビュー要求)

- unit `TestWatchDBPool_RejectsUnregisteredID`: `RegisterDBTarget("db1", "mysql", dsn)`
  のみ済ませた状態で `WatchDBPool("mysql-db1_3306-isuconp", db)` を呼ぶと
  `errors.Is(err, isutools.ErrUnknownTarget)` が真で、**watch 件数が 0 のまま**
  (= v5 の誤った使用例そのものを回帰テストにする)
- unit `TestWatchDBPool_RejectsAliasWithoutHash`: proxy 観測で自動導出された
  ID から末尾の `-hash26` を落とした文字列は `ErrUnknownTarget`
- unit `TestWatchDBPool_ByteExactID`: `"db1"` 登録済みのとき `"DB1"` /
  `" db1"` は `ErrUnknownTarget`(01 §明示 ID の制約: byte 完全一致)
- unit `TestWatchDBPool_AcceptsRegisteredID`: `RegisterDBTarget` で登録した
  `"db1"` をそのまま渡すと成功し、entry の `TargetID == "db1"`
- unit `TestWatchDBPool_AcceptsTargetIDForDSN`: `TargetIDForDSN("mysql", dsn)`
  が返した ID をそのまま渡すと成功する(lookup 形の例の回帰)
- unit `TestWatchDBPool_NilDB` / `_Duplicate` / `_Max16`:
  `ErrNilDB` / `ErrDuplicatePool` / `ErrTooManyPools`
- unit `TestUnwatchDBPool_Idempotent`: 未 watch の登録済み ID は `nil`、
  未登録 ID は `ErrUnknownTarget`

### 02 collector 契約への適合

- unit `TestDBPoolCapture_Idempotent`: 同一 (runID, epoch) の `CaptureBaseline`
  再呼び出しが `At` を含めて同一の `SampleResult` を返す
- unit `TestDBPoolCapture_StaleEpoch`: 古い epoch → `runctl.ErrStaleEpoch`
- unit `TestDBPoolCapture_Committed`: 成功時 `Committed == true`、
  期限切れ ctx で `Committed == false` かつ `err != nil`(zero value を返さない)
- unit `TestDBPoolCollect_NoIODuringCollect`: fake Statser の `Stats()` 呼び出し
  回数が `Collect` の前後で不変(02 の `TestBaselineCollect_UsesFrozenSamplesOnly` 相当)
- unit `TestDBPoolRelease_Idempotent`: 二重 `Release` が no-op

### デルタとライフサイクル

- unit `TestDBPoolCollect_Delta`: 4 shard(4 pool)の並記と、`sql.DBStats` の
  9 フィールド全て(点観測 4 + 区間デルタ 5)の値検証。fake は
  `watchStats` フックで注入する
- unit `TestDBPoolCollect_CounterRewind`: 後退時に `Partial = true` /
  `Code = "counter-rewind"` / 表示値が final の current 値
- unit `TestDBPoolWatchMidRun_NotInCurrentRun`(v3 テストの置換):
  `CaptureBaseline` 後に `WatchDBPool` → 当該 run の `[]Entry` に含まれず、
  既存 pool の entry は完全なまま、**run の `Validity` が変化しない**。
  health に `dbpool-registered-mid-run` が記録される
- unit `TestDBPoolWatchMidRun_IncludedInNextRun`: 次の `CaptureBaseline` 以降は
  通常 entry として現れる
- unit `TestDBPoolUnwatchMidRun_TruncatedEntry`: run 中の `UnwatchDBPool` →
  entry は残り `Partial = true` / `Code = "unwatched-mid-run"` /
  `FinalAt == unwatchedAt`(< `final.SampledAt`)
- integration: snapshot JSON / HTML に注記文言(並列 wait の合計・平均 wait)が
  出ること
- integration: watch 0 件のとき DB Pool セクションが出ず、health に
  `dbpool-not-registered`(info)が載ること

## リスク

| リスク | 対策 |
|---|---|
| 登録忘れで空表示 | セクション自体を出さず、health に `dbpool-not-registered`(「WatchDBPool 未登録」)を info で記録 |
| TargetID の手書きミス | registry 登録済み ID のみ受理(`ErrUnknownTarget`)+ 推奨形は `RegisterDBTarget` の明示 ID。自動 ID の手書きは 01 §ID の受理規則で禁止 |
| プール再作成の検出漏れ(たまたま単調増加) | 完全検出は不可能。`Code = "counter-rewind"` は best-effort と doc に明記。確実に区切りたい場合は `UnwatchDBPool` → `WatchDBPool` を使う |
| run 途中登録の pool が計測されない | deferred activation を doc と health(`dbpool-registered-mid-run`)で明示。次の `/reset` から計測される |
| 他 collector と契約がずれる | 02 §collector 契約の conformance test を 06 の受け入れ条件に含める(実装ステップ 2) |

## 見積もり

**1.5 日**(v5 の 1 日 + 0.5 日。v5 の値は v3 から据え置きだったため、
README §リリース対応の増分表と同じ **v5 基準**で表記する):

| 追加項目 | 増分 |
|---|---|
| `BaselineCollector` 実装((runID, epoch) 冪等 cache・`ErrStaleEpoch`・conformance test) | +0.25 日 |
| deferred activation / mid-run Unwatch の farewell サンプルとテスト | +0.25 日 |
| **合計増分** | **+0.5 日** |
