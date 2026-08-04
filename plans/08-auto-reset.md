# 08: 計測開始の自動化 — v6

種別: 機能 / 対象リリース: v1.3.0 / 依存: **02 v6**(run lifecycle coordinator:
`StartRun` / `Preempt` / `SerializeInitialize` / nonce)/ 変更箇所:
`isutools.go`(02 が追加する公開ラッパへの initialize 向け既定値の配線。
ctx マーカーの実装・検査は 02 が所有する)、`httpstats`(observer hook)、
`docs/INTEGRATION.md`、`examples/`

`ResetNow*` / `SerializeInitialize` の**実装本体は 02 が所有**する。
本計画はそれらを**呼ぶ側の契約**(いつ・どの順で・どう失敗を扱うか)と
besteffort モードだけを定義する。

**本ファイルはアプリ側 initialize 統合の唯一の正**である。
状態機械・型名・エラー名・予算値は **02 v6 を引用するだけ**で、
本ファイルでは再定義も改名もしない。

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] 同時 initialize の契約を全面書き換え**。
   v5 の「`ErrResetInProgress` を受けたら進行中の遷移完了を bounded に待ち
   (上限 15s)、自分固有の nonce で再 reset する」は **02 の状態機械上
   実現不能**だった(StartRun 後の状態は `started` で、`FinishRun` か
   `AbortRun` なしに `idle` へ戻らないため、待っても永遠に開始できない)。
   v6 は 02 の **`StartRunOptions{Preempt: true}`**(= `ResetNow` の既定)を
   使い、後発 initialize が active run を**明示的に abort してから**
   新 run を開始する経路に置き換えた(§同時 initialize)
2. **[CRITICAL] 「ResetNow だけでは先行 run の汚染を防げない」問題に契約を与えた**。
   境界の瞬間だけを直列化しても、後発 initialize の **DB 再構築**が
   先行 run の計測区間に載る。v6 は **initialize handler 本体全体**
   (DB 再構築 + `ResetNow`)を 02 の **`isutools.SerializeInitialize`** で
   直列化することをアプリ側の**必須要件**とし、不変条件 I1 として明文化した。
   規範例コード・禁止事項・未直列化時の health キーも定義した(§同時 initialize)
3. **[MEDIUM] 撤回済み API 参照の除去**。`runctl.StateInvalid` /
   `runctl.StatePartial` は 02 v6 で撤回済み。判定は
   **`StartResult.Validity`**(`ValidityValid` / `ValidityPartial` /
   `ValidityInvalid`)で行う(§依存する契約)
4. **[MEDIUM] `ErrResetInProgress` を `runctl.ErrRunActive` へ置換**
   (02 v6 §API の sentinel と一対一。02 の指摘どおり)
5. **[MEDIUM] `DrainPrevious` という API 名を撤回**。02 v6 に該当 API は無く、
   「StartRun 後に background で走る prev handle 群の `Drain`
   (detached ctx・`DrainBudget` 10s)」が正しい表現(§デッドロック)
6. **[MEDIUM] テスト期待値を新契約から導出し直した**。
   v5 の「2 本とも順番に成功し、世代は 2 回進み、最後の nonce の run が有効」は
   **preempt を前提としない旧契約の文**なので撤回し、
   「先行 run は `aborted` + `ValidityInvalid` として残る」まで含む
   期待値に書き換えた(§テスト計画)
7. **[MINOR] 文書ヘッダの版を v3 → v6 に更新**(本文は v4/v5 の追記で
   更新されていたがヘッダが取り残されていた)
8. **[MINOR] 見積もりを 1.5 日 → 2.0 日へ再算定**(§見積もり)
9. **[v6 監査反映] 他ファイルとの相互参照を突き合わせて 3 点を修正した**
   (v6 監査での cross-file 矛盾指摘への対応):
   - **「plans/README.md の再算定が必要」という注記を撤回**した。
     README v6 は既に再算定済みで、08 = **2.0 日**・
     v1.3.0 = raw 9.5 日 と本ファイルの §見積もり が一致している(§見積もり)
   - **ctx マーカーを「02 への additive な要求」として書いていた箇所を撤回**し、
     **02 v6 §preempt が定義・所有する確定済み契約
     (`initializeGuardKey{}`)を消費する**書き方へ改めた。
     マーカーの付与・検査の実装本体は 02、health キー
     `initialize-unserialized` の所有は 08(§同時 initialize)
   - **引用している 02 v6 の API 名を全件再検証**した
     (`StartRunOptions` / `ResetNow` / `ResetNowWithNonce` / `ResetNowOpts` /
     `SerializeInitialize` / `StartResult`(全 10 フィールド)/ `AbortResult.Reason` /
     `ErrRunActive` / `ErrInitializeBusy` / `Preempt` / `PreemptedRunID` /
     `Validity`・`ValidityValid`・`ValidityPartial`・`ValidityInvalid` /
     `StateStarted`・`StateAborted` / `PreemptTotalBudget` 8s /
     `AbortJoinBudget` 2s / `StartRunBudget` 6s / `InitializeGuardBudget` 30s /
     `DrainBudget` 10s / `NonceTTL` 10m / `NonceHistoryMax` 64 /
     `FinishLease` 20s / `AckedBy="preempt"`)。**乖離は無く、修正は不要だった**

## v5 から撤回する主張

| v5(本ファイル)の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| 「`ErrResetInProgress` を受けたら進行中の遷移完了を**上限 15s 待って**、自分固有の nonce で再 reset する」 | 02 の状態機械では StartRun 後は `started` のままで、Finish / Abort なしに `idle` へ戻らない。待っても開始できず永久ブロックする | `ResetNow` は `Preempt=true` 既定。**待たずに active run を abort してから**開始する。上限は 02 の `PreemptTotalBudget = 8s`(§同時 initialize) |
| 「`ErrResetInProgress`」というエラー名 | 02 v6 に存在しない | `runctl.ErrRunActive`(02 §API)。`Preempt=true` では通常発生しない |
| 「先行 run は短い完結した世代として残り、上書きされない」 | 後発 initialize の DB 再構築を丸ごと含む区間であり、計測値として信用できない | 先行 run は **`aborted` + `ValidityInvalid`**。snapshot は作られず一覧にも出ない(02 §preempt) |
| 「`start.State == runctl.StateInvalid` / `StatePartial` を見る」 | 02 v6 で lifecycle state と妥当性を分離。当該定数は存在しない | `start.Validity == runctl.ValidityInvalid` / `ValidityPartial` |
| 「`ResetNow` = StartRun + **`DrainPrevious`**(非同期)」 | `DrainPrevious` という API は 02 に存在しない | 「StartRun + background の prev handle `Drain`(`DrainBudget` 10s)」 |
| 「`ResetNow` を呼べば同時 initialize による汚染を防げる」 | `ResetNow` は**境界を張る瞬間しか直列化しない**。後発の DB 再構築は先行 run の区間に載る | `SerializeInitialize` で **handler 本体全体**を直列化する。汚染された run は preempt で invalid として捨てられる(不変条件 I1) |
| テスト期待「2 本とも順番に成功し、世代は 2 回進み、最後の nonce の run が有効になる」 | 「待って再 reset」前提の文。先行 run の終状態(aborted/invalid)と guard の非重複を検証していない | §テスト計画の `TestConcurrentInitialize_Serialized_LastRunWins` の期待値へ差し替え |
| 「unit: POST /initialize 200 → `Controller.Reset` 呼び出し 1 回」 | `Controller.Reset` は 02 v6 に存在しない | `Controller.StartRun` 呼び出し 1 回(`Preempt=true`) |

## 依存する 02 v6 の契約(引用のみ・本ファイルで再定義しない)

| 種別 | 名前 | 本計画での使い方 |
|---|---|---|
| 関数 | `isutools.ResetNow(ctx) (runctl.StartResult, error)` | initialize 末尾の同期境界。**`Preempt=true` 既定** |
| 関数 | `isutools.ResetNowWithNonce(ctx, nonce)` | ベンチ側が nonce を制御する場合 |
| 関数 | `isutools.ResetNowOpts(ctx, runctl.StartRunOptions)` | besteffort モードが `Trigger` を変えて呼ぶ |
| 関数 | `isutools.SerializeInitialize(ctx, fn func(context.Context) error) error` | **initialize handler 本体全体**の直列化ガード |
| 型 | `runctl.StartRunOptions{Nonce, Preempt, Reason, Trigger}` | `Reason: "initialize"` を必ず設定 |
| 型 | `runctl.StartResult{RunID, Nonce, Epoch, State, Validity, Collectors, GenerationWindow, BoundaryWindow, PreemptedRunID, StartedAt}` | 呼び出し側は **`Validity` を必ず検査**(02 §戻り値の規約) |
| 定数 | `runctl.ValidityValid` / `ValidityPartial` / `ValidityInvalid` | 分岐条件 |
| 定数 | `runctl.StateStarted` / `StateAborted` | テストの状態検証 |
| error | `runctl.ErrRunActive` | `Preempt=false` を明示した場合のみ発生 |
| error | `runctl.ErrInitializeBusy` | `SerializeInitialize` の取得待ち超過 |
| 予算 | `PreemptTotalBudget = 8s`(= `AbortJoinBudget 2s` + `StartRunBudget 6s`) | `ResetNow` の応答上限 |
| 予算 | `InitializeGuardBudget = 30s` | guard 取得待ちの上限 |
| 予算 | `DrainBudget = 10s` | prev handle の background Drain |
| 記録 | `AbortResult.Reason = "preempted-by:<runID>"` / `StartResult.PreemptedRunID` | preempt の追跡 |

**独自の秒数・独自の状態名・独自の error 名を本ファイルで定義しない**
(02 が唯一の権威)。

## 自己デッドロックが起きない理由(v3 の結論を維持・用語のみ v6 化)

v2 は「initialize handler 末尾で ResetNow を同期実行」としたが、当時の
ResetNow が旧世代の in-flight 完了を待つ設計だったため、
**呼び出し元の initialize リクエスト自身が in-flight** で永久に待った
(middleware は handler 開始時に in-flight を増やし終了時に減らす —
`httpstats/httpstats.go:213`, `:328`)。02 v6 の契約で構造的に解消する:

- `ResetNow` = **`StartRun`**。同期部は
  **start-boundary phase**(全 generation collector の `BeginBoundary`・
  `PhaseStartBoundaryBudget` 500ms)+
  **start-baseline phase**(全 baseline collector の `CaptureBaseline`・
  `PhaseStartBaselineBudget` 5s)だけで、**旧世代 in-flight の完了待ちを含まない**
- 旧世代 handle の `Drain` は **StartRun 応答後の background**
  (detached ctx・`DrainBudget` 10s)。呼び出し元の initialize リクエストは
  境界前に開始しているため**旧世代に計上**され、
  Drain は initialize handler の終了(= in-flight 減少)後に自然に完了する
- **`ResetNow` は前 run の background Drain 完了を待たない**。
  待つのは HTTP の `POST /reset`(full barrier・上限 16s)だけであり、
  こちらはベンチスクリプトから呼ばれるため待ってよい(02 §HTTP)
- 応答時点で開始境界と baseline の両方が確定済みなので、
  initialize 応答後に始まる本負荷は冒頭から新世代に全量計上される
  (冒頭欠落の修正と整合)

## initialize 統合の規範形(推奨・同期 API)

### 3 層モデル(どの層が何を保証するか)

| 層 | 手段 | 保証すること | 保証**しない**こと |
|---|---|---|---|
| 1 | `isutools.SerializeInitialize` | initialize 本体(DB 再構築 + `ResetNow`)が同時に 2 本走らない | 実行順序(取得順は保証されない) |
| 2 | `ResetNow`(`Preempt=true` 既定) | 最後に完了した initialize の run が現行になる | 先行 run が汚染され**ない**こと |
| 3 | 02 の preempt 記録 | 汚染され得る run は `aborted` + `ValidityInvalid` で残り、snapshot も一覧も作られない | — |

3 層あわせて「**汚染された run が valid のまま残らない**」を保証する。

### 規範例(02 v6 §preempt の規範例と同一形)

```go
func initializeHandler(w http.ResponseWriter, r *http.Request) {
    // 層 1: initialize 本体**全体**を直列化する(DB 再構築を含む)。
    // ResetNow だけを直列化しても、後発の rebuildDB が先行 run を汚染する。
    err := isutools.SerializeInitialize(r.Context(), func(ctx context.Context) error {
        if err := rebuildDB(ctx); err != nil {
            return fmt.Errorf("rebuild db: %w", err)
        }
        // 層 2: fn の**最後**に呼ぶ。Preempt=true 既定で active run を abort してから開始する。
        start, err := isutools.ResetNow(ctx)
        if err != nil {
            return fmt.Errorf("isutools reset: %w", err)
        }
        // 02 §戻り値の規約: collector の失敗は err ではなく Validity に出る。必ず検査する。
        if start.Validity == runctl.ValidityInvalid {
            return fmt.Errorf("isutools: run %s invalid: %v", start.RunID, start.Collectors)
        }
        if start.Validity == runctl.ValidityPartial {
            // 既定ポリシー: required が揃っていれば partial は計測続行可。
            // 厳格運用では error を返して 500 にする(caller policy を必ず明示する)。
            log.Printf("isutools: run %s started partial: %v", start.RunID, start.Collectors)
        }
        return nil
    })
    if err != nil {
        // 任意(推奨): 混雑と失敗を区別する。
        if errors.Is(err, runctl.ErrInitializeBusy) {
            w.Header().Set("Retry-After", "1")
            http.Error(w, "initialize busy", http.StatusServiceUnavailable)
            return
        }
        http.Error(w, "initialize failed", http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusOK) // 境界確定後に応答
}
```

- `ResetNow` は 02 の process-wide Controller を直接呼ぶ
  (HTTP 自己呼び出しは存在しない。admin 無効でも動作する)
- run に `reset_trigger: "api"`、`StartRunOptions.Reason = "initialize"` を記録する

### 禁止事項(docs・examples に明記する)

1. **`ResetNow` の `err` および `ValidityInvalid` を log だけして 200 を
   返してはならない**(v3 の「409 は log して処理継続してよい」は
   v4 で撤回済み。維持。なお `Preempt=true` 既定では
   `runctl.ErrRunActive`(409)自体が通常発生しない)
2. **`fn` の中で `ResetNow` より後に DB を触ってはならない**。
   `ResetNow` は `fn` の**最後の操作**であること。これが不変条件 I1 の前提
3. **`SerializeInitialize` は再入不可**。`fn` の内部から再度
   `SerializeInitialize` を呼ばない(`ResetNow` はガードを取得しないので安全)
4. **`ResetNow` を `w.WriteHeader` の後に呼ばない**(境界前に応答すると
   ベンチの本負荷が旧世代に混入する)

## 同時 initialize の扱い(v6 全面改訂)

### なぜ v5 の「待ってから再 reset」が実現不能だったか(撤回の説明)

02 の状態機械では `StartRun` 成功後の状態は `started` であり、
`FinishRun`(`/save` / `/finish`)か `AbortRun` が来るまで `idle` に戻らない。
ベンチ実行中の run は当然 `started` のまま数分続く。したがって
v5 の「進行中の遷移完了を上限 15s 待ってから自分の nonce で再 reset する」は
**15s 待って必ずタイムアウトする**経路であり、
「2 本とも順番に成功する」という v5 のテスト期待値も達成不能だった。
**v5 の当該記述は撤回する。**

### 後発 initialize は「待つ」のではなく「先取りする」

```go
// ResetNow の既定。明示したい場合はこちら。
start, err := isutools.ResetNowOpts(ctx, runctl.StartRunOptions{
    Preempt: true, Reason: "initialize", Trigger: "api",
})
```

- 02 の遷移表 A の `StartRun(other, Preempt=true)` 列に従い、
  active run(`starting` / `started` / `finishing`)を
  **fence → cancel → join(≤ `AbortJoinBudget` 2s)→ handle 破棄**してから
  新 run を開始する。経路全体の上限は `PreemptTotalBudget = 8s`
- **先取りされた run は `aborted` + `ValidityInvalid`** として記録される。
  `AbortResult.Reason = "preempted-by:<新 runID>"`、
  新 run の `StartResult.PreemptedRunID` に旧 runID が入る。
  先取りされた run の snapshot は**作られず・保存されず・一覧に出ない**
  (v5 の「先行 run は短い完結した世代として残る」= 黙って再利用される、は撤回)
- 直前の run が既に `finished` の場合、その snapshot は**破棄されない**
  (02 遷移表 A の注記。暗黙 Ack として `AckedBy="preempt"` を記録)。
  preempt の対象は「進行中の run」だけである
- `Preempt=false` を明示した呼び出しが active run に当たった場合のみ
  `runctl.ErrRunActive`。待ち行列は作らない

### `ResetNow` だけでは汚染を防げない(レビュー指摘の核心)

`ResetNow` が直列化するのは**境界を張る瞬間だけ**である。
guard 無しで initialize A / B が並走すると:

```
t0  A: rebuildDB 開始
t2  A: rebuildDB 完了
t3  A: ResetNow → run R_A(started)・A は 200 応答 → ベンチが本負荷を開始
t4  B: rebuildDB 開始              ← R_A の計測区間に DB 再構築負荷が載る(汚染)
t9  B: rebuildDB 完了              ← この間ベンチのリクエストは R_A に計上され続ける
t10 B: ResetNow(Preempt=true)→ R_A を abort(invalid)→ R_B(started)
```

preempt は「R_A が汚染された事実」を **invalid として可視化する**が、
**汚染そのものは防げない**。さらに guard が無いと、生き残る run(R_B)の
開始が t10 までずれ込み、t3〜t10 の本負荷を丸ごと取りこぼす(冒頭欠落)。

### 必須要件と不変条件 I1

> **アプリケーションは initialize handler の本体全体
> (DB 再構築 + `ResetNow`)を `isutools.SerializeInitialize` で
> 直列化しなければならない。**

**不変条件 I1**:
`SerializeInitialize` の `fn` 内で **`ResetNow` を最後の操作として**呼ぶ限り、

- 任意の時点で「`aborted` でない現行 run」の開始境界は、
  **その時点までに完了した全 DB 再構築より後**にある
- 新たな DB 再構築が始まる場合、それは必ず guard 取得後であり、
  その initialize の末尾の `ResetNow(Preempt=true)` が
  当該 run を必ず `aborted` + `ValidityInvalid` にする

ゆえに **汚染された run が valid のまま残ることはない**(帰納法で成立)。
I1 は「run が汚染されないこと」ではなく
「汚染され得る run が valid のまま生き残らないこと」を保証する契約である。

### guard あり / なしの比較(同時 2 本の initialize)

| | guard あり(必須要件を満たす) | guard なし(非推奨・未サポート) |
|---|---|---|
| DB 再構築の重なり | 起きない(`fn` 実行区間が排他) | 起きる(アプリ自身も壊れ得る) |
| 先行 run R_A | `aborted` + `ValidityInvalid`(`Reason="preempted-by:R_B"`) | 同左 |
| 現行 run R_B | `started` + `ValidityValid`。**B の rebuild 完了後**に開始 | `started`。他の rebuild が並走中なら汚染され、次の preempt で破棄される |
| 取りこぼし | B の rebuild 中のみ。2 本を**同時に**投げるベンチは B の応答を待つため実負荷はまだ流れていない。A の応答で負荷を開始する運用なら A 応答〜B の `ResetNow` の負荷が R_A ごと破棄される(ベンチ側の異常系。invalid run として検知できる) | 同左に加え、B の rebuild と本負荷が重なって R_B も汚染され得る |
| 検知 | — | health `initialize-unserialized` を記録 + ダッシュボードに「境界非保証」バッジ |

`initialize-unserialized`(本計画が**所有**する health キー。全体で 1 つ。
02 の `runctl-*` 4 キーとは別枠で、package `isutools` が持つ):
`Reason == "initialize"` の `StartRun` が `SerializeInitialize` の外側から
呼ばれたときに記録する。判定は **ctx マーカー**で行う。

> **ctx マーカーの契約は 02 v6 §preempt が定義・所有する。**
> 本ファイルはそれを**引用して消費する**だけで、再定義も改名もしない。
>
> **[v6 監査反映]** v5 / v6 初稿の「**02 への additive な要求**」という
> *未解決の要求*としての書き方は**撤回する**。02 v6 は
> §preempt「ctx マーカーの規約(08 が依存する — 本ファイルが実装を所有する)」で
> 既に本契約を明文化しており、08 は確定済み契約の消費側である:
>
> - `SerializeInitialize` は `fn` へ渡す ctx へ
>   `context.WithValue(ctx, initializeGuardKey{}, struct{}{})` を必ず載せる
>   (`type initializeGuardKey struct{}` は package `isutools` の
>   unexported 型の key。`runctl` の API は変更しない)
> - `ResetNowOpts` が `ctx.Value(initializeGuardKey{})` を検査し、
>   マーカー無しで `Reason == "initialize"` の `StartRun` が来たときに
>   health `initialize-unserialized` を記録する。
>   記録しても `StartRun` 自体は続行する
> - 判定は ctx 経由のみ。**goroutine-local は使わない**
>
> **検知機構(マーカーの付与と検査)の実装本体は 02 が所有**し、
> **health キー `initialize-unserialized` の所有は 08** である。
> 本計画の `TestInitializeWithoutGuard_HealthRecorded` はこの契約に依存する
> (02 §preempt にも同じ依存関係が明記されている)。

### 範囲外(02 と同一)

- **複数プロセス / 複数ホストの initialize 直列化は範囲外**。
  `SerializeInitialize` はプロセス内 mutex であり、別プロセスの DB 再構築は
  止められない。10 の hub が全 participant に StartRun バリアを張る形で解決する
- 直列化しない運用を選ぶ場合、run が preempt により invalid になり得ることを
  `docs/INTEGRATION.md` に明記する(黙って汚染された valid run は作らない)

## 冪等化(02 の nonce を使用)

- `ResetNow` は initialize リクエストごとに一意の nonce を内部発番する。
  `ResetNowWithNonce(ctx, nonce)` も公開し、ベンチ側が nonce を制御できる
- **同一 nonce の再送は preempt を起こさない**。02 遷移表 A の
  `StartRun(same nonce)` 列に従い、`started` 中なら
  **保存済み `StartResult` をそのまま返す**(`NonceTTL` 10m /
  `NonceHistoryMax` 64 件)。ネットワーク再送やクライアント側リトライで
  世代が余計に進むことはない
- ベンチマーカーの initialize **リトライ(別リクエスト)は別 nonce** =
  新 run。preempt により最後の initialize が有効になる(正しい挙動として文書化)
- v2 にあった「5 秒 debounce」は導入しない(時刻ではなくリクエスト単位の
  一意性で扱う)

## 補助(best-effort): middleware 検知

方針は v2 から不変(既存 `responseWriter` への observer callback。
二重 wrapper を作らない)。v6 での明確化:

- `ISUTOOLS_RESET_ON_INITIALIZE=besteffort` で有効化(既定 off)
- 発火は応答完了後・**非同期**。呼ぶのは
  `ResetNowOpts(ctx, runctl.StartRunOptions{Preempt: true, Reason: "initialize",
  Trigger: "initialize-besteffort"})`
  (HTTP 経由ではない。v5 の旧 `Reset(WaitDrain:false)` 表記は撤回済み)
- **besteffort は handler 本体を直列化できない**(handler は既に return 済み)。
  したがって不変条件 I1 は**成立しない**。この事実を明示し、
  health `initialize-unserialized`(reason: `besteffort`)を必ず記録して
  ダッシュボードに「境界非保証」バッジを出す
- 応答完了後の発火なので、先頭数リクエストの前世代混入があり得る
- observer 有効状態でも Flusher / Hijacker / ReaderFrom / Unwrap の
  透過契約テストを全通しする(既存回帰テストに observer 有効ケースを追加)

## multi-app / multi-host

複数インスタンス・複数ホストへの伝播は 10 の distributed reset
(StartRun / FinishRun バリア)で扱う。本計画は単一プロセスのみ。
10 の per-peer deadline は 02 §予算モデルの表(StartRun バリア per-peer 8s /
total 12s)を使う。

## 実装ステップ(TDD)

1. `ResetNow` / `ResetNowWithNonce` / `ResetNowOpts`(**実装本体は 02**)の
   initialize 向け配線: `Reason: "initialize"` 既定・`Trigger: "api"` 記録・
   HTTP 自己呼び出しを一切使わないことの確認テスト
2. **デッドロック回帰テスト**: 計測 middleware 配下の initialize handler 内から
   `ResetNow` → タイムアウトなしで返り、世代が進み、initialize リクエスト自身が
   旧世代に計上されること(02 と共通のテストを本計画の受け入れ条件にも設定)
3. `SerializeInitialize` を使った規範 handler の統合テスト
   + 02 の ctx マーカー(`initializeGuardKey{}`)経由で
   health `initialize-unserialized` が記録されることの検証
   (マーカーの付与・検査そのものの実装は 02 が所有)
4. 同時 initialize の並行テスト群(§テスト計画。`-race`)
5. httpstats observer オプション + 透過契約回帰
6. besteffort モード判定(パス・メソッド・status・既定 off)+ バッジ +
   `initialize-unserialized`(reason: `besteffort`)
7. docs: `docs/INTEGRATION.md`「同期 API(推奨・`SerializeInitialize` 必須)/
   besteffort(暫定・I1 非成立)」、`examples/` の initialize handler 例、
   禁止事項 4 項目、`ErrRunActive` / `ErrInitializeBusy` の扱い

## テスト計画(受け入れ条件)

### 単体

- `TestInitializeCallsStartRunOnce`: POST /initialize 200 →
  fake Controller の **`StartRun` 呼び出しが 1 回**、かつ
  `StartRunOptions{Preempt: true, Reason: "initialize"}` であること
  (v5 の `Controller.Reset` 参照は撤回)
- `TestInitializeNotTriggered`: 別パス / GET / 500 応答 / モード off →
  `StartRun` が呼ばれないこと
- `TestResetNowInvalid_Returns500`: `StartResult.Validity == ValidityInvalid` →
  handler が 500(log-and-continue しないこと)
- `TestResetNowPartial_DefaultPolicyContinues`: `ValidityPartial` →
  既定ポリシーで 200、log に collector 一覧が出ること

### 同時 initialize(新契約から導出した期待値。v5 の期待文は撤回)

- **`TestConcurrentInitialize_Serialized_LastRunWins`**(`-race`)。
  規範 handler(`SerializeInitialize` + 末尾 `ResetNow`)に
  POST /initialize を 2 本同時に投げ、次を検証する:
  1. 両方の HTTP 応答が **200**
  2. `fn` の実行区間が**重ならない**(`fn` 開始/終了時刻を記録し、
     `[start_A, end_A]` と `[start_B, end_B]` が非重複)
  3. `StartRun` の呼び出しは**ちょうど 2 回**、いずれも `Preempt: true`
  4. 先着 run `R_A`: `Status(R_A).State == runctl.StateAborted` かつ
     `Validity == runctl.ValidityInvalid` かつ
     `Reason == "preempted-by:" + R_B`
  5. 後着 run `R_B`: `Status(R_B).State == runctl.StateStarted` かつ
     `Validity == runctl.ValidityValid` かつ
     `StartResult(R_B).PreemptedRunID == R_A`
  6. `R_A` の snapshot が **store に存在しない**・run 一覧に出ない
  7. generation collector の世代番号は **+2**(`StartRun` 1 回につき +1)
  8. `Epoch` は **3 回**進む(A の StartRun +1 / B の preempt の fence +1 /
     B の StartRun +1 — 02 §用語「StartRun 成功ごと・AbortRun ごとに +1」と
     §epoch fencing 手順 1 から導出)。最低限
     `StartResult(R_B).Epoch > StartResult(R_A).Epoch` を必ず検証する
- `TestConcurrentInitialize_NoRebuildOverlapsSurvivingRun`(不変条件 I1):
  `rebuildDB` fake が実行区間を記録する。検証は
  「**生き残った run(`aborted` でない run)の `StartedAt` が、
  記録された全 rebuild 区間の `end` より後**」であること
- `TestInitializeSameNonce_NoPreempt`: 同一 nonce で
  `ResetNowWithNonce` を 2 回呼ぶ → 2 回目は保存済み `StartResult` を返し、
  `StartRun` の実処理は 1 回・世代は +1・`PreemptedRunID` は空
- `TestInitializeGuardBusy_Returns503`: `InitializeGuardBudget` を注入で
  短縮し、`runctl.ErrInitializeBusy` → 503 + `Retry-After` を検証
- `TestInitializeWithoutGuard_HealthRecorded`: `SerializeInitialize` を
  使わずに `ResetNow` を呼ぶ → health `initialize-unserialized` が立ち、
  run に「境界非保証」バッジが付くこと(未サポート運用の可視化)

### 結合・回帰

- integration: `ResetNow` 完了時点で generation +1、以後のリクエストが新世代、
  **呼び出し元の initialize リクエスト自身は旧世代**に計上されること
- integration: `ResetNow` の応答が `PreemptTotalBudget`(8s)以内に返ること
  (前 run の background Drain を待たないことの回帰)
- 回帰: httpstats 透過契約(observer 有効 / 無効の両方で
  Flusher / Hijacker / ReaderFrom / Unwrap)

## リスク

| リスク | 対策 |
|---|---|
| アプリが `SerializeInitialize` を使わない | health `initialize-unserialized` + バッジで可視化。`docs/INTEGRATION.md` の必須要件に記載。テスト `TestInitializeWithoutGuard_HealthRecorded` |
| `fn` 内で `ResetNow` の後に DB を触る実装 | 禁止事項 2 として docs / example に明記。不変条件 I1 の前提であることを併記 |
| `ResetNow` の呼び場所誤り(応答送信後) | 禁止事項 4。besteffort が保険(ただし I1 非成立を明示) |
| preempt により run が量産される | 02 の `PreemptedRunID` / `reset_trigger` 記録で一覧から判別可能 |
| background Drain により prev 確定が遅れる | 02 の仕様どおり `/save` 側で `Await`(`FinishLease` 20s)して確定 |
| besteffort の誤用 | trigger 記録・バッジ・health で run を判別可能にする |
| 複数プロセス構成で guard が効かない | 範囲外として明記し、10 の hub バリアへ委譲 |

## 見積もり

**2.0 日**(v5 の 1.5 日 + 0.5 日。02 完了が前提):

| 追加項目 | 増分 |
|---|---|
| `SerializeInitialize` を使う規範 handler の配線 + 02 定義の ctx マーカーを消費する検証 | +0.25 日 |
| 同時 initialize の並行テスト群(preempt 終状態・I1 検証) | +0.25 日 |

`SerializeInitialize` / preempt / ctx マーカー検知の**実装本体は
02 の見積もり**(+0.75 日)に含まれており、本ファイルでは二重計上しない。

**[v6 監査反映]** v6 初稿の「**plans/README.md の再算定が必要**」という
注記は**撤回する**。README v6 は全 11 計画の §見積もり の実数から
既に再算定を完了しており、§リリース対応の表で
**v1.3.0 = 07(3.0)+ 08(2.0)+ 09(4.5)= raw 9.5 日 / +30% buffer 12.5 日**、
v5 → v6 増分表で **08: 1.5 → 2.0 日(+0.5)** と記載している。
本ファイルの **2.0 日**と一致しており、README 側に未処理の作業は残っていない。
README の計上規則にも「**08 の 2.0 日に `SerializeInitialize` / preempt の
実装本体は含まない**(実装本体は 02 の 9.5 日に +0.75 日として計上済み)」と
明記されており、上の二重計上の禁止と整合する。
