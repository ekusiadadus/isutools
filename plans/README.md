# 実装計画インデックス(v1.2.0 リリース後)

最終更新: 2026-08-05 / 残り計画: **1 件**

計測ギャップ解消の実装計画は全 11 件だった。**01〜09 と 11 は v1.2.0 で
実装・出荷済み**であり、計画文書は本 pass で削除した。残るのは
**[10 複数台横断計測](./10-multi-host.md)** の 1 件のみである。

| # | 計画 | 状態 | 実体 |
|---|---|---|---|
| 01 | DB target registry | **出荷済み (v1.2.0)** | `sqlstats/registry.go` |
| 02 | Run lifecycle coordinator | **出荷済み (v1.2.0)** | `internal/runctl/` |
| 03 | hoststats | **出荷済み (v1.2.0)** | `hoststats/` |
| 04 | SQL 行効率 | **出荷済み (v1.2.0)** | `sqlrows/` |
| 05 | ネットワーク観測 | **出荷済み (v1.2.0)** | `netstats/`(collector 名は `network`) |
| 06 | DB プール統計 | **出荷済み (v1.2.0)** | `dbpool/` + `isutools.WatchDBPool` |
| 07 | runtime プロファイル | **出荷済み (v1.2.0)** | `web/pprof.go`(`ProfilePair` / `ProfileManifest`) |
| 08 | 計測開始の自動化 | **出荷済み (v1.2.0)**(残件 1、下記) | `isutools.ResetNow*` / `SerializeInitialize` |
| 09 | EXPLAIN 自動化 | **出荷済み (v1.2.0)** | `queryplan/` |
| 10 | **複数台横断計測** | **未実装** | — → [10-multi-host.md](./10-multi-host.md) |
| 11 | nginx transport 検査 + MTU | **出荷済み (v1.2.0)** | `advisor/transport.go`、`netstats` の MTU 列 |

削除した 10 件の計画文書(v6・第5回レビュー反映版)は **git 履歴に残っている**。
設計判断の経緯・撤回した主張・テスト計画・受け入れ条件を読み直す必要が出た場合は、
本 commit の 1 つ前の `plans/` を参照すること。実装の**正**は計画文書ではなく
コードであり、両者が食い違った場合はコードが正しい(v1.2.0 時点で実際に
食い違っていた箇所は下記「出荷時の残件」に列挙した)。

実装・検証の結果は [docs/IMPLEMENTATION_STATUS.md](../docs/IMPLEMENTATION_STATUS.md)
に記録してある。本ファイルは**これから実装する分**だけを扱う。

## 出荷時の残件(計画にあってコードに無いもの)

計画文書を削除するにあたり、**「計画に書いてあったが v1.2.0 では実装しなかった」
項目**をここに残す。これを記録しないと、削除によって未実装であることの記憶ごと
消えてしまう。

| 出所 | 未実装の項目 | 現状のコード | 影響 |
|---|---|---|---|
| 08 §補助(best-effort) | `ISUTOOLS_RESET_ON_INITIALIZE=besteffort`(middleware で initialize 応答を検知して非同期に `ResetNow` を撃つモード)+ `httpstats` observer hook | 同期 API(`SerializeInitialize` + 末尾 `ResetNow`)のみ。guard の外で initialize reset を取った場合は health `initialize-unserialized` が degraded で記録される | 小。計画自身が「besteffort は不変条件 I1 を満たさない暫定手段」と位置づけていた。同期 API が正規経路 |
| 01 §allowlist 表示 | registry の target 一覧と `sqlstats.Notes()` を専用セクションとしてダッシュボードに出す | target ID は `SQL 行効率` / `DB Pool` / `Query Plans` の各セクション経由でのみ見える。`Notes()` はどこからも呼ばれていない | 小。registry の登録失敗理由が UI に出ない |

## 残る計画: 10 複数台横断計測

**規模: raw 26.5 日 / +30% buffer で 34.5 日(≈5〜6 週間)。対象リリース v1.4.0。**
全 11 計画 59.0 日(buffer 込み 77 日)のうち、単独で 45% を占める最大の計画である。
`10-multi-host.md` の §見積もり内訳と本表は一致している(計画側は
`26.5 × 1.3 = 34.45` を「≈34 日」と丸めており、差は丸めのみ)。

構成は 2 形態:

- **(a) embedded `PeerHandler`** — アプリプロセスにライブラリとしてリンクし、
  02 の singleton Controller を共有する。`ISUTOOLS_PEER`(既定 off)+
  Bearer token(`ISUTOOLS_PEER_TOKEN`)+ loopback 強制
- **(b) standalone agent(`cmd/isutools-agent`)** — DB / DNS / proxy ホスト用。
  host/OS/DB セクション(`hoststats` / `network` / `sqlrows`、`accesslog` は
  `--accesslog` 指定時のみ)だけを自プロセスの Controller に登録し、
  `dbinspect` / `queryplan` / `advisor-static` は非 collector セクションとして
  snapshot 構築時に直接組み立てる。in-process セクション(`httpstats` /
  `sqlstats` / `counters` / `dbpool` / `procstats`)は**原理的に提供できず**、
  hub の preflight が形態別セクション能力表で不足を検出する

lease と `sealRun` により、系の停止は 90 秒に有界化する。

## 10 の依存(すべて充足済み)

10 の前提だった 01 / 02 / 03 / 05 / 11 は**すべて出荷済み**である。したがって
10 は「前提待ち」ではなく、着手可能な状態にある。計画が引用している契約が
実際にコードに存在することは v1.2.0 リリース時に確認した。

| 依存先 | 種別 | 契約(コード上の実体) |
|---|---|---|
| 01 registry | **API** | `sqlstats.TargetInfo{ID, Driver, Display, Schema, Purposes}` を targets.json と `PeerInfoDTO.targets` へ写像する。**DSN は wire に出さない** |
| 02 coordinator | **wire への一対一写像** | `RunState` 9 値 / `Validity` 3 値 / `Epoch` / `StartResult` / `FinishAccepted` / `AbortResult` / `RunStatus` / `CollectorBoundary` / `BoundaryWindow`、および sentinel → HTTP status(`ErrRunActive`・`ErrRunTransitioning`→409、`ErrRunAborted`→410、`ErrUnknownRun`→404)。`StartRunOptions{Nonce, Preempt, Reason:"hub", Trigger}`、`AbortResult.Reason = ReasonHubAbort`(`"hub-abort"`)、`AckedByHub` / `AckedByLease` はいずれも `internal/runctl` に**定義済み** |
| 02 の予算 | **引用のみ** | `StartRunBudget` 6s / `FinishSyncBudget` 6s / `FinishLease` 20s / `AbortJoinBudget` 2s / `DrainBudget` 10s / `SnapshotBuildBudget` 5s。hub deadline の下限計算に使う。**02 に対応物のある**状態名・予算値を 10 が独自定義することは禁止(lease・wire budget・fetch/Ack/validate deadline は 10 自身が定義する) |
| (a) embedded peer | **実装順** | 02 + 既存 in-process collector(`httpstats` / `sqlstats` / `counters` / `procstats`)+ 04 `sqlrows` / 06 `dbpool`。`RunRecord.Origin = "peer"` |
| (b) standalone agent | **実装順** | 01 + 03 `hoststats` + 05 `netstats` + 11 の静的 check |

**着手前に確認すべき差分が 1 点ある**: 02 の実体は `internal/runctl` であり、
モジュール外から import できない。10 は「02 の型を wire へ一対一で写す」設計
だが、hub / agent が別モジュールになる場合は公開パッケージ側にミラー型が要る。
v1.2.0 には既に前例がある — `web.BoundaryWindow` などのミラー型と、
`isutools.StartResult`(`= runctl.StartResult` の type alias)である。
wire DTO をどちらの上に定義するかは 10 の着手時に決めること。

## 10 に適用される共通契約(全計画共通の契約から、10 に関係する分)

1. **fail-open(原則)と fail-closed(例外)**。観測の失敗はアプリのリクエスト
   処理を止めない(該当セクションだけを落とし、skip 理由を health に残す)。
   ただし **run 境界の control plane と required participant は fail-closed** で
   ある。10 では次の 2 つが例外にあたる:
   - **hub の `/reset`**: required participant が preflight(接続 / protocol /
     schema / capability / section)または StartRun バリアで失敗した場合、
     **run を開始せず 503 + invalid** を返し、返す**前に**
     `sealRun(SealAbort)` を実行する
   - **peer の認証**: token の不一致・欠落は **401**
     (`ErrorDTO{code:"unauthorized"}` のみ。状態を漏らさない)

   10 の optional participant は常に partial 継続(fail-open 側)。
   判断基準は「その失敗を無視した場合、後でダッシュボードに出る数値が**嘘に
   なるか**」— 嘘になるなら fail-closed、ならないなら fail-open。
2. **feature flag 必須**。`ISUTOOLS_PEER`(embedded peer・**既定 off**。
   listener を増やすため)。flag ではない設定値は
   `ISUTOOLS_PEER_TOKEN` / `ISUTOOLS_AGENT_TARGETS_FILE`。
3. **機能単位 ABBA**。リリース tag 前に「baseline vs 当該機能のみ on」を必ず
   実施する。全体 on/off だけでは追加機能単体の影響を分離できない。
   なお v1.2.0 の ABBA は 2 ブロックにとどまり、`examples/abba.sh` が要求する
   3 ブロック(信頼区間の下限)に届いていない。10 のゲートを設計するときは
   この積み残しを前提にすること(→ IMPLEMENTATION_STATUS.md)。
4. **schema version 契約**。additive(省略可能キーの追加)は bump しない。
   `meta.capabilities` 配列に機能名を追加して宣言する。既存キーの意味変更は
   bump、キーの削除・型変更は bump + 移行注記。peer 互換判定は revision では
   なく `protocol_version` **完全一致** / `schema_version` は **hub ≥ peer** /
   `library_version` は不一致を許容し記録のみ、で行う。
5. **TDD** + 集約カバレッジ 80%(CI ゲート)。`/proc`・`/sys` は FS 注入 +
   `fstest.MapFS`。procfs と sysfs は**別の FS として注入**する。`fs.FS` で
   差し替えられない syscall(statfs / readlink / `filepath.EvalSymlinks`)は
   `Options` の関数シームとして注入する。
6. **advisor 閾値は実測に基づく**。新しい warn 閾値は private-isu での
   フィールド検証を経てから既定有効にする。それまでは表示のみ、または
   provisional と明記する。
7. **ドキュメント**。各 PR に README 環境変数表・`docs/INTEGRATION.md`・
   `docs/IMPLEMENTATION_STATUS.md` の更新を含める。

## レビュー履歴(10 を引き継ぐ人向け)

計画一式は **5 回のレビュー**を経ている。10 は毎回大きく書き換わっており、
現行の `10-multi-host.md` は **v6(第5回レビュー反映版)**である。
経緯を知らずに読むと「なぜここまで複雑なのか」がわからないため、要点を残す。

| 回 | 10 に対する主な差し戻し | v6 での帰結 |
|---|---|---|
| 第1〜2回 | 旧 06「複数台対応」を ADR からやり直し | agent protocol / hub / distributed reset の 3 段階へ再構成 |
| 第3回 | run 境界契約が 02 と食い違っていた | 02 の状態機械を wire へ一対一で写す方針に統一 |
| 第4回 | 単一形態の peer では DB/proxy ホストを賄えない | **embedded peer と standalone agent の 2 形態**へ分離 |
| 第5回 | 系が停止しうる経路が有界でない / 「無条件 fail-open」が hub の 503 と矛盾 | **lease** と **`sealRun`** で停止を 90 秒に有界化。共通契約 1 を原則 + 例外の 2 段に書き換え |

第5回レビュー後、確定前に**ファイル横断の突き合わせ監査を 2 回**実施し、
**23 件の相互矛盾**を検出して反映した。矛盾の類型は 4 つ —(1) 撤回済み
API / 型の参照が残っている、(2) 引用の欠落、(3) 他計画の実装を過小・過大に
述べている、(4) 見積もりの基準値不一致。10 を改訂するときも、02 / 01 の
**コード**と突き合わせる同種の監査を行うこと(v1.2.0 以降は計画文書ではなく
コードが突き合わせ先である)。

見積もりの内訳(第5回レビューでの増分 +9.5 日、17 日 → 26.5 日)は
`10-multi-host.md` §見積もり に残っている。増分の主因は lease / seal
マトリクス / 2 形態 deployment / wire DTO 確定 / 上限と budget の wire 化。

## 調査根拠(レビューでの事実訂正を反映)

10 の動機になった一次情報。事実誤認の訂正を経た版である。

| 記事 | 事実 |
|---|---|
| ISUCON12 優勝 | **NaruseJun**。各インスタンスを Netdata で監視。app/DB 分離、DB 4 台化 |
| ISUCON13 優勝 | **同じく NaruseJun**(二連覇)。全サーバに Netdata。DB・DNS・app を分離。slp の Rows examined/sent 比、/initialize フックで自動計測 |
| ISUCON14 感想戦 (kawaemon) | flamegraph、ロック保持時間計測。**TIME_WAIT 枯渇は競技サーバ側ではなくベンチマーカー側の送信元ポート枯渇** |
| ISUCON10 予選作問 (progfay) | EXPLAIN(Using filesort)、降順/空間インデックス |
| isucandar 解説 (catatsuy) | ベンチマーカー設計論(isutools 側ギャップなし) |

「全サーバに Netdata」が 10 の存在理由であり、単一ホストに閉じた v1.2.0 では
**app と DB を別ホストに分けた瞬間に DB ホストの資源が見えなくなる**という
ギャップが残っている。
