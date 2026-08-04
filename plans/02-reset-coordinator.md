# 02: Reset coordinator(run 契約の導入)— v3

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `internal/runctl`(新規)、`web`、`isutools.go`、各 collector

## v2 からの変更点(レビュー差し戻し対応)

1. **[CRITICAL] 二段階境界(rotate/drain)への変更**。v2 の「coordinator が
   既存 Reset() を順に呼ぶ」設計では、計測 middleware 配下の handler
   (/initialize)から ResetNow を呼ぶと**自己デッドロック**する:
   現行 middleware は handler 開始時に in-flight を増やし終了時に減らす
   (httpstats/httpstats.go:213)。一方 `httpstats.Reset()` は旧世代の
   全 in-flight 終了を待つ(同 :328)。呼び出し元自身が in-flight のため
   永遠に待つ。
2. **[HIGH] 並行 reset の待ち行列化を廃止**。v2 の直列化では、1 本目の
   reset 応答後にベンチが始まった直後、待機していた 2 本目が世代を
   切り替え、1 本目の run を汚染したまま valid にしてしまう。
   → **進行中は 409 で拒否** + nonce による冪等化。
3. **[HIGH] 「StartedAt の最大差 = skew」を撤回**。実際の境界は
   collector ごとに異なる(HTTP は table swap 時、procstats は baseline
   取得完了時)。→ collector ごとの **BoundaryAt** と drain 期間を記録し、
   「境界ウィンドウ」(最小〜最大 BoundaryAt)として表示する。
   ホスト間の skew は本計画で扱わない(→ 10 が hub 観測の送信/ACK 区間で
   扱う)。
4. **[HIGH] 所有権の明確化**。現行 `Handler()` は呼ぶたびに新しい
   handler / proc collector を作り、admin 無効時は handler 自体が
   存在しない(isutools.go:283)。→ **process-wide singleton の
   Controller** を一度だけ初期化し、`Handler()` と `ResetNow()` は
   そこへ委譲する。

## ゴール

1. run_id・ResetResult・collector 別境界時刻を持つ run 契約を導入する
2. **計測対象 handler の内側から呼んでもデッドロックしない** reset を提供する
3. 応答済み run を後続 reset が無言で無効化する経路を作らない
4. admin 無効・DB 未接続・Handler() 複数回生成のいずれでも一貫して動く

## 設計

### collector 契約: BeginBoundary + Drain の二段階

全 collector(sql / http / accesslog / proc / counters / dbinspect)を
次の契約へ移行する:

```go
type BoundaryCollector interface {
    // BeginBoundary は新世代への切り替えだけを行い、即座に返る。
    // ブロッキング(in-flight 待ち・I/O 待ち)を含んではならない。
    // 返り値は「この collector の計測境界時刻」。
    BeginBoundary(runID string) (BoundaryAt time.Time, err error)
    // Drain は旧世代を確定する(in-flight の完了待ち、baseline 取得、
    // ログ flush 等)。BeginBoundary の後、非同期に呼ばれてよい。
    Drain(ctx context.Context) error
}
```

- httpstats: BeginBoundary = table swap(既存実装の世代切替部分)、
  Drain = 旧世代 in-flight 待ち(既存 Reset の待機部分)。**分割リファクタ**
- procstats: BeginBoundary = 世代 ID 更新のみ、Drain = baseline 取得
  (/proc 読み)。boundary は baseline 完了時刻ではなく「切替宣言時刻」と
  し、baseline 完了は DrainedAt として別記録
- accesslog: BeginBoundary = オフセット記録、Drain = 追い付き collect
- 移行は collector ごとに独立 PR 可能(旧 Reset() を
  BeginBoundary+Drain の逐次呼びでラップする互換 shim から始める)

### ResetResult

```go
type ResetResult struct {
    RunID       string           `json:"run_id"`   // <unixnano>-<seq>-<hosthash8>
    Nonce       string           `json:"nonce,omitempty"`
    RequestedAt time.Time        `json:"requested_at"`
    BoundaryWindow Window        `json:"boundary_window"` // 全 collector の BoundaryAt の min/max
    Collectors  []CollectorReset `json:"collectors"`
    State       RunState         `json:"state"`    // valid | partial | invalid
}
type CollectorReset struct {
    Name           string    `json:"name"`
    BoundaryAt     time.Time `json:"boundary_at"`
    DrainStartedAt time.Time `json:"drain_started_at,omitempty"`
    DrainedAt      time.Time `json:"drained_at,omitempty"`
    Err            string    `json:"err,omitempty"`
}
```

- 時刻は monotonic 併用で取得し、BoundaryWindow は同一プロセス内でのみ
  意味を持つ(ホスト間比較には使わない、と型ドキュメントに明記)
- collector の BeginBoundary 失敗 → run は partial。Drain 失敗 →
  該当 collector の旧世代データ(prev)を partial 表示
- `invalid` は 10 の必須 peer 欠落用に予約(型・表示のみ先行定義)

### Controller(process-wide singleton)

```go
// isutools 内部。sync.Once で一度だけ生成。admin サーバとは独立。
type Controller struct { /* collectors, mu, inFlightReset atomic, lastResult */ }

func (c *Controller) Reset(ctx context.Context, opts ResetOptions) (ResetResult, error)
// ErrResetInProgress: 進行中の reset がある場合。呼び出し側が 409 等へ写像。
```

- `Handler()` は Controller から collector 群を参照して handler を組む
  (`Handler()` を何度呼んでも同一 Controller・同一 collector)
- `ResetNow(ctx)` は Controller.Reset を直接呼ぶ公開 API
  (admin 無効でも動作する)
- テスト必須項目: admin off での ResetNow / DB 接続前の ResetNow /
  Handler() 複数回生成での世代共有

### Reset の 2 モード

| モード | 挙動 | 用途 |
|---|---|---|
| `Reset(ctx, {WaitDrain: true})` | BeginBoundary 全実行 → Drain 完了まで待って返る | `POST /reset`(bench スクリプト。計測対象 handler の外) |
| `Reset(ctx, {WaitDrain: false})` | BeginBoundary 全実行後に即返り、Drain は background | `ResetNow`(計測対象 handler の内側 → 自己デッドロック回避) |

- どちらのモードでも**応答時点で境界は確定済み**(以後のリクエストは
  新世代)。WaitDrain=false では旧世代(prev)の確定が遅れるだけで、
  新 run の正しさに影響しない
- background Drain の完了前に snapshot/save が prev を読む場合は
  Drain 完了を待つ(タイムアウト付き。超過は prev を partial 表示)
- **統合テスト必須**: 計測 middleware 配下の /initialize handler 内から
  `ResetNow` を呼んでもデッドロックせず、initialize リクエスト自体は
  旧世代に計上されること

### 並行 reset の扱い(409 + nonce)

- 進行中(BeginBoundary 開始〜完了)の Reset がある間の新規要求は
  `ErrResetInProgress` → `POST /reset` は **409** を返す。
  **待ち行列にしない**(応答済み run の無言無効化を防ぐ)
- 冪等化: `POST /reset` は `X-Isutools-Reset-Nonce` ヘッダ(または
  `?nonce=`)を受け付ける。直近 run と同一 nonce なら**新しい rotate を
  行わず**同じ run_id を 200 で返す(bench スクリプトのリトライ安全化)。
  08 の auto-reset は initialize リクエスト毎に一意の nonce を生成する
- 連続した正当な reset(別 nonce)は通常どおり新 run を開始する。
  先行 run は「短い区間の完結した run」として残る(汚染ではない)

### HTTP API

- `POST /reset`: 既存 204 を維持 + `X-Isutools-Run-Id` ヘッダ。
  `?format=json` で `{"run_id":..., "state":...}` 200。
  進行中は 409、同一 nonce は 200(既存 run_id)
- snapshot の Meta に additive で `run_id` / `reset`(ResetResult)を追加

## 実装ステップ(TDD)

1. runctl: Controller・409/nonce・ResetResult(境界ウィンドウ・partial)を
   fake collector でテスト先行
2. httpstats の BeginBoundary/Drain 分割(既存の透過契約・世代テストを
   全通し)。他 collector は互換 shim → 順次分割
3. isutools.go: singleton 初期化・Handler()/ResetNow 委譲
   (admin off / DB 未接続 / Handler 複数回のテスト)
4. web: /reset の 409・nonce・ヘッダ、Meta 反映、prev の Drain 待ち
5. **middleware 内 ResetNow のデッドロック回帰テスト**(タイムアウト付き)
6. docs: bench 契約(409 時のリトライは同一 nonce で)

## リスク

| リスク | 対策 |
|---|---|
| collector 分割リファクタの退行 | 互換 shim で段階移行 + 既存テスト網 |
| background Drain の失敗が見逃される | health 記録 + prev の partial 表示 |
| nonce の永続性(プロセス再起動) | 直近 1 run 分のみ保持と明記(再起動後は新 run) |

## 見積もり

3 日(v2 の 2 日から増。httpstats 分割と統合テストを含む)。
