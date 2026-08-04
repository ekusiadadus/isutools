# 03: hoststats(ホスト資源とホスト同一性)— v6

種別: 基盤 / 対象リリース: v1.2.x /
依存: 02(`BaselineCollector` 契約・予算モデル・`Validity`・境界ウィンドウ)/
新規パッケージ: `hoststats` / 変更箇所: `web`、`isutools.go`

**本ファイルは自前の予算値・自前の validity 型・自前の phase 名を定義しない**。
時間予算・collector 契約・区間妥当性は 02(唯一の正)を引用する。

## v6 での変更点(第5回レビュー差し戻し対応)

第5回レビューに **03 固有の指摘は無い**。本改訂は
(a) 02 v6 で確定した collector 契約への追随、
(b) v5 が開始側しか書いていなかった**終了側(`CaptureFinal`)の補完**、
(c) cgroup scope 節の内部整合の 3 点である。
各項目の重大度は、追随元となった **02 の指摘の重大度**を引く。

1. **[HIGH](02 の指摘 2 に追随)`Reset()` / `Snapshot()` を撤回し
   `runctl.BaselineCollector` を実装する**。
   `CaptureBaseline` / `CaptureFinal` は**採取値を内包した不変 handle**を返し、
   `Collect(base, final)` が固定済み 2 サンプルだけから `*Section` を作る。
   `SampleResult.Committed`・(runID, epoch) 冪等・`ErrStaleEpoch`・
   「`Collect` は `/proc` / `/sys` / syscall に一切触れない」までを具体化した
   (§collector 契約への適合)
2. **[HIGH](02 の指摘 6 に追随)並列採取と境界ウィンドウの会計を明記**。
   hoststats は `SerialOnly:false` で `BaselineConcurrency = 8` の errgroup に載る。
   `SampleResult.At` が `BoundaryWindow` に入ること、optional であるため
   スプレッド超過時も **partial 止まりで invalid にはならない**ことを明記した
   (§予算と境界ウィンドウの会計)
3. **[HIGH](02 の指摘 4 に追随)予算を 02 から引用のみにした**。
   03 は独自の秒数を書かない。`PerCollectorBaselineBudget`(3.5s)の内側で、
   ソース単位に `ctx.Err()` を検査して打ち切る規則を定義した
4. **[HIGH](02 の指摘 3 に追随)optional collector の失敗が run を invalid に
   できないことを明文化**。v5 の「カウンタ後退 = **区間 invalid 扱い**で Err 記録」は
   撤回し、**metric 単位の `Partial` + `Code`**(06 の entry 単位 Partial と同型)へ移した
   (§カウンタ後退とホスト変化)
5. **[MEDIUM](02 の指摘 5・06 の指摘 4 に追随)phase 名の置換と live provider 禁止**。
   `/collect` が非終端になったため「reset 時 / snapshot 時」という phase 名は
   撤回し、`start-baseline` / `finish-final` に置換した。
   web は immutable snapshot を描画し、**描画時に `/proc` を読み直さない**
6. **[MEDIUM] 終了側(`CaptureFinal`)の仕様を新設**。v5 はデータソース表に
   「reset 時と snapshot 時の両方を表示」と書くだけで、終了境界で何を採り、
   区間値をどう作るかが未定義だった。ソース別に
   「両境界で点観測 / 区間デルタ」を確定させ、レートの区間端点を
   **hoststats 自身の 2 つの `SampledAt`** と定義した(§データソースと採取 phase)
7. **[MEDIUM] cgroup scope の決定表を新設**。v5 は「既定は visible-root」と
   「自プロセスの実 cgroup パスを解決して読む」が併記されるだけで、
   どの条件で 4 値のどれになるかが決まらなかった。
   あわせて「**初期 cgroup namespace であることの外部証拠がある場合**」という
   検証不能な条件を撤回し、`host` は `ISUTOOLS_CGROUP_SCOPE=host` の
   明示指定のみとした(§cgroup の解決)
8. **[MINOR] 型名 `hoststats.Snapshot` を `hoststats.Section` へ改名**。
   02 の `Snapshot`(run の immutable snapshot)と紛らわしいため。
   04 が `Collect` から `*Section` を返すのと同形にした
9. **[MINOR] statfs / readlink / EvalSymlinks の注入シームを明記**。
   共通契約 5 の `fs.FS` 注入では syscall を差し替えられないため、
   `Options` に関数シームを置く(§注入シーム)
10. **[MINOR] hoststats 固有 health キーを 5 つに固定**(02 の `runctl-*` 4 キーとは
    別名前空間)。**見積もりを 2 日 → 2.5 日へ再算定**
11. **[MINOR] handle からサンプルを取り出す経路を 02 の契約として引用**。
    `runctl.BaselineHandle` が内包する `sample`(unexported)を取り出すアクセサ
    `func (h BaselineHandle) Sample() any` は **02 が提供する**。
    03 はこれを**消費する契約**として引用する
    (04 / 05 / 06 も同じアクセサを消費する。§collector 契約への適合)

- **v6 監査反映(クロスファイル整合)**: (a) 05 は独立パッケージ **`netstats`**(`New(procFS, sysFS fs.FS)`、
  `Options` 無し)であり、v5 の「hoststats 配下のサブパッケージ」「同じ `Options` を
  共有」を撤回。共有するのは procfs/sysfs 分離の**注入設計のみ**。
  (b) 05 v6 は既に 02 の collector 契約を実装済みのため「05 の改訂が必要」を撤回。
  (c) `BaselineHandle.Sample() any` は 02 が提供する契約であり、03 の未解決の
  追補要求ではない(項 11 を書き換え)。(d) plans/README.md v6 は 03 = 2.5 日で
  再算定済みのため「README の再算定が必要」を撤回
  (§02 から引用する契約 / §collector 契約への適合 / §注入シーム / §設計 / §見積もり)

## v5 から撤回する主張

| v5 の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| collector パターン `New(procFS, sysFS)` / `Reset()` / `Snapshot()` | 02 v6 の baseline 型 collector は `CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release` であり、`Reset`/`Snapshot` は存在しない | `runctl.BaselineCollector` を実装(§collector 契約への適合) |
| 「procstats と同じ **reset-to-snapshot** デルタ」 | `/collect` が非終端になり「snapshot 時」に対応する境界が消えた(02 §HTTP) | 区間は **start-baseline → finish-final**(02 §Phase) |
| データソース表の「reset 時と snapshot 時の両方を表示」 | 同上。phase 名が 02 に存在しない | 「baseline 点観測 / final 点観測」(§データソースと採取 phase) |
| 「カウンタ後退 = 再起動検出 → **区間 invalid 扱い**で Err 記録」 | (a) 02 v6 で validity は run 単位の `Validity` 軸であり collector が「区間を invalid にする」語彙を持たない。(b) hoststats は optional なので 02 §結果表 6/12/16 により**最悪でも partial**。(c) `Collect` から err を返すと Host セクション**全体**が除外され、点観測まで失われる | metric 単位の `Partial` + `Code="counter-rewind"`。`Collect` は err を返さない(§カウンタ後退とホスト変化) |
| 型名 `Snapshot.Host *hoststats.Snapshot` | 02 の `Snapshot`(run の immutable snapshot)と名前衝突して読み手が混乱する | `Snapshot.Host *hoststats.Section` |
| `host` scope を「初期 cgroup namespace であることの**外部証拠**がある場合」にも許す | 「外部証拠」の判定手続きが未定義で実装不能。cgroup namespace 内では `/proc/self/cgroup` も mountinfo も仮想化されるため、プロセス内から初期 namespace を確定する手段は無い | `host` は `ISUTOOLS_CGROUP_SCOPE=host` の**明示指定のみ**。自動昇格は行わない(§cgroup の解決) |
| 「v4 修正」「v5」等の版数注記を本文中に散在させる | 版数注記が本文の規則と混ざり、どれが現行仕様か読めない | 規則は本文へ、版間の差分は §v6 での変更点 / 本表へ集約 |

## 背景(レビュー指摘)

旧 06 は「agent で OS 資源が見える」と書いたが、現行 procstats +
旧 03(network)で取れるのは CPU・プロセス RSS・NIC 平均・TCP 点観測のみ。
DB ホストの診断に必要な以下が**存在しない**:

- 使用可能メモリ、page cache、swap、major fault
- ディスク read/write throughput・IO 時間・キュー(/proc/diskstats)
- PSI(/proc/pressure/{cpu,memory,io})
- filesystem 使用率、cgroup 制限

また 10(multi-host)の peer 識別に必要なホスト同一性情報
(machine-id、boot-id、namespace)も未整備。

## ゴール

1. ベンチ区間(start-baseline → finish-final)の memory / disk / PSI を
   ローカルホストで観測する
2. ホスト同一性(identity)を snapshot に含め、10 の重複 peer 検出と
   コンテナ可視性の判定材料にする
3. v1 は**表示のみ**(advisor 閾値はフィールド実測後)

## 非ゴール

- Netdata 代替の常時時系列監視(区間サマリに徹する)
- 全 mount / 全 device の網羅(主要対象に限定し、限定を明示する)
- 自前の run lifecycle・自前の予算値・自前の validity 型(すべて 02 が権威)

## 02 から引用する契約(唯一の出所)

| 出所 | 名前 | 本計画での使い方 |
|---|---|---|
| 02 | `runctl.BaselineCollector` | hoststats はこのインタフェースで登録する |
| 02 | `runctl.SampleResult` / `runctl.BaselineHandle` / `runctl.Epoch` / `runctl.ErrStaleEpoch` | 境界戻り値と fencing |
| 02 | `func (h runctl.BaselineHandle) Sample() any` | handle 内包 sample の取り出し。`Collect` の**唯一の入力経路**(§collector 契約への適合) |
| 02 | `runctl.Registration{Name:"hoststats", Required:false, SerialOnly:false}` | optional collector(02 §登録の既定: required は sqlstats / httpstats のみ) |
| 02 | `PhaseStartBaselineBudget`(5s)/ `PhaseFinishFinalBudget`(5s)/ `PerCollectorBaselineBudget`(3.5s)/ `BaselineConcurrency`(8) | 予算と並列度(§予算と境界ウィンドウの会計) |
| 02 | `PerTargetBudget`(1s) | **適用対象外**。hoststats は 01 の `Inspect` を呼ばない |
| 02 | `Phase`(`PhaseStartBaseline` / `PhaseFinishFinal`) | 区間の端点を表す唯一の phase 名 |
| 02 | `BoundaryWindow` / `SpreadLimitBoundary`(1500ms) | 採取時刻の会計(§予算と境界ウィンドウの会計) |
| 02 | `Validity`(`ValidityValid` / `ValidityPartial` / `ValidityInvalid`) | 区間妥当性。03 は独自 validity 型を作らない |
| 02 | 結果表 6 / 7 / 12 / 16 行(optional baseline の失敗・phase 予算超過) | hoststats の失敗は run を **partial** にする(invalid にはしない) |
| 02 | §FinishRun 手順 7「**固定値だけから** immutable snapshot を構築」 | web に live provider を置かない根拠 |

**05(network)との関係**: network は 02 §登録では **hoststats とは別の
baseline collector**(`Name() == "network"`、flag `ISUTOOLS_NETSTATS`)である。
05 v6 で network は**独立パッケージ `netstats`** に分離されたため、
v5 の「実装上は `hoststats` パッケージ配下のサブパッケージに置く」という
記述は**撤回する**。また「05 は同じ `Options` を共有する」も**撤回する**:
`netstats` は `func New(procFS, sysFS fs.FS) *Collector` を採り、
`Options` 構造体を持たない。両者が共有するのは
**procfs / sysfs を別 `fs.FS` として注入する設計**のみである。
`RegisterBaseline` は**別々に 2 回**行い、`Sample` も `Section` も別にする。
**05 v6 は既に 02 の collector 契約
(`CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release`)を
実装済みであり、03 から 05 への改訂要求は無い**
(v5 の「05 は collector 契約に未追随」という記述は撤回する)。

## collector 契約への適合(02 §collector 契約)

```go
package hoststats

type Collector struct {
    opt Options
    mu  sync.Mutex
    // (runID, epoch, phase) → 確定済み SampleResult(冪等再送用)
    results map[runKey]runctl.SampleResult
    // (runID, epoch, phase) → 採取済み Sample(handle が内包する実体)
    samples map[runKey]*Sample
    epoch   runctl.Epoch // 受理済みの最大 epoch(これより古い呼び出しは ErrStaleEpoch)
}

type runKey struct {
    RunID string
    Epoch runctl.Epoch
    Phase runctl.Phase
}

// 非 Linux / procFS 不在なら ErrUnsupportedOS を返す(呼び出し側は登録しない)。
func New(o Options) (*Collector, error)

func (c *Collector) Name() string { return "hoststats" }

// meminfo / vmstat / diskstats / PSI / statfs / cgroup / identity を読み、
// deep copy 済みの *Sample を内包した不変 handle を返す。
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)

// 固定済み 2 サンプルだけから *Section を作る。
// /proc・/sys・statfs・readlink・c.samples に一切アクセスしない。
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) // → *Section

// handle が内包する Sample 参照を落とす。冪等(二重 Release は no-op)。
func (c *Collector) Release(h runctl.BaselineHandle)

// sentinel error(errors.Is で判定可能)。02 の runctl.ErrStaleEpoch は
// そのまま返し、03 で別名を作らない。
var (
    ErrUnsupportedOS = errors.New("hoststats: unsupported OS or missing procfs")
    ErrNoSource      = errors.New("hoststats: no source readable")
)
```

**02 から引用する handle アクセサ(03 では別名を作らない)**:
`Collect(base, final)` が「handle だけから」区間値を作るには、
`runctl.BaselineHandle` が内包する `sample`(02 §用語と基本型では
**unexported** の `sample any`)を collector 側で取り出す経路が要る。
02 はこのアクセサを `func (h BaselineHandle) Sample() any` として提供する。
**これは 03 が出した未解決の追補要求ではなく、02 が定義し 03 が消費する契約である**
(v5 の「02 側に未定義」「追加は 02 で行う(要求)」という記述は撤回する)。
03 は `h.Sample().(*hoststats.Sample)` で復元する。
04 / 05 / 06 も同じアクセサを消費する(`BaselineCollector` を実装する 4 計画すべて)。
03 が独自のアクセサや `c.samples` 経由の抜け道を作ることはしない。

登録(`isutools.go`):

```go
if hc, err := hoststats.New(hoststats.Options{}); err == nil && hostStatsEnabled() {
    ctrl.RegisterBaseline(runctl.Registration{
        Name:       "hoststats",
        Required:   false, // 02 §登録の既定(required は sqlstats / httpstats のみ)
        SerialOnly: false, // 小さな procfs ファイルの読み取りのみ。並列採取で安全
    }, hc)
}
```

- `ISUTOOLS_HOSTSTATS=off`(既定 on)または `New` が `ErrUnsupportedOS` を
  返した場合は **`RegisterBaseline` を呼ばない**。登録されない collector は
  phase 予算も 02 §結果表も一切消費しない
  (テスト `TestHostStatsDisabled_NotRegistered`)

### `Committed` の決め方(02 §Committed セマンティクスへの適合)

| 状況 | err | Committed |
|---|---|---|
| `/proc/meminfo` が読めた(他ソースは一部 skip / 欠損) | nil | **true** |
| 呼び出し冒頭で `ctx.Err() != nil` | `ctx.Err()` | **false** |
| `/proc/meminfo` すら読めない(全ソース失敗) | `ErrNoSource` | **false** |

- `/proc/meminfo` を**必須ソース**とし、これが読めた時点でサンプルは確定する。
  他ソース(vmstat / diskstats / PSI / statfs / cgroup)は個別に skip 可能で、
  skip は `Sample.Codes` に `"not-captured:<source>"` として記録する
- `nil` + `Committed=false` は 02 の契約違反なので**発生させない**。
  上表を `TestHostStatsCommittedMatrix` で固定する
- 同一 (runID, epoch, phase) の再呼び出しは `results` から**同一値**
  (`At` も同一)を返す。より古い epoch は `runctl.ErrStaleEpoch` を返す
- ctx 期限超過時も `Committed` を正しく設定して返す(02 の要求)

| 契約項目 | hoststats の実装 | 出所 |
|---|---|---|
| `Committed` | 上表。zero value を返さない | 02 §Committed セマンティクス |
| 冪等性 | (runID, epoch, phase) をキーに `results` を cache し、再呼び出しは `At` を含めて同一値 | 02 §Committed セマンティクス |
| 古い epoch | `runctl.ErrStaleEpoch` | 02 §Committed セマンティクス |
| `Collect` の純粋性 | handle 内包の `*Sample` 2 つのみを読む。`/proc`・`/sys`・`statfs`・`readlink`・`c.samples` へ一切アクセスしない | 02 `BaselineCollector.Collect` / `TestBaselineCollect_UsesFrozenSamplesOnly` |
| per-collector 予算 | `PerCollectorBaselineBudget`(**3.5s**)の内側。03 は独自の秒数を定義しない。実測見込みは小ファイル約 10 本で **5ms 未満** | 02 §予算モデル |
| per-target 予算 | **適用対象外**(01 の `Inspect` を呼ばない) | 02 §予算モデル |
| 並列採取 | `BaselineConcurrency = 8` の errgroup で他 baseline collector と並列。`SerialOnly = false` | 02 §並列採取 |
| 境界ウィンドウ | `SampleResult.At` が `StartResult.BoundaryWindow` / `FinishAccepted.BoundaryWindow` に入る | 02 §境界ウィンドウ |
| 失敗時の run 評価 | optional なので最悪でも `ValidityPartial`。`ValidityInvalid` にはならない | 02 §結果表 6 / 12 / 16 行 |

### 不変 sample のデータモデル

```go
type Sample struct {                 // BaselineHandle.sample の実体(構築後は不変)
    Phase    runctl.Phase            // PhaseStartBaseline | PhaseFinishFinal
    At       time.Time               // = SampleResult.At = BaselineHandle.SampledAt
    Identity Identity                // 両境界で採取(boot_id 変化の検出に使う)
    Mem      MemRaw                  // /proc/meminfo の生値(bytes 換算済み)
    MajFault uint64                  // /proc/vmstat pgmajfault(累積)
    Disks    map[string]DiskRaw      // key: device 名。/proc/diskstats の累積値
    PSI      map[string]PSIRaw       // key: "cpu" | "memory" | "io"
    FS       map[string]FSRaw        // key: "/" と DataDir
    CGroup   *CGroupRaw              // 読めない/skip なら nil
    Codes    []string                // "not-captured:psi" 等(ソース単位の skip 理由)
}

type DiskRaw struct {                // /proc/diskstats のフィールド番号は仕様どおり
    ReadSectors  uint64              // field 3
    WriteSectors uint64              // field 7
    IOTicksMS    uint64              // field 10(IO 中の時間 ms)
    WeightedMS   uint64              // field 11(加重 IO 時間 ms = キューの積分)
}

type PSIRaw struct {                 // some/full の 2 行
    SomeAvg10, SomeAvg60 float64
    SomeTotalUS          uint64      // 累積 stall 時間(µs)
    FullAvg10, FullAvg60 float64
    FullTotalUS          uint64
    HasFull              bool        // full 行が存在したか
}
```

`MemRaw` / `FSRaw` / `CGroupRaw` は、対応する Section 型
(`Memory` / `FSUsage` / `CGroup`)の **片境界分**の生値を持つ構造体である
(例: `MemRaw{TotalBytes, AvailableBytes, CachedBytes, DirtyBytes,
SwapTotalBytes, SwapFreeBytes}`、`FSRaw{TotalBytes, AvailBytes}`、
`CGroupRaw{Scope, Path, CPUMaxCores, MemoryMaxBytes, MemoryCurrentBytes}`)。
`Collect` が 2 境界分を突き合わせて Section 型へ組み立てる。

`Collect(base, final)` は **この 2 つの `Sample` だけ**から `*Section` を作り、
FS・syscall・collector の可変状態に一切触れない
(02 の `TestBaselineCollect_UsesFrozenSamplesOnly` 相当を
`TestHostStatsCollect_UsesFrozenSamplesOnly` として実装する)。

## 予算と境界ウィンドウの会計

- 03 は**秒数を定義しない**。02 が渡す ctx(`PerCollectorBaselineBudget` = 3.5s の
  子)をそのまま使い、**ソースを 1 つ読む前ごとに `ctx.Err()` を検査**する。
  期限切れ後のソースは読まずに `Codes` へ `"not-captured:<source>"` を追加し、
  health `hoststats-source-skipped`(ソース名付き)を立て、
  既読分で `Committed=true` のまま返す(必須ソース `/proc/meminfo` は最初に読む)。
  読み取りエラー(ファイル欠如・権限不足)も同じ経路で skip として扱う
- phase 予算(`PhaseStartBaselineBudget` / `PhaseFinishFinalBudget` = 5s)を
  使い切って hoststats が呼ばれなかった場合は、02 §結果表 7 行により
  optional = **partial**。03 側の追加規則は無い
- `SampleResult.At` は **`/proc/meminfo` 読み取り直前**に `Options.Now()` で取り、
  以降のソース読みで値をずらさない(境界時刻の定義を 1 点に固定する)
- **スプレッドへの寄与**: hoststats の採取は 5ms 未満の見込みで、
  `SpreadLimitBoundary`(1500ms)を押し上げない。万一押し上げた場合でも
  hoststats は required 集合に含まれないため、02 §境界ウィンドウ判定表により
  `Spread(required のみ)` は変化せず、run は **partial 止まり**(invalid にならない)。
  超過時は 02 の health `runctl-boundary-spread` に collector 名 `hoststats` が載る
- レート算出の区間端点は **hoststats 自身の 2 つの `SampledAt`** であり、
  `BoundaryWindow`(全 collector を跨ぐ幅を持つ区間)ではない。
  他 collector の遅れを hoststats のレートに混ぜないため

## データソースと採取 phase

`start-baseline` / `finish-final` の**両方**で同じ読み取りを行う。
区間値の作り方はソースごとに下表で確定する。

| ソース | 取得内容 | start-baseline | finish-final | 区間値の作り方 |
|---|---|---|---|---|
| `/proc/meminfo` | MemTotal / MemAvailable / Cached / Dirty / SwapTotal / SwapFree | 点観測 | 点観測 | **両点を並べて表示**(デルタにしない。available は増減どちらも意味を持つ) |
| `/proc/vmstat` | pgmajfault | 累積値 | 累積値 | **区間デルタ** |
| `/proc/diskstats` | device 別 read/write セクタ(×512B)・IO 時間 ms・加重 IO 時間 | 累積値 | 累積値 | **区間デルタ**からレート算出(仕様: docs.kernel.org/admin-guide/iostats.html) |
| `/proc/pressure/{cpu,memory,io}` | some/full の avg10・avg60 + total(µs) | 点観測 + 累積 | 点観測 + 累積 | avg10/avg60 は **final の点観測**(直前 10s/60s を代表)。stall 比は **total の区間デルタ ÷ 区間長**(仕様: docs.kernel.org/accounting/psi.html)。カーネル非対応(< 4.20 / 無効)は skip |
| `statfs("/")`・`statfs(DataDir)` | 使用率 | 点観測 | 点観測 | **両点を並べて表示**(区間中の増加が分かる) |
| cgroup v2(§cgroup の解決) | `cpu.max` / `memory.max` / `memory.current` | 点観測 | 点観測 | limit は final を表示。両境界で limit が変わっていれば `Code="limit-changed"`。`memory.current` は両点 |
| identity(§identity) | hostname / machine-id / boot-id / namespace | 点観測 | 点観測 | base 側を採用。boot_id 変化は §カウンタ後退とホスト変化 |

- 区間長 `IntervalSec = final.At.Sub(base.At).Seconds()`。
  **1ms 未満ならレート系(MB/s・util%・queue・stall 比)を `nil`** にする
  (05 の「区間 0 秒はレート nil」と同一規則)
- disk の派生値(すべて表示のみ・閾値判定なし):
  - `ReadMBPerSec = Δ(ReadSectors) × 512 ÷ 1e6 ÷ IntervalSec`
  - `UtilPercent = Δ(IOTicksMS) ÷ (IntervalSec × 1000) × 100`
  - `QueueAvg = Δ(WeightedMS) ÷ (IntervalSec × 1000)`
  - **NVMe 等の multi-queue デバイスでは `UtilPercent` は飽和を意味しない**旨の
    固定注記を表示に付ける(テストで文言を固定)
- PSI の `cpu` の `full` は system レベルでは定義上 0(cgroup レベルでのみ意味を持つ)。
  `HasFull == false`(カーネル < 5.13)と合わせて **cpu の full は表示しない**

### 注入シーム(共通契約 5)

`fs.FS` では statfs / readlink / symlink 解決を差し替えられないため、
関数シームを `Options` に置く。nil のフィールドは OS 実装へ fallback する。

```go
type Options struct {
    ProcFS       fs.FS                              // 既定 os.DirFS("/proc")
    SysFS        fs.FS                              // 既定 os.DirFS("/sys")。procfs と別 FS(共通契約 5)
    CGroupFS     fs.FS                              // 既定: mountinfo から解決した cgroup2 mount root
    CGroupRoot   string                             // 同 mount の絶対パス(symlink 検証用)
    Statfs       func(path string) (FSRaw, error)   // 既定 unix.Statfs
    Readlink     func(name string) (string, error)  // 既定 os.Readlink(namespace 取得)
    EvalSymlinks func(name string) (string, error)  // 既定 filepath.EvalSymlinks
    DataDir      string                             // statfs 対象 2 つ目
    Now          func() time.Time                   // 既定 time.Now
}
```

- 05(`netstats`)とは **procfs / sysfs を別 `fs.FS` として注入する設計だけ**を
  共有する。`Options` 構造体そのものは共有しない(`netstats` は
  `New(procFS, sysFS fs.FS)` を採り `Options` を持たない)
- テストは `fstest.MapFS` + 上記関数の fake で全経路を駆動する

## cgroup の解決

`/sys/fs/cgroup` 直下(root)を固定で読むと、systemd service / container では
**自プロセスの実 cgroup ではなく cgroup root** を測ってしまう。
さらに agent と mysqld が別 cgroup の場合、agent の limit を DB の limit と
誤読する危険がある。したがって `/proc/self/cgroup`(v2 行 `0::<path>`)と
`/proc/self/mountinfo`(fstype=`cgroup2` の行)から **mount root と自プロセスの
cgroup パスを解決**してから読む。

**「root と実パスが同一なら host」という判定は行わない**:
cgroup namespace 内では `/proc/self/cgroup` と mountinfo の見え方自体が
仮想化され、コンテナ内の現在 cgroup が `/` に見える(cgroup_namespaces(7))。

### `cgroup.scope` 決定表(上から順に評価。最初に一致した行が結果)

| # | 条件 | `cgroup.scope` | 読む対象 |
|---|---|---|---|
| 1 | `ISUTOOLS_CGROUP_PATH` が設定され、§パス境界の検証を通過し、`cpu.max` / `memory.max` の少なくとも一方が読めた | `configured-cgroup` | 指定 cgroup |
| 2 | `ISUTOOLS_CGROUP_PATH` が設定されているが検証失敗 or 読めない | **cgroup 全体を skip**(`Section.CGroup = nil`) | — |
| 3 | `ISUTOOLS_CGROUP_SCOPE=host` が明示設定されている | `host` | 解決した自 cgroup パス |
| 4 | `/proc/self/cgroup` の v2 行が `0::/` | `visible-root` | mount 直下 |
| 5 | 上記以外(`0::/system.slice/mysqld.service` 等) | `agent-cgroup` | 解決した自 cgroup パス |
| 6 | v2 行が無い(cgroup v1 のみ) | **cgroup 全体を skip** + health `hoststats-cgroup-v1` | — |

- **行 2 は fail-closed**。明示指定が壊れているときに黙って別 cgroup へ
  fallback すると「DB の limit のつもりで agent の limit を表示する」誤読を
  生むため、fallback しない
- **`host` への自動昇格は行わない**。プロセス内から初期 cgroup namespace を
  確定する手段が無いため、`host` は明示設定のみ。10 の集約は
  「**明示的な host scope の agent だけを代表値に採用**」する(10 §identity の 2 層分離)
- `ISUTOOLS_CGROUP_PATH` / `ISUTOOLS_CGROUP_SCOPE` は**設定であって feature flag ではない**
  (機能の on/off は `ISUTOOLS_HOSTSTATS`)
- 表示は scope を必ず併記し、「agent の limit ≠ 観測対象サービスの limit」で
  あり得ることを注記する(注記文言をテストで固定)

### パス境界の検証(`ISUTOOLS_CGROUP_PATH`)

指定は **解決済み cgroup2 mount root からの相対パスに限定**する。

| 検証 | 実装 | 拒否コード |
|---|---|---|
| 絶対パス・`..`・空要素・先頭 `/` | `fs.ValidPath(rel)` が false なら拒否(構造的に 3 種とも弾ける) | `absolute` / `dotdot` |
| symlink による mount 外への脱出 | `EvalSymlinks(CGroupRoot + "/" + rel)` の結果が `CGroupRoot + "/"` を prefix に持たなければ拒否 | `escapes-mount` |
| 実在しない | `EvalSymlinks` が `fs.ErrNotExist` | `not-found` |
| TOCTOU | 検証で得た**絶対パスをそのまま開く**(再解決しない) | — |

- 空文字列 / `"."` は「未指定」と同じ扱い(決定表の行 3 以降へ落ちる)
- 拒否時は決定表の行 2(cgroup 全体 skip)+ health
  `hoststats-cgroup-path-rejected`(拒否コード付き)
- テスト fixture: `escape-absolute`(`/etc`)、`escape-dotdot`(`../../etc`)、
  `escape-symlink`(mount 内 symlink → mount 外)、`ok-relative`
  (`system.slice/mysqld.service`)

## identity

```go
type Identity struct {
    Hostname      string `json:"hostname"`
    MachineIDHash string `json:"machine_id_hash"` // sha256(machine-id)[:16]
    BootIDHash    string `json:"boot_id_hash"`    // sha256(boot_id)[:16]
    PIDNS         string `json:"pid_ns"`          // Readlink("/proc/self/ns/pid")
    NetNS         string `json:"net_ns"`
    MntNS         string `json:"mnt_ns"`
    CgroupNS      string `json:"cgroup_ns"`       // cgroup.scope の解釈材料
    Role          string `json:"role,omitempty"`  // ISUTOOLS_ROLE=app|db|dns|proxy(自由記述)
    AgentVersion  string `json:"agent_version"`   // buildinfo
}
```

- machine-id / boot_id は生値を出さずハッシュ短縮(識別には十分、
  値自体は host 固有情報のため)
- namespace ID は「agent がホストの何を見ているか」の証拠になる
  (コンテナ内 agent の可視性問題 → 10 の E2E で使用)
- `CgroupNS` は `cgroup.scope` と**常に併記**する。scope は
  「どの cgroup を読んだか」、`CgroupNS` は「どの namespace から見たか」であり、
  両方が無いと観測範囲が確定しない
- **10 の dedup キーはここが唯一の出所**:
  `(machine_id_hash, boot_id_hash, pid_ns, net_ns, mnt_ns, cgroup_ns, cgroup.scope)`
  (10 §identity の 2 層分離)。03 は値を提供するだけで dedup 判定は行わない
- 劣化: `Readlink` 不可 → 当該 namespace は空文字列。
  `/etc/machine-id` 欠如 → `MachineIDHash` は空文字列(エラーにしない)

## Section データモデル(`Collect` の戻り値)

```go
type Section struct {
    Identity    Identity   `json:"identity"`
    Interval    Interval   `json:"interval"`
    Memory      Memory     `json:"memory"`
    Disks       []Disk     `json:"disks,omitempty"`       // Device 昇順(決定性のため)
    PSI         *PSI       `json:"psi,omitempty"`         // 非対応カーネルは nil
    Filesystems []FSUsage  `json:"filesystems,omitempty"` // Path 昇順
    CGroup      *CGroup    `json:"cgroup,omitempty"`      // skip 時は nil
    Partial     bool       `json:"partial,omitempty"`
    // Codes は空 = 正常。03 が定義する安定コードのみ
    //(runctl.CollectorBoundary.Code とは別物):
    //   "not-captured:<source>" ソース単位の skip(psi / diskstats / vmstat / statfs / cgroup)
    //   "counter-rewind:<device|vmstat>" カウンタ後退を検出
    //   "boot-id-changed"       区間中に boot_id が変化(区間デルタを出さない)
    //   "machine-id-changed"    区間中に machine_id が変化(同上)
    //   "limit-changed"         cgroup limit が区間中に変化
    Codes []string `json:"codes,omitempty"`
}

type Interval struct {
    BaselineAt time.Time `json:"baseline_at"` // = base.SampledAt
    FinalAt    time.Time `json:"final_at"`    // = final.SampledAt
    Seconds    float64   `json:"seconds"`
}

type Memory struct { // すべて bytes。meminfo の kB から換算
    TotalBytes        uint64 `json:"total_bytes"`
    AvailableBaseline uint64 `json:"available_baseline_bytes"`
    AvailableFinal    uint64 `json:"available_final_bytes"`
    CachedBaseline    uint64 `json:"cached_baseline_bytes"`
    CachedFinal       uint64 `json:"cached_final_bytes"`
    DirtyBaseline     uint64 `json:"dirty_baseline_bytes"`
    DirtyFinal        uint64 `json:"dirty_final_bytes"`
    SwapTotalBytes    uint64 `json:"swap_total_bytes"`
    SwapFreeBaseline  uint64 `json:"swap_free_baseline_bytes"`
    SwapFreeFinal     uint64 `json:"swap_free_final_bytes"`
    PageMajorFaults   uint64 `json:"page_major_faults"` // 区間デルタ(pgmajfault)
}

type Disk struct {
    Device        string   `json:"device"`
    ReadBytes     uint64   `json:"read_bytes"`  // 区間デルタ
    WriteBytes    uint64   `json:"write_bytes"`
    ReadMBPerSec  *float64 `json:"read_mb_per_s,omitempty"`  // 区間 < 1ms / 後退時は nil
    WriteMBPerSec *float64 `json:"write_mb_per_s,omitempty"`
    IOTimeMillis  uint64   `json:"io_time_ms"` // 区間デルタ
    UtilPercent   *float64 `json:"util_percent,omitempty"`
    QueueAvg      *float64 `json:"queue_avg,omitempty"`
    Appeared      bool     `json:"appeared,omitempty"` // baseline に無い device(レート無し)
    Code          string   `json:"code,omitempty"`     // "counter-rewind"
}

type PSI struct {
    CPU    PSIResource `json:"cpu"`    // full は system レベルでは出さない
    Memory PSIResource `json:"memory"`
    IO     PSIResource `json:"io"`
}

type PSIResource struct {
    SomeAvg10      float64  `json:"some_avg10"` // final 境界の点観測
    SomeAvg60      float64  `json:"some_avg60"`
    SomeStallRatio *float64 `json:"some_stall_ratio,omitempty"` // Δtotal(µs) ÷ 区間(µs)
    FullAvg10      float64  `json:"full_avg10,omitempty"`
    FullAvg60      float64  `json:"full_avg60,omitempty"`
    FullStallRatio *float64 `json:"full_stall_ratio,omitempty"`
}

type FSUsage struct {
    Path          string `json:"path"` // "/" と DataDir
    TotalBytes    uint64 `json:"total_bytes"`
    AvailBaseline uint64 `json:"avail_baseline_bytes"`
    AvailFinal    uint64 `json:"avail_final_bytes"`
}

type CGroup struct {
    Scope                 string   `json:"scope"` // §決定表の 4 値
    Path                  string   `json:"path"`  // cgroup2 mount 相対
    CPUMaxCores           *float64 `json:"cpu_max_cores,omitempty"`   // cpu.max の quota÷period。"max" は nil
    MemoryMaxBytes        *uint64  `json:"memory_max_bytes,omitempty"` // "max" は nil
    MemoryCurrentBaseline uint64   `json:"memory_current_baseline_bytes"`
    MemoryCurrentFinal    uint64   `json:"memory_current_final_bytes"`
    Code                  string   `json:"code,omitempty"` // "limit-changed"
}
```

### カウンタ後退とホスト変化

v5 の「カウンタ後退 = 区間 invalid 扱いで Err 記録」は撤回する。
hoststats は optional collector であり、`Collect` から err を返すと
02 §結果表 16 行により **Host セクション全体が snapshot から除外**され、
点観測(メモリ・cgroup limit・identity)まで失われる。したがって:

| 検出 | 判定 | `Collect` の戻り |
|---|---|---|
| `final.Disks[d].* < base.Disks[d].*` のいずれか | 当該 `Disk` のみ `Code="counter-rewind"`。デルタとレートを出さず(`nil`)、点観測は残す | **err を返さない**。`Section.Partial=true` + `Codes` に `"counter-rewind:<device>"` + health `hoststats-counter-rewind` |
| `final.MajFault < base.MajFault` | `Memory.PageMajorFaults = 0` + `Codes` に `"counter-rewind:vmstat"` | 同上 + health `hoststats-counter-rewind` |
| baseline に無い device | 当該 `Disk.Appeared=true`。デルタ・レートを出さない | 同上(05 の `appeared` と同一規則) |
| `final.Identity.BootIDHash != base.Identity.BootIDHash` | **区間デルタ系を全廃**(disk / pgmajfault / PSI stall 比を `nil` / 0)。点観測と identity は残す | 同上 + `Codes` に `"boot-id-changed"` + health `hoststats-host-changed` |
| `final.Identity.MachineIDHash != base.Identity.MachineIDHash` | 同上 | 同上 + `Codes` に `"machine-id-changed"` |
| cgroup limit が両境界で不一致 | `CGroup.Code="limit-changed"`。final の limit を表示 | 同上 |

- run の `Validity` は 03 からは降格させない。降格は 02 の
  結果表(optional → partial)経由でのみ起きる

### health キー(03 固有 — 02 の `runctl-*` 4 キーとは別名前空間)

`hoststats-source-skipped` / `hoststats-cgroup-path-rejected` /
`hoststats-cgroup-v1` / `hoststats-counter-rewind` / `hoststats-host-changed`
の **5 つに固定**する(テスト `TestHostStatsHealthKeys` で列挙を固定)。

## 設計

- 新パッケージ `hoststats`。02 §登録の **baseline 型 collector**
  (procstats が 02 実装ステップ 5 で handle 化されるのと同じ形)
- **procfs と sysfs は別 FS として注入**(共通契約 5。
  現行 procstats の root=/proc では /sys を読めない問題の解消。
  05 `netstats` とはこの**注入設計のみ**を共有し、`Options` 型は共有しない)
- 出力は `Snapshot.Host *hoststats.Section`(additive、omitempty)。
  値は `Collect(base, final)` の戻り値をそのまま格納する。
  **web に live provider(`func() *hoststats.Section`)は置かない**
  (02 §FinishRun 手順 7「固定値だけから snapshot を構築」)。
  meta.capabilities に `hoststats` を追加(共通契約 4 の additive 規則)
- feature flag: `ISUTOOLS_HOSTSTATS=off` で無効化(既定 on。off の場合は
  `RegisterBaseline` を呼ばない)。読み取りは境界 2 回・小ファイルのみで、
  単独 ABBA で影響ゼロを確認して出荷
- 表示: 「Host」セクション新設。memory(baseline→final の available 変化)、
  disk(device 別 MB/s・util%・注記)、PSI(final の avg10 + 区間 stall 比)、
  fs 使用率(2 点)、cgroup(scope 併記)、identity(折りたたみ)
- Linux 以外・コンテナで読めないファイルは**ソース単位**で skip し
  health に理由を残す(fail-open。共通契約 1)

## 実装ステップ(TDD)

1. meminfo / vmstat / diskstats / PSI パーサをテスト先行
   (フォーマットゆらぎ・欠損・カーネル非対応・kB→bytes 換算)
2. `Options` の注入シーム(`fstest.MapFS` + fake statfs / readlink /
   EvalSymlinks)と `New` の `ErrUnsupportedOS`
3. cgroup: mount root 解決 → §決定表 6 行 → §パス境界の検証(escape fixture 4 種)
4. identity 取得(readlink 不可・machine-id 欠如の劣化)
5. `runctl.BaselineCollector` の実装
   (`CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release`)。
   (runID, epoch, phase) 冪等・`ErrStaleEpoch`・`Committed` 表・
   `Collect` の非 I/O を conformance test で固定(02 §collector 契約)
6. `Collect` の区間値(デルタ・レート・stall 比・区間 < 1ms・後退・appeared・
   boot_id 変化)
7. `isutools.go`: flag 判定 + `ctrl.RegisterBaseline`(未登録経路のテスト含む)
8. web 配線 + template(注記文言の固定)+ capabilities。**live provider は使わない**
9. docs: INTEGRATION.md「ホスト資源観測」節(コンテナでの可視性の注意 —
   namespace 依存であること、`cgroup.scope` の読み方、`host` は明示設定のみ)
10. 単独 ABBA(flag off↔on)で影響ゼロを実測し記録

## テスト計画

- unit `TestHostStatsCommittedMatrix`: §`Committed` の決め方 3 行を table-driven
- unit `TestHostStatsCapture_Idempotent`: 同一 (runID, epoch, phase) の再呼び出しが
  `At` を含めて同一の `runctl.SampleResult` を返す
- unit `TestHostStatsCapture_StaleEpoch`: より古い epoch → `runctl.ErrStaleEpoch`
- unit `TestHostStatsCollect_UsesFrozenSamplesOnly`: 採取後に全 FS / statfs /
  readlink シームを**エラーを返す fake へ差し替えて**も `Collect` が成功する
  (02 の `TestBaselineCollect_UsesFrozenSamplesOnly` 相当)
- unit `TestHostStatsRelease_Idempotent`: 二重 `Release` が no-op
- unit `TestHostStatsBudget_PartialSourcesCommitted`: 期限切れ ctx で
  meminfo だけ読めた場合に `Committed=true` + `Codes` に `not-captured:*`
- unit `TestHostStatsScopeMatrix`: §cgroup 決定表 6 行を table-driven
- unit `TestHostStatsCGroupPathEscape_Rejected`: escape fixture 4 種
  (`absolute` / `dotdot` / `escapes-mount` / `not-found`)で cgroup 全体 skip +
  health `hoststats-cgroup-path-rejected`
- unit `TestHostStatsCGroupV1_Skipped`: v2 行が無い環境で cgroup のみ skip
- unit `TestHostStatsPSIAbsent_SkipsPSIOnly`: PSI 無効カーネル(ファイル欠如)で
  PSI のみ skip(他ソースは出る)
- unit `TestHostStatsDiskstats_SectorsAndPartitions`: セクタ→バイト換算・
  パーティション行の除外(主デバイスのみ。判定は `/sys/block` 配下の存在で行う)
- unit `TestHostStatsCounterRewind_PartialNotDropped`: 後退時に `Collect` が
  err を返さず、`Section.Partial=true` + 当該 `Disk.Code="counter-rewind"`、
  点観測が残ること
- unit `TestHostStatsBootIDChanged_DropsDeltas`: boot_id 変化で区間デルタが
  全廃され、点観測と identity が残ること
- unit `TestHostStatsZeroInterval_NilRates`: 区間 < 1ms でレート系がすべて nil
- unit `TestHostStatsDisabled_NotRegistered`: `ISUTOOLS_HOSTSTATS=off` /
  `ErrUnsupportedOS` で `RegisterBaseline` が呼ばれないこと
- unit `TestHostStatsHealthKeys`: 03 固有 health キーが 5 つであること
- integration: Linux CI でのスモーク(panic しない・JSON 生成・
  `Snapshot.Host` が omitempty で消えないこと)
- 集計カバレッジ 80%(共通契約 5)

## リスク

| リスク | 対策 |
|---|---|
| device 名の多様性(nvme/vd/xvd/dm) | /sys/block 由来の実在確認で機械判定。フィルタ規則を doc 化 |
| コンテナで meminfo がホスト値 | identity の namespace 情報と `cgroup.scope` を併記し、解釈を読者に委ねる注記 |
| PSI の some/full の誤読 | 表示ラベルに some/full を明記。cpu の full は出さない。閾値判定はしない |
| util% を飽和と誤読(NVMe multi-queue) | 固定注記をテンプレートに含め、テストで文言を固定 |
| 境界 2 回の読み取りが phase 予算を圧迫 | ソース単位の `ctx.Err()` 検査で打ち切り、optional として partial に留める(02 §結果表 7) |
| `cgroup.scope` の誤解釈で 10 の集約が壊れる | `host` は明示設定のみ。10 は明示 host scope のみを代表値に採用(10 §identity の 2 層分離) |

## 見積もり

**2.5 日**(v5 の 2 日 + 下表の 0.5 日):

| 追加項目 | 増分 |
|---|---|
| `BaselineCollector` 適合(handle / `Committed` / (runID,epoch,phase) 冪等 / `Collect` 純粋化 / conformance test) | +0.25 日 |
| 終了側(`CaptureFinal`)の区間値規則 + cgroup scope 決定表 + escape fixture | +0.25 日 |
| **合計増分** | **+0.5 日** |

**plans/README.md との整合(再算定は完了済み)**: README v6 §リリース対応の
再算定表は v1.2.x を `03(2.5)+ 05(2.0)+ 06(1.5)+ 11(1.5)= 7.5 日` と
計上しており、03 分 **2.5 日**は本節と一致する。
v5 の「plans/README.md の再算定が必要」という注記は**撤回する**。
