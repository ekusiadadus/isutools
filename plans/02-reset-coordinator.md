# 02: Reset coordinator(run 契約の導入)— v4

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `internal/runctl`(新規)、`web`、`isutools.go`、各 collector

## v4 での変更点(第3回レビュー差し戻し対応)

1. **[CRITICAL] collector を 2 種類に分離**。v3 は baseline 取得を
   境界後の非同期 Drain に置いたため、境界宣言(T0)〜baseline 実採取(T2)
   の間の CPU・SQL・network・dbpool が baseline に吸収され、
   **新 run の冒頭が欠落**していた(現行 procstats も実サンプル時刻を
   StartedAt にしている — procstats/procstats.go:202, :242)。
   → **generation collector**(世代スワップ型)と
   **baseline collector**(基準値採取型)の契約を分け、baseline は
   StartRun 内で **bounded I/O を同期実行**する。禁止事項は
   「旧世代 in-flight の完了待ちを StartRun 内で行わない」ことだけに絞る
2. **[CRITICAL] Drain の世代識別と遷移期間**。v3 は Drain(ctx) に世代
   token がなく、reset 拒否も BeginBoundary 完了までだったため、
   reset1 の background Drain 中に reset2 が開始でき、どの旧世代を
   Drain しているか不定だった。
   → **reset 遷移は DrainPrevious 完了まで継続**とし、その間の新規 reset
   は 409(同一 nonce は保存済み結果を返す)。Drain handle は世代 token 付き
3. **[HIGH] 返却値の不変化**。WaitDrain:false の返却値に後から確定する
   DrainedAt/Err が含まれ、snapshot/health と競合していた。
   → 不変の **StartResult** と、別途取得する **DrainStatus /
   PreviousRunResult** に分割。background 処理は request context から
   切り離す(`context.WithoutCancel` + 内部 timeout)

## ゴール

1. run_id・StartResult・collector 別の**実測**境界時刻を持つ run 契約
2. 計測対象 handler の内側から呼んでもデッドロックしない reset
3. **新 run の冒頭を欠落させない**(baseline は境界と同期)
4. 応答済み run を後続 reset が無言で無効化・上書きしない
5. admin 無効・DB 未接続・Handler() 複数回生成で一貫動作

## 設計

### collector 契約(2 種類)

```go
// 世代スワップ型: httpstats / sqlstats / accesslog / counters。
// BeginBoundary は table swap 等の高速な世代切替のみ(非ブロッキング)。
// BoundaryAt はスワップ実行時刻。
type GenerationCollector interface {
    BeginBoundary(runID string) (BoundaryAt time.Time, err error)
    // DrainPrevious は handle が指す旧世代のみを確定する
    // (in-flight 完了待ち・追い付き collect)。非同期に呼ばれる。
    DrainPrevious(ctx context.Context, handle GenerationHandle) error
}

// 基準値採取型: procstats / sqlrows / dbpool / network / hoststats。
// CaptureBaseline は bounded I/O を同期実行し、実サンプル完了時刻を返す。
// 旧世代の in-flight には依存しないため StartRun 内で安全に呼べる。
type BaselineCollector interface {
    CaptureBaseline(ctx context.Context, runID string) (SampledAt time.Time, err error)
}
```

- BoundaryAt / SampledAt は**宣言時刻ではなく実測時刻**。
  BoundaryWindow(min/max)はこの実測値から計算する
- GenerationHandle は世代 token(runID + collector 内世代番号)を持ち、
  「どの旧世代を Drain しているか」を常に一意にする

### StartRun のシーケンス

```
1. 遷移ガード: state==idle のみ許可(下記)。nonce 一致は保存済み
   StartResult を返す
2. generation collectors: BeginBoundary(高速スワップ)
3. baseline collectors: CaptureBaseline を同期実行
   (collector ごと timeout、全体 budget 2s。失敗した collector は
   partial 記録して続行)
4. 不変の StartResult を確定して返す
5. state=draining にして background で DrainPrevious(全 generation
   collectors、世代 handle 付き、request context から切り離した
   timeout 付き context、上限 10s)。完了で state=idle
```

- StartRun 内に「旧世代 in-flight の完了待ち」は存在しない
  → 計測対象 handler 内からの呼び出しでもデッドロックしない
  (baseline の /proc 読みや P_S クエリは in-flight と無関係)
- baseline 同期化により T0〜T2 問題は消える: ベンチ負荷は StartRun
  応答後に始まり、baseline はその前に採取済み

### 遷移状態機械と並行 reset

```
idle → starting(StartRun 実行中)→ draining(旧世代確定中)→ idle
```

- **starting / draining 中の新規 reset は 409**(ErrResetInProgress)。
  同一 nonce は保存済み StartResult を 200 で返す(冪等)
- DrainPrevious の上限 10s で遷移は必ず終わる(timeout 時は prev を
  partial 記録)。長寿命 in-flight リクエストが遷移を延ばす trade-off は
  文書化する(WS/SSE は既に世代から detach 済みで対象外)
- 世代 handle により「reset2 が reset1 の Drain 対象を上書きする」経路は
  存在しない(遷移中 409 + handle の二重防御)

### 返却値の分離

```go
// StartRun が返す不変値。以後書き換えない。
type StartResult struct {
    RunID          string              `json:"run_id"`
    Nonce          string              `json:"nonce,omitempty"`
    RequestedAt    time.Time           `json:"requested_at"`
    BoundaryWindow Window              `json:"boundary_window"` // 実測 min/max
    Collectors     []CollectorBoundary `json:"collectors"`      // 実測時刻 + 種別
    State          RunState            `json:"state"`           // valid | partial
}

// 旧世代の確定結果。Drain 完了後に別途取得(snapshot の prev に添付)。
type PreviousRunResult struct {
    RunID     string    `json:"run_id"`
    DrainedAt time.Time `json:"drained_at"`
    TimedOut  bool      `json:"timed_out,omitempty"`
    Errors    []string  `json:"errors,omitempty"`
}
```

- snapshot の Meta には現行 run の StartResult(immutable)を、
  prev には PreviousRunResult を添付する
- health への反映は Drain 完了イベントとして別経路(StartResult は不変)

### Controller(process-wide singleton、v3 から維持)

- sync.Once で一度だけ生成。admin HTTP と独立。
  `Handler()` / `ResetNow()` は同一 Controller へ委譲
- テスト必須: admin off の ResetNow / DB 未接続 / Handler() 複数回

### HTTP API(v3 から維持 + 遷移拡張)

- `POST /reset`: StartRun + Drain 完了まで待って応答(bench 用の
  full barrier)。204 + `X-Isutools-Run-Id`。`?format=json` で
  StartResult。遷移中 409 / 同一 nonce 200
- `ResetNow`: StartRun 完了(境界 + baseline 確定)で返る。
  Drain は background(呼び出し元が in-flight のため待てない)

## 実装ステップ(TDD)

1. runctl: 状態機械(idle/starting/draining・409・nonce・timeout)を
   fake collector でテスト先行
2. StartResult 不変性(background 完了後も値が変わらない)+
   PreviousRunResult 分離のテスト
3. httpstats を GenerationCollector に分割(swap/drain、handle 付き)。
   透過契約・世代テスト全通し
4. procstats を BaselineCollector 化(SampledAt = 実測。既存
   StartedAt セマンティクスの維持を回帰テスト)
5. **デッドロック回帰テスト**: 計測 handler 内 ResetNow(タイムアウト付き)
6. **冒頭欠落回帰テスト**: StartRun 応答直後に発生した負荷が新 run に
   全量計上されること(fake clock で T0/T1/T2 を再現)
7. web /reset・Meta/prev 反映・docs

## リスク

| リスク | 対策 |
|---|---|
| baseline 同期化による StartRun 遅延 | 全体 budget 2s + collector 別 timeout。実測値を docs に記録 |
| 遷移中 409 が bench リトライと衝突 | nonce 冪等で同一 run を返す。INTEGRATION.md に手順明記 |
| collector 分割リファクタの退行 | 互換 shim で段階移行 + 既存テスト網 |

## 見積もり

4 日(v3 の 3 日から増。2 契約分離と状態機械、回帰テスト 2 本を含む)。
