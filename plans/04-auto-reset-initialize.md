# 04: /initialize 検知による計測自動開始

対象リリース: v1.2.x / 変更箇所: `isutools.go`(middleware ラッパ)

## 背景

ISUCON13 優勝チームの pprotein は「`/initialize` の最後にフックを挟むことで
自動で計測を開始」する。isutools は現状、ベンチスクリプトが `POST /reset` を
呼ぶ運用が前提(IMPLEMENTATION_STATUS の open item 7 も「bench.sh が
/reset の完了を待つ」ことに依存)。

ISUCON のベンチマーカーは開始時に必ず `POST /initialize` を叩くため、
ここに自動フックできれば **ベンチスクリプト側の統合が不要になり**、
「reset を忘れて前区間のデータが混ざる」事故も消える。

## ゴール

1. `POST /initialize` の成功応答を HTTP ミドルウェアが観測したら、
   自動で reset(全 collector の世代切り替え + 自動 CPU プロファイル開始)
2. アプリコード側から明示的に呼べる公開 API も提供する
3. 既定 off(明示 opt-in)。既存の `POST /reset` 運用は不変

## 非ゴール

- ベンチ終了の自動検知(`POST /save` の自動化)。終了はスコア入力が
  必要なため手動のまま
- initialize 処理自体の計測除外(reset が initialize 後に走るので
  自然に除外される)

## 設計

### トリガー条件

- 環境変数 `ISUTOOLS_RESET_ON_INITIALIZE=1` で有効化
- パスは `ISUTOOLS_INITIALIZE_PATH`(既定 `/initialize`)。完全一致
- 条件: メソッド POST(ISUCON 慣習)かつレスポンスステータス < 400
- **応答完了後**にトリガー(pprotein と同じ「最後にフック」)。
  initialize 内の重い処理(DB 再構築)を計測区間に含めない

### 実装位置

`isutools.go` の Middleware ラッパ(現状 `return httpstats.Middleware(next)`)
を、有効時のみ observer でさらに包む:

```go
func Middleware(next http.Handler) http.Handler {
    h := httpstats.Middleware(next)
    if resetOnInitialize() {
        h = initializeObserver(h)
    }
    return h
}
```

- observer は `httpstats.Middleware` の**外側**(先に実行)に置き、
  ステータス捕捉用に軽量な responseWriter ラッパを使う。
  対象パス以外は分岐 1 回で素通し(オーバーヘッドは path 比較のみ)
- httpstats 内部には手を入れない(責務分離)

### reset の実行方法

admin サーバの `POST /reset` へ **loopback 自己呼び出し**する:

```go
go func() {
    // debounce: 前回の自動 reset から 5 秒未満なら無視
    resp, err := http.Post("http://"+adminAddr()+"/reset", "", nil)
    ...log + health...
}()
```

採用理由:

- web handler の reset 直列化(resetMu)・世代管理・pprof 自動開始を
  そのまま再利用でき、経路が 1 本になる
- admin サーバ無効(`ISUTOOLS_ADDR=off`)時は adminAddr()=="" で
  スキップし health に記録(fail-open)

非同期(goroutine)にするのは、ベンチマーカーの initialize タイムアウト
(多くの回で 20-30 秒制限)に isutools が影響を与えないため。
「initialize 応答 → ベンチ本負荷開始」までは実測で余裕があるが、
万一 reset 完了前に最初のリクエストが来ても、混入するのは数リクエスト分
(既存の手動運用でも同じ性質)。厳密性が必要な場合の同期モード
`ISUTOOLS_RESET_ON_INITIALIZE=sync` も用意する(応答完了後・返却前に
reset を待つ。responseWriter を Flush してから実行)。

### 公開 API(手動フック)

```go
// アプリの initialize handler 末尾で呼ぶ(1 行統合)
func ResetNow(ctx context.Context) error
```

- 中身は同じ自己呼び出し(同期)。middleware 方式が使えない構成
  (isutools の Middleware を挟んでいないルータ)向けの escape hatch

### debounce / 多重防止

- 自動 reset は 5 秒のクールダウン(atomic な last-fired timestamp)。
  ベンチマーカーの initialize リトライで世代が乱れるのを防ぐ
- 手動 `POST /reset` との競合は既存の resetMu 直列化に委ねる

## 実装ステップ(TDD)

1. observer のテスト先行: 対象パス・メソッド・ステータス・debounce・
   既定 off で発火しないこと(httptest + fake admin)
2. `ResetNow` + sync モード
3. health 記録(`collectorHealth.Set("autoreset", ...)`)
4. docs: README(環境変数表 + 「ベンチスクリプト統合不要」の節)、
   INTEGRATION.md の統合手順を「自動」「手動」二本立てに再構成、
   examples/ のベンチスクリプト例にコメント追記

## テスト計画

- unit: POST /initialize 200 → admin へ POST /reset が 1 回飛ぶ
- unit: GET / 404 / 別パス / 500 応答 → 飛ばない
- unit: 5 秒以内の連続 initialize → 2 回目は抑止
- unit: `ISUTOOLS_ADDR=off` → スキップ + health degraded
- integration: sync モードで応答完了後に世代が進んでいること

## リスク

| リスク | 対策 |
|---|---|
| initialize 応答遅延への影響 | 既定 async(応答経路に I/O を挟まない) |
| ベンチ中の誤発火(アプリが /initialize を再利用) | 完全一致 + POST 限定 + debounce + opt-in |
| admin 無効構成 | skip + health で可視化 |
| reset 未完了で本負荷開始 | 実害は数リクエスト混入。厳密には sync モード |

## 見積もり

実装 0.5 日 + docs 0.5 日。
