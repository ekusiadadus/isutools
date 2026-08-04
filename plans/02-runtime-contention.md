# 02: ロック競合・DB プール待ちの計測

対象リリース: v1.2.0 / 変更箇所: `isutools.go`, `web/pprof.go`, 新規 `dbpool`

## 背景

ISUCON14 感想戦記事は Mutex 保持時間の計測を自作し、
`tokio::sync::Mutex` → `parking_lot` 移行や RwLock→dashmap 置換の判断に
使っている。Go では標準の mutex/block プロファイルで同等の情報が取れるが、
isutools は `runtime.SetMutexProfileFraction` / `runtime.SetBlockProfileRate`
を**設定しておらず**、`/pprof/` の mutex・block エンドポイントは実質空。
自動採取も CPU プロファイルのみ(`web/pprof.go` の StartCPUProfile)。

また DB 接続プールの詰まり(`sql.DBStats` の WaitCount/WaitDuration)は
プール上限チューニング(ISUCON12: db pool 調整で 30 万点到達)の直接証拠だが、
isutools はドライバをラップするだけで `*sql.DB` ハンドルを持たず未収集。

## ゴール

1. opt-in で mutex/block プロファイルを有効化できる
2. `POST /save` 時に mutex / block / heap プロファイルを自動保存し、
   ダッシュボードの files 一覧から取得できる
3. アプリが 1 行で `*sql.DB` を登録すると、ベンチ区間の
   プール待ち回数・待ち時間・使用状況が snapshot に出る
4. プール待ちが顕著なら advisor が warn を出す

## 非ゴール

- goroutine dump の自動採取(必要なら /pprof/ から手動で取れる)
- フレームグラフの内蔵レンダリング(`go tool pprof -http` を案内)

## 設計

### 2a. mutex / block プロファイル有効化

- 環境変数(両方とも既定 0 = off。**既定オーバーヘッドゼロを維持**):
  - `ISUTOOLS_MUTEX_FRACTION`: `runtime.SetMutexProfileFraction(n)` に渡す。
    推奨値 100(1/100 サンプリング、実測コストほぼゼロ)
  - `ISUTOOLS_BLOCK_RATE_NS`: `runtime.SetBlockProfileRate(n)` に渡す。
    推奨値 100000(100µs 以上のブロックを記録)。block は mutex より
    高コストのため README で「診断時のみ」と明記
- 設定箇所: `startAdmin()` 内(adminOnce と同じライフサイクル)。
  `ISUTOOLS=off` 時は当然設定しない
- health: 有効化した場合は `collectorHealth.Set("profile", ...)` に
  設定値を記録し、snapshot の meta.health から確認可能にする

### 2b. save 時のプロファイル自動保存

- `POST /save` のスナップショット永続化と同じタイミングで、DataDir へ
  `<run-id>-mutex.pb.gz` / `<run-id>-block.pb.gz` / `<run-id>-heap.pb.gz`
  を `rpprof.Lookup(name).WriteTo(f, 0)` で書き出す
- mutex/block は rate 未設定(=空)なら書かない。heap は常に書ける
  (GC 済みヒープの点観測。sync.Pool 化・アロケーション削減の判断材料)
- 既存の files 一覧(DataDir 列挙)にそのまま載る。追加 UI 不要
- 失敗は log + health degrade で握りつぶす(fail-open)

### 2c. DB プール統計(新パッケージ `dbpool`)

アプリ側 API(README の Minimal integration に追記):

```go
db, _ := sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
isutools.WatchDBPool("main", db.DB)   // 追加 1 行
```

```go
// dbpool パッケージ
type Entry struct {
    Name            string        `json:"name"`
    MaxOpen         int           `json:"max_open"`
    Open            int           `json:"open"`
    InUse           int           `json:"in_use"`
    Idle            int           `json:"idle"`
    WaitCount       int64         `json:"wait_count"`        // 区間デルタ
    WaitDuration    time.Duration `json:"wait_duration_ns"`  // 区間デルタ
    MaxIdleClosed   int64         `json:"max_idle_closed"`   // 区間デルタ
    MaxLifetimeClosed int64       `json:"max_lifetime_closed"`
}
```

- `WatchDBPool` は `interface{ Stats() sql.DBStats }` を受ける
  (`*sql.DB` を直接要求しない: sqlx 等のラッパでも `.DB` を渡せば良い)
- WaitCount/WaitDuration/…Closed は累積カウンタなので、reset 時に
  baseline を取り snapshot でデルタ表示(procstats と同型)。
  Open/InUse/Idle/MaxOpen は snapshot 時点の点観測
- 複数登録可(sharding 構成: ISUCON12 の user-shard 4 DB を想定)。
  name 重複は後勝ちでなく登録エラー(健全性のため)
- web.Provider に `DBPools func() []dbpool.Entry` を追加。
  template は Connections セクションに「DB Pool」表を追加

### 2d. advisor 統合

`advisor.WithDBPools(checks []Check, pools []dbpool.Entry, interval time.Duration) []Check`:

- `dbpool-wait`: WaitDuration > interval の 1% または
  (WaitCount > 0 かつ Open == MaxOpen) → warn。
  「SetMaxOpenConns/SetMaxIdleConns をコア数・MySQL max_connections と
  合わせて引き上げる(サーバ側 max_connections check と突き合わせる)」
- `dbpool-lifetime-churn`: MaxLifetimeClosed が Count 比で大きい → info
  (SetConnMaxLifetime が短すぎて再接続を繰り返している)
- 登録なし → skip(「WatchDBPool で *sql.DB を登録すると検査できます」)

## 実装ステップ(TDD)

1. `dbpool`: fake Stats 実装でデルタ・複数登録・名前衝突をテスト先行
2. advisor `WithDBPools` 閾値テスト
3. isutools.go: `WatchDBPool` 公開 + profile 環境変数 2 つ + health 記録
4. web: Provider 配線 + save 時のプロファイル書き出し
   (rate 未設定時に mutex/block を書かないことをテスト)
5. docs: README(1 行統合例と環境変数表)、INTEGRATION.md
   (「ロック競合の診断」節: fraction/rate 推奨値、`go tool pprof` の見方)

## テスト計画

- unit: dbpool デルタ / baseline 未取得時 / カウンタ後退(プール再作成)
- unit: advisor 閾値境界(interval 1%、Open==MaxOpen)
- integration: save 後の DataDir に heap が常にあり、mutex は
  fraction>0 のときだけあること
- ABBA: 既定(全 off + WatchDBPool のみ)で overhead 差なしを確認。
  Stats() は atomic 読みのみで理論上ゼロ

## リスク

| リスク | 対策 |
|---|---|
| block profile のオーバーヘッド | 既定 off + README で「診断時のみ」明記 |
| プール再作成でカウンタ後退 | 後退検出で baseline 再取得(デルタ 0 扱い) |
| WatchDBPool の呼び忘れ | advisor skip の detail で登録方法を案内 |
| DataDir 未設定で save 時に書けない | 既存 CPU プロファイルと同じ分岐(スキップ) |

## 見積もり

2a+2b 0.5 日、2c+2d 1 日、docs/検証 0.5 日。
