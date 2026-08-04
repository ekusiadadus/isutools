# 08: 計測開始の自動化 — v3

種別: 機能 / 対象リリース: v1.3.0 / 依存: 02(二段階境界 + nonce) / 変更箇所: `isutools.go`, `httpstats`(observer hook)

## v3 での変更点(レビュー差し戻し対応)

**[CRITICAL] 自己デッドロックの解消**。v2 は「initialize handler 末尾で
ResetNow を同期実行」としたが、ResetNow が旧世代の in-flight 完了を待つ
設計(v2 の 02)では、**呼び出し元の initialize リクエスト自身が
in-flight** のため永遠に待つ(middleware は handler 開始時に in-flight を
増やし終了時に減らす — httpstats/httpstats.go:213, :328)。

v3 の 02 で導入した二段階境界により解消する:

- `ResetNow` = **BeginBoundary(同期・非ブロッキング)+ Drain(非同期)**
- 呼び出し元の initialize リクエストは境界前に開始しているため
  **旧世代に計上**され、Drain は initialize handler の終了(= in-flight
  減少)後に自然に完了する。デッドロックは構造的に起きない
- 応答時点で境界は確定済みなので、initialize 応答後に始まる本負荷は
  すべて新世代に入る(pprotein と同等の境界保証は維持)

## モード設計

### 推奨(正確): 明示同期 API

```go
func initializeHandler(w http.ResponseWriter, r *http.Request) {
    // ... DB 再構築など ...
    runID, err := isutools.ResetNow(r.Context())  // 境界確定して返る(Drainは非同期)
    if err != nil { log.Printf("isutools: reset: %v", err) }
    w.WriteHeader(http.StatusOK)                  // 境界確定後に応答
}
```

- `ResetNow` は 02 の process-wide Controller を直接呼ぶ
  (HTTP 自己呼び出しは存在しない。admin 無効でも動作)
- run に `reset_trigger: "api"` を記録
- **ErrResetInProgress** を受けた場合(同時 initialize 等)はエラーを
  返す。handler 側は log して処理を続行してよい(直前の reset が
  境界を張っているため)

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
- 発火は応答完了後・**非同期**。`Controller.Reset(WaitDrain:false)` を
  リクエスト固有 nonce 付きで呼ぶ(HTTP 経由ではない)
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
- unit: 同時 initialize 2 本 → 片方が ErrResetInProgress、
  世代は 1 回だけ進む
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
