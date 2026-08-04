# 07: runtime プロファイル(mutex / block / heap)— v6

種別: 機能 / 対象リリース: v1.3.0 / 由来: 旧02 から分離 /
変更箇所: `isutools.go`, `web/profiles.go`(新規), `web/pprof.go`, `web/web.go`

**依存(02 v6 の公開契約を引用するだけであり、07 は `runctl` に API を追加しない)**:

- `runctl.StartResult{RunID, Epoch, Validity, GenerationWindow, BoundaryWindow, StartedAt, PreemptedRunID}`
- `runctl.FinishAccepted{RunID, Epoch, Validity, GenerationWindow, BoundaryWindow, AcceptedAt}`
- `runctl.BoundaryWindow{Min, Max, Spread}` / `runctl.Validity`(`ValidityValid` / `ValidityPartial` /
  `ValidityInvalid`)/ `runctl.AbortResult.Reason`
- 02 §FinishRun 手順 6「不変 `FinishAccepted` を**即座に返す**(Drain・snapshot 構築は待たない)」
- 02 §予算モデルの背景処理値(`DrainBudget` 10s / `SnapshotBuildBudget` 5s /
  `EnrichBudget` 2s / `FinishLease` 20s)を**引用のみ**する

07 は 02 の `RegisterGeneration` / `RegisterBaseline` を呼ばない(collector として登録しない)。
したがって 02 の階層予算・`ErrBudgetInversion` の対象外であり、
`StartRunBudget` / `FinishSyncBudget` / phase 予算を一切消費しない。

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[MEDIUM] save 側の採取点を「snapshot 永続化時」から「finish freeze 直後」へ移す
   (option A: 独立した採取点を置く)**。
   v5 の採取点では diff に 02 の post-finish 処理が丸ごと入る。
   混入量は 02 v6 の背景処理予算の合計 `DrainBudget`(10s)+ `SnapshotBuildBudget`(5s)+
   `EnrichBudget`(2s)= **最大 17s** であり、しかもその中身は isutools 自身の
   Drain / Collect / EXPLAIN / snapshot 構築 ——
   **mutex / block プロファイルで見たい対象(アプリのロック競合)ではなく、
   計測器自身のロック競合**である。すなわち v5 の diff は「run に post-finish の尾が付く」
   だけでなく「isutools 内部の競合に系統的に偏る」。これは注記で防げる誤読ではないので、
   採取点そのものを動かす(§採取点)
2. **[MEDIUM] 採取点の呼称を `_reset` / `_save` → `_open` / `_close` に改名**。
   採取点が変わったのにファイル名が同じだと、v5 で採った artifact と v6 の artifact が
   同じ名前で並び、diff の意味が混ざる。改名により**古い名前のファイルは
   v6 の pair 組み立て対象から外れる**(§ファイル名)
3. **[MEDIUM] 残差(近似であること)を実測値としてメタと UI に出す**。
   option A でも誤差はゼロにならない(02 v6 の境界は
   `BoundaryWindow` という**幅を持つ区間**であり、瞬間ではない)。
   `ProfileCapture` / `ProfilePair` に `HeadLossNs` / `TailExcessNs` / `ApproxErrorNs` を
   freeze 点(`GenerationWindow.Max`)基準で記録し、Runs 詳細「Profiles」小節に
   **確定文言**で表示する(§メタデータ、§UI 文言)
4. **[MEDIUM] 採取は呼び出し側 goroutine で同期実行し、採取瞬間を実測記録する**。
   非同期にすると採取瞬間が scheduler 依存になり `TailExcessNs` が信用できない。
   `runtime/pprof` の `WriteTo` は ctx を取らず**中断できない**ため、
   予算は「種別の間のゲート」として実装する(§採取の実行規約)
5. **[MEDIUM] 採取の冪等性を `(RunID, Epoch, Point)` キーで定義**。
   `/finish` → `/save` の 2 段呼び出し(02 v6 で `/save` の FinishRun は冪等)でも
   close 採取は 1 回だけ行う。2 回目は既存 artifact を再利用する(§冪等性)
6. **[MEDIUM] abort / preempt 時の pair を定義**。close 採取は行わず、
   open 側 artifact を `orphan` として記録し retention で最優先削除する(§abort / preempt)
7. **[MINOR] 「ダッシュボードの files 一覧に載る」を撤回**。
   `listFiles()` は `.html` のみ列挙する(`web/web.go:514`)。
   profile artifact の発見経路は snapshot の manifest と Runs 詳細のみである(§UI 文言)
8. **[MINOR] 見積もりを 2 日 → 3 日へ再算定**(§見積もり)

## v5 から撤回する主張

| v5 の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| 「取得点: save 時(snapshot 永続化と同じ場所)」 | diff に post-finish の最大 17s(Drain + Collect + EXPLAIN + snapshot 構築)が入り、`-diff_base` が run に対応しない。かつ isutools 内部の競合へ系統的に偏る | close 採取点 = `FinishRun` 返却直後・background Drain worker と並行(§採取点) |
| 「取得点: reset 完了直後(**02 coordinator の post-reset hook**)」 | 02 v6 に hook API は存在せず、追加すれば 02 の境界順序を拘束することになる | 02 の外側(呼び出し側)で配線する。`/reset` handler と `isutools.ResetNow` の返却直後(§採取点) |
| ファイル名の `_{reset,save}` サフィックス | 採取点が移動したのに同名では v5 artifact と混ざる | `_{open,close}`(§ファイル名) |
| 「取得ごとに実測 capture window(開始/終了時刻)を artifact メタに保存する」 | 採取自体の所要時間しか分からず、「境界からどれだけ遅れたか」が分からない | freeze 点基準の `LagFromRefNs` / `HeadLossNs` / `TailExcessNs` / `ApproxErrorNs`(§メタデータ) |
| 「ダッシュボード: files 一覧に載る」 | `listFiles()` は `.html` だけを列挙する(`web/web.go:514`)ため事実に反する | manifest + Runs 詳細「Profiles」小節が唯一の発見経路(§UI 文言) |

## 旧計画(旧02)からの変更点

レビュー指摘を反映:

1. **rate の意味論を修正**。
   - `SetBlockProfileRate(r)`: 「r ns 以上のブロックだけ記録」**ではなく**、
     累積 block 時間 r ns あたり平均 1 イベントをサンプルする指定
     (pkg.go.dev/runtime#SetBlockProfileRate)。r=1 で全記録
   - `SetMutexProfileFraction(n)`: 競合イベントの平均 1/n を記録
   - README・INTEGRATION.md の説明文をこの定義で書く
2. **累積性の明示**。mutex/block/heap のサンプルは**プロセス開始からの
   累積**であり、1 回取るだけでは run 単位のプロファイルにならない。対応:
   - **run の両端 2 点(open / close)で取得**し、両方を artifact として保存
     (v6 で採取点を再定義。§採取点が正)
   - run 範囲の観察は `go tool pprof -diff_base <open> <close>` で行う
     (INTEGRATION.md に具体コマンドを記載)
   - 各 artifact のメタ(採取時刻・run_id・epoch・freeze 点からの遅れ・
     累積範囲がプロセス開始からであること)をファイル名・sidecar・
     snapshot の manifest で明示(§メタデータが正)
3. **heap の扱いを訂正**。「GC 済みの点観測」とするための明示的
   `runtime.GC()` は**呼ばない**(アプリへの影響がある)。
   heap profile は「直近 GC 時点の live heap + 累積 alloc」であることを
   注記して提供する
4. **拡張子を `.pprof` に統一**。現行 `/files/` handler は
   `.html` / `.json` / `.pprof` のみ配信する(`web/web.go:618-624` で確認済み)。
   旧計画の `.pb.gz` では配信に載らない。
   命名は既存 CPU profile(`<ts>_gen<N>_cpu.pprof`)に合わせる
   (v6 のサフィックスは §ファイル名が正)
5. **PR 分割**(レビュー要求): (a) 有効化 + health、(b) artifact 保存。
   v6 では (b) をさらに b-1(採取点の配線)/ b-2(保存)/ b-3(pair と manifest)/
   b-4(UI)へ細分する(§実装ステップが正)

## ゴール

1. opt-in で mutex/block/heap プロファイルを有効化できる
   (**3 種とも既定 off** — 既定構成のオーバーヘッドはゼロのまま)
2. 有効時、run の両端(open / close)で mutex・block・heap を DataDir に
   atomic に保存し、`/files/` から取得できる
3. run 範囲の見方(diff_base)がドキュメント化されている
4. **diff pair が run 区間を可能な限り狭く挟み、残る誤差が実測値として
   メタと UI に出る**(v6 追加。「近似である」という注記だけで済ませない)
5. 02 の run lifecycle に**一切干渉しない**(02 の phase 順序を拘束せず、
   02 の同期予算を消費しない)

## 設計

### PR-a: 有効化

- `ISUTOOLS_MUTEX_FRACTION`(既定 0 = off。README 推奨値 100 と
  「1/100 サンプリング」の正しい説明)
- `ISUTOOLS_BLOCK_RATE_NS`(既定 0 = off。README 推奨値と
  「累積 block 時間 r ns あたり 1 サンプル」の正しい説明、
  低い値ほど高コストである旨)
- 設定箇所は **`isutools.go` の singleton 初期化経路**(singleton
  `runctl.Controller` を生成するのと同じ `sync.Once` の中)で
  `applyProfileEnv()` を 1 回だけ呼ぶ
  (v4 修正: `startAdmin()` では admin 無効時に設定されず、
  singleton Controller 方針と矛盾していた。**07 は 02 側に初期化 hook を追加しない** —
  呼ぶのは `isutools` package 側である)。設定値は health(`profile`)に
  記録し snapshot の meta.health で確認できる
- **env 未設定時はアプリ自身が設定した既存の profile rate を
  上書きしない**(v4 追加の契約): env が空なら
  SetMutexProfileFraction / SetBlockProfileRate を一切呼ばない

### PR-b: artifact 保存

#### 採取点(v6 の中核 — option A: 独立した採取点)

**方針の選択と根拠**。レビューが示した 2 案のうち **option A(finish freeze 直後の
独立した採取点)を採る**。理由は 3 つ:

1. v5 の採取点(snapshot 永続化時)が取り込む尾は、02 v6 の背景処理予算どおり
   `DrainBudget`(10s)+ `SnapshotBuildBudget`(5s)+ `EnrichBudget`(2s)= **最大 17s**。
   60s の run に対して最大 28% であり、注記で読み替えられる量ではない
2. その 17s の中身は isutools 自身の Drain / Collect / EXPLAIN / snapshot 構築であり、
   **mutex / block プロファイルが拾うのは計測器自身のロック競合**である。
   すなわち v5 の diff は単に長いのではなく、**アプリのロック競合ではない方向へ系統的に偏る**
3. option A は 02 に手を入れずに実装できる。02 v6 §FinishRun 手順 6 が
   `FinishAccepted` を **Drain を待たず即座に返す**ため、
   呼び出し側にはすでに「freeze 済み・Drain 進行中」という採取点が存在する

**02 への非干渉(明文)**。07 は `runctl` の内部に hook を持たない。
採取はすべて `StartRun` / `FinishRun` が**返った後**に呼び出し側で行う。
したがって 02 の phase 順序を拘束せず、`StartRunBudget`(6s)・
`FinishSyncBudget`(6s)・phase 予算を**一切消費しない**
(02 v6 §予算モデル「07(profiles): StartRun 予算の外側(境界後の近似観測)。本表に拘束されない」と一致)。
**v5 の「02 coordinator の post-reset hook」は撤回する**(02 v6 に hook API は無く、発明もしない)。

| 採取点 | 呼称 | 実行場所(呼び出し側) | 02 v6 上の位置 | 実行 |
|---|---|---|---|---|
| open(`/reset` 経由) | `open` | `web` の `/reset` handler。前 run の Drain バリア完了後・204 応答直前 | `StartRun` 返却後。`/reset` の barrier 後 | 同期 |
| open(`ResetNow` 経由) | `open` | `isutools.ResetNow` の返却直後(08 の initialize 本体内) | `StartRun` 返却後。前 run の Drain 完了は待たない | 同期 |
| close | `close` | `web` の `/finish` / `/save` handler。`FinishRun` 返却直後、`Await` の**前** | 手順 6 の直後。手順 7 の background worker と**並行** | 同期 |

- **close が `Await` より前**であることが本 v6 の要点である。`Await` は
  `finishing → finished` を待つ、すなわち Drain + Collect + snapshot 構築の完了を待つ。
  ここで採ると v5 と同じ尾が戻る。実装上は `FinishRun` の戻り値を受けた直後の行で採る
- open 側で `/reset` は barrier 後に採り、`ResetNow` は待たずに採る。
  差は `OpenGate` フィールドに記録する(`"post-prev-drain"` / `"post-start-return"`)。
  `/reset` は前 run の Drain 完了後に採ることで、**前 run の Drain 競合が
  新 run の diff に混入するのを避ける**。この間はベンチ負荷が始まっていないため
  `HeadLossNs` が増えても実害が小さい。`ResetNow` は initialize の
  レイテンシに直結するため待たない
- **区間の意味の明示**(v4 から維持): profile は**プロセス全体の累積**であり
  HTTP 世代と厳密には一致しない。特に `ResetNow`(08)経由の open profile には
  **まだ実行中の initialize handler 後半が混入する**。
  表示・docs に「process-wide cumulative(HTTP 世代 profile ではない)」と明記する

#### 採取の実行規約

- 採取は**呼び出し側 goroutine で同期実行**する。非同期にすると採取瞬間が
  scheduler 依存になり `TailExcessNs` が実測値として信用できなくなるため
- 順序は **mutex → block → heap** に固定する(安いものから採り、
  heap の所要時間が mutex/block の採取瞬間を遅らせないようにする)
- `runtime/pprof` の `WriteTo` は `context.Context` を取らず**中断できない**。
  したがって予算は timeout ではなく **種別と種別の間のゲート**として実装する:
  次の種別を始める前に残り lease を確認し、残りが負なら以降の種別を
  `Code = "capture-lease-exceeded"` として skip する
- `ProfileCaptureLease = 3s`(**02 v6 の数値から導出。独自発明ではない**):
  02 v6 の不等式 `DrainBudget + SnapshotBuildBudget + EnrichBudget (17s) < FinishLease (20s)` が
  持つ余裕がちょうど 3s である。close 採取は background worker と並行して走るため、
  この 3s を超えると `FinishLease` 失効(→ 02 の `runctl-lease-expired` で run が強制 abort)を
  誘発しうる。超過は health `profile-capture-lag` を degrade にして記録する。
  **自動 off にはしない**(観測条件を黙って変えないため。off の判断は運用者が INTEGRATION.md に従う)
- open 採取にも同じ `ProfileCaptureLease = 3s` を適用する。
  根拠は close と異なり、`/reset` の応答上限(02 v6: `StartRunBudget` 6s +
  前 run の `DrainBudget` 10s = 16s)と、08 の initialize レイテンシを
  これ以上伸ばさないためである
- close 採取が呼び出し側を待たせるのは `/finish` / `/save` の**応答**であって
  run の境界ではない。`/reset` と 08 の initialize についても同様に
  「境界が確定した後の応答遅延」であり、02 の同期予算には入らない
- ただし**応答時間そのものは伸びる**ので明記する:
  profile 有効時、`/reset` の応答は 02 v6 が定める上限
  (`StartRunBudget` 6s + 前 run の `DrainBudget` 10s = 16s)に加えて
  最大 `ProfileCaptureLease`(3s)伸び、`/finish` / `/save` も同じだけ伸びる。
  **既定(3 種とも off)では採取が一切走らないので 02 の 16s 上限はそのまま成立する**。
  02 側の数値は変更しない(07 は opt-in の追加コストとして自分の文書と
  INTEGRATION.md / README 実測表に記載する)
- 内容: `rpprof.Lookup("mutex"|"block"|"heap").WriteTo(f, 0)`
  - mutex/block は rate=0(無効)なら**書かない**
  - **heap も `ISUTOOLS_HEAP_PROFILE=1` の明示 opt-in・既定 off**
    (v3 修正。大きな heap の WriteTo は initialize 遅延やメモリ/cache
    状態へ影響し得るため、「既定構成のオーバーヘッドゼロ」と矛盾しない
    ように既定 off にする)
- **atomic publication**(v3 追加): `0600` の一時ファイル
  (`.pprof.tmp` / `.meta.json.tmp` — どちらも `/files/` の配信対象外拡張子)へ書き、
  Close 成功後に `.pprof` / `.meta.json` へ rename する。失敗時は temp を削除。
  未完成ファイルが配信されることを構造的に防ぐ
- 失敗は log + health degrade(fail-open)。DataDir 未設定なら全スキップ
- **reset 所要時間の計測**(v3 追加): profile 無効時と有効時
  (heap on / mutex on / block on)を分けて実測し、ABBA 結果とともに
  README へ記録する(「数 ms」を仮定で書かない)。
  v6 では **close 採取の所要時間(`ProfileCaptureLease` 3s に対する実測)** も
  同じ表に載せる

#### ファイル名

```
<ts>_gen<N>_<runid8>_<kind>_<point>.pprof        # kind = mutex | block | heap
<ts>_gen<N>_<runid8>_<kind>_<point>.meta.json    # sidecar(ProfileCapture 1 件)
point = open | close
```

- **`<ts>` は open / close とも「run の開始時刻」を使う**
  (`StartResult.StartedAt` を既存の `reportTZ` で `20060102-150405` に整形)。
  close 側で採取時刻を使うと open と prefix が食い違い、prefix による pair 照合が壊れる。
  同様に `<N>` は run が計測する世代(開始境界で開いた世代)、
  `<runid8>` は `RunID` の先頭 8 文字で、**両者は open / close で同一**である。
  結果として pair は `<ts>_gen<N>_<runid8>_<kind>_` の一致で決まり、
  差は `_open` / `_close` だけになる
- v5 の `_{reset,save}` は**撤回する**。v5 で採った `_save` artifact は
  採取点が異なる(post-finish の尾を含む)ため、v6 の pair 組み立てでは
  `_close` のみを対象とし、`_save` を含む名前は**無視する**
  (混在ディレクトリでも誤った pair を作らない)
- sidecar が `.meta.json` なのは `/files/` handler が `.json` 接尾辞を配信するため
  (`web/web.go:618-624`)。temp の `.meta.json.tmp` は接尾辞が一致せず配信されない
- open 側 artifact は reset 時点で書かれ、その時点では snapshot が存在しない。
  したがって **sidecar が耐久的な一次記録**であり、snapshot の manifest は
  save 時に同一 runID の sidecar を読んで組み立てる

#### メタデータ(採取瞬間を freeze 点基準で記録する)

```go
// web/profiles.go
type ProfilePoint string
const (
    ProfilePointOpen  ProfilePoint = "open"
    ProfilePointClose ProfilePoint = "close"
)

// 1 ファイル 1 件。sidecar `.meta.json` の内容そのもの。
type ProfileCapture struct {
    RunID     string       `json:"run_id"`
    Epoch     uint64       `json:"epoch"`     // runctl.Epoch
    Point     ProfilePoint `json:"point"`     // "open" | "close"
    Kind      string       `json:"kind"`      // "mutex" | "block" | "heap"
    File      string       `json:"file"`      // rename 済み .pprof 名
    OpenGate  string       `json:"open_gate,omitempty"` // "post-prev-drain" | "post-start-return"

    // ---- 基準点(02 v6 の StartResult / FinishAccepted から転記)----
    RefPhase     string    `json:"ref_phase"`      // runctl.PhaseStartBoundary | runctl.PhaseFinishFreeze
    RefAt        time.Time `json:"ref_at"`         // 対応する GenerationWindow.Max
    RefSpreadNs  int64     `json:"ref_spread_ns"`  // 同 GenerationWindow.Spread
    BoundaryAt   time.Time `json:"boundary_at"`    // 同 BoundaryWindow.Max(baseline 採取まで含む)
    BoundarySpreadNs int64 `json:"boundary_spread_ns"`

    // ---- 実測の採取瞬間 ----
    StartedAt   time.Time `json:"started_at"`     // WriteTo 呼び出し直前
    FinishedAt  time.Time `json:"finished_at"`    // WriteTo 完了直後
    LagFromRefNs int64    `json:"lag_from_ref_ns"`// StartedAt - RefAt(= 基準点からの遅れ)
    DurationNs  int64     `json:"duration_ns"`    // FinishedAt - StartedAt
    Bytes       int64     `json:"bytes"`

    Status string `json:"status"`         // "ok" | "failed" | "skipped"
    Code   string `json:"code,omitempty"` // "capture-lease-exceeded" | "rate-disabled" |
                                          // "no-datadir" | "aborted" | "write-failed"
    Err    string `json:"err,omitempty"`
}

// pair(diff の単位)。open / close が揃ったときだけ作る。
type ProfilePair struct {
    Kind          string `json:"kind"`
    OpenFile      string `json:"open_file"`
    CloseFile     string `json:"close_file"`
    RunSpanNs     int64  `json:"run_span_ns"`     // finishRefAt - startRefAt
    HeadLossNs    int64  `json:"head_loss_ns"`    // openStartedAt - startRefAt(diff に**入らない** run 冒頭)
    TailExcessNs  int64  `json:"tail_excess_ns"`  // closeStartedAt - finishRefAt(diff に**入る** run 外の尾)
    ApproxErrorNs int64  `json:"approx_error_ns"` // HeadLossNs + TailExcessNs
    DiffCommand   string `json:"diff_command"`
}

type ProfileManifest struct {
    RunID    string           `json:"run_id"`
    Epoch    uint64           `json:"epoch"`
    Validity string           `json:"validity"` // runctl.Validity をそのまま転記
    Captures []ProfileCapture `json:"captures"`
    Pairs    []ProfilePair    `json:"pairs"`    // open / close が揃った kind のみ
}
```

- **基準点の規約**: 両端とも `GenerationWindow.Max` を使う。
  02 v6 の generation collector は開始で `BeginBoundary`、終了で `Freeze` を
  逐次実行するので、`.Max` は「最後の世代切替/freeze が終わった瞬間」を指す。
  両端で同じ規約を使うことで `RunSpanNs` が 02 の `GenerationWindow` と
  同一定義になり、`HeadLossNs` / `TailExcessNs` が必ず非負になる
- `RefSpreadNs` / `BoundarySpreadNs` を併記するのは、**基準点自体が幅を持つ**
  (02 v6: `SpreadLimitGeneration = 50ms` / `SpreadLimitBoundary = 1500ms`)ためである。
  `ApproxErrorNs` を読むときは spread を下限誤差として合わせて見る
- `BoundaryAt`(= `BoundaryWindow.Max`)も記録するので、
  `LagFromRefNs` のうち「02 の finish-final phase(baseline 並列採取)分」と
  「07 自身の遅れ」を分離して読める
- manifest は `web.Meta` へ **additive** に追加する:
  `Profiles *ProfileManifest \`json:"profiles,omitempty"\``。
  v1.0 の snapshot JSON を読む既存ツールは影響を受けない
- **health 判定(head と tail を分けて評価する)**:
  - `TailExcessNs > 1s` または `TailExcessNs > RunSpanNs / 100`
    → health `profile-capture-lag` を degrade。close 側の遅れは常に異常である
  - `HeadLossNs > 1s` **かつ** `OpenGate == "post-start-return"` → 同じく degrade
  - `OpenGate == "post-prev-drain"` の `HeadLossNs`(前 run の Drain 完了待ち。
    最大 `DrainBudget` 10s)は**意図的なトレードオフ**なので degrade しない。
    ただし `ApproxErrorNs` と UI 文言には必ず出す(黙って隠さない)
  - open / close のどちらかが欠けた kind は health `profile-pair-incomplete`
  - 採取自体が失敗した capture は health `profile-capture-failed`

#### 冪等性 `(RunID, Epoch, Point)`

- 採取は `(RunID, Epoch, Point)` をキーとして**高々 1 回**行う。
  02 v6 では `/save` の `FinishRun` が冪等(「保存済み `FinishAccepted`」を返す)なので、
  `/finish` → `/save` と 2 段で呼ばれると素朴な実装は close を 2 回採ってしまい、
  2 回目は v5 と同じ「永続化時の採取」になる。これを構造的に禁じる
- 実装: `map[profileKey]*ProfileManifest`(key = runID + epoch)を `web` の handler が持ち、
  当該 point が既に `Status="ok"` なら**既存 artifact 名を返して即 return** する
- 同じ nonce による `StartRun` 再送(02 v6: 保存済み `StartResult` を返す)も同様に
  open を 2 回採らない。epoch が異なれば別 run なので別キーになる
- テスト: `TestCloseCaptureIdempotent_FinishThenSave`

#### abort / preempt / invalid run

- `POST /abort`(02 v6 新設)および `Preempt=true` による先取りでは
  **freeze が起きないので close 採取を行わない**。
  open 側 sidecar に `Status="skipped"` / `Code="aborted"` の close エントリを追記し、
  pair は作らない
- `FinishAccepted.Validity == runctl.ValidityInvalid` の run は pair を作るが、
  `ProfileManifest.Validity = "invalid"` を立て、UI では diff コマンドを
  **折りたたみ**で出す(区間として信用できないため)。
  `ValidityPartial` は通常表示のまま `partial` バッジを付ける
- open だけ存在して close が無い artifact を **orphan** と呼び、
  retention の削除順で最優先にする
- テスト: `TestAbortedRun_NoCloseArtifact`

#### 保持上限(retention)

- profile artifact は**直近 20 run 分・合計 512MiB**を上限とし、
  超過分は古い run から削除する(既存 snapshot html/json は対象外)。
  方針を INTEGRATION.md に明記
- 削除は `.pprof` と対応する `.meta.json` を**組で**行う(sidecar だけが残らない)。
  sidecar のバイト数も 512MiB に算入する
- 削除順: (1) orphan(close 欠落)→ (2) `Validity=invalid` の run →
  (3) 古い run 順

#### UI 文言(Runs 詳細「Profiles」小節)

**v5 の「files 一覧に載る」は撤回する**。`listFiles()` は `.html` のみを列挙する
(`web/web.go:514`)ため、profile artifact は一覧に出ない。
発見経路は **snapshot manifest(`meta.profiles`)と Runs 詳細の「Profiles」小節**だけであり、
そこから `/files/<name>` への直リンクを張る。

小節は pair ごとに次の**確定文言**を出す(N / M は manifest の実測値を ms で描画):

```
run 区間(基準: 02 GenerationWindow.Max)  12:00:00.412 → 12:01:00.088 (59.676s)
mutex   open +41ms (post-start-return) / close +118ms
        欠落 41ms・余剰 118ms(合計 159ms = run 長の 0.27%)

この pair はプロセス累積プロファイルの差分です。
run 冒頭の 41ms を含まず、finish freeze 後の 118ms を含みます。

go tool pprof -diff_base \
  20260805-120000_gen7_a1b2c3d4_mutex_open.pprof \
  20260805-120000_gen7_a1b2c3d4_mutex_close.pprof
```

- 「run 冒頭の N を含まず、finish freeze 後の M を含みます」は
  **常に表示する**(誤差が小さくても省略しない)。option A を採っても
  pair は run の近似であるという事実は変わらないため
- `OpenGate` を括弧内にそのまま出す。`post-prev-drain` の場合は
  N が秒オーダーになり得るため、続けて
  「前 run の Drain 完了を待ってから採取しているため、この欠落区間に
  ベンチ負荷はありません」の 1 行を追加する
- health `profile-capture-lag` が立った pair にだけ行頭へ `⚠ 採取遅延` バッジを付け、
  health エントリへリンクする(条件は §メタデータの health 判定と**同一**。
  UI 側で別の閾値を持たない)
- `Status="skipped"` / `"failed"` の capture は `Code` をそのまま表示する
  (`capture-lease-exceeded` / `aborted` / `rate-disabled` / `no-datadir` / `write-failed`)
- 07 が使う health キーは `profile`(PR-a の設定値記録)・
  `profile-capture-lag`・`profile-capture-failed`・`profile-pair-incomplete` の 4 つに固定する
  (02 v6 の `runctl-*` 4 キーとは名前空間が重ならない)

### capabilities / flag

- capabilities: `runtime-profiles`
- 機能単位 ABBA: `ISUTOOLS_MUTEX_FRACTION=100` 単独 on、
  `ISUTOOLS_BLOCK_RATE_NS=100000` 単独 on をそれぞれ計測して
  READMEに実測値を記録(「block は診断時のみ」の根拠を実測で示す)

## 実装ステップ(TDD)

1. PR-a: env パース(不正値は off + health warn)・設定反映・health
2. PR-b-1: 採取点の配線(`/reset` barrier 後 / `ResetNow` 返却直後 /
   `FinishRun` 返却直後・`Await` の**前**)+ `(RunID, Epoch, Point)` 冪等キー
3. PR-b-2: 保存(rate=0 で mutex/block を書かない・**heap は
   `ISUTOOLS_HEAP_PROFILE=1` のときのみ書く**・mutex → block → heap 順・
   `ProfileCaptureLease` ゲート・ファイル名規約・sidecar `.meta.json`・
   atomic rename)・`/files/` 配信テスト
4. PR-b-3: `ProfileCapture` → `ProfilePair` 組み立て(`HeadLossNs` /
   `TailExcessNs` / `ApproxErrorNs`)・`web.Meta.Profiles` の additive 追加・
   retention(orphan → invalid → 古い順)
5. PR-b-4: Runs 詳細「Profiles」小節の確定文言 + `⚠ 採取遅延` バッジ
6. docs: INTEGRATION.md「ロック競合の診断」節
   (rate 意味論、diff_base 手順、`go tool pprof -http` の見方、
   **pair が run の近似であること・残差の読み方**)

## テスト計画

- unit: env 境界(0 / 負値 / 非数 → off)
- integration: fraction>0 で reset→save 後に mutex の open/close ペアが
  DataDir に存在し `/files/` で 200(sidecar `.meta.json` も 200)
- integration: **既定(全 off)では artifact が一切生成されない**こと
  (heap も off — v3 修正)
- integration: 書き込み途中で失敗させ、`.pprof` が残らず temp が
  削除されること / 保持上限超過で古い run の artifact だけが消えること
  (`.pprof` と `.meta.json` が組で消えること)
- `TestCloseCaptureBeforeDrainCompletes`(**v6 の中核回帰**):
  `Drain` が 2s ブロックする fake generation collector を登録し、
  `/finish` を呼ぶ。検証: close sidecar の `FinishedAt` が
  `Await` の完了時刻より**前**であること、および
  `TailExcessNs < 500ms`(= 02 の背景処理 17s が入っていないこと)
- `TestProfilePairMetadata_RecordsLagFromFreeze`:
  `HeadLossNs >= 0` / `TailExcessNs >= 0` /
  `ApproxErrorNs == HeadLossNs + TailExcessNs` /
  `RunSpanNs == finishRefAt - startRefAt`(基準は両端とも `GenerationWindow.Max`)
- `TestCloseCaptureIdempotent_FinishThenSave`: `/finish` → `/save` で
  close artifact が 1 つだけ生成され、2 回目は既存名を返すこと
- `TestAbortedRun_NoCloseArtifact`: `/abort` 後に close が
  `Status="skipped"` / `Code="aborted"` で pair が作られず、
  open が orphan として retention 最優先で削除されること
- `TestCaptureLeaseExceeded_SkipsRemainingKinds`: mutex 採取に
  `ProfileCaptureLease` 超過を注入し、block/heap が
  `Code="capture-lease-exceeded"` で skip されること
- `TestProfileUIText_ContainsApproximationNotice`: Runs 詳細 HTML に
  「run 冒頭の」「finish freeze 後の」の両文言が含まれること(文言の回帰固定)
- `TestLegacySaveSuffixIgnored`: `_save` を含む v5 世代のファイルが
  DataDir にあっても pair に採用されないこと
- `TestHeadLossGate`: `OpenGate="post-prev-drain"` で `HeadLossNs = 5s` でも
  `profile-capture-lag` が立たず、`OpenGate="post-start-return"` の同値では立つこと

## リスク

| リスク | 対策 |
|---|---|
| block profile の高コスト設定 | 既定 off + 推奨値と実測値を README に併記 |
| プロファイル取得自体の stop-the-world | 取得は**境界後の観測**であり、02 の phase 順序を拘束せず 02 の同期予算も消費しない(v5 から維持)。v6 は採取瞬間を `StartedAt` / `FinishedAt` / `LagFromRefNs` として実測記録し、遅延・失敗を `Status` / `Code` と health `profile-capture-*` に落とす |
| close 採取が background worker と並行し `FinishLease`(20s)を圧迫する | `ProfileCaptureLease = 3s`(02 v6 の `FinishLease` − 背景処理合計 17s の余裕から導出)を種別間ゲートとして適用し、超過時は残りの種別を skip + health degrade。`WriteTo` は中断できないため timeout ではなくゲートで実装する |
| heap の `WriteTo` が initialize(08 の `ResetNow` 経路)を遅らせる | heap は `ISUTOOLS_HEAP_PROFILE=1` の明示 opt-in・既定 off(v3 から維持)。採取順を mutex → block → heap にして安い 2 種の採取瞬間を守る。実測値を README の「reset 所要時間」表に載せる |
| diff の区間が run と一致しない | option A(freeze 直後の独立採取点)で post-finish の尾を最大 17s → 実測 100ms 級へ縮め、残差を `HeadLossNs` / `TailExcessNs` / `ApproxErrorNs` として**常に** UI に表示する(§UI 文言) |
| ファイル数の増加 | 保持上限契約(直近 20 run・合計 512MiB、超過は orphan → invalid → 古い順に自動削除)に従う(v5: 「手動削除」表記を撤回し PR-b の retention 契約に一本化) |

## 見積もり

**3 日**(v5 の 2 日 + 下表の 1 日):

| 項目 | 日数 |
|---|---|
| PR-a: env / 設定反映 / health | 0.5 |
| PR-b-1: 採取点の配線 + 冪等キー(v6 追加) | 0.25 |
| PR-b-2: 保存 + sidecar + lease ゲート | 0.75 |
| PR-b-3: pair 組み立て + manifest + retention(v6 で拡張) | 0.5 |
| PR-b-4: Runs 詳細「Profiles」小節(v6 追加) | 0.25 |
| docs(INTEGRATION.md + README 実測表) | 0.75 |
| **合計** | **3.0** |
