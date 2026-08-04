# 08: 計測開始の自動化 — v3

種別: 機能 / 対象リリース: v1.3.0 / 依存: 02(二段階境界 + nonce) / 変更箇所: `isutools.go`, `httpstats`(observer hook)

## v3 での変更点(レビュー差し戻し対応)

**[CRITICAL] 自己デッドロックの解消**。v2 は「initialize handler 末尾で
ResetNow を同期実行」としたが、ResetNow が旧世代の in-flight 完了を待つ
設計(v2 の 02)では、**呼び出し元の initialize リクエスト自身が
in-flight** のため永遠に待つ(middleware は handler 開始時に in-flight を
増やし終了時に減らす — httpstats/httpstats.go:213, :328)。

02(v4)の contract により解消する:

- `ResetNow` = **StartRun(世代スワップ + baseline 同期採取。
  旧世代 in-flight の完了待ちを含まない)+ DrainPrevious(非同期)**
- 呼び出し元の initialize リクエストは境界前に開始しているため
  **旧世代に計上**され、DrainPrevious は initialize handler の終了
  (= in-flight 減少)後に自然に完了する。デッドロックは構造的に起きない
- StartRun には baseline 採取(bounded I/O)が含まれるため、
  応答時点で境界と基準値の両方が確定済み。initialize 応答後に始まる
  本負荷は冒頭から新世代に全量計上される(v4 の冒頭欠落修正と整合)

## モード設計

### 推奨(正確): 明示同期 API

```go
func initializeHandler(w http.ResponseWriter, r *http.Request) {
    // ... DB 再構築など ...
    // v5: ResetNow は runID 文字列ではなく不変の StartResult を返す。
    // 02 は required collector の失敗を State で表現するため、
    // 呼び出し側は partial/invalid を判定できる(できないと
    // 汚れた run のまま 200 を返してしまう)。
    start, err := isutools.ResetNow(r.Context())
    if err != nil || start.State == runctl.StateInvalid {
        http.Error(w, "isutools reset failed", http.StatusInternalServerError)
        return
    }
    if start.State == runctl.StatePartial {
        // 既定ポリシー: required が揃っていれば partial は計測続行可。
        // 厳格運用では 500 にする(caller policy を必ず明示する)。
        log.Printf("isutools: run %s started partial: %v", start.RunID, start.Collectors)
    }
    w.WriteHeader(http.StatusOK)                  // 境界確定後に応答
}
```

- `ResetNow` は 02 の process-wide Controller を直接呼ぶ
  (HTTP 自己呼び出しは存在しない。admin 無効でも動作)
- run に `reset_trigger: "api"` を記録

### 同時 initialize の扱い(v4 修正)

v3 の「409 は log して処理継続してよい」は誤りだった: initialize A が
境界を張った後、initialize B が DB 再構築を続けてから 409 を握り潰すと、
**A の run が B の初期化処理後半で汚染されたまま valid になる**。

v4 の `ResetNow` セマンティクス:

1. `ErrResetInProgress`(進行中の遷移がある)を受けたら、
   **進行中の遷移完了を bounded に待ち(上限 15s = StartRun budget +
   Drain 上限)、自分固有の nonce で再 reset する**。
   これにより最後の initialize が必ず新しいクリーンな境界を張る
   (先行 run は短い完結した世代として残り、上書きされない)
2. 待機 timeout・再 reset 失敗時は**エラーを返す**。呼び出し側の
   規範は上記例のとおり initialize を 500 で失敗させること。
   **「log だけして正常応答」をドキュメント・example に書かない**
   (禁止事項として明記する)
3. `ResetNow` 内部の待機・再試行は request context ではなく
   内部 timeout 付き context で行う(02 の返却値分離と同じ方針。
   クライアント切断で境界処理が中途停止しない)

### 冪等化(02 の nonce を使用)

- `ResetNow` は内部で initialize リクエストごとに一意の nonce を発番する
  (`ResetNowWithNonce(ctx, nonce)` も公開し、ベンチ側が nonce を
  制御できるようにする)
- ベンチマーカーの initialize リトライ(別リクエスト)は別 nonce =
  新 run。最後の initialize が有効になる(正しい挙動として文書化)。
  v2 にあった「5 秒 debounce」は導入しない(時刻ではなくリクエスト
  単位の一意性で扱う)

### 補助(best-effort): middleware 検知

v2 から変更なしの方針(既存 responseWriter への observer callback、
二重 wrapper なし)+ v3 の修正:

- `ISUTOOLS_RESET_ON_INITIALIZE=besteffort` で有効化(既定 off)
- 発火は応答完了後・**非同期**。02 の `Controller.StartRun` を
  リクエスト固有 nonce 付きで直接呼ぶ(HTTP 経由ではない。
  v5: 旧 `Reset(WaitDrain:false)` 表記を StartRun API へ統一)
- 先頭数リクエストの前世代混入があり得ることを明示し、run に
  `reset_trigger: "initialize-besteffort"` + ダッシュボードに
  「境界非保証」バッジ
- observer 有効状態でも Flusher / Hijacker / ReaderFrom / Unwrap の
  透過契約テストを全通しする(既存回帰テストに observer 有効ケースを追加)

### multi-app / multi-host

複数インスタンス・複数ホストへの伝播は 10 の distributed reset
(ResetRun/FinishRun バリア)で扱う。本計画は単一プロセスのみ。

## 実装ステップ(TDD)

1. `ResetNow` / `ResetNowWithNonce`(02 の Controller 公開)
2. **デッドロック回帰テスト**: 計測 middleware 配下の initialize handler
   内から ResetNow → タイムアウトなしで返り、世代が進み、
   initialize リクエストが旧世代に計上されること(02 と共通のテストを
   本計画の受け入れ条件にも設定)
3. httpstats observer オプション + 透過契約回帰
4. besteffort モード判定(パス・メソッド・status・既定 off)+ バッジ
5. docs: INTEGRATION.md「同期 API(推奨)/ besteffort(暫定)」、
   examples の initialize handler 例、ErrResetInProgress の扱い

## テスト計画

- unit: POST /initialize 200 → Controller.Reset 呼び出し 1 回(fake)
- unit: 別パス / GET / 500 / モード off → 呼ばれない
- unit: 同時 initialize 2 本 → **2 本とも順番に成功し、世代は 2 回
  進み、最後の nonce の run が有効になる**(v5: ResetNow は待機 →
  自 nonce 再 reset のため。「片方 409・世代 1 回」の旧期待値を撤回)
- integration: ResetNow 完了時点で generation +1、以後のリクエストが
  新世代、呼び出し元リクエストは旧世代
- 回帰: httpstats 透過契約(observer 有効/無効の両方)

## リスク

| リスク | 対策 |
|---|---|
| ResetNow の呼び場所誤り(応答送信後) | INTEGRATION.md で「WriteHeader 前」を強調。besteffort が保険 |
| Drain 非同期化により prev 確定が遅れる | 02 の仕様どおり snapshot/save 側で Drain 待ち(タイムアウト付き) |
| besteffort の誤用 | trigger 記録とバッジで run を判別可能にする |

## 見積もり

1 日 + docs 0.5 日(02 完了が前提)。
