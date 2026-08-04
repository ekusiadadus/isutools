# 07: runtime プロファイル(mutex / block / heap)— 旧02 から分離

種別: 機能 / 対象リリース: v1.3.0 / 依存: 02(run_id を artifact 名に使用) / 変更箇所: `isutools.go`, `web/pprof.go`, `web/web.go`

## 旧計画(旧02)からの変更点

レビュー指摘を反映:

1. **rate の意味論を修正**。
   - `SetBlockProfileRate(r)`: 「r ns 以上のブロックだけ記録」**ではなく**、
     累積 block 時間 r ns あたり平均 1 イベントをサンプルする指定
     (pkg.go.dev/runtime#SetBlockProfileRate)。r=1 で全記録
   - `SetMutexProfileFraction(n)`: 競合イベントの平均 1/n を記録
   - README・INTEGRATION.md の説明文をこの定義で書く
2. **累積性の明示**。mutex/block/heap のサンプルは**プロセス開始からの
   累積**であり、save 時に 1 回取るだけでは run 単位のプロファイルに
   ならない。対応:
   - **reset 時と save 時の 2 点で取得**し、両方を artifact として保存
   - run 範囲の観察は `go tool pprof -diff_base <reset時> <save時>` で行う
     (INTEGRATION.md に具体コマンドを記載)
   - 各 artifact のメタ(取得時刻・run_id・累積範囲がプロセス開始からで
     あること)をファイル名と snapshot の注記で明示
3. **heap の扱いを訂正**。「GC 済みの点観測」とするための明示的
   `runtime.GC()` は**呼ばない**(アプリへの影響がある)。
   heap profile は「直近 GC 時点の live heap + 累積 alloc」であることを
   注記して提供する
4. **拡張子を `.pprof` に統一**。現行 `/files/` handler は
   `.html` / `.json` / `.pprof` のみ配信する(web/web.go:610 付近で確認済み)。
   旧計画の `.pb.gz` では一覧にも配信にも載らない。
   命名は既存 CPU profile(`<ts>_gen<N>_cpu.pprof`)に合わせ
   `<ts>_gen<N>_<runid8>_{mutex,block,heap}_{reset,save}.pprof`
5. **PR 分割**(レビュー要求): (a) 有効化 + health、(b) artifact 保存

## ゴール

1. opt-in で mutex/block/heap プロファイルを有効化できる
   (**3 種とも既定 off** — 既定構成のオーバーヘッドはゼロのまま)
2. 有効時、reset / save の 2 点で mutex・block・heap を DataDir に
   atomic に保存し、`/files/` から取得できる
3. run 範囲の見方(diff_base)がドキュメント化されている

## 設計

### PR-a: 有効化

- `ISUTOOLS_MUTEX_FRACTION`(既定 0 = off。README 推奨値 100 と
  「1/100 サンプリング」の正しい説明)
- `ISUTOOLS_BLOCK_RATE_NS`(既定 0 = off。README 推奨値と
  「累積 block 時間 r ns あたり 1 サンプル」の正しい説明、
  低い値ほど高コストである旨)
- 設定箇所は **02 の process-wide singleton runtime の初期化**
  (v4 修正: `startAdmin()` では admin 無効時に設定されず、
  singleton Controller 方針と矛盾していた)。設定値は health(`profile`)に
  記録し snapshot の meta.health で確認できる
- **env 未設定時はアプリ自身が設定した既存の profile rate を
  上書きしない**(v4 追加の契約): env が空なら
  SetMutexProfileFraction / SetBlockProfileRate を一切呼ばない

### PR-b: artifact 保存

- 取得点:
  - reset 完了直後(02 coordinator の post-reset hook)
  - save 時(snapshot 永続化と同じ場所)
- **区間の意味の明示**(v4 追加): profile は**プロセス全体の累積**であり
  HTTP 世代と厳密には一致しない。特に ResetNow(08)経由の reset 時
  profile には**まだ実行中の initialize handler 後半が混入する**。
  表示・docs に「process-wide cumulative(HTTP 世代 profile ではない)」と
  取得タイミングの不確実性を明記する
- 内容: `rpprof.Lookup("mutex"|"block"|"heap").WriteTo(f, 0)`
  - mutex/block は rate=0(無効)なら**書かない**
  - **heap も `ISUTOOLS_HEAP_PROFILE=1` の明示 opt-in・既定 off**
    (v3 修正。大きな heap の WriteTo は initialize 遅延やメモリ/cache
    状態へ影響し得るため、「既定構成のオーバーヘッドゼロ」と矛盾しない
    ように既定 off にする)
- **atomic publication**(v3 追加): `0600` の一時ファイル
  (`.pprof.tmp` — 現行 files 一覧・配信の対象外拡張子)へ書き、
  Close 成功後に `.pprof` へ rename する。失敗時は temp を削除。
  未完成ファイルが一覧に出ることを構造的に防ぐ
- **保持上限**(v3 追加): profile artifact は直近 20 run 分・
  合計 512MiB を上限とし、超過分は古い run から削除する
  (既存 snapshot html/json は対象外)。方針を INTEGRATION.md に明記
- run manifest: snapshot の Meta に当該 run の artifact ファイル名一覧を
  additive で記録する(完成した rename 済みファイルのみ)
- 失敗は log + health degrade(fail-open)。DataDir 未設定なら全スキップ
- ダッシュボード: files 一覧に載る。Runs 詳細に「Profiles」小節を足し、
  reset/save ペアと diff_base コマンド例を表示する
- **reset 所要時間の計測**(v3 追加): profile 無効時と有効時
  (heap on / mutex on / block on)を分けて実測し、ABBA 結果とともに
  README へ記録する(「数 ms」を仮定で書かない)

### capabilities / flag

- capabilities: `runtime-profiles`
- 機能単位 ABBA: `ISUTOOLS_MUTEX_FRACTION=100` 単独 on、
  `ISUTOOLS_BLOCK_RATE_NS=100000` 単独 on をそれぞれ計測して
  READMEに実測値を記録(「block は診断時のみ」の根拠を実測で示す)

## 実装ステップ(TDD)

1. PR-a: env パース(不正値は off + health warn)・設定反映・health
2. PR-b: 保存(rate=0 で mutex/block を書かない・**heap は
   ISUTOOLS_HEAP_PROFILE=1 のときのみ書く**・reset/save の 2 点・
   ファイル名規約・capture window メタ)・files 配信テスト
3. docs: INTEGRATION.md「ロック競合の診断」節
   (rate 意味論、diff_base 手順、`go tool pprof -http` の見方)

## テスト計画

- unit: env 境界(0 / 負値 / 非数 → off)
- integration: fraction>0 で reset→save 後に mutex の reset/save ペアが
  DataDir に存在し `/files/` で 200
- integration: **既定(全 off)では artifact が一切生成されない**こと
  (heap も off — v3 修正)
- integration: 書き込み途中で失敗させ、`.pprof` が残らず temp が
  削除されること / 保持上限超過で古い run の artifact だけが消えること

## リスク

| リスク | 対策 |
|---|---|
| block profile の高コスト設定 | 既定 off + 推奨値と実測値を README に併記 |
| プロファイル取得自体の stop-the-world | v5 で整理: profile 取得は**境界後の近似観測**と位置づける(StartRun budget に含めず、02 の境界順序も拘束しない)。取得ごとに実測 capture window(開始/終了時刻)を artifact メタに保存し、取得失敗・遅延は profile 側の partial として記録する |
| ファイル数の増加 | 保持上限契約(直近 20 run・合計 512MiB、超過は古い run から自動削除)に従う(v5: 「手動削除」表記を撤回し PR-b の retention 契約に一本化) |

## 見積もり

PR-a 0.5 日 + PR-b 1 日 + docs 0.5 日。
