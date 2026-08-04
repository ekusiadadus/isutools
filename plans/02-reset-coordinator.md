# 02: Reset coordinator(run 契約の導入)

種別: 基盤 / 対象リリース: v1.2.0 / 変更箇所: `web`(内部 coordinator)、`isutools.go`

## 背景(レビュー指摘)

現行の reset は web handler 内で collector ごとに**逐次**切り替えるだけで:

- run を識別する ID がない
- 「どの collector がいつ reset されたか」の証拠が snapshot に残らない
- collector 間の区間ズレ(skew)が観測できない
- IMPLEMENTATION_STATUS の open item 7(cross-collector shared generation
  gate)として既知の未解決事項

旧 04(auto-reset)と旧 06(distributed reset)はどちらもこの契約の
上に成り立つため、基盤として先行する。

## ゴール

1. coordinator が一意な run_id を生成し、reset の全過程を
   `ResetResult` として記録する
2. snapshot に「その世代を開いた reset の結果」を **immutable** に保存する
3. 各 collector の実測区間(started/ended)と最大 skew を表示する
4. run の状態(valid / partial / invalid)を定義する
5. HTTP handler と公開 API が**同一の内部 API** を直接呼ぶ
   (HTTP 自己呼び出しをどこにも作らない)

## 非ゴール

- 複数ホストへの reset 伝播(→ 10。ただし本計画の契約を前提にする)
- ベンチ開始の強制(bench が reset 応答を待つことは運用契約。
  ドキュメントで明記し、snapshot 側で検証可能な証拠を残すのが本計画)

## 設計

### データモデル

```go
// internal/runctl(新規)
type ResetResult struct {
    RunID       string           `json:"run_id"`      // <unixnano>-<seq>-<hosthash8>
    StartedAt   time.Time        `json:"started_at"`
    CompletedAt time.Time        `json:"completed_at"`
    Collectors  []CollectorReset `json:"collectors"`
    State       RunState         `json:"state"`       // valid | partial
}

type CollectorReset struct {
    Name        string    `json:"name"`        // sql / http / accesslog / proc / counters / db
    StartedAt   time.Time `json:"started_at"`
    CompletedAt time.Time `json:"completed_at"`
    Err         string    `json:"err,omitempty"`
}

type RunState string // "valid" | "partial" | "invalid"
```

- `RunID` は coordinator 生成。ホスト短縮ハッシュを含み、10 の
  複数ホスト構成でも衝突しない形式を最初から採る
- collector reset の失敗は当該 collector を `Err` 付きで記録し、
  run 全体を `partial` にする(現行の fail-open は維持しつつ証拠を残す)
- `invalid` は本計画では未使用(10 の「必須 peer 欠落」用に予約し、
  型と表示だけ先に定義する)

### snapshot への反映

- `Snapshot.Meta` に additive で追加:
  - `run_id`、`reset` (ResetResult)、`max_skew_ms`
    (全 collector の StartedAt の最大差)
- 各 collector の実測区間: procstats は既に StartedAt/EndedAt を持つ。
  sql/http/accesslog にも世代開始時刻を持たせ、snapshot 時刻との対で
  表示する(collector 側は開始時刻の記録のみの小変更)
- ResetResult は世代生成時に確定し、以後の snapshot では**再計算しない**
  (immutable)。`POST /save` で保存される snapshot にそのまま残る

### API 統合

```go
// web パッケージ内
func (h *handler) coordinator() *runctl.Coordinator

// POST /reset handler は coordinator.Reset(ctx) を呼ぶだけにする
// 公開 API(08 で使用):
//   isutools.ResetNow(ctx) (runID string, err error)
//   → web handler と同じ coordinator インスタンスを直接呼ぶ
```

- coordinator は既存の resetMu 直列化を引き継ぐ(同時 Reset は
  後続が先行の完了を待つ。coalesce はしない — run_id が別になるため)
- `POST /reset` の応答: 既存の 204 を維持しつつ
  `X-Isutools-Run-Id` ヘッダを追加(既存テスト・bench スクリプトを
  壊さない additive な変更)。JSON が欲しい場合のために
  `POST /reset?format=json` で `{"run_id": ...}` 200 を返す
- bench 運用契約を INTEGRATION.md に明文化:
  「ベンチ開始は POST /reset の応答受信後。応答前に送った負荷は
  前世代に計上される」+ examples/ の bench.sh 例を更新

### isutools.go / Provider

- `web.Provider` の reset 経路(SQL rotate、collector Reset 群)を
  coordinator に登録する形へ再配線。既存の provider callback 形式は
  維持し、coordinator が名前付きで包む

## 実装ステップ(TDD)

1. runctl: run_id 生成(一意性・ホストハッシュ)・ResetResult 構築・
   partial 判定のテスト先行
2. web handler の再配線(既存 reset テストが回帰検知。204 維持)
3. Meta への additive 反映 + max_skew 計算 + template 表示
   (Runs ヘッダに run_id と state)
4. `ResetNow` 公開(08 の前提 API)
5. docs: INTEGRATION.md の bench 契約、IMPLEMENTATION_STATUS の
   open item 7 を「単一ホストは解決、複数ホストは 10」と更新

## テスト計画

- unit: collector 1 つが失敗 → State=partial、他 collector は完了
- unit: 並行 Reset 2 本 → 直列化され run_id が 2 つ、世代も 2 進む
- unit: max_skew 計算(順序・単調時計)
- integration: reset→snapshot→save で ResetResult が不変のまま
  保存されること / v3 スナップショット(run なし)の読み込み互換

## リスク

| リスク | 対策 |
|---|---|
| reset 経路の再配線での挙動退行 | 既存 reset/generation テストが厚い。204 と世代セマンティクスを不変に保つ |
| 時計後退(NTP)で skew が負値 | monotonic clock(time.Since 系)で計測し、絶対時刻は表示用のみ |
| collector 追加時の登録漏れ | coordinator 登録を Provider 配線の必須経路にし、未登録 collector を health で警告 |

## 見積もり

2 日。
