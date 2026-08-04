# 02: Run lifecycle coordinator — v6

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `internal/runctl`(新規)、`web`、`isutools.go`、各 collector

**本ファイルは run lifecycle 状態機械・collector 契約・時間予算の唯一の正**である。
10 は状態機械を wire へ一対一で写し、04/06/08/09 は本ファイルの数値と型名を
**引用する**(独自の予算値・独自の状態名を発明しない)。

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] finishing 中の Abort と background worker の競合を解消**。
   Controller が run ごとに `cancel` と `done` を所有し、AbortRun は
   **epoch を進めて fence → cancel → bounded join → handle 破棄**の順で
   実行する。join timeout 時の挙動(detached)も定義した。
   stale worker からの state 変更・snapshot 公開は epoch fencing で
   構造的に不可能にし、race detector 下の必須テストを追加(§epoch fencing)
2. **[HIGH] BaselineCollector に handle と `Collect` を追加**。
   `CaptureBaseline`/`CaptureFinal` は**不変 handle(採取値を内包)**を返し、
   `Collect(base, final)` が固定値だけから区間値を作る。
   GenerationCollector.Collect と同形にした(§collector 契約)
3. **[HIGH] 「切替前に失敗」と「切替後に結果を返せず失敗」を
   `Committed bool` で区別**。全操作を (runID, epoch) 単位で冪等にし、
   **phase × collector 種別 × required の結果表**を追加した(§結果表)
4. **[HIGH] 階層予算モデルを本ファイルに一元定義**。
   run 予算 > phase 予算 > per-collector 予算 > per-target 予算。
   04 の「per-target 1s / 全体 3s」が成立する具体値にした(§予算モデル)
5. **[HIGH] `POST /collect` を非終端のまま維持**。run の終了は
   `POST /save` と**新設 `POST /finish`** のみ。互換性保証を明文化した(§HTTP)
6. **[HIGH] baseline の採取を並列化し、境界を「幅を持つ区間」として扱う**。
   開始・終了の両境界で `BoundaryWindow{Min,Max,Spread}` を記録し、
   許容スプレッドの上限値と超過時の partial/invalid 判定を定義した(§境界ウィンドウ)
7. **[CRITICAL] 後発呼び出しによる先取り(preempt)を API 化**。
   `StartRun(ctx, StartRunOptions{Preempt: true})` が active run を
   原子的に abort してから新 run を開始する。preempt された run は
   **aborted + invalid として記録**され、再利用しない。
   併せて初期化処理全体を直列化する `SerializeInitialize` を公開した(§preempt)
8. **[MINOR] 見積もりを 5 日 → 9.5 日へ再算定**(§見積もり)

### v6 監査反映(cross-file 整合の確定 — 本ファイルが lifecycle 契約の所有者として裁定)

- **[BLOCKING] `func (h BaselineHandle) Sample() any` を追加**。03/04/05/06 の
  `Collect(base, final)` は handle 内包サンプルの読み出しを前提に書かれており、
  アクセサ不在では実装不能だった(§collector 契約 / §`BaselineHandle.Sample()`)
- **`AckedBy` の値集合を `explicit` / `save` / `preempt` / `hub` / `lease` へ拡張**。
  10 が同フィールド・同 JSON tag で `hub` / `lease` を出すため、
  所有側である本ファイルで値集合を定義した(§API)
- **`SerializeInitialize` の ctx マーカー(`initializeGuardKey{}`)を明文化**。
  08 の health `initialize-unserialized` と
  `TestInitializeWithoutGuard_HealthRecorded` が依存する(§preempt / 実装ステップ 9)
- **`RetainedRuns = 2` の破棄規則を明記**(進行中を含む件数・overflow 時は最古の
  非 active record を `TombstoneTTL` を待たず即破棄)。10 が引用できる形にした(§lease / TTL)
- **`/reset` 応答上限に 07 の `ProfileCaptureLease`(3s)分の伸びを追記**(§HTTP / §予算モデル)
- **未知 runID への `AbortRun` は冪等な no-op 成功へ確定**(10 の E2E に合わせ、
  「破棄済み runID は `ErrUnknownRun`」という読み方を撤回。Finish/GET/Snapshot/Ack は 404 のまま)(§遷移表 A)
- **遷移表 B に外部起因の 2 遷移を追加**:
  `finished → acknowledged`(10 の `PeerAckLease` 満了・`AckedBy="lease"`)と
  `started → aborting`(10 の `PeerStartedLease` 満了 → `AbortRun(..., "hub-abort")`)
- **[実地検証] `Drain` の実装機構を指定**。現行 `httpstats` の `sync.Cond` は
  `ctx.Done()` で中断できず `DrainCancelGrace` 契約が実装不能なため、
  per-generation done channel への置換を必須要件にした(§Drain の実装機構)
- **見積もりの誤りを訂正**(`01(2.5)+02(9.5)+04(3.5) = 15.5 日 → 20 日`)し、
  「README の再算定が必要」注記を撤回。roll-up の権威は README v6(§見積もり)
- 他計画への「改訂が必要」注記を現況へ更新(10 v6 §deadline 採用済み / 08 v6 置換済み)

## v5 から撤回する主張

| v5 の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| 「AbortRun はどの状態からでも受理し、aborted にして idle へ戻す」 | in-flight worker の cancel/join が無く、stale worker が snapshot を保存して finished へ戻せる | epoch fence → cancel → join → 破棄 → idle(§epoch fencing) |
| 「`CaptureBaseline/CaptureFinal` は SampledAt(時刻)だけを返す」 | snapshot 構築時に collector の可変内部状態を読むことになり「固定値だけから構築」が成立しない | 不変 handle + `Collect(base, final)`(§collector 契約) |
| 「baseline collector 全体 budget 2s」 | 04 が自身の契約(全体 3s)を満たせない | 階層予算モデル(§予算モデル) |
| 「freeze phase が全セクション共通の計測終了**境界**(瞬間)である」 | baseline を逐次採取する限り境界は瞬間ではない | 境界は `BoundaryWindow` という**幅を持つ区間**。並列採取で幅を縮め、幅を実測記録し上限を定める(§境界ウィンドウ) |
| 「`POST /collect` / `POST /save` = FinishRun を内包」 | `/collect` は現行 README のエンドポイント表どおり **buffered accesslog の非終端 flush** であり、run を終了させない。破壊的変更になる | `/collect` は非終端のまま。終了は `/save` と新設 `/finish`(§HTTP) |
| 「StartRun は idle のみ受理。started 中は先に FinishRun か AbortRun」 | 08 の「後発 initialize が必ず新しい境界を張る」が実現不能 | `Preempt` option を追加(§preempt) |
| 08 が参照する `runctl.StateInvalid` / `runctl.StatePartial` | lifecycle state と妥当性(validity)を混同していた | `RunState`(lifecycle)と `Validity`(直交軸)に分離。08 は `StartResult.Validity` を見る(§状態機械) |
| 「BeginBoundary 途中失敗の扱い」を BeginBoundary だけに規定 | CaptureBaseline / Freeze / CaptureFinal / Collect の失敗規則が未定義だった | 全 phase を結果表で網羅(§結果表) |

## ゴール

1. run の開始・終了・中止・破棄・回収を含む**完全な lifecycle** を単一の
   Controller が所有する
2. 計測対象 handler の内側から呼んでもデッドロックしない
3. 新 run の冒頭・末尾を欠落/混入させない(開始 baseline 同期採取・
   終了 freeze 同期固定)
4. どの失敗経路からも**次の run を開始できる**(Abort / Preempt による回復)
5. **中止された run のデータが決して保存・公開されない**(epoch fencing)

## 用語と基本型

```go
package runctl

// RunState: lifecycle 上の位置。10 が wire へ一対一で写す。
type RunState string
const (
    StateIdle         RunState = "idle"
    StateStarting     RunState = "starting"     // 開始境界の実行中(owner: StartRun 呼び出し元)
    StateStarted      RunState = "started"      // 計測中(owner なし)
    StateFinishing    RunState = "finishing"    // Drain/Collect/構築 中(owner: background worker)
    StateFinished     RunState = "finished"     // immutable snapshot 確定
    StateAcknowledged RunState = "acknowledged" // 取得完了(終端)
    StateAborting     RunState = "aborting"     // fence 済み・join 待ち
    StateAborted      RunState = "aborted"      // 中止(終端)
    StateExpired      RunState = "expired"      // TTL 満了で snapshot を解放(終端・tombstone)
)

// Validity: RunState とは直交する「データとしての妥当性」。
// v5 の StateInvalid / StatePartial は撤回し、この軸へ移した。
type Validity string
const (
    ValidityValid   Validity = "valid"
    ValidityPartial Validity = "partial" // 一部 collector が欠けるが区間としては使える
    ValidityInvalid Validity = "invalid" // 区間として信用できない。advisor/diff から除外
)

// epoch: Controller 単調増加カウンタ。StartRun 成功ごと・AbortRun ごとに +1。
// 「今どの run が現行か」を表す fencing token。
type Epoch uint64

// GenerationHandle: 閉じた(または freeze した)世代への不変参照。
type GenerationHandle struct {
    RunID     string
    Epoch     Epoch
    Collector string
    Gen       uint64
    token     any // collector 内部の世代参照(unexported)
}

// BaselineHandle: 採取済みサンプルを内包する不変値。
// 保持者は collector の可変内部状態に一切触らない。
type BaselineHandle struct {
    RunID     string
    Epoch     Epoch
    Collector string
    Phase     Phase
    SampledAt time.Time
    sample    any // 採取時点で deep copy 済み(unexported)
}

// Sample は handle が内包する採取済みサンプルを返す**唯一の公式アクセサ**。
// 返り値は `SampledAt` 時点で deep copy されて固定されたスナップショットであり、
// **呼び出し側は決してこれを変更してはならない**(handle は複製・共有され得るため、
// 変更が必要なら呼び出し側が自前で複製する)。
// BaselineCollector 実装は `Collect(base, final)` の中でこの値だけを読み、
// 自分のサンプル型へ type assert する(例: `h.Sample().(*hoststats.Sample)`)。
func (h BaselineHandle) Sample() any

type Phase string
const (
    PhaseStartBoundary Phase = "start-boundary" // generation: BeginBoundary
    PhaseStartBaseline Phase = "start-baseline" // baseline:   CaptureBaseline
    PhaseFinishFreeze  Phase = "finish-freeze"  // generation: Freeze
    PhaseFinishFinal   Phase = "finish-final"   // baseline:   CaptureFinal
    PhaseCollect       Phase = "collect"        // 両種:       Drain → Collect(background)
)
```

## collector 契約(v6)

```go
type GenerationCollector interface {
    Name() string

    // 開始境界: 新世代へスワップし、閉じた旧世代の handle を返す。
    // 高速・非ブロッキング(per-collector 予算 100ms)。
    BeginBoundary(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error)

    // 終了境界: 現在世代を freeze し、その handle を返す。高速・同期。
    // freeze 後の観測は次世代(run 外)に入る。
    Freeze(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error)

    // handle が指す世代のみを確定する(in-flight 完了待ち・追い付き collect)。
    // ctx.Done() から DrainCancelGrace(1s)以内に必ず return し、
    // return 後に当該世代を変更する goroutine を残さない(conformance test)。
    // 待ち合わせは **per-generation done channel** で実装する(§Drain の実装機構)。
    Drain(ctx context.Context, h GenerationHandle) error

    // Drain 済み handle の確定データを読む。可変内部状態を読まない。
    Collect(h GenerationHandle) (any, error)

    // handle が押さえている資源を解放する。冪等。二重 Release は no-op。
    Release(h GenerationHandle)
}

type BaselineCollector interface {
    Name() string

    // 開始・終了とも bounded I/O の同期採取。
    // v6: 時刻だけでなく **採取値を内包した不変 handle** を返す。
    CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error)
    CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error)

    // 固定済み 2 サンプルだけから区間値を作る。読んでよいのは
    // `base.Sample()` / `final.Sample()` が返す固定値だけ。
    // collector の可変内部状態・DB・/proc へ**一切アクセスしない**
    //(conformance test: Collect 呼び出し中に I/O が発生しないことを fake で検証)。
    Collect(base, final BaselineHandle) (any, error)

    // 冪等。二重 Release は no-op。
    Release(h BaselineHandle)
}

// 境界操作の結果。err != nil でも **必ず** 返す(zero value を返さない)。
type BoundaryResult struct {
    Handle    GenerationHandle
    At        time.Time // スワップ / freeze の実測時刻
    Committed bool      // この runID について切替が**発効しているか**
}

type SampleResult struct {
    Handle    BaselineHandle
    At        time.Time // 採取の実測時刻(= Handle.SampledAt)
    Committed bool      // この runID についてサンプルが**確定しているか**
}
```

### `BaselineHandle.Sample()`(`Collect(base, final)` の唯一の入口)

`Collect(base, final)` を「固定値だけから区間値を作る」契約にした以上、
**実装が固定値へ到達する経路が公開されていなければならない**。
そのアクセサが `func (h BaselineHandle) Sample() any` である。

- 返り値は `SampledAt` 時点で **deep copy されて固定されたスナップショット**。
  Controller も collector も採取後にこれを書き換えない
- **呼び出し側は返り値を変更してはならない**。handle は値としてコピー・共有され
  得るため、変更は他の保持者から観測される。加工が必要なら呼び出し側が複製する
- baseline collector は `base.Sample()` / `final.Sample()` を自分のサンプル型へ
  type assert して使う(例: 03 の `h.Sample().(*hoststats.Sample)`)。
  **collector が独自のアクセサや自身の可変フィールド(`c.samples` 等)を経由する
  抜け道を作ってはならない** — それは「可変内部状態を読まない」契約の違反である
- 型が合わない場合(`Collector` 名の取り違え等)は `Collect` が
  `Code = "collect-failed"` の err を返す。panic させない
- 03 / 04 / 05 / 06 の `Collect` 実装はすべてこのアクセサを前提に書かれている。
  conformance test は `TestBaselineCollect_UsesFrozenSamplesOnly`(§実装ステップ 5)

### `Committed` セマンティクス(err との直交・冪等性)

`Committed` は「**この呼び出しが**切替をしたか」ではなく
「**この runID について**切替が発効済みか」という状態述語である。
これにより再送が同じ値を返し、曖昧さが消える。

| err | Committed | 意味 | Controller の扱い |
|---|---|---|---|
| nil | true | 成功 | 正常 |
| nil | false | **契約違反**(切替せずに成功を返した) | required 失敗と同じ扱い + health `runctl-contract-violation`。conformance test で検出 |
| != nil | false | 切替前に失敗。プロセス状態は**旧世代のまま** | 当該 collector は run に参加しない(セクション除外) |
| != nil | true | 切替は発効したが結果の確定に失敗 | Handle は有効。Drain → Release して破棄。セクションは除外 |

- 全操作は **(runID, epoch) 単位で冪等**。同一 (runID, epoch) での再呼び出しは
  最初と同一の `BoundaryResult` / `SampleResult` を返す(`At` も同一値)。
  異なる epoch での呼び出しは `ErrStaleEpoch` を返す
- collector は **`ctx` の期限超過時も `Committed` を正しく設定して返す**。
  「ctx 超過だから zero value」は契約違反

### `Drain` の実装機構(現行コードの実測に基づく必須要件)

**[v6 監査 / 実地検証] 現行 `httpstats` の待ち合わせ機構のままでは、上の `Drain`
契約(`ctx.Done()` から `DrainCancelGrace` 以内に必ず return)は実装できない。**
実測した現行コードは次のとおり:

- `httpstats/httpstats.go` の `Collector.Reset()`(328-346 行)は
  `for old.inFlight != 0 { c.changed.Wait() }`(341-342 行)で待つ
- `c.changed` は `New()` 内の `sync.NewCond(&c.mu)`(167 行)で作った
  `*sync.Cond` であり、`Broadcast()` は `release`(357-364 行)と
  `finish`(366-374 行)の中で **`g.inFlight` が 0 になったときにしか発火しない**
  (361 行 / 371 行)

`sync.Cond.Wait()` は **`ctx.Done()` では中断できない**。in-flight のリクエストが
1 本でも返らなければ(hijack した長寿命接続・応答を返さない handler 等)
`Broadcast()` は永久に発火せず、park した waiter はタイムアウトでも起きない。
よって現行機構のままでは `DrainCancelGrace` 契約は**実装不能**である。

**必須要件(実装ステップ 4 の受け入れ条件。`sync.Cond` を置換する)**:

1. `generation` に **per-generation の done channel**(`done chan struct{}`)と
   **`sealed bool`**、`closeOnce sync.Once` を持たせ、世代の生成時に作る
2. **close 条件は `sealed && inFlight == 0`**。`sealed` は
   `BeginBoundary` / `Freeze` の swap 時(その世代が current でなくなる瞬間)に
   `c.mu` の下で立てる。close は次の 2 箇所から `closeOnce` で 1 回だけ行う:
   - swap 時点で既に `inFlight == 0` なら **swap 側**が close
   - そうでなければ、最後の `release` / `finish` が `inFlight` を 0 にした時に
     **`sealed` が真である場合に限り** close

   > **[v6 監査反映] 撤回**: 「`inFlight` が 0 に落ちた時点で close する
   > (現行 `Broadcast()` の位置をそのまま置き換える)」という指示は**誤りであり撤回する**。
   > 現行の `if g.inFlight == 0` 分岐は **live generation でも発火する**
   > (`begin()` は新規リクエストを `c.current` に付けるため、断続的なトラフィックでは
   > current 世代の `inFlight` が何度も 0 になる)。そのまま close すると
   > **`close of closed channel` で panic** するか、once ガードを付けた場合は
   > 「live のうちに `done` が閉じ → その後 in-flight が増え → swap 後の `Drain(h)` が
   > **即座に nil を返す**」という無言の正確性の穴になり、
   > 「return 後に当該世代を変更する goroutine を残さない」契約を破る。
   > `sealed` を条件に加えることで、close は**その世代が current でなくなった後**にのみ
   > 起きることが保証される。
3. `Drain` はこの channel で待つ:

```go
select {
case <-h.done():      // inFlight == 0 で close 済み
    return nil
case <-ctx.Done():
    return ctx.Err()  // DrainCancelGrace 内に必ず戻る
}
```

4. `Collector` の `changed *sync.Cond` フィールドは**撤去する**。
   互換のため旧 `Reset()` を残す場合も、内部実装を done channel へ差し替える
5. ctx 打ち切りで return した後に遅れて完了した in-flight は、
   **その世代の table にだけ**書き込む(次世代を汚さない)。Controller 側は
   err で戻った世代を結果表 13 / 14 の `drain-timeout` として扱い、部分データを載せる

**Drain conformance test はこの機構に依存する**: 実 collector で in-flight を
1 本ブロックさせたまま `ctx` を cancel し、(a) `Drain` が `DrainCancelGrace`(1s)
以内に return すること、(b) return 後にブロックを解除しても当該世代以外へ
書き込む goroutine が残らないこと、を `-race` 下で検証する。
`sync.Cond` 実装のままではこのテストは必ず timeout するため、
**機構の回帰検知そのもの**として機能する。

### 登録

```go
type Registration struct {
    Name       string
    Required   bool // 既定: sqlstats / httpstats は true、その他 false
    SerialOnly bool // 並列採取が安全でない場合のみ true(既定 false)
}

func (c *Controller) RegisterGeneration(r Registration, g GenerationCollector) error
func (c *Controller) RegisterBaseline(r Registration, b BaselineCollector) error
```

- 世代型: httpstats / sqlstats / accesslog(EOF offset を freeze 点で記録)/ counters
- baseline 型: procstats / sqlrows(04)/ dbpool(06)/ network(05)/ hoststats(03)
- 現時点で `SerialOnly = true` の collector は**無い**。04 は 01 の
  `Inspect`(target ごとの専用 `*sql.Conn`)を使うため target 間で並列安全

## 各フェーズの手順(v5 の StartRun / FinishRun 節を統合・明確化)

### StartRun(nonce 付き・冪等・Preempt 可)

1. 遷移ガード(遷移表 A)。`Preempt=true` なら先に active run を
   fence → cancel → join → 破棄する(§epoch fencing)
2. runID / epoch を発番し、slot を `starting` にする。
   内部 ctx = `context.WithTimeout(context.WithoutCancel(caller), StartRunBudget)`
3. **start-boundary phase(逐次・500ms)**: 全 generation collector の
   `BeginBoundary` → `(prev handle, 実測時刻, Committed)`
4. **start-baseline phase(並列・5s)**: 全 baseline collector の
   `CaptureBaseline` → `(BaselineHandle, 実測時刻, Committed)`
5. `GenerationWindow` / `BoundaryWindow` を算出し、スプレッド判定を適用
6. 失敗があれば結果表(§phase × 種別 × required)に従って validity を決める。
   required 失敗なら遷移表 B に従い `aborting` → `aborted`(記録は残す)
7. 不変 `StartResult` を確定して返す
   (成功時 `State=started`、required 失敗時 `State=aborted` + `Validity=invalid`)
8. background: prev handle 群の `Drain`(detached ctx・`DrainBudget` 10s)。
   完了しても state は `started` のまま(Drain は run の終了ではない)。
   この Drain worker も slot の `cancel` / `done` に紐づく(Abort の join 対象)

**戻り値の規約(err と Validity の役割分担)**:

- `err != nil` は「Controller が処理そのものを行えなかった」場合だけ
  (遷移拒否 = `ErrRunActive` 等、内部 ctx 超過)。このとき `StartResult` は zero
- collector の失敗は `err = nil` + `StartResult.Validity`(partial / invalid)で表す。
  境界処理自体は完遂しているため error にしない。
  **呼び出し側は必ず `Validity` を検査する**(08 の規範例を参照)
- `FinishRun` / `AbortRun` も同じ規約に従う

### FinishRun(終了境界 — 全 collector 共通)

1. 遷移ガード(遷移表 A)。冪等: 同一 runID の再送は保存済み `FinishAccepted`
2. slot を `finishing` にし、`lease = now + FinishLease` を設定。
   内部 ctx = `context.WithTimeout(context.WithoutCancel(caller), FinishSyncBudget)`
3. **finish-freeze phase(逐次・500ms)**: 全 generation collector の `Freeze`
4. **finish-final phase(並列・5s)**: 全 baseline collector の `CaptureFinal`
5. 2 つの `BoundaryWindow` を算出しスプレッド判定 → validity 決定
6. 不変 `FinishAccepted` を**即座に返す**(Drain・snapshot 構築は待たない)
7. background worker(owner):
   `Drain`(10s)→ `Collect`(generation は handle、baseline は base/final の 2 handle)
   → 付加取得(09 の EXPLAIN。`EnrichBudget` 2s)→ **固定値だけから** immutable
   snapshot を構築 → `publish(epoch, snap)` → `commit(epoch, state=finished)`。
   worker は `defer` で必ず `done` を close し、未 Release の handle を Release する

### AbortRun

§epoch fencing の 5 手順が仕様。

## 予算モデル(階層 — 本ファイルが唯一の権威)

**親が権威**であり、子の予算は**必ず親より厳密に小さい**。
子が親を超える値を要求した場合は登録時エラー(`ErrBudgetInversion`)にする。
下流計画は下表の定数名と数値を**引用**し、独自値を定義しない。

| 段 | 定数 | 既定値 | 適用範囲 |
|---|---|---|---|
| run(同期部) | `StartRunBudget` | **6s** | StartRun の同期処理全体 |
| run(同期部) | `FinishSyncBudget` | **6s** | FinishRun の同期処理全体(freeze + final) |
| phase | `PhaseStartBoundaryBudget` | **500ms** | 全 generation collector の BeginBoundary |
| phase | `PhaseStartBaselineBudget` | **5s** | 全 baseline collector の CaptureBaseline |
| phase | `PhaseFinishFreezeBudget` | **500ms** | 全 generation collector の Freeze |
| phase | `PhaseFinishFinalBudget` | **5s** | 全 baseline collector の CaptureFinal |
| collector | `PerCollectorGenerationBudget` | **100ms** | 1 世代型 collector の 1 境界操作 |
| collector | `PerCollectorBaselineBudget` | **3.5s** | 1 baseline collector の 1 採取 |
| target | `PerTargetBudget` | **1s** | collector 内で 1 DB target を叩く操作(01 の `Inspect` 1 回) |

背景処理(同期予算の**外側**。run 応答を待たせない):

| 定数 | 既定値 | 適用範囲 |
|---|---|---|
| `DrainBudget` | 10s | 1 run の全 handle の Drain(detached ctx) |
| `DrainCancelGrace` | 1s | ctx.Done() から Drain が return するまでの上限 |
| `SnapshotBuildBudget` | 5s | Collect + immutable snapshot 構築 |
| `EnrichBudget` | 2s | freeze 後の付加取得(09 の EXPLAIN。per-digest 250ms は 09 の値) |
| `AbortJoinBudget` | 2s | AbortRun が worker の完了を待つ上限 |
| `PreemptTotalBudget` | 8s | preempt 経路全体(= `AbortJoinBudget` + `StartRunBudget`) |
| `InitializeGuardBudget` | 30s | `SerializeInitialize` の取得待ち上限(超過 → `ErrInitializeBusy`) |

不等式(CI の定数テスト `TestBudgetHierarchy` で固定する):

```
StartRunBudget(6s)  >  PhaseStartBaselineBudget(5s) > PerCollectorBaselineBudget(3.5s) > PerTargetBudget(1s)
StartRunBudget(6s)  >  PhaseStartBoundaryBudget(500ms) > PerCollectorGenerationBudget(100ms)
PhaseStartBoundaryBudget + PhaseStartBaselineBudget (5.5s) <= StartRunBudget(6s)
PhaseFinishFreezeBudget  + PhaseFinishFinalBudget  (5.5s) <= FinishSyncBudget(6s)
DrainBudget + SnapshotBuildBudget + EnrichBudget (17s) <  FinishLease(20s)
```

### 下流計画への指示(数値の出所を一本化する)

- **04(sqlrows)**: 「per-target 1 秒・全体 3 秒」は
  `PerTargetBudget = 1s` / `PerCollectorBaselineBudget = 3.5s` の**内側**に収まる。
  04 は自前の秒数を書かず本表を引用すること。16 target は
  `BaselineConcurrency = 8` の並列度で 2 波、最悪 2s < 3.5s
- **09(EXPLAIN)**: 「250ms/digest・全体 2s」は `EnrichBudget` の内側。
  **freeze 予算には含めない**(freeze 後の background 付加取得)
- **07(profiles)**: v5 どおり StartRun 予算の外側(境界後の近似観測)。本表に拘束されない。
  ただし 07 の採取は `/reset` / `/finish` / `/save` の**応答時間を延ばす**:
  profile 有効時はそれぞれ `ProfileCaptureLease`(3s)分だけ上限が伸びる
  (07 §採取の実行規約。§HTTP の応答上限を参照)
- **10(wire)**: peer 側は本表の予算をそのまま使うため、hub の deadline は
  **本表 + RTT マージン**でなければならない。v5 の 10 が書いた
  「StartRun バリア per-peer 3s」は `StartRunBudget = 6s` より小さく**成立しない**。
  10 が採るべき下限は次のとおり(**10 v6 §deadline が採用済み**):

  | フェーズ | per-peer 下限 | total 下限 |
  |---|---|---|
  | StartRun バリア | `StartRunBudget` + 2s = **8s** | 12s |
  | FinishRun freeze 受付 | `FinishSyncBudget` + 2s = **8s** | 12s |
  | snapshot polling | `FinishLease` + 5s = **25s** | 40s |
  | AbortRun 伝播 | `AbortJoinBudget` + 2s = **4s** | 8s |

## 境界ウィンドウと許容スプレッド

v5 の「freeze phase = 全セクション共通の**瞬間**境界」は撤回する。
境界は幅を持つ区間であり、幅を**縮め・実測し・上限を定める**。

### 並列採取

- generation collector の `BeginBoundary` / `Freeze` は**逐次**実行する
  (100ms 上限・非ブロッキングであり、順序が世代の整合性に効くため)
- baseline collector の `CaptureBaseline` / `CaptureFinal` は
  **`errgroup` で並列実行**する。`BaselineConcurrency = 8`。
  `SerialOnly = true` の collector のみ、並列群の**前に**逐次実行する
- phase 予算(5s)を使い切った時点で未採取の collector は
  `not-captured` として扱う(required なら invalid、optional なら partial)

### BoundaryWindow

```go
type BoundaryWindow struct {
    Min    time.Time     `json:"min"`
    Max    time.Time     `json:"max"`
    Spread time.Duration `json:"spread"` // Max - Min
}
```

`StartResult` / `FinishAccepted` はそれぞれ 2 つのウィンドウを持つ:

- `GenerationWindow`: generation collector の切替/freeze 時刻のみ
- `BoundaryWindow`: 全 collector(generation + baseline)の実測時刻

### 許容スプレッドと判定

| 定数 | 既定値 | 対象 |
|---|---|---|
| `SpreadLimitGeneration` | **50ms** | `GenerationWindow.Spread` |
| `SpreadLimitBoundary` | **1500ms** | `BoundaryWindow.Spread` |

判定は開始境界・終了境界のそれぞれで独立に行う(決定的・テスト可能):

| 条件 | 結果 |
|---|---|
| `Spread(全体) <= limit` | valid(その要因では変化なし) |
| `Spread(全体) > limit` かつ `Spread(required のみ) <= limit` | **partial** + health `runctl-boundary-spread`(超過 collector 名を列挙) |
| `Spread(required のみ) > limit` | **invalid** + health `runctl-boundary-spread` |

- `Spread(required のみ)` は required collector の実測時刻だけで再計算した値
- スプレッド値は snapshot の meta に必ず載せる(実測を docs に記録する)
- **予算とスプレッド上限は別物**(意図的に非対称):
  予算(per-collector 100ms / 3.5s)は「アプリを止めないための強制打ち切り」、
  スプレッド上限(50ms / 1500ms)は「境界として使えるかの品質閾値」である。
  したがって「予算内だがスプレッド超過」は正常な判定結果であり、
  その run は partial(または required なら invalid)になる。
  期待実測値は generation の切替が 1ms 未満、baseline 並列採取が 200ms 未満。
  これを超える場合は collector 側の実装問題として調査対象にする

## run lifecycle 状態機械(唯一の正 — 10 が一対一で写す)

### 状態と owner

| 状態 | 終端 | owner goroutine | 保持するもの |
|---|---|---|---|
| `idle` | — | なし | 直近 `RetainedRuns = 2` 件の記録 |
| `starting` | — | StartRun 呼び出し元 | `cancel` / `done` |
| `started` | — | prev handle の background Drain worker(完了後は無し) | 旧世代 handle(Drain 中)・baseline handle 群 |
| `finishing` | — | background worker | `cancel` / `done` / freeze 済み handle 群 / lease deadline |
| `finished` | — | なし | immutable snapshot |
| `acknowledged` | ✔ | なし | immutable snapshot(TTL まで) |
| `aborting` | — | join 実行中の AbortRun 呼び出し元 | `cancel` / `done` |
| `aborted` | ✔ | なし | 中止理由・部分開始 collector 一覧(snapshot は**持たない**) |
| `expired` | ✔ | なし | tombstone(runID + 理由のみ) |

図(**正は下の遷移表**。図は概観):

```
idle ─StartRun─▶ starting ─成功─▶ started ─FinishRun─▶ finishing ─worker完了─▶ finished
                                                                                  │
                                                                    Ack ──────────┤
                                                                                  ▼
                                                                            acknowledged
                                                                                  │
   finished / acknowledged ─FinishedTTL─▶ expired ◀────────────────FinishedTTL────┘

   starting(required 失敗) ┐
   started / finishing     ├─ AbortRun / Preempt / FinishLease 失効 / StartedTTL ─▶ aborting ─join─▶ aborted
   finished(明示 Abort)   ┘

   aborted / expired ─TombstoneTTL─▶ 記録破棄
                                     (以後その runID は ErrUnknownRun。
                                      ただし AbortRun だけは no-op 成功)
```

### 遷移表 A: 呼び出し操作(単一ホスト。10 は HTTP status を付けて写す)

`same` = 同一 runID(+同一 nonce)、`other` = 別 runID/別 nonce。

| state \ 操作 | StartRun(same nonce) | StartRun(other, Preempt=false) | StartRun(other, Preempt=true) | FinishRun(same) | FinishRun(other) | AbortRun(same) | Ack(same) | Await(same) |
|---|---|---|---|---|---|---|---|---|
| idle | nonce が履歴に有れば保存済み StartResult、無ければ新規開始 | **新規開始** | 新規開始(abort 対象なし) | `ErrUnknownRun` | `ErrUnknownRun` | no-op 成功 | `ErrUnknownRun` | `ErrUnknownRun` |
| starting | 完了を待って(≤ `StartRunBudget`)同一 StartResult | `ErrRunActive` | **fence → cancel → join → 新規開始** | `ErrRunTransitioning` | `ErrRunActive` | fence → cancel → join → aborted | `ErrRunActive` | starting 完了まで待つ |
| started | 保存済み StartResult | `ErrRunActive` | **fence → cancel → join → 新規開始** | freeze 実行 → FinishAccepted | `ErrRunActive` | fence → cancel → join → aborted | `ErrRunActive` | 即 `RunStatus{started}` |
| finishing | 保存済み StartResult | `ErrRunActive` | **fence → cancel → join → 新規開始** | 保存済み FinishAccepted | `ErrRunActive` | fence → cancel → join → aborted | `ErrRunActive` | finished/aborted まで待つ |
| finished | 保存済み StartResult | `ErrRunActive` | snapshot を**保持したまま** `AckedBy="preempt"` → 新規開始 | 保存済み FinishAccepted | `ErrRunActive` | aborted(snapshot 破棄) | acknowledged へ | 即 `RunStatus{finished}` |
| acknowledged | 保存済み StartResult | **新規開始** | 新規開始 | 保存済み FinishAccepted | `ErrUnknownRun` | no-op 成功 | no-op 成功 | 即 `RunStatus{acknowledged}` |
| aborting | `ErrRunTransitioning` | `ErrRunActive` | join 完了を待って新規開始 | `ErrRunAborted` | `ErrRunActive` | join 完了を待って成功 | `ErrRunAborted` | aborted まで待つ |
| aborted | `ErrRunAborted` | **新規開始** | 新規開始 | `ErrRunAborted` | `ErrUnknownRun` | no-op 成功 | `ErrRunAborted` | 即 `RunStatus{aborted}` |
| expired | `ErrUnknownRun` | **新規開始** | 新規開始 | `ErrUnknownRun` | `ErrUnknownRun` | no-op 成功 | `ErrUnknownRun` | `ErrUnknownRun` |

- **`finished` から `Preempt=true` で新規開始する場合、finished の snapshot は破棄しない**
  (確定済みの計測結果は valid なデータであり、preempt の対象は「進行中の run」だけ)。
  暗黙 Ack として `AckedBy="preempt"` を記録する
- `finished` に対する `AbortRun(same)` は明示的な破棄要求なので snapshot を破棄する
- **未知の runID に対する `AbortRun` は冪等な no-op 成功である**
  (idle 行と同じ扱い)。`TombstoneTTL` 満了や `RetainedRuns` overflow で
  記録が破棄された runID、そもそも Controller が知らない runID のいずれも含む。
  `AbortResult{RunID: runID, AbortedAt: now}` を `err = nil` で返す
  (`Detached=false` / `Partial=nil`)。**v6 監査でここを 10 に合わせて確定した**:
  「破棄済み runID への abort は `ErrUnknownRun`」という読み方は**撤回**する。
  10 の E2E「abort 消失 → 次 run 成功」は、abort の再送が
  いつでも安全に成功することに依存しているため
- 一方 **`FinishRun` / `Status`(GET)/ snapshot 取得 / `Ack` は
  未知 runID に対して `ErrUnknownRun`(→ 404)のまま**である。
  「知らない run を終わらせる/読む」ことは成立しないが、
  「知らない run を止める」ことは既に成立しているからである
  (abort だけが冪等側へ倒れる非対称は意図的)
- `ErrRunActive` は 08 が待つべきエラー。**v5 の 08 が使っていた
  `ErrResetInProgress` は本名称へ置換する**(08 v6 が置換済み)

### 遷移表 B: 内部イベント

| 現状態 | イベント | 遷移 | 付随処理 |
|---|---|---|---|
| starting | 全 collector 成功 | → started | StartResult 確定・background Drain(prev handles)起動 |
| starting | required 失敗 / required スプレッド超過 | → aborting → aborted | 理由 `required-failed`。validity=invalid で**記録は残す**。切替済み世代は Drain 後 Release |
| starting | optional 失敗 | → started | validity=partial・当該セクション除外 |
| started | `StartedTTL`(30m)経過 | → aborting → aborted | 理由 `started-ttl` |
| started | **[外部起因]** 10 の `PeerStartedLease`(45s)満了 | → aborting → aborted | 10 が `runctl.AbortRun(LocalRunID, "hub-abort")` を呼ぶ形で解決する。以降は AbortRun と完全に同一経路(fence → cancel → join)。`AbortResult.Reason = "hub-abort"` |
| finished | **[外部起因]** 10 の `PeerAckLease`(90s)満了による自己 Ack | → acknowledged | 10 が `runctl.Ack(LocalRunID)` を呼ぶ。**`AckedBy = "lease"`**。**snapshot は破棄しない**(`FinishedTTL` まで保持) |
| finishing | worker 正常完了 | → finished | snapshot を `publish(epoch, snap)` で公開 |
| finishing | worker が `FinishLease`(20s)を超過 | → aborting → aborted | 理由 `finish-lease-expired`。watchdog が 1s 周期で検査 |
| finishing | Collect が required で失敗 | → finished | validity=invalid(snapshot は保存する。他セクションは有用) |
| finishing | Drain が `DrainBudget` 超過 | → finished | validity=partial・当該セクションに `drain-timeout` 記録 |
| aborting | join 成功 | → aborted | handle を Release |
| aborting | join が `AbortJoinBudget`(2s)超過 | → aborted(`Detached=true`) | handle 解放は worker の defer に委譲。health `runctl-worker-detached`。reaper が最大 60s まで `done` を監視 |
| finished | `FinishedTTL`(10m)経過 | → expired | snapshot 解放 |
| acknowledged | `FinishedTTL`(10m)経過 | → expired | snapshot 解放 |
| aborted / expired | `TombstoneTTL`(10m)経過、または `RetainedRuns` overflow による即時 evict | 記録破棄 | 以後その runID は `ErrUnknownRun`(FinishRun / GET / snapshot / Ack)。**`AbortRun` だけは冪等な no-op 成功**(遷移表 A の注記) |

**[外部起因] 行について**: この 2 行は **02 の状態機械が定義する遷移**であり、
それを駆動するタイマ(`PeerStartedLease` / `PeerAckLease`)を所有するのは 10 である。
10 はこの 2 行を**写す**のであって、独自の遷移を発明しない。単一ホスト構成では
どちらも発火しない(タイマが存在しないため)。逆に言えば、10 が lease 満了時に
行ってよいのは **02 が既に公開している `AbortRun` / `Ack` の呼び出しだけ**であり、
02 の slot を直接書き換えることは許されない(epoch fencing の外側になるため)。

### lease / TTL 一覧

| 名前 | 値 | 対象 |
|---|---|---|
| `FinishLease` | 20s | finishing worker の生存期限(超過 → 強制 abort) |
| `StartedTTL` | 30m | 誰も Finish しない run の自動回収 |
| `FinishedTTL` | 10m | finished / acknowledged snapshot の保持 |
| `TombstoneTTL` | 10m | aborted / expired 記録の保持 |
| `NonceTTL` | 10m | nonce → StartResult 冪等キャッシュ(`NonceHistoryMax = 64` 件) |
| `RetainedRuns` | 2 | 同時に保持する run 記録の数(**進行中の run を含む**。10 と同一) |

**`RetainedRuns` の破棄規則(本ファイルが所有。10 はこれを引用し、
独自の破棄規則を定義しない)**:

- `RetainedRuns = 2` は **進行中(active)の run を含めた**件数である。
  したがって run が 1 本走っている間に保持できる過去の記録は 1 件だけ
- 新しい run の記録を追加して 2 件を超える場合、
  **最古の非 active record を `TombstoneTTL` の満了を待たず即座に破棄する**。
  `TombstoneTTL`(10m)は「誰も溢れさせなかった場合の上限」であって、
  overflow 時の破棄を遅らせる根拠にはならない
- active(`starting` / `started` / `finishing` / `aborting`)な record は
  **決して evict 対象にしない**。非 active な record が 1 件も無い場合は
  新規 StartRun 側が遷移表 A に従う(`ErrRunActive` / preempt)
- 破棄された runID は以後 `ErrUnknownRun`(→ 404)になる。
  ただし `AbortRun` だけは冪等な no-op 成功である(遷移表 A の注記)
- 帰結として **2 世代前の run は必ず 404** になる。
  abort / start を高速反復しても保持件数は 2 件を超えない

### epoch fencing と worker の cancel / join(CRITICAL 対応の中核)

Controller は run ごとに次の slot を持つ:

```go
type runSlot struct {
    runID    string
    epoch    Epoch
    state    RunState
    validity Validity
    cancel   context.CancelFunc // owner goroutine の detached ctx を切る
    done     chan struct{}      // owner goroutine の defer で close
    lease    time.Time          // finishing のみ
    gen      []GenerationHandle
    base     []BaselineHandle
    detached bool
}
```

**worker 側の唯一の状態変更経路**(直接 slot を触らせない):

```go
// 現行 epoch と一致する場合のみ mutate を実行する。不一致は ErrStaleEpoch。
func (c *Controller) commit(ep Epoch, mutate func(*runSlot)) error

// snapshot の公開も同じ fence を通す。stale worker は保存できない。
// snap は run の immutable snapshot(web.Snapshot 相当。10 では LocalSnapshot)。
func (c *Controller) publish(ep Epoch, snap *Snapshot) error
```

**AbortRun の順序**(この順序が仕様):

1. `c.mu.Lock()` — state を `aborting` にし、**epoch を +1 して fence を張る**。
   `cancel` と `done` をローカルへ退避して `c.mu.Unlock()`
   (**mutex を保持したまま collector や worker を待たない** — 目標 2 のデッドロック回避)
2. `cancel()` を呼ぶ(worker の Drain / Collect / 構築を打ち切る)
3. `select { case <-done: case <-time.After(AbortJoinBudget): detached = true }`
4. joined なら Controller が `Release` を全 handle に対して呼ぶ。
   detached なら worker の `defer` が Release する(二重 Release は no-op 契約)
5. state を `aborted` にして `idle` 相当(= 次の StartRun 受理可能)へ戻す

**なぜ join timeout でも安全か**: epoch の +1 は手順 1 の mutex 内で完了しており、
detached worker の `commit` / `publish` は必ず `ErrStaleEpoch` で拒否される。
また `Drain` / `Collect` は **handle が指す旧世代しか触らない**契約なので、
detached worker が次の run のデータを汚染することもない。
join timeout は「資源解放が遅れる」以上の意味を持たない。

**必須テスト(race detector 下)**:

- `TestAbortDuringFinishing_NoSnapshotPublished`
  (`internal/runctl/abort_race_test.go`、`go test -race -count=200`)。
  Drain が channel でブロックする fake collector を使い、
  FinishRun 直後に AbortRun を並行実行する。検証:
  (a) store に当該 runID の snapshot が**一度も**現れない、
  (b) state が `finished` に戻らない、
  (c) `publish` が `ErrStaleEpoch` を返した回数 ≧ 1(fence が実際に働いた証跡)
- `TestStaleWorkerCommitRejected`: detached worker が
  `commit` / `publish` を呼んでも state と store が不変であること
- `TestAbortJoinTimeout_DetachedRecorded`: join を意図的に超過させ、
  `AbortResult.Detached == true` と health `runctl-worker-detached` を検証。
  その直後の `StartRun` が**成功**すること

## phase × collector 種別 × required の結果表

`Committed` は §Committed セマンティクスの状態述語。
「run 状態」列は **`Validity`**(lifecycle の `RunState` ではない)。
開始側の required 失敗はこれに加えて `RunState` が `aborted` になる(遷移表 B)。
「切替済み世代の扱い」は**そのフェーズで既に切替/固定が済んでいる collector**への影響。

| # | phase | 種別 | required | 失敗の形 | run 状態 | 継続 | 切替済み世代の扱い |
|---|---|---|---|---|---|---|---|
| 1 | start-boundary | generation | required | err, Committed=false | **invalid** | 残り collector も**切替まで完了**させる | 全 handle を Drain 後 Release(seal・snapshot 化しない) |
| 2 | start-boundary | generation | required | err, Committed=true | **invalid** | 同上 | 同上(当該 handle も Drain → Release) |
| 3 | start-boundary | generation | optional | err, Committed=false | **partial** | 続行 | 当該 collector は**旧世代のまま**。セクション除外 + `dropped` 記録 |
| 4 | start-boundary | generation | optional | err, Committed=true | **partial** | 続行 | handle は通常どおり Drain。セクション除外 + `dropped` 記録 |
| 5 | start-baseline | baseline | required | err(Committed 不問) | **invalid** | 残り baseline も採取まで実施 | generation 側の切替は取り消さない(プロセス状態は新世代で統一)。全 handle Release |
| 6 | start-baseline | baseline | optional | err(Committed 不問) | **partial** | 続行 | 当該セクション除外 + `dropped` 記録 |
| 7 | start-baseline | baseline | 両方 | phase 予算(5s)超過で未採取 | required=**invalid** / optional=**partial** | 続行 | 未採取 collector を `not-captured` 記録 |
| 8 | finish-freeze | generation | required | err, Committed=false | **invalid** | 続行(**snapshot は構築・保存する**) | 他 collector の freeze は有効。当該セクションのみ除外 |
| 9 | finish-freeze | generation | required | err, Committed=true | **invalid** | 続行(snapshot 保存) | handle は Drain → Collect 可。データは載せるが run は invalid |
| 10 | finish-freeze | generation | optional | err(Committed 不問) | **partial** | 続行 | Committed=true なら通常 Drain、false ならセクション除外 |
| 11 | finish-final | baseline | required | err(Committed 不問) | **invalid** | 続行(snapshot 保存) | 当該セクションは baseline のみ保持 → 区間値を出さず除外 |
| 12 | finish-final | baseline | optional | err(Committed 不問) | **partial** | 続行 | 同上 |
| 13 | collect | generation | required | `Drain` が `DrainBudget` 超過 | **partial** | 続行 | 部分データを載せ `drain-timeout` を記録(データは存在する) |
| 14 | collect | generation | optional | 同上 | **partial** | 続行 | 同上 |
| 15 | collect | 両種 | required | `Collect` が err | **invalid** | 続行 | 当該セクション除外 + `dropped` |
| 16 | collect | 両種 | optional | `Collect` が err | **partial** | 続行 | 同上 |
| 17 | 開始/終了境界 | 両種 | — | スプレッド超過 | §境界ウィンドウの判定表に従う | 続行 | 変更なし |

- **原則**: 開始側の required 失敗は run を**成立させない**(abort へ)。
  終了側の required 失敗は run を **invalid として保存する**
  (区間の冒頭は既に確定しており、他セクションのデータは調査に使えるため)
- validity は**単調に悪化**するのみ(valid → partial → invalid)。
  一度 invalid になった run が partial に戻ることはない
- テスト: `TestPhaseMatrix`(上表 17 行を table-driven で網羅。
  fake collector に phase・required・err・Committed を注入する)

## API(単一ホスト)

### Go API

```go
type StartRunOptions struct {
    Nonce   string // 空なら Controller が発番
    Preempt bool   // true: 進行中の run を abort してから開始する
    Reason  string // 記録用("api" / "initialize" / "http" / "hub")
    Trigger string // run に記録する reset_trigger
}

type StartResult struct { // immutable
    RunID            string
    Nonce            string
    Epoch            Epoch
    State            RunState
    Validity         Validity
    Collectors       []CollectorBoundary // 名前 / required / At / Committed / err 文字列
    GenerationWindow BoundaryWindow
    BoundaryWindow   BoundaryWindow
    PreemptedRunID   string // preempt した場合のみ
    StartedAt        time.Time
}

type FinishAccepted struct { // immutable
    RunID            string
    Epoch            Epoch
    Validity         Validity
    Collectors       []CollectorBoundary // frozenAt / final SampledAt
    GenerationWindow BoundaryWindow
    BoundaryWindow   BoundaryWindow
    AcceptedAt       time.Time
}

type AbortResult struct { // immutable
    RunID string
    Epoch Epoch
    // Reason は定義済みの安定文字列のみ:
    //   "explicit"(API 明示)/ "required-failed"(開始側 required 失敗)/
    //   "preempted-by:<runID>" / "finish-lease-expired" / "started-ttl" / "hub-abort"(10)
    Reason    string
    Detached  bool // join timeout で worker を切り離した
    AbortedAt time.Time
    Partial   []string // 部分開始していた collector 名(10 の記録用)
}

// 1 collector の 1 境界の実測記録(StartResult / FinishAccepted の要素)。
type CollectorBoundary struct {
    Name      string    `json:"name"`
    Kind      string    `json:"kind"`      // "generation" | "baseline"
    Required  bool      `json:"required"`
    Phase     Phase     `json:"phase"`
    At        time.Time `json:"at"`        // 実測時刻(未実行なら zero)
    Committed bool      `json:"committed"`
    // Code は機械可読な安定コード。空 = 正常。定義済みの値のみを使う:
    //   "not-captured" / "drain-timeout" / "collect-failed" /
    //   "boundary-failed" / "spread-exceeded" / "contract-violation"
    Code    string `json:"code,omitempty"`
    Err     string `json:"err,omitempty"`     // 人間向けの原文
    Dropped bool   `json:"dropped,omitempty"` // snapshot からセクション除外
}

// 状態照会の戻り値。10 の GET /peer/runs/{id} が写す。
type RunStatus struct {
    RunID    string    `json:"run_id"`
    Epoch    Epoch     `json:"epoch"`
    State    RunState  `json:"state"`
    Validity Validity  `json:"validity"`
    Reason   string    `json:"reason,omitempty"`   // aborted/expired の理由
    // AckedBy は定義済みの安定文字列のみ(値集合は下表 — 本ファイルが所有):
    //   "explicit" | "save" | "preempt" | "hub" | "lease"
    AckedBy  string    `json:"acked_by,omitempty"`
    Detached bool      `json:"detached,omitempty"` // abort join timeout
    Since    time.Time `json:"since"`
}

func (c *Controller) StartRun(ctx context.Context, o StartRunOptions) (StartResult, error)
func (c *Controller) FinishRun(ctx context.Context, runID string) (FinishAccepted, error)
func (c *Controller) AbortRun(ctx context.Context, runID, reason string) (AbortResult, error)
func (c *Controller) Ack(runID string) error
func (c *Controller) Status(runID string) (RunStatus, bool)
func (c *Controller) Await(ctx context.Context, runID string) (RunStatus, error) // finishing → finished を待つ

// sentinel error(遷移表 A / 結果表と一対一)。10 は HTTP status へ写す。
var (
    ErrRunActive        = errors.New("runctl: another run is active")        // → 409。08 が待つ/preempt するエラー
    ErrRunTransitioning = errors.New("runctl: run is transitioning")         // → 409(starting/aborting 中)
    ErrRunAborted       = errors.New("runctl: run was aborted")              // → 410
    ErrUnknownRun       = errors.New("runctl: unknown run")                  // → 404
    ErrStaleEpoch       = errors.New("runctl: stale epoch")                  // fencing による拒否(内部)
    ErrBudgetInversion  = errors.New("runctl: child budget >= parent budget")// 登録時の設定エラー
    ErrInitializeBusy   = errors.New("isutools: initialize guard busy")      // SerializeInitialize
)
```

**`AckedBy` の値集合(本ファイルが所有。10 は同名フィールド・同 JSON tag
`acked_by` で 1:1 に写し、独自の値を追加しない)**:

| 値 | 意味 | 発生元 |
|---|---|---|
| `explicit` | `Ack(runID)` の明示呼び出し | 単一ホスト API |
| `save` | `POST /save` が `Await` 後に暗黙 Ack した | 単一ホスト HTTP |
| `preempt` | `finished` の run を `Preempt=true` の StartRun が暗黙 Ack した(snapshot は保持。遷移表 A) | 単一ホスト / 10 |
| `hub` | **10 の hub による ACK**(hub が peer へ ack 要求を送り、peer が自 run を `acknowledged` にする) | 10 のみ |
| `lease` | **10 の `PeerAckLease`(90s)満了による自己 ACK**(hub が消えても `finished` が系を塞がないための解錠。snapshot は破棄しない) | 10 のみ |

- `hub` / `lease` は単一ホスト構成では出現しないが、**フィールドの値集合は
  本ファイルが定義する**(10 は同じフィールドを 1:1 で写すため、
  10 側で値を発明させない)。`lease` を書き込む遷移は遷移表 B の
  `finished → acknowledged`(外部起因)である

**10 への写像**: `RunState` はそのまま wire の state になる。
`expired` および記録破棄後の未知 runID は **404**、`aborted` は **410**、
`ErrRunActive` / `ErrRunTransitioning` は **409** に対応する
(10 の遷移表と同じ)。

**health キー(本計画が追加するもの。全体で 4 つに固定)**:
`runctl-boundary-spread`(スプレッド超過)/
`runctl-contract-violation`(`err=nil` かつ `Committed=false` 等)/
`runctl-worker-detached`(abort join timeout)/
`runctl-lease-expired`(`FinishLease` 失効による強制 abort)。

package `isutools` の公開ラッパ:

```go
// ResetNow は Preempt=true 既定(08 の「最後の initialize が勝つ」を実現)。
func ResetNow(ctx context.Context) (runctl.StartResult, error)
func ResetNowWithNonce(ctx context.Context, nonce string) (runctl.StartResult, error)
// ResetNowOpts は ctx に SerializeInitialize のガードマーカーが載っているかを検査し、
// Reason=="initialize" かつマーカー無しなら health `initialize-unserialized` を記録する
// (キーの所有は 08。§preempt の「ctx マーカーの規約」)。
func ResetNowOpts(ctx context.Context, o runctl.StartRunOptions) (runctl.StartResult, error)

// SerializeInitialize: initialize 本体全体を直列化するプロセス内ガード(§preempt)。
// fn へ渡す ctx へ unexported な initializeGuardKey{} マーカーを必ず載せる。
func SerializeInitialize(ctx context.Context, fn func(context.Context) error) error
```

- `ctx` はキャンセル伝播に使わない。Controller は内部で
  `context.WithoutCancel(ctx)` + 予算表の timeout を張る
  (クライアント切断で境界処理が中途停止しないため。v4 から維持)

### HTTP エンドポイント(**互換性保証**)

| ルート | v6 の意味 | 終端か | 互換性 |
|---|---|---|---|
| `POST /reset` | StartRun(full barrier: 応答前に**前 run の** background Drain 完了も待つ) | — | v4 から不変。204 + `X-Isutools-Run-Id`、409 / nonce 冪等 |
| `POST /collect` | **buffered accesslog の非終端 flush(据え置き)** | **いいえ** | **現行 README のエンドポイント表どおり**。run を終了させない・世代を進めない・snapshot を作らない |
| `POST /finish` | **新設**。FinishRun のみ(freeze 受付)。202 + `FinishAccepted` JSON | はい | 新規追加(additive)。既存スクリプトに影響なし |
| `POST /save?score=N` | FinishRun(冪等)→ `Await` → 永続化 → `Ack` | はい | 外形は v1.0 と同じ(`/save` で結果が確定・保存される) |
| `POST /abort` | AbortRun(冪等)。204 | はい | 新規追加(additive)。10 の `POST /peer/runs/{id}/abort` の単一ホスト版 |

**`/reset` の応答上限**: `StartRunBudget`(6s)+ 前 run の `DrainBudget`(10s)
= **16s**。ただし **profile(07)有効時は `ProfileCaptureLease`(3s)分だけ延びる**
(07 §採取の実行規約。07 の open 採取は `/reset` の barrier 後・204 応答直前に
同期で入るため。したがって profile 有効時の上限は 16s + 3s = **19s**)。
`/finish` / `/save` も同じく `ProfileCaptureLease` 分だけ延びる。
この Drain 待ちは `StartRunBudget` の**外側**であり、
`ResetNow`(計測 handler の内側から呼ばれる)は**この待ちを行わない**
(目標 2 のデッドロック回避。`/reset` はベンチスクリプトから呼ばれるため待ってよい)。

**互換性保証(明文)**:

1. `reset → bench → collect → save` という現行の運用手順は **v6 でもそのまま動く**。
   `/collect` の request/response・status code・意味は変更しない
2. `/collect` は run 状態を変えない。`started` 中は当該 run の accesslog を flush し、
   run が無い(idle)場合も従来どおり単独で flush して 204 を返す。
   `finishing` / `finished` に対しては **409**(既に境界が固定されているため flush 内容を
   区間に入れられない)を返し、`Retry-After` を付ける
3. run を終了させるのは `POST /save` と `POST /finish` だけである
4. `web` の既存 `beginOperation` 排他(409「another reset, collect, or save is already
   running」)は維持し、Controller の状態機械と**二重に**は数えない
   (Controller が権威、`beginOperation` は同時実行の入り口制御)
5. 回帰テスト `TestCollectIsNonTerminal`: `/collect` 呼び出し後も
   `Status(runID).State == StateStarted` であり、世代番号が増えないこと

## preempt と初期化の直列化(08 の CRITICAL 対応・契約側)

### なぜ preempt が要るか

v5 の状態機械では StartRun 後に `started` になり、Finish か Abort なしに
idle へ戻らない。したがって 08 の「進行中の遷移完了を待ってから再 reset する」は
**待っても idle にならない**ため実現不能だった。v6 は後発呼び出しが
**決定的に先取りする**手段を与える。

```go
start, err := ctrl.StartRun(ctx, runctl.StartRunOptions{
    Preempt: true, Reason: "initialize", Trigger: "api",
})
```

- 動作: 遷移表 A の `Preempt=true` 列。active run(`starting` / `started` /
  `finishing`)を **fence → cancel → join(≤ `AbortJoinBudget`)→ handle 破棄**
  してから新 run を開始する。全体上限は `PreemptTotalBudget = 8s`
- **先取りされた run は `aborted` + `ValidityInvalid` として記録**する。
  `AbortResult.Reason = "preempted-by:" + newRunID`、
  新 run の `StartResult.PreemptedRunID` に旧 runID を記録する。
  先取りされた run の snapshot は**作られず・保存されず・一覧に出ない**
  (「黙って再利用」は起こり得ない)
- `Preempt=false` で active run に当たった場合は `ErrRunActive` を返す。
  待ち行列は作らない(v3 で廃止した方針を維持)
- テスト: `TestStartRunPreempt_AbortsActiveRunAndRecordsInvalid`、
  `TestStartRunPreempt_UnderRace`(`-race`。preempt と finishing worker の並行)

### ResetNow だけでは初期化の汚染を防げない(明文)

`ResetNow`(= StartRun)は**境界を張る瞬間しか直列化しない**。
initialize A が境界を張った後に initialize B が DB 再構築を続ければ、
**A の run は B の再構築負荷で汚染される**。preempt はこの汚染を
「A を invalid にする」形で可視化するが、**A が汚染される事実そのものは防げない**。
したがって:

> **アプリケーションは initialize handler の本体全体(DB 再構築 + ResetNow)を
> 直列化しなければならない。** 本計画はそのためのガードを提供する。

```go
// プロセス内の initialize 直列化ガード。
// - 同時に 1 本だけ fn を実行する
// - 取得待ちは InitializeGuardBudget(既定 30s)で打ち切り ErrInitializeBusy
// - 取得順序は保証しない。「最後に完了した initialize が最終的な run を持つ」
//   ことは、fn の末尾で ResetNow(Preempt=true)を呼ぶことで保証される
// - fn へ渡す ctx には **ガード内であることを示すマーカー**を必ず載せる(下記)
func SerializeInitialize(ctx context.Context, fn func(context.Context) error) error

// ガードマーカー(package isutools の unexported key)。
// SerializeInitialize が fn へ渡す ctx に載せ、ResetNowOpts が検査する。
type initializeGuardKey struct{}
```

**ctx マーカーの規約(08 が依存する — 本ファイルが実装を所有する)**:

- `SerializeInitialize` は `fn` へ渡す ctx へ
  `context.WithValue(ctx, initializeGuardKey{}, struct{}{})` を必ず載せる。
  key は package `isutools` の **unexported 型**であり、`runctl` の API は変更しない
- `ResetNowOpts` は `ctx.Value(initializeGuardKey{})` を検査し、
  **`Reason == "initialize"` の StartRun がマーカー無しで呼ばれた場合に
  health `initialize-unserialized` を記録する**(= ガードの外側から
  initialize 由来の reset が来たことの検知)。記録しても StartRun 自体は続行する
- health キー `initialize-unserialized` の**所有は 08**であり、
  02 の `runctl-*` 4 キーとは別枠(本ファイルは検知機構だけを提供する)
- 判定は ctx 経由のみで行う。**goroutine-local は使わない**
- 08 の `TestInitializeWithoutGuard_HealthRecorded`
  (`SerializeInitialize` を使わずに `ResetNow` を呼ぶ → health が立つ)は
  このマーカーの存在に依存する。マーカーを載せない実装は 08 を実装不能にする

規範例(08 の example はこの形へ差し替える):

```go
func initializeHandler(w http.ResponseWriter, r *http.Request) {
    err := isutools.SerializeInitialize(r.Context(), func(ctx context.Context) error {
        if err := rebuildDB(ctx); err != nil { return err }
        start, err := isutools.ResetNow(ctx) // Preempt=true 既定
        if err != nil { return err }
        if start.Validity == runctl.ValidityInvalid {
            return fmt.Errorf("isutools: run %s invalid", start.RunID)
        }
        return nil
    })
    if err != nil { http.Error(w, "initialize failed", 500); return }
    w.WriteHeader(http.StatusOK)
}
```

- テスト: `TestSerializeInitialize_NoOverlap`(2 本同時 → 実行区間が重ならない)、
  `TestSerializeInitialize_Busy`(budget 超過 → `ErrInitializeBusy`)

### 範囲外(明示)

- **複数プロセス / 複数ホストの initialize 直列化は本計画の範囲外**。
  `SerializeInitialize` はプロセス内 mutex であり、別プロセスの DB 再構築は止められない。
  10 の hub が全 participant に対して StartRun バリアを張る形で解決する
  (10 側の要件。本ファイルは単一プロセスの契約のみを保証する)
- 直列化しない運用を選ぶ場合、run が preempt により invalid になり得ることを
  INTEGRATION.md に明記する(黙って汚染された valid run は作らない)

## 実装ステップ(TDD)

1. 状態機械: 遷移表 A / B を table-driven でテスト化 → 実装
   (`TestTransitionTableA` / `TestTransitionTableB`)
2. epoch fencing・`commit`/`publish`・cancel/join・detached reaper
   + `TestAbortDuringFinishing_NoSnapshotPublished`(`-race -count=200`)
3. 予算定数と階層不等式(`TestBudgetHierarchy`)+ 登録時の `ErrBudgetInversion`
4. `GenerationCollector` への httpstats 分割(handle 付き)。
   **先に `httpstats` の `sync.Cond` を per-generation done channel へ置換する**
   (§Drain の実装機構。`httpstats/httpstats.go` の `Reset` 328-346 行 /
   `release` 357-364 行 / `finish` 366-374 行 / `changed` フィールド 136 行・167 行)
   + **Drain conformance test**(ctx cancel で `DrainCancelGrace` 以内に return・
   return 後に旧世代を触る goroutine が残らないことを race detector 下で検証。
   done channel 機構に依存し、`sync.Cond` へ戻ると必ず失敗する)
5. `BaselineCollector`(procstats)の handle 化 + **`BaselineHandle.Sample()` の追加**
   + `Collect(base, final)`
   + `TestBaselineCollect_UsesFrozenSamplesOnly`(Collect 中に I/O を起こさない fake。
   `Collect` が読むのは `base.Sample()` / `final.Sample()` の固定値だけであること、
   および `Sample()` の返り値を変更しても他の handle 保持者へ影響しないことを検証)
6. `Committed` セマンティクスの conformance test
   (`TestBoundaryResult_CommittedOnError` / `nil+false` は契約違反として検出)
7. `TestPhaseMatrix`(結果表 17 行)
8. 並列 baseline 採取 + `BoundaryWindow` 記録 + `TestBoundarySpread_PartialAndInvalid`
9. Preempt 経路 + `SerializeInitialize` + 並行テスト。
   **`SerializeInitialize` が `fn` の ctx へ `initializeGuardKey{}` マーカーを
   載せ、`ResetNowOpts` が `ctx.Value` で検査して health
   `initialize-unserialized` を記録する経路を含む**(§preempt。
   08 の `TestInitializeWithoutGuard_HealthRecorded` が依存)
10. FinishRun freeze phase と「固定値のみから snapshot 構築」の検証
    (freeze 後に故意の負荷を掛け、snapshot に混入しないこと)
11. lease / TTL(`FinishLease` 失効 watchdog・`StartedTTL`・tombstone)
12. web 配線: `/reset`・`/collect`(非終端の回帰固定)・新設 `/finish`・
    `/save`(Finish → Await → 永続化 → Ack)・`/abort`、docs

## テスト計画(受け入れ条件)

- `-race` 必須: 実装ステップ 2 / 4 / 9 のテスト群
- 回帰(v4 から維持): デッドロック回帰(計測 middleware 配下の handler から
  ResetNow)、冒頭欠落回帰(境界直後のリクエストが全量新世代)
- `TestCollectIsNonTerminal`(§HTTP の互換性保証 5)
- `TestFinishLeaseExpiry_AbortsRun`: lease 失効 → aborted → 次 run 成功。
  **`FinishLease` と TTL 群はテストで注入可能にする**(実時間 20s / 30m を待たない)
- `TestStartRunPreempt_*`、`TestSerializeInitialize_*`
- 集計カバレッジ 80%(共通契約 5)

## リスク

| リスク | 対策 |
|---|---|
| 契約が大きくなり移行コスト増 | 互換 shim(旧 `Reset()` を新契約で包む)で collector ごとに段階移行 |
| freeze / baseline phase の予算超過 | 階層予算 + 並列採取。超過 collector は結果表 7 に従い partial/invalid。実測値を docs に記録 |
| Drain の cancellation 対応漏れ | conformance test を全 collector の受け入れ条件に固定(`DrainCancelGrace` 1s)。加えて **`sync.Cond` による待ち合わせを禁止**し per-generation done channel を必須にする(§Drain の実装機構。`sync.Cond.Wait()` は ctx で中断できず契約が実装不能になるため) |
| join timeout による資源解放遅延 | epoch fence により正しさは保たれる。detached を health に出し、reaper が 60s まで監視 |
| preempt の乱用で run が量産される | `PreemptedRunID` と `reset_trigger` を記録し、一覧で判別可能にする |
| 下流計画が独自の予算値を持つ | 予算表を本ファイルに一元化し、10 の deadline 下限まで指定(§予算モデル) |

## 見積もり

**9.5 日**(v5 の 5 日 + 下表の 4.5 日):

| 追加項目 | 増分 |
|---|---|
| epoch fencing + cancel/join + detached reaper + race テスト | +1.0 日 |
| BaselineCollector の handle 化 + `Collect` + immutability conformance | +0.5 日 |
| `Committed` セマンティクス + 結果表 17 行の table-driven テスト | +0.5 日 |
| 階層予算モデル + `TestBudgetHierarchy` + `ErrBudgetInversion` | +0.25 日 |
| 並列 baseline 採取 + `BoundaryWindow` / スプレッド判定 | +0.75 日 |
| Preempt 経路 + `SerializeInitialize` + 並行テスト | +0.75 日 |
| `POST /finish` / `POST /abort` 新設 + `/collect` 非終端の回帰固定 | +0.25 日 |
| lease / TTL / watchdog / tombstone | +0.5 日 |
| **合計増分** | **+4.5 日** |

**v1.2.0 の合計**: `01(2.5)+ 02(9.5)+ 04(3.5) = 15.5 日 → 20 日`
(**README §リリース対応に反映済み**。roll-up の権威は plans/README.md v6 であり、
本ファイルは自身の 9.5 日のみを所有する)。

> **v6 監査で撤回**: v5 のこの節にあった
> `01(2日) + 02(9.5日) + 04(2.5日) = 実装 14 日 → +30% ≈ 18 日` は**誤りである**。
> 01 v6 は 2.5 日、04 v6 は 3.5 日であり、正しくは 15.5 日 → 20 日。
> あわせて「plans/README.md の再算定が必要」という注記も**撤回する** —
> README v6 は既に全 11 計画の §見積もり から全数値を再導出済みである。
