# 08: 計測開始の自動化 — 再設計版

種別: 機能 / 対象リリース: v1.3.0 / 依存: 02(ResetCoordinator) / 変更箇所: `isutools.go`, `httpstats`(observer hook)

## 旧計画(旧04)からの変更点

レビューの CRITICAL/HIGH 指摘を反映:

1. **既定 async の廃止**。/initialize のレスポンスを受けたベンチマーカーは
   直ちに本負荷を開始できるため、非同期 reset はその先頭リクエスト群と
   競合し計測境界を保証しない。「既存の手動運用でも同じ」は誤り
   (正式運用は POST /reset の**応答を待って**からベンチ開始)
2. **「Flush してから reset」の削除**。Flush した時点でクライアントは
   レスポンスを受信でき、本負荷を開始できる。逆だった
3. **HTTP 自己呼び出しの全廃**。0.0.0.0 bind 構成で実 bound address への
   自己呼び出しが Host 検査に拒否され得るうえ、timeout・proxy 環境変数・
   response body close の契約も不足していた。
   → HTTP handler と公開 API が**共通の内部 `ResetCoordinator.Reset(ctx)`
   を直接呼ぶ**(02 で導入済みの経路)
4. **同期 API を主軸に、middleware 検知は best-effort に格下げ**
5. **二重 responseWriter の廃止**。現行 wrapper は Flusher / Hijacker /
   ReaderFrom / Unwrap の透過契約と回帰テストを持つ。新しい軽量 wrapper を
   重ねる代わりに、**既存 httpstats wrapper の内部から observer へ
   status を通知**する
6. **5 秒 debounce の廃止**。短時間の正当な再ベンチを抑止してしまう。
   initialize リクエスト単位の一意性で扱う

## モード設計

### 推奨(正確): 明示同期 API

```go
// アプリの initialize handler 末尾、レスポンス送信前に呼ぶ:
func initializeHandler(w http.ResponseWriter, r *http.Request) {
    // ... DB 再構築など ...
    if runID, err := isutools.ResetNow(r.Context()); err == nil {
        log.Printf("isutools run %s", runID)
    }
    w.WriteHeader(http.StatusOK)  // reset 完了後に応答
    // ...
}
```

- `ResetNow` は 02 の coordinator を直接呼ぶ同期実行。
  応答がクライアントへ届く前に世代が切り替わるため、
  **本負荷の先頭から新世代で計測される**(pprotein と同等の保証)
- snapshot の run に `reset_trigger: "api"` を記録

### 補助(best-effort): middleware 検知

- `ISUTOOLS_RESET_ON_INITIALIZE=besteffort` で有効化(既定 off)。
  対象: `ISUTOOLS_INITIALIZE_PATH`(既定 `/initialize`)への POST が
  status < 400 で完了したとき
- 実装: httpstats の既存 responseWriter に **observer callback**
  (パッケージ変数でなく Middleware オプション)を追加し、
  isutools.go 側でパス・メソッド・status を判定して
  coordinator.Reset を**非同期**に呼ぶ
- **保証しないことを明示する**: 先頭数リクエストが前世代に混入し得る。
  run に `reset_trigger: "initialize-besteffort"` を記録し、
  ダッシュボードの run ヘッダに「境界非保証」バッジを表示する
  (汚染された run を後から識別できることが要件)
- 用途: アプリ改修が一切できない状況の暫定手段。INTEGRATION.md では
  同期 API を第一に案内する

### 多重発火の扱い

- coordinator は直列化済み(02)。同一 initialize リクエストからの発火は
  構造上 1 回
- ベンチマーカーの initialize リトライ(=複数リクエスト)は
  それぞれ新しい run を開始する(最後の initialize が有効になる。
  正しい挙動としてドキュメント化)
- 進行中の Reset と重なった場合は coordinator の直列化に従う
  (coalesce しない。run_id はそれぞれ発番される)

### multi-app / multi-host

- 複数アプリインスタンス・複数ホストへの reset 伝播は 10 の
  distributed reset で扱う(本計画は単一プロセスの境界のみ)

## 実装ステップ(TDD)

1. httpstats: observer オプション追加(既存の透過契約テストを
   observer 有効状態でも全通しする回帰テストを含む)
2. `ResetNow`(02 の公開のみ。実体は coordinator)+ reset_trigger 記録
3. besteffort モード判定(パス・メソッド・status・既定 off)
4. template: 境界非保証バッジ
5. docs: INTEGRATION.md を「同期 API(推奨)/ besteffort(暫定)」の
   二段構成で記載。examples の initialize handler 例を追加

## テスト計画

- unit: POST /initialize 200 → coordinator.Reset 呼び出し 1 回
  (fake coordinator で検証)
- unit: GET / 別パス / 500 / モード off → 呼ばれない
- unit: ResetNow が同期で世代を進めること(呼び出し完了時点で
  generation が +1)
- integration: besteffort run の snapshot に trigger 種別が出ること
- 回帰: httpstats の Flusher/Hijacker/ReaderFrom/Unwrap テスト全通し

## リスク

| リスク | 対策 |
|---|---|
| ResetNow の呼び忘れ・呼び場所誤り | INTEGRATION.md に「レスポンス送信前」を強調した例。besteffort が保険 |
| initialize の応答遅延(同期 reset 分) | reset は数 ms オーダー(実測を doc に記録)。initialize 制限時間(20-30s)に対して無視可能 |
| besteffort の誤用(正式計測に使う) | run へのトリガー記録とバッジで判別可能にする |

## 見積もり

1 日 + docs 0.5 日。
