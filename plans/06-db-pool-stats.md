# 06: DB プール統計(表示のみ)— 旧02 から分離(v3)

種別: 機能 / 対象リリース: v1.2.x / 依存: 01(TargetID)、02(区間 baseline の契約) / 新規パッケージ: `dbpool`

## 旧計画(旧02)からの変更点

レビュー指摘を反映:

1. **advisor 閾値を v1 から全廃**。旧計画の判定式は成立していなかった:
   - `WaitDuration` は**並列 wait の合計**であり、wall interval の 1% との
     単純比較は意味を持たない(64 並列なら interval の 64 倍まで積み上がる)
   - 「過去に wait した」ことと snapshot 時点の `Open == MaxOpen` は
     因果を結び付けられない
   - プール増加が常に正しいとは限らない(DB 側過負荷を悪化させ得る)
   - まず統計を表示し、advisor 閾値は private-isu 実測後に別 PR で導入
2. **API が error を返す**(旧計画は「登録エラーにする」と書きながら
   例に戻り値が無かった)
3. **`MaxIdleTimeClosed` を追加**(欠落していた。database/sql の
   DBStats 全フィールドを網羅)
4. **lifetime churn の「Count 比」廃止**(未定義だった)

## ゴール

アプリが 1 行で `*sql.DB` を登録すると、ベンチ区間のプール統計が
snapshot に表示される。判断は読者(+将来の advisor)に委ねる。

## API(v3 修正)

```go
// isutools パッケージ。第一引数は 01 の TargetID(同じ名前空間で
// sqlrows / dbinspect / queryplan と結合する)。
// 任意 interface は受けない(typed-nil / panic / blocking 実装の混入を
// 防ぐため *sql.DB に限定。sqlx 等は .DB を渡す)。
func WatchDBPool(targetID string, db *sql.DB) error   // 重複 ID・nil はエラー。上限 16
// v5: targetID は 01 の registry に登録済みの TargetID のみ受理する
// (手書き文字列の typo で 04/09 と結合できなくなるのを防ぐ)。
// 未登録 ID はエラー。不一致ケースをテストに含める。
func UnwatchDBPool(targetID string) error             // プール再作成時は Unwatch → Watch
```

使用例(README の Minimal integration に追記):

```go
db, _ := sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
if err := isutools.WatchDBPool("mysql-db1_3306-isuconp", db.DB); err != nil { log.Print(err) }
```

ライフサイクル(v3 で定義):

- **run 途中の Watch/Unwatch は run 状態自体を partial にする**
  (entry の Partial だけでは「baseline が run 開始より遅い」ことが
  run 全体の評価に伝わらないため。02 の coordinator に通知する)
- Unwatch 済み ID の再 Watch は新しい baseline で開始
- feature flag: `ISUTOOLS_DBPOOL=off` で登録を無効化(既定は
  Watch された場合のみ有効)。機能単位 ABBA 用の kill-switch

## データモデル

```go
type Entry struct {
    Name              string        `json:"name"`
    // 点観測(snapshot 時)
    MaxOpen           int           `json:"max_open"`
    Open              int           `json:"open"`
    InUse             int           `json:"in_use"`
    Idle              int           `json:"idle"`
    // 区間デルタ(reset 時 baseline との差。02 の世代境界に従う)
    WaitCount         int64         `json:"wait_count"`
    WaitDuration      time.Duration `json:"wait_duration_ns"` // 並列 wait の合計(表示に注記)
    MaxIdleClosed     int64         `json:"max_idle_closed"`
    MaxIdleTimeClosed int64         `json:"max_idle_time_closed"`
    MaxLifetimeClosed int64         `json:"max_lifetime_closed"`
    Partial           bool          `json:"partial,omitempty"` // カウンタ後退(プール再作成)検出
}
```

- baseline は 02 coordinator の reset hook で取得(collector として登録)
- カウンタ後退(アプリがプールを作り直した等)は当該 entry を
  Partial にし、current 値をそのまま表示(値の意味を注記)
- `WaitDuration` の表示ラベルに「並列 wait の合計(wall 時間ではない)」を
  固定注記。`WaitCount > 0` なら `平均 wait = WaitDuration / WaitCount` を
  併記(これは分布に依存しない安全な導出)

## advisor(v1 では実装しない)

将来 PR の候補シグナルとして記録のみ(private-isu 実測で評価してから):

- 平均 wait(WaitDuration/WaitCount)が SQL p95 と同オーダー
- WaitCount / (WaitCount + 総クエリ数) の比
- MaxLifetimeClosed が大きい(SetConnMaxLifetime 過小)

いずれも「プールを増やせ」と短絡しない文面にする(DB 側飽和の可能性を
併記する)ことを要件にする。

## 実装ステップ(TDD)

1. dbpool: fake Statser でデルタ・後退 Partial・登録上限・重複・nil を
   テスト先行
2. isutools.go: `WatchDBPool` 公開 + 02 coordinator への collector 登録
3. web: Provider `DBPools func() []dbpool.Entry` + Connections セクションに
   「DB Pool」表(注記文言含む)
4. docs: README(1 行統合)+ INTEGRATION.md
5. 単独 ABBA(`Stats()` は atomic 読みのみ。理論ゼロを実測で確認)

## テスト計画

- unit: baseline 未取得(登録が reset 後)→ 初回 snapshot は登録時点比
- unit: 複数プール(shard 4 本)の並記
- integration: snapshot JSON / HTML に注記文言が出ること

## リスク

| リスク | 対策 |
|---|---|
| 登録忘れで空表示 | セクション自体を出さず、health に「WatchDBPool 未登録」を info で記録 |
| プール再作成の検出漏れ(たまたま単調増加) | 完全検出は不可能。Partial は best-effort と doc に明記 |

## 見積もり

1 日。
