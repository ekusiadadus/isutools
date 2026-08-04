# 02: Run lifecycle coordinator — v5

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `internal/runctl`(新規)、`web`、`isutools.go`、各 collector

## v5 での変更点(第4回レビュー差し戻し対応)

1. **[CRITICAL] GenerationHandle の取得手段を API に定義**(v4 は
   BeginBoundary が時刻しか返さず、DrainPrevious に渡す handle を
   取得できなかった = 実装不能)
2. **[CRITICAL] run lifecycle を Start だけでなく
   Start → Finish → Abort → Ack/Expire の完全な状態機械として定義**。
   特に **Finish(終了境界)を全 collector 共通の契約**にする:
   世代型は現在世代を freeze して handle を返し、baseline 型は
   終了サンプルを同期取得する。immutable snapshot は
   **固定済みの値だけ**から構築する(10 が本契約を wire に一対一で写す)
3. **[HIGH] BeginBoundary 途中失敗の扱いを定義**(required/optional
   collector 区分、invalid 化、切替済み世代の seal)
4. **[HIGH] Drain の ctx cancellation 契約**(現行 httpstats は無期限
   待機する — httpstats/httpstats.go:341。契約 + conformance test で保証)

## ゴール

1. run の開始・終了・中止・破棄を含む**完全な lifecycle** を単一の
   Controller が所有する
2. 計測対象 handler の内側から呼んでもデッドロックしない
3. 新 run の冒頭・末尾を欠落/混入させない(開始 baseline 同期採取・
   終了 freeze 同期固定)
4. どの失敗経路からも**次の run を開始できる**(Abort による回復)

## collector 契約(v5)

```go
type GenerationCollector interface {
    // 開始境界: 新世代へスワップし、閉じた旧世代の handle を返す。
    // 高速・非ブロッキング。boundaryAt はスワップ実行時刻(実測)。
    BeginBoundary(runID string) (prev GenerationHandle, boundaryAt time.Time, err error)

    // 終了境界: 現在世代を freeze し、その handle を返す。高速・同期。
    // freeze 後の観測は次世代(run 外)に入る。
    Freeze(runID string) (cur GenerationHandle, frozenAt time.Time, err error)

    // handle が指す世代のみを確定する(in-flight 完了待ち・追い付き
    // collect)。ctx.Done() で必ず return し、return 後に当該世代を
    // 変更する goroutine を残さないこと(conformance test で保証)。
    Drain(ctx context.Context, h GenerationHandle) error

    // Drain 済み handle の確定データを読む(immutable snapshot 構築用)。
    Collect(h GenerationHandle) (any, error)
}

type BaselineCollector interface {
    // 開始・終了とも bounded I/O の同期採取。SampledAt は実測時刻。
    CaptureBaseline(ctx context.Context, runID string) (SampledAt time.Time, err error)
    CaptureFinal(ctx context.Context, runID string) (SampledAt time.Time, err error)
}
```

- 世代型: httpstats / sqlstats / accesslog(EOF offset を freeze 点で
  記録)/ counters。baseline 型: procstats / sqlrows / dbpool /
  network / hoststats
- collector は登録時に **required / optional** を宣言する
  (既定: sql・http は required、その他 optional。Provider 配線で変更可)

## run lifecycle 状態機械

```
idle ─ StartRun ─→ started ─ FinishRun ─→ finishing ─→ finished ─→ (acknowledged | expired)
         │                        │
         └──── AbortRun ──→ aborting ──→ aborted(→ idle)
```

### StartRun(nonce 付き・冪等)

1. 遷移ガード: idle のみ。同一 nonce は保存済み StartResult を返す。
   それ以外の状態は 409(started 中は先に FinishRun か AbortRun)
2. 全 generation collector: `BeginBoundary` → (prev handle, 実測時刻)
3. **途中失敗の扱い(v5)**: required collector の失敗 →
   残りの collector も**切替まで完了させて**プロセス状態を新世代で
   統一した上で、run を **invalid** とし、切替済みの旧世代 handle は
   seal(Drain 後に「invalid run の断片」として破棄可能マーク)する。
   optional の失敗 → partial で続行
4. 全 baseline collector: `CaptureBaseline` を同期実行(全体 budget 2s)
5. 不変 **StartResult**(RunID / Nonce / collector 別実測境界・
   required 区分 / State)を確定して返す
6. background: prev handles の `Drain`(detached ctx、上限 10s。
   **ctx cancellation で必ず終了する契約**)

### FinishRun(終了境界 — v5 で全 collector に拡張)

1. started のみ受理(冪等: 同一 runID の再送は同じ FinishAccepted を返す)
2. **freeze phase(高速・同期)**: 全 generation collector の
   `Freeze` + 全 baseline collector の `CaptureFinal`(全体 budget 2s)。
   ここが**全セクション共通の計測終了境界**(v4 は HTTP と accesslog
   しか固定していなかった)
3. **FinishAccepted**(collector 別 frozenAt / final SampledAt)を
   即座に返す(Drain や snapshot 構築は待たない)
4. background: frozen handle の `Drain` → `Collect` で
   **固定値だけから immutable run snapshot を構築**して保存 →
   state=finished。以後この run のデータは変化しない
5. snapshot 構築の完了は状態(finishing/finished)として照会できる

### AbortRun(冪等 — v5 新設)

- どの状態からでも受理(idle では no-op 成功)。started/finishing の
  run を **aborted** にし、freeze/seal 済み世代を破棄可能にして
  idle へ戻す。**部分開始・部分失敗からの回復経路**として、
  10 の分散 abort がこれを一対一で呼ぶ
- StartRun 途中の required 失敗(上記 3)は内部的に AbortRun 相当へ
  遷移する(invalid run として記録は残す)

### Ack / Expire

- finished run の結果は **Ack(取得完了の明示)または TTL(10 分)**まで
  保持する。単一ホストでは `POST /save` の永続化成功が Ack に相当。
  10 では hub の明示 ACK API が対応する
- 保持は直近 2 run 分(10 と同一の契約)

## API(単一ホスト)

- `POST /reset` = StartRun(full barrier: 応答前に background Drain の
  完了も待つ従来動作を維持)。204 + `X-Isutools-Run-Id`、
  409 / nonce 冪等は v4 のまま
- `POST /collect` / `POST /save` = FinishRun を内包(freeze →
  snapshot 構築 → 保存)
- `ResetNow(ctx) (StartResult, error)`(v5 変更: runID 文字列ではなく
  **StartResult を返す**。08 が partial/invalid を判定できるようにする)
- `AbortRun` は内部 API(単一ホストでは次の StartRun が実質的な
  代替になるが、10 の wire 写像のために定義する)

## 実装ステップ(TDD)

1. 状態機械(全遷移 × API の受理/拒否表を先にテスト化)
2. handle 付き BeginBoundary/Freeze/Drain/Collect への httpstats 分割
   + **Drain conformance test**(ctx cancel で return・return 後に
   旧世代を変更する goroutine が残らないことを race detector 下で検証)
3. baseline collector(procstats)の Capture 2 点化
4. StartRun 途中失敗(required/optional)・seal・invalid のテスト
5. FinishRun freeze phase と「固定値のみから snapshot 構築」の検証
   (freeze 後に故意の負荷を掛け、snapshot に混入しないこと)
6. AbortRun 冪等性と「部分失敗 → abort → 次 run 成功」のテスト
7. デッドロック回帰・冒頭欠落回帰(v4 から維持)
8. web /reset・/save 配線、docs

## リスク

| リスク | 対策 |
|---|---|
| 契約が大きくなり移行コスト増 | 互換 shim(旧 Reset() を新契約で包む)で collector ごとに段階移行 |
| freeze phase の budget 超過 | 2s budget + 超過 collector は partial。実測値を docs に記録 |
| Drain の cancellation 対応漏れ | conformance test を全 collector の受け入れ条件に固定 |

## 見積もり

**5 日**(v4 の 4 日から増。Finish/Abort 契約・conformance test・
状態機械の遷移表テストを含む)。
