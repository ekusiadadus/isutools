# 計測ギャップ解消 実装計画 v6(2026-08-05 第5回レビュー反映版)

初版(5fbe54c)→ v2(基盤+機能へ再構成)→ v3(run 境界契約)→
v4(0b68d6c、2 契約分離)→ v5(run lifecycle 完全化)と改訂し、
v5 も第5回レビューで **CRITICAL 5 件・HIGH 10 件・MEDIUM 12 件**により
差し戻された。本 v6 はその全件対応版である。

v6 の中核変更:

- **01** が「1 TargetID = 1 DSN」を撤回し、`Purpose`(app / stats / explain)で
  **1 論理 target に複数 credential** を紐づける。統計・EXPLAIN 用接続は
  既定データベースを持たない(計測区間の自己汚染防止)
- **02** が run lifecycle に **epoch fencing / `Committed` / 階層予算 /
  `BoundaryWindow` / preempt + `SerializeInitialize`** を追加し、
  collector 契約を **不変 handle + `Collect(base, final)`** に統一した
- **10** が peer を **embedded `PeerHandler`(アプリ内)** と
  **standalone agent(DB/proxy ホスト)** の 2 形態に分離し、
  **lease** と **`sealRun`** で系の停止を 90 秒に有界化した
- **共通契約 1** に fail-closed 例外を定義した(下記 §全計画共通の契約 1)

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[MEDIUM] 共通契約 1「fail-open」が 08 / 10 と矛盾していた**。
   v5 は「計測失敗はアプリを止めない」を**全計画に無条件で**適用していたが、
   08 v6 は `StartResult.Validity == ValidityInvalid` で **HTTP 500** を返し
   (log-and-continue を禁止事項に格上げ)、10 v6 は required participant の
   preflight / StartRun 失敗で **run を開始せず 503 + invalid** を返す。
   「無条件 fail-open」という v5 の記述は**撤回する**。
   → 共通契約 1 を **原則(fail-open)と例外(fail-closed)の 2 段**に書き換え、
   どのコンポーネントがどちらに属するかを名指しで固定した
   (§全計画共通の契約 1)
2. **[MINOR] 依存関係節の見出しと内容が v4 のまま**だった。
   → **§依存関係(v6)** に改題し、v6 で確定した契約
   (01 の purpose 別 inspector credential、02 の lifecycle + 階層予算、
   10 の embedded peer / standalone agent 分離)を**契約名つきの表**に
   書き換えた(§依存関係)
3. **[MINOR] 見積もりが再算定されていなかった**。v5 の表は 01=2 / 02=5 /
   03=2 / 04=2.5 / 05=1.5 / 06=1 / 07=2 / 08=1.5 / 09=3 / 10=17 / 11=1 日
   (raw 合計 38.5 日)を前提にしており、v6 で**全 11 計画すべての
   日数が増えた**ため全リリースが実態と乖離していた。
   → 各計画の §見積もり の**実数を読み直して再構築**し、raw(実装)と
   +30% buffer 後の両方を表示した(§リリース対応)
4. **[MINOR] 第5回レビューの全指摘に対する対応表を新設**した
   (§第5回レビュー指摘 → v6 対応の対応表)。第1〜4回の表は
   履歴として**そのまま残す**
5. **[MINOR] 本文の陳腐化を修正**。ヘッダを v5 → v6、実装順序表の
   10 の規模「2〜3 週間」→ **5〜6 週間**、07 の採取点「reset/save の 2 点」→
   **open/close の 2 点**、05 を **新規パッケージ `netstats`**、
   feature flag 一覧に **`ISUTOOLS_PEER`(10・既定 off)** を追加した
6. **[v6 監査反映] v6 一式に対する 2 回の監査で見つかった相互矛盾を解消**した。
   本ファイルの修正は 5 点 —(a)「04 は `Sample()` を直接は要求しない」の
   **撤回**(04 の `Collect` は `pending` を読まないため 03/04/05/06 の 4 計画
   すべてが要求する。§依存関係)、(b) `runctl.GenerationCollector` の引用に
   欠けていた **`Release`** の追加(§依存関係)、(c)「10 は状態名も予算値も
   独自定義しない」を「**02 に対応物のある**ものを独自定義しない」へ限定
   (10 は lease / wire budget / fetch・Ack・validate deadline を自前で定義
   している。§依存関係)、(d) **05 の v5 基準値 1.75 → 1.5 日**への訂正
   (MTU 列の二重計上の解消。v5 raw 合計 38.75 → **38.5 日**、増分 +20.25 →
   **+20.5 日**。v6 側の各リリース値と合計 59.0 / 77 日は不変。§リリース対応)、
   (e) 第3回履歴表の `PreviousRunResult` に **v6 では `StartResult` に一本化**
   された旨の注記(§第3回レビュー指摘)。経緯は §第5回レビュー後の監査反映

### v5 から撤回する主張(本ファイル)

| v5(本ファイル)の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| 共通契約 1「**fail-open**: 計測失敗はアプリを止めない」(無条件) | 08 の `/initialize` 500 と 10 hub の `/reset` 503 が契約違反に見える。run 境界の失敗を握り潰すと「一見 valid な嘘の run」が残る | 原則 fail-open + **run 境界 control plane と required participant は fail-closed**(§全計画共通の契約 1) |
| 「## 依存関係(v4: 依存種別を明示)」 | 見出しの版数が v4 のまま。表の内容も `Inspect(ctx, id, fn)` 時代の 01 と、handle を持たない 02 collector 契約を前提にしていた | §依存関係(v6)。契約名・API 署名つきの表へ全面改訂 |
| 「10: agent protocol / hub / distributed reset の 3 段階、**2〜3 週間規模**」 | 10 v6 の見積もりは 26.5 日(≈5〜6 週間)。lease / seal / 2 形態 deployment / wire DTO 確定を含む | 「embedded peer と standalone agent の 2 形態 + lease + seal。**5〜6 週間**」 |
| 「07: reset/save の 2 点 + diff_base 手順」 | 07 v6 は採取点を **freeze 直後(close)** へ移し `_reset` / `_save` を `_open` / `_close` に改名した | 「**open/close** の 2 点 + `-diff_base` 手順 + 残差の実測表示」 |
| 見積もり表の `05(1.75日※)` / `10 = 17 日` ほか全数値 | 全 11 計画が v6 で増えた。加えて **`05(1.75日※)` は v5 時点の誤記**で、05 §見積もり の v5 実数は **1.5 日**(11 から委譲された MTU 列は未計上)である。合計 raw は 38.5 日 → **59.0 日** | §リリース対応の再算定表。v5 基準は **05 = 1.5 日 / raw 合計 38.5 日**に訂正 |
| 実装順序表の 05 にパッケージ名が無く、依存関係表も「03/05 は FS 注入設計の共有」としか書いていなかった | 05 v6 は独立した `netstats` パッケージであり、`RegisterBaseline` を 03 とは**別々に 2 回**呼ぶ。共有するのは FS 注入設計だけである | 実装順序表・依存関係表とも `netstats` 表記に統一(§実装順序・§依存関係) |

## 第5回レビュー後の監査反映(v6 監査)

v6 一式(本書 + 01〜11)の確定前に、**ファイル横断の突き合わせ監査を 2 回**
実施した。目的は「各ファイル単体では正しいが、他ファイルの v6 契約と
食い違っている記述」の摘出である。2 回の監査で **23 件の相互矛盾**を検出し、
本 pass ですべて該当ファイルに反映した。矛盾の類型は次の 4 つ:

- **撤回済み API / 型の参照が残っている**(例: `PreviousRunResult`)
- **引用の欠落**(例: `GenerationCollector` の `Release`)
- **他計画の実装を過小・過大に述べている**(例: 04 の `Sample()` 要否、
  「10 は予算値を独自定義しない」)
- **見積もりの基準値不一致**(例: 05 の v5 実数と MTU 列の二重計上)

**この節の存在理由**: 上記の修正は個々には小さいが、レビュー時に
「なぜ v6 の記述が第5回レビュー対応表と微妙に違うのか」を説明できないと
新たな差し戻し要因になる。**意図的な突き合わせの結果である**ことを
ここに記録しておく。各修正の実体は該当節に `[v6 監査反映]` の印で残し、
主張を取り下げたものは**撤回である旨を本文に明記**した
(本書では §v6 での変更点 6 / §依存関係 / §リリース対応 / 第3回対応表)。

## 調査根拠(レビューでの事実訂正を反映)

| 記事 | 事実 |
|---|---|
| ISUCON12 優勝 | **NaruseJun**。各インスタンスを Netdata で監視。app/DB 分離、DB 4 台化 |
| ISUCON13 優勝 | **同じく NaruseJun**(二連覇)。全サーバに Netdata。DB・DNS・app を分離。slp の Rows examined/sent 比、/initialize フックで自動計測 |
| ISUCON14 感想戦 (kawaemon) | flamegraph、ロック保持時間計測。**TIME_WAIT 枯渇は競技サーバ側ではなくベンチマーカー側の送信元ポート枯渇** |
| ISUCON10 予選作問 (progfay) | EXPLAIN(Using filesort)、降順/空間インデックス |
| isucandar 解説 (catatsuy) | ベンチマーカー設計論(isutools 側ギャップなし) |

## 実装順序(レビュー推奨順)

基盤(先行必須):

| # | 計画 | 内容 | 解消する前提問題 |
|---|---|---|---|
| [01](./01-db-target-registry.md) | DB target registry | **安定 TargetID**(driver+net+addr+database の canonical tuple から base32 26 文字 hash)+ **`Purpose` 別 inspector credential**(app / stats / explain)+ 既定 DB なし接続 + allowlist 表示 | shard 非対応・接続順命名の不安定・DSN(credential)の露出・**09 の最小権限ユーザーを登録できない**・**collector 自身の digest 汚染**。04/06/09/10 の前提 |
| [02](./02-reset-coordinator.md) | Run lifecycle coordinator | **完全な run lifecycle(Start/Finish/Abort/Ack/Expire + epoch fencing)**・不変 handle 型の collector 契約(generation / baseline)・**階層予算モデル**・`BoundaryWindow`・**preempt + `SerializeInitialize`**・409+nonce・singleton Controller | 自己デッドロック・冒頭欠落・終了境界の混入・部分失敗からの回復・**finishing 中 abort と worker の競合**・**同時 initialize の汚染**。全 collector / 08 / 10 の前提 |
| [03](./03-hoststats.md) | hoststats | memory/disk/PSI/cgroup/ホスト同一性(namespace 含む)。`runctl.BaselineCollector` 実装、型名は `hoststats.Section` | 「OS 資源が見える」の過大表現の解消。10 の前提 |

機能:

| # | 計画 | 旧番号 | 要点 |
|---|---|---|---|
| [04](./04-sql-row-stats.md) | SQL 行効率 | 旧01 | **全 digest baseline** 方式。`WHERE SCHEMA_NAME = ?` に `TargetInfo.Schema` をバインドし**自己汚染を排除**。NULL digest overflow・counter 後退・sent=0・DB 側 UTC 4 点の順序検証 |
| [05](./05-network-stats.md) | ネットワーク観測 | 旧03 | v1 は**表示のみ**(advisor 閾値なし)。新規パッケージ **`netstats`**。`readLinkAttrs` で speed / MTU を別々の受理規則で読む |
| [06](./06-db-pool-stats.md) | DB プール統計 | 旧02の一部 | v1 は**表示のみ**。registry 登録済み TargetID のみ受理(自動 ID の手書き禁止)。deferred activation |
| [07](./07-runtime-profiles.md) | runtime プロファイル | 旧02の一部 | **累積プロファイル**として正しく扱い、**open/close** の 2 点 + `-diff_base` 手順 + 残差(`ApproxErrorNs`)の実測表示を提供 |
| [08](./08-auto-reset.md) | 計測開始の自動化 | 旧04 | HTTP 自己呼び出し廃止。**`SerializeInitialize` で handler 本体全体を直列化 + 末尾 `ResetNow`(`Preempt:true`)**。`ValidityInvalid` は 500 |
| [09](./09-query-plan-capture.md) | EXPLAIN 自動化 | 旧05 | **raw exemplar 廃止**。MySQL 8 の QUERY_SAMPLE_TEXT 経路に限定。`PurposeExplain` credential + `SET ROLE NONE`。実行は freeze 後の `EnrichBudget` 内 |
| [10](./10-multi-host.md) | 複数台横断計測 | 旧06 | **ADR からやり直し**。**embedded `PeerHandler` と standalone agent の 2 形態**、lease、`sealRun`、wire DTO 確定。**5〜6 週間規模** |
| [11](./11-nginx-transport.md) | nginx transport / ランタイム | 新規 | ISUCON 本 9-8/9-9 由来: UDS 機会(`/run/<app>/app.sock`)・`listen backlog=`(root conf tree から解決)・PGO の静的検査 + MTU 表示(05 に委譲)。依存なし・即実装可 |

## 依存関係(v6)

**02 は全計画の実装順依存**である。02 が定める collector 契約
(不変 handle + `Collect(base, final)`)と階層予算は、
03/04/05/06 の**実装形そのもの**を決める。

| 依存元 | 依存先 | 種別 | v6 で確定した具体的契約(この名前で参照する) |
|---|---|---|---|
| 04 sqlrows | 01 registry | **API** | `Inspect(ctx, targetID, isutools.PurposeStats, fn)` / `TargetInfo.Schema` を `WHERE SCHEMA_NAME = ?` に**バインド** / `PurposeStats` 接続は `DBName=""`(自己汚染防止) |
| 09 queryplan | 01 registry | **API** | `RegisterDBInspector(targetID, PurposeExplain, driverName, dsn)` / `Inspect(ctx, id, PurposeExplain, fn)`。未登録は `ErrPurposeNotRegistered` で当該 target を skip し、**`PurposeApp` / `PurposeStats` に fallback しない**。加えて `Querier.ExecContext`(v6 監査で 01 が受理。`SET` 系のみ allowlist、違反は `ErrExecNotAllowed`)を pin 済み `*sql.Conn` 上の `SET ROLE NONE` / `SET time_zone` に使う |
| 06 dbpool | 01 registry | **API** | **registry に存在する TargetID のみ受理**(`ErrUnknownTarget`)。自動 ID は手書き禁止 → `RegisterDBTarget(id, driverName, dsn)` か `TargetIDForDSN(driverName, dsn)` |
| 10 multi-host | 01 registry | **API** | `TargetInfo{ID, Driver, Display, Schema, Purposes}` を targets.json と `PeerInfoDTO.targets` へ写像(**DSN は wire に出さない**) |
| 03 hoststats / 04 sqlrows / 05 netstats / 06 dbpool | 02 coordinator | **API + 実装順** | `runctl.BaselineCollector`(`CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` / `Release`)・`runctl.BaselineHandle`(不変・sample 内包)・`SampleResult.Committed`・`ErrStaleEpoch`・(runID, epoch, phase) 冪等・階層予算(`PhaseStartBaselineBudget` 5s > `PerCollectorBaselineBudget` 3.5s > `PerTargetBudget` 1s)・`BoundaryWindow` |
| httpstats / sqlstats / accesslog / counters(既存) | 02 coordinator | **API + 実装順** | `runctl.GenerationCollector`(`BeginBoundary` / `Freeze` / `Drain` / `Collect` / `Release`)・`PerCollectorGenerationBudget` 100ms・`DrainCancelGrace` 1s。**`Required: true` の既定は httpstats / sqlstats のみ** |
| 07 profiles | 02 coordinator | **型の引用のみ**(登録しない) | `StartResult` / `FinishAccepted` / `BoundaryWindow` / `Validity` / `AbortResult.Reason` と背景処理予算(`DrainBudget` 10s / `SnapshotBuildBudget` 5s / `EnrichBudget` 2s)。**collector として登録しないため階層予算・`ErrBudgetInversion` の対象外**。artifact 名に `run_id` を使う |
| 08 auto-reset | 02 coordinator | **API**(実装本体は 02 所有) | `isutools.SerializeInitialize(ctx, fn)` / `ResetNow`(= `StartRunOptions{Preempt: true}` 既定)/ `runctl.ErrRunActive` / `runctl.ErrInitializeBusy` / `StartResult.Validity` / `PreemptTotalBudget` 8s / `InitializeGuardBudget` 30s |
| 09 queryplan | 02 coordinator | **予算の引用** | EXPLAIN は freeze **後**の background 付加取得であり、`EnrichBudget` 2s の内側(per-digest 250ms は 09 の値)。freeze 予算には含めない |
| 10 multi-host | 02 coordinator | **wire への一対一写像** | `RunState` 9 値 / `Validity` 3 値 / `Epoch` / `StartResult` / `FinishAccepted` / `AbortResult` / `RunStatus` / `CollectorBoundary` / `BoundaryWindow` / sentinel→HTTP status(`ErrRunActive`→409、`ErrRunAborted`→410、`ErrUnknownRun`→404)。10 は **02 に対応物のある** 状態名・予算値を独自定義しない(lease・wire budget・fetch/Ack/validate deadline は 10 が定義する — 10 §deadline / §lease / §budget) |
| 09 queryplan | 04 sqlrows | **API** | digest delta と `db_clock` の DB 側 UTC 4 点。**順序異常時は 09 が鮮度判定を行わない**(`Freshness = unknown`) |
| 10 (a) embedded `PeerHandler` | 02 + 既存 in-process collector 群(httpstats / sqlstats / counters / procstats)+ 04 / 06 | **実装順** | アプリプロセスに**ライブラリとして**リンクし 02 の singleton Controller を共有する。`RunRecord.Origin = "peer"`。`ISUTOOLS_PEER`(既定 off)+ Bearer token + loopback 強制 |
| 10 (b) standalone agent(`cmd/isutools-agent`) | 01 + 03 + 05 + 11 | **実装順** | 自プロセスの Controller に **host/OS/DB セクションのみ**(`hoststats` / **`network`**(Go パッケージ名は `netstats`)/ `sqlrows`、`accesslog` は `--accesslog` 指定時のみ)を**登録**し、`dbinspect` / `queryplan` / `advisor-static` は **非 collector セクション**として snapshot 構築時に直接組み立てる(10 §形態別セクション能力表 (1)/(2))。in-process セクション(httpstats / sqlstats / counters / dbpool / procstats)は**原理的に提供できず**、hub の preflight が形態別セクション能力表で不足を検出する |
| 11 nginx-transport | 05 netstats | **実装委譲** | MTU 列は 05 の `readLinkAttrs(sysFS, ifname)` に含める(11 の見積もりには計上しない)。11 の静的 check 2 件と go-pgo は独立 |
| 03 hoststats / 05 netstats | (相互) | **設計共有のみ** | procfs と sysfs を**別の `fs.FS` として注入**する設計を共有する。lifecycle は各々が 02 契約を実装し、`RegisterBaseline` は**別々に 2 回**呼ぶ |

**横断契約の追補(02 が本 pass で提供する)**: 03 §collector 契約への適合が
記録しているとおり、`runctl.BaselineHandle.sample` は unexported であり、
`Collect(base, final)` が採取値を取り出す経路が必要になる。
**02 が `func (h BaselineHandle) Sample() any` を公開する**ことで、
`runctl.BaselineCollector` を実装する **03 / 04 / 05 / 06 の 4 計画すべて**が
同一経路(`h.Sample().(*<pkg>.Sample)`)で採取値を復元する。
**02 が §`BaselineHandle.Sample()` で定義済みであり**、本書と 03 / 04 / 05 / 06 は
これを引用する(v6 監査反映。「02 への未解決の追補要求」という書き方は撤回する)。

> **[v6 監査反映] 撤回**: v6 初稿の「04 は `runKey{RunID, Epoch}` を鍵にした
> 自前 map から採取値を引く設計のため `Sample()` を直接は要求しない」という
> 記述は**誤りであり撤回する**。04 §collector 契約への適合は
> 「**`Collect` は `pending` を読まない**(handle だけから区間値を作る)」と
> 明記しており、`pending` は `CaptureFinal` が DIGEST_TEXT 取得対象を
> 決めるためだけの map である。したがって **04 も 03 / 05 / 06 と同様に
> `Sample()` を必要とする**(要求するのは 3 計画ではなく 4 計画)。

## リリース対応(見積もり v6 再算定)

各計画の §見積もり の**実数**を読み直して再構築した。
buffer は従来どおり **+30%**(統合・機能単位 ABBA・ドキュメント・
レビュー対応分)。

| リリース | 構成(各計画 §見積もり の実数) | 実装 raw | +30% buffer |
|---|---|---|---|
| v1.2.0 | 01(2.5)+ 02(9.5)+ 04(3.5) | **15.5 日** | 20.15 → **20 日** |
| v1.2.x | 03(2.5)+ 05(2.0)+ 06(1.5)+ 11(1.5) | **7.5 日** | 9.75 → **10 日** |
| v1.3.0 | 07(3.0)+ 08(2.0)+ 09(4.5) | **9.5 日** | 12.35 → **12.5 日** |
| v1.4.0 | 10(26.5) | **26.5 日** | 34.45 → **34.5 日** |
| **合計** | 全 11 計画 | **59.0 日** | 76.7 → **77 日** |

計上規則(二重計上の禁止):

- **05 の 2.0 日は 11 から委譲された MTU 列(+0.25 日)を含む**。
  11 の 1.5 日には含めない。**[v6 監査反映]** v5 基準値は 05 §見積もり の実数に
  合わせて **1.5 日**(MTU 未計上)とする。v6 初稿が v5 基準を 1.75 日
  (= 基本 1.5 + MTU 0.25)と書いていたのは MTU を v5 側に**先取り計上**した
  二重計上であり、本規則に反するため**撤回する**
- **08 の 2.0 日に `SerializeInitialize` / preempt の実装本体は含まない**。
  実装本体は 02 の 9.5 日に(+0.75 日として)計上済み
- 10 §見積もりは `26.5 × 1.3 = 34.45` を切り捨てて「≈34 日」と表記している。
  本表は 0.5 日単位に丸めるため 34.5 日とする。**差は丸めのみで数値の不一致ではない**

v5 からの増分(raw 38.5 日 → 59.0 日、**+20.5 日**)の内訳:

| 計画 | v5 | v6 | 増分 | 主因 |
|---|---|---|---|---|
| 10 | 17 | 26.5 | **+9.5** | lease / seal マトリクス / 2 形態 deployment / wire DTO 確定 / 上限と budget wire 化 |
| 02 | 5 | 9.5 | **+4.5** | epoch fencing / handle 化 / `Committed` / 階層予算 / 並列 baseline / preempt / `/finish`・`/abort` |
| 09 | 3 | 4.5 | **+1.5** | role 展開つき権限検証 / 非計装セッション / エラー構造化 |
| 04 | 2.5 | 3.5 | **+1.0** | 自己汚染排除の SQL 全書き換え + integration テスト / collector 適合 |
| 07 | 2 | 3 | **+1.0** | 採取点移動 / 冪等キー / manifest / Runs 詳細 UI |
| 01 | 2 | 2.5 | **+0.5** | Purpose 分離 / DSN 正規化 / schema 非汚染テスト |
| 03 | 2 | 2.5 | **+0.5** | `BaselineCollector` 適合 / `CaptureFinal` / cgroup 決定表 |
| 06 | 1 | 1.5 | **+0.5** | `BaselineCollector` 適合 / deferred activation |
| 08 | 1.5 | 2.0 | **+0.5** | `SerializeInitialize` 配線 / 同時 initialize 並行テスト |
| 11 | 1 | 1.5 | **+0.5** | include 解決 loader(`loadNginxConfTree`) |
| 05 | 1.5 | 2.0 | **+0.5** | MTU 列 +0.25(11 から委譲)/ `BaselineCollector` 化 +0.25 |

## 全計画共通の契約

1. **fail-open(原則)と fail-closed(例外)**
   — v5 の「計測失敗はアプリを止めない」を**無条件契約として書いていたのは
   撤回する**。08 の `/initialize` 500 と 10 hub の `/reset` 503 は
   契約違反ではなく、下記の**明示された例外**である。
   - **原則: fail-open(通常のデータ収集器)**。観測に失敗しても
     アプリのリクエスト処理を止めず、当該セクションだけを落として続行し、
     skip 理由を health に残す。該当:
     **01 の target 解決 / inspector 接続**(パース不能 DSN は target 化しない)、
     **03 hoststats・04 sqlrows・05 netstats・06 dbpool・09 queryplan**、
     **07 profiles**(保存失敗は health degrade)、
     **11 の静的 check**(判定不能は `StatusSkip`)、
     既存 httpstats / sqlstats / accesslog / counters / procstats の**観測**部分。
     02 §結果表の 3/4/6/10/12/14/16 行(optional collector)がこれに当たり、
     **optional の失敗は最悪でも `ValidityPartial` 止まりで invalid にしない**
   - **例外: fail-closed(run 境界の control plane と required participant)**。
     これらは「**正しさを証明できない run を作らない**」ことが役割であり、
     失敗を握り潰して一見 valid な run を返すことを禁止する。該当:
     1. **02 Controller の境界処理**。`Required: true` の collector
        (既定 = httpstats / sqlstats)が start-boundary / start-baseline で
        失敗した場合、run は `Validity = ValidityInvalid` かつ
        `RunState = aborted` になり、**snapshot を作らない**
        (02 §結果表 1/2/5/7 行、および §結果表の原則
        「開始側の required 失敗は run を成立させない」)。
        **終了側**(finish-freeze / finish-final / collect)の required 失敗は
        02 §結果表 8/9/11/15 行のとおり **snapshot を保存するが `Validity` は
        invalid のまま**であり、valid へ昇格させない点で同じく fail-closed である
        (区間の冒頭は確定済みで、他セクションのデータは調査に使えるため破棄しない)
     2. **08 の `/initialize` handler**。`StartResult.Validity == ValidityInvalid`
        なら **HTTP 500** を返す。log-and-continue は 08 の禁止事項であり、
        受け入れ条件 `TestResetNowInvalid_Returns500` で固定する
     3. **10 hub の `/reset`**。required participant が preflight
        (接続 / protocol / schema / capability / section)または StartRun バリアで
        失敗した場合、**run を開始せず 503 + invalid** を返し、
        返す**前に** `sealRun(SealAbort)` を実行する(10 §seal 決定表 #1/#3/#5)
     4. **10 peer の認証**。token の不一致・欠落は **401**
        (`ErrorDTO{code:"unauthorized"}` のみ。状態を漏らさない)
   - **見分け方(実装時の判断基準)**: 「その失敗を無視した場合、
     後でダッシュボードに出る数値が**嘘になるか**」。嘘になるなら fail-closed
     (その run を捨てる)、ならないなら fail-open(そのセクションを落として続行)
   - **fail-closed でも計測対象アプリの通常リクエスト経路は止めない**。
     500 / 503 を返すのは**ベンチ制御用エンドポイント**
     (アプリの `/initialize`、hub の `/reset`)だけであり、
     計測対象のアプリ handler ではない。
     10 の optional participant は常に partial 継続(fail-open 側)
2. **feature flag 必須**: 各機能は専用の環境変数で単独 on/off できる。
   既定値は各計画に明記(ランタイムコストのあるものは既定 off)。
   v6 時点の一覧: `ISUTOOLS_SQLROWS`(04・既定on)、
   `ISUTOOLS_HOSTSTATS`(03・既定on)、`ISUTOOLS_NETSTATS`(05・既定on)、
   `ISUTOOLS_DBPOOL`(06・登録時のみ)、`ISUTOOLS_MUTEX_FRACTION` /
   `ISUTOOLS_BLOCK_RATE_NS` / `ISUTOOLS_HEAP_PROFILE`(07・**既定off**)、
   `ISUTOOLS_RESET_ON_INITIALIZE`(08・besteffort モード・**既定off**)、
   `ISUTOOLS_EXPLAIN`(09・**既定off**)、
   `ISUTOOLS_PEER`(10 の embedded peer・**既定off**。listener を増やすため)。
   flag ではない設定値(secret / パス)は上表に含めない:
   `ISUTOOLS_PEER_TOKEN`(10)、`ISUTOOLS_NGINX_ROOT_CONF` /
   `ISUTOOLS_NGINX_PREFIX`(11)、`ISUTOOLS_CGROUP_SCOPE` /
   `ISUTOOLS_CGROUP_PATH`(03)、`ISUTOOLS_AGENT_TARGETS_FILE`(10)。
   **例外(v4 で明文化、v5 で表現修正、v6 で維持)**: 設定ファイル・
   buildinfo の読み取りだけで完結する静的 advisor check(11 の 3 check、
   既存の nginx/OS check 類)は、**ベンチ実行中の追加観測を行わない**
   (設定 I/O と解析は境界時のみ)ため専用 flag を要求しない。
   ランタイム観測・追加クエリ・追加 I/O を伴う機能のみ flag 必須とする。
3. **機能単位 ABBA**: `examples/abba.sh` を拡張し、
   (a) 全機能 off vs 全機能 on、(b) baseline vs 単一機能 on、の両モードを
   サポートする。リリース tag 前に対象機能の (b) を必ず実施する。
   全体 on/off だけでは追加機能単体の影響を分離できないため。
4. **schema version 契約**:
   - additive(新しい省略可能キーの追加): bump しない。
     `meta.capabilities` 配列(additive で導入)に機能名を追加して宣言する
   - 既存キーの意味変更: bump する
   - キーの削除・型変更(破壊): bump + 移行注記
   - peer 互換判定は revision ではなく
     `schema_version` + `protocol_version` + `capabilities` で行う(→ 10)。
     10 v6 の要件は **`protocol_version` 完全一致 / `schema_version` は
     hub ≥ peer / `library_version` は不一致を許容し記録のみ**
5. **TDD** + 集計カバレッジ 80%(CI ゲート)。/proc・/sys は FS 注入 +
   `fstest.MapFS`。procfs と sysfs は**別の FS として注入**する
   (現行 procstats の注入 root は /proc であり /sys を読めないため)。
   `fs.FS` で差し替えられない syscall(statfs / readlink /
   `filepath.EvalSymlinks`)は **`Options` の関数シーム**として注入する
   (03 §注入シーム)。
6. **advisor 閾値は実測に基づく**: 新しい warn 閾値は private-isu での
   フィールド検証を経てから既定有効にする。それまでは表示のみ、
   または provisional と明記する。05 の MTU は**表示のみ・判定なし**を維持し、
   **MTU 値が advisor 出力を一切変えない**ことを退行防止テストで固定する。
7. **ドキュメント**: 各 PR に README 環境変数表・INTEGRATION.md・
   IMPLEMENTATION_STATUS.md の更新を含める。

## レビュー指摘 → 対応計画の対応表

| 指摘(要約) | 対応 |
|---|---|
| [CRITICAL] 旧06 reset 伝播が同一区間を保証しない | 02(単一ホストの契約)+ 10(ACK-all / invalid run) |
| [CRITICAL] 旧04 async 既定は計測境界を保証しない / 自己 HTTP 呼び出し | 08(内部 Coordinator 直呼び + 同期 API 主軸) |
| [CRITICAL] 旧01 上位200 baseline は delta にならない / NULL overflow | 04(全 digest baseline・overflow 独立 health) |
| [CRITICAL] 旧05 exemplar 前提が hook 実装と不一致 | 09(QUERY_SAMPLE_TEXT 経路へ全面変更) |
| [HIGH] 旧06 app peer の SQL/HTTP が統合されない / identity 欠落 / OS 資源過大表現 | 10(全セクション取り込み)+ 03(hoststats/identity) |
| [HIGH] 旧02 profile rate 意味論・累積性・.pb.gz 配信不可 | 07(意味論修正・diff_base・.pprof 名) |
| [HIGH] 旧02 dbpool advisor 判定式不成立 | 06(表示のみに縮小) |
| [HIGH] 旧03 TIME_WAIT 50% 閾値は誤警報 / FS・単位・新規NIC | 05(表示のみ・モデル修正) |
| [HIGH] 旧04 二重 responseWriter が透過契約を壊す / 5秒 debounce | 08(既存 wrapper から通知・request 単位の一意性) |
| [HIGH] README「06以外は独立」不成立 / FirstConn 単一 DSN | 本書依存図 + 01 |
| [MEDIUM] 機能単位 ABBA 不可 / schema 契約未定義 / 旧06 E2E 不足 | 共通契約 3・4 + 10 の E2E マトリクス |

## 第2回レビュー指摘(差し戻し)→ v3 対応の対応表

| 指摘(要約) | 対応 |
|---|---|
| [CRITICAL] ResetNow の自己デッドロック(in-flight 待ち) | 02: BeginBoundary/Drain 二段階 + 08: ResetNow は境界のみ同期。middleware 内呼び出しの統合テストを両計画の受け入れ条件に |
| [CRITICAL] 分散に終了バリアがない(fetch skew・collect 非伝播) | 10: FinishRun バリア + immutable `/peer/runs/{run_id}` 取得 |
| [CRITICAL] required peer の protocol 不一致が invalid にならない矛盾 | 10: preflight 不適合は reset 前に invalid + 503。optional のみ partial |
| [HIGH] 並行 reset の待ち行列が応答済み run を汚染 | 02: 409 拒否 + nonce 冪等化(待ち行列廃止) |
| [HIGH] StartedAt 最大差は skew ではない / ホスト間時計 | 02: collector 別 BoundaryAt + 境界ウィンドウ。10: hub 観測の送信/ACK 区間 |
| [HIGH] coordinator の所有権未設計(Handler() 毎回生成・admin off) | 02: process-wide singleton Controller + admin off/DB 未接続/複数 Handler のテスト |
| [HIGH] dbN 接続順命名の不安定性 | 01: DSN 構造から決定的導出する安定 TargetID + RegisterDBTarget。04/06/09/10 で同一名前空間 |
| [HIGH] go run @latest 配布 | 10: tag 固定の事前 build binary + checksum。@latest は例示にも不使用 |
| [HIGH] P_S の TRUNCATE 検出不十分 / NULL digest の重複表示 | 04: server_uuid+Uptime+keyset 消失+counter 後退の複合判定 + 検出限界の契約明記 + server_uuid 単位 dedup |
| [HIGH] QUERY_SAMPLE_TEXT の鮮度未検証 | 09: QUERY_SAMPLE_SEEN 取得・run 区間判定・stale 表示・advisor 除外 |
| [HIGH] heap profile が既定 on でゼロオーバーヘッド主張と矛盾 | 07: ISUTOOLS_HEAP_PROFILE 明示 opt-in・既定 off + reset 所要の個別実測 |
| [MEDIUM] Inspect の接続大量生成 / HasDSNParam 3値 / redaction 文字列規則 | 01: target 所有 inspector(接続1・再利用)+ Querier 制限 interface + typed Features(known) + allowlist 再構築 |
| [MEDIUM] cgroup root 誤読 | 03: /proc/self/cgroup+mountinfo 解決 + cgroup_scope 記録 + ISUTOOLS_CGROUP_PATH |
| [MEDIUM] DBStatser の緩さ / run 途中登録 | 06: *sql.DB + TargetID 限定 + Unwatch lifecycle + run 状態 partial 化 |
| [MEDIUM] PeerSnapshot の再帰 / budget 未定義 | 10: LocalSnapshot DTO(Peers/Prev なし)+ hub 優先の決定的 budget |
| [MEDIUM] artifact の atomic publication / 保持 | 07: 0600 tmp→rename、保持上限、run manifest |
| [LOW] 見積もり不整合 / 06・09 の flag 欠如 | 本書: 単純合計 + 30% buffer に修正。flag 一覧に ISUTOOLS_DBPOOL / ISUTOOLS_EXPLAIN を追加 |

## 第3回レビュー指摘(差し戻し)→ v4 対応の対応表

| 指摘(要約) | 対応 |
|---|---|
| [CRITICAL] baseline を非同期 Drain に置くと新 run 冒頭が欠落 | 02: generation/baseline の 2 契約に分離。baseline は StartRun 内で同期採取、BoundaryAt は実測時刻。冒頭欠落の回帰テストを受け入れ条件化 |
| [CRITICAL] Drain に世代 token なし・遷移が BeginBoundary までで終わる | 02: 遷移状態機械(idle/starting/draining)。Drain 完了まで 409、世代 handle 付き Drain、Drain 上限 10s |
| [CRITICAL] 同時 initialize の 409 握り潰しで先行 run が汚染 | 08: 待機(上限 15s)→ 自 nonce で再 reset。失敗はエラー返却で initialize を 500 に。log-and-continue を禁止事項として明記 |
| [CRITICAL] 分散バリアの非対称(hub 先行 freeze・順序逆) | 10: hub = participant #0。全 participant が「freeze point 固定 → 固定点まで Drain」の同一順序。両バリアに送信/ACK 不確実性区間 |
| [HIGH] ResetResult の可変性 / request context の background 波及 | 02: 不変 StartResult + PreviousRunResult 分離(**v6 では `StartResult` に一本化**。`PreviousRunResult` は 02 v6 に存在しない)。background は WithoutCancel + 内部 timeout |
| [HIGH] RegisterDBTarget(id, dsn) で driver 不明 | 01: RegisterDBTarget(id, driverName, dsn)。10: agent は driver 必須の JSON targets ファイル |
| [HIGH] unparsed-sha256[:8] の不安定・衝突・照合器化 | 01: hash fallback 廃止。パース不能 DSN は明示登録必須(未登録は target 化せず health 記録) |
| [HIGH] SAMPLE_SEEN をアプリ時計と比較 / schema 条件欠落 | 09: session UTC 固定 + DB 側 UTC_TIMESTAMP(6) だけで鮮度判定。WHERE SCHEMA_NAME=DATABASE() AND DIGEST=? |
| [HIGH] cgroup namespace で「root=実パス→host」が偽 | 03: 既定 scope=visible-root、host は明示設定のみ。identity に CgroupNS 追加 |
| [HIGH] 分散 run の再試行・保持の状態機械なし | 10: idle→started→finished→acknowledged、run_id+nonce 冪等再送、競合 409、直近 2 run を ACK/TTL まで保持、nonce は TTL 付き履歴 |
| [HIGH] profile 設定が startAdmin / initialize 後半の混入 | 07: singleton runtime 初期化へ移動、env 未設定時は既存 rate を上書きしない、process-wide cumulative と取得不確実性を明示 |
| [HIGH] required peer の section drop が valid に見える | 10: top-N 行縮小(許容・記録)とセクション全欠落(required は partial、必須 capability は invalid)を分離 |
| [MEDIUM] メモリ楽観値 / クエリ数不正確 / CTE 分類 | 04: allocation benchmark を受け入れ条件化、実クエリ数を明記、WITH…SELECT を SELECT 扱い + fixture |
| [MEDIUM] Querier の *sql.Rows が接続を pin | 01: 追跡 wrapper Rows(Inspect 終了時に強制 Close) |
| [MEDIUM] 重複 peer 判定が同一ホスト複数プロセスを拒否 | 10: host identity(hoststats dedup)と agent_id(peer 識別)の 2 層分離 |
| [MEDIUM] backlog の解析単位 / https UDS 提案 | 11: listen endpoint(address:port)単位の保守的解析。https:// は UDS 対象外、前提条件 3 点を文言化 |
| [MEDIUM] 依存図と 02 契約の不一致 | 本書: 依存種別(API / 実装順 / artifact 参照)付きの表へ書き換え |
| [LOW] 見積もりに 11/MTU 欠落・buffer 非数値化・flag 例外未定義 | 本書: v1.2.x に 11+MTU を計上、全リリースの +30% を数値化、静的 check の flag 例外を明文化 |

## 第4回レビュー指摘(再差し戻し)→ v5 対応の対応表

| 指摘(要約) | 対応 |
|---|---|
| [CRITICAL] GenerationHandle の取得手段がなく実装不能 | 02: `BeginBoundary → (handle, 実測時刻)` / `Freeze → (handle, 実測時刻)` に契約変更 |
| [CRITICAL] Finish が HTTP/accesslog しか固定せず終了値に混入 | 02: 全 collector 共通の終了契約(世代型 Freeze + baseline 型 CaptureFinal)。immutable snapshot は固定値のみから構築。10 が一対一で使用 |
| [CRITICAL] 部分開始 run を abort できず次計測が停止 | 02: AbortRun(冪等)+ aborting/aborted 状態。10: required 失敗時に hub が全 started peer へ AbortRun 伝播、部分開始一覧を記録、「部分開始→abort→次 run 成功」を E2E 必須化 |
| [CRITICAL] EXPLAIN SELECT でも stored function 副作用があり得る | 09: 最小権限 inspector ユーザー必須(DML・EXECUTE なし)、SHOW GRANTS 検証、確認不能 target は skip、副作用 fixture テスト |
| [HIGH] BeginBoundary 途中失敗が未定義 | 02: required/optional 区分、required 失敗は全切替完了後に invalid + seal、optional は partial |
| [HIGH] Drain の ctx cancellation 契約なし(httpstats は無期限待機) | 02: ctx.Done() で必ず return + 旧世代を触る goroutine を残さない契約を conformance test で保証 |
| [HIGH] finish の deadline(5s)と Drain 上限(10s)の矛盾 | 10: FinishRun を「freeze 受付 → polling」に変更し、start/finish-freeze/polling/fetch の deadline を分離 |
| [HIGH] 再送 200 と 409 の矛盾 | 10: state × API × run_id × nonce の遷移表で HTTP status と DTO を一意化(同一 run 再送は常に冪等 200) |
| [HIGH] acknowledged 状態に対応する ACK API がない | 10: 冪等 `POST /peer/runs/{id}/ack` を新設、ack または TTL で解放 |
| [HIGH] MaxOpenConns(1) はセッション保証でない | 01: Inspect ごとに db.Conn(ctx) を取得し制限付き Querier で包み callback 終了時に Close。session 初期化は Conn 上で毎回実施 |
| [HIGH] ResetNow が runID しか返さず partial を検知できない | 08: StartResult を返し、invalid は 500、partial は caller policy 必須(example を更新) |
| [HIGH] hoststats dedup が観測範囲差を無視 | 10: dedup キーを (host identity, namespace, cgroup_scope) にし、代表値は明示 host scope のみ |
| [HIGH] 自動 TargetID の slug 衝突が dedup される | 01: 表示 alias + canonical tuple 由来の hash suffix で内部 ID を分離。異 tuple の衝突はエラー |
| [MEDIUM] wire DTO が旧 ResetResult / uncertainty 単一配列 | 10: PeerResult を Start/Finish + StartSendAck/FinishSendAck に更新 |
| [MEDIUM] 04 に DB 側 UTC 取得がない | 04: baseline/final の前後で UTC_TIMESTAMP(6) を取得し before/after 区間を保存。09 は区間で保守的に鮮度判定 |
| [MEDIUM] EXPLAIN 全列の nullability | 09: PlanRow を全列 pointer/sql.Null* 化 |
| [MEDIUM] targets JSON の安全性契約なし | 10: regular file・所有者・0600・64KiB・symlink 拒否 + DSN 非出力テスト |
| [MEDIUM] CGROUP_PATH の境界未定義 | 03: cgroup mount 相対のみ・絶対/../symlink escape 拒否 + fixture |
| [MEDIUM] WatchDBPool の TargetID 未検証 | 06: registry 登録済み ID のみ受理 |
| [MEDIUM] DIGEST_TEXT 取得に SCHEMA_NAME 条件なし | 04: (SCHEMA_NAME, DIGEST) 条件 + 複数 schema fixture |
| [MEDIUM] 08 の旧 API 参照・旧テスト期待値 | 08: StartRun API へ統一、同時 initialize は「2 本とも成功・世代 2 回・最後の nonce 有効」へ修正 |
| [MEDIUM] 07 の取得順序矛盾・budget 未定 | 07: 境界後の近似観測と明示、capture window を保存、StartRun budget 外 |
| [MEDIUM] 11 の conf 連結では endpoint 解析不能 | 11: source/include を保持する保守的 parser を実装範囲に追加 + endpoint テスト群 |
| [LOW] README の ResetResult 表記 / 静的 check の「コストゼロ」/ 07 の heap・手動削除残存 / 04 の risk 表先祖返り / 見積もり未再算定 | 各該当箇所を修正(v5 に反映済み) |

## 第5回レビュー指摘 → v6 対応の対応表

内訳: **CRITICAL 5 / HIGH 10 / MEDIUM 12 + 一貫性(MINOR)4**。
同一指摘が複数ファイルに跨る場合は 1 行にまとめ、「対応計画」列に
関係する全ファイルを列挙する(各ファイルの §v6 での変更点 と対応)。

### CRITICAL(5 件)

| # | 指摘(要約) | 対応計画 | 具体的な解決 |
|---|---|---|---|
| C1 | 「1 TargetID = 1 DSN」では 09 が要求する最小権限 EXPLAIN ユーザーを登録できず、v5 の CRITICAL 対応が**実装不能**だった(同一 ID の再登録は重複エラー、別 ID にすると 04 の digest と結合できない) | **01** / **09** | 01: `Purpose`(app/stats/explain)を型付き enum で導入し `RegisterDBInspector(targetID, purpose, driverName, dsn)` で 1 targetID に複数 credential を紐づける。`Inspect(ctx, id, purpose, fn)` へ変更(v5 の `Inspect(ctx, id, fn)` は撤回)。09: `RegisterDBInspector(id, PurposeExplain, ...)` をそのまま使い、未登録は `ErrPurposeNotRegistered` で **skip**(`PurposeApp` / `PurposeStats` に fallback しない) |
| C2 | collector 自身のクエリが**アプリ schema の digest として performance_schema に記録され計測区間を汚染**する。baseline 採取文が baseline 確定の後に計上されるため**必ず delta に現れる** | **01** / **04** | 01: `PurposeStats` / `PurposeExplain` の DSN を `DBName=""` に正規化して開く。schema 名は `TargetInfo.Schema`(非 secret)として保持。04: `WHERE SCHEMA_NAME = DATABASE()` を撤回し `WHERE SCHEMA_NAME = ?` に `TargetInfo.Schema` をバインド。SQL を全文書き換え、**自己汚染 integration テスト**を必須の受け入れ条件に |
| C3 | finishing 中の `AbortRun` と background worker が競合し、stale worker が snapshot を保存して finished へ戻せる | **02** | run ごとに `cancel` / `done` を Controller が所有し、AbortRun は **epoch を進めて fence → cancel → bounded join(`AbortJoinBudget` 2s)→ handle 破棄**の順で実行。join timeout 時は detached。stale worker の state 変更・snapshot 公開を epoch fencing で構造的に不可能にし、**race detector 下の必須テスト**を追加 |
| C4 | 同時 initialize の v5 契約(`ErrResetInProgress` を受けて上限 15s 待ち→再 reset)が **02 の状態機械上実現不能**。かつ `ResetNow` は境界の瞬間しか直列化せず、後発 initialize の **DB 再構築が先行 run の区間に載る** | **02** / **08** | 02: `StartRun(ctx, StartRunOptions{Preempt: true})` を新設し active run を原子的に abort してから開始(preempt された run は **aborted + `ValidityInvalid`**)。初期化全体を直列化する `SerializeInitialize` を公開。08: initialize handler **本体全体**(DB 再構築 + `ResetNow`)を `SerializeInitialize` で包むことをアプリ側の**必須要件**(不変条件 I1)とし、規範例・禁止事項・`initialize-unserialized` health キーを定義 |
| C5 | peer の started / finishing に lease が無く、abort 送信が失われると 02 の `StartedTTL`(30 分)まで新 run を 409 で拒否し続ける | **10** | `PeerStartedLease`(45s・hub が `POST /peer/runs/{id}/lease` で更新)と `PeerAckLease`(90s・更新しない)を新設。finishing/aborting は 02 の `FinishLease`(20s)/ `AbortJoinBudget`(2s)を使う。**どの状態からでも hub 沈黙後 `PeerMaxBlockingWindow` = 90 秒**で新 run を受理できる。E2E 必須「abort 消失 → lease 満了 → 次 run 成功」 |

### HIGH(10 件)

| # | 指摘(要約) | 対応計画 | 具体的な解決 |
|---|---|---|---|
| H1 | `CaptureBaseline` / `CaptureFinal` が `SampledAt` だけを返すため、snapshot 構築時に collector の**可変内部状態**を読むことになり「固定値だけから構築」が成立しない。また「切替前の失敗」と「切替後に結果を返せない失敗」が区別できない | **02** / 03 / 04 / 05 / 06 | 02: **不変 handle(採取値を内包)+ `Collect(base, final)`** に変更し `GenerationCollector.Collect` と同形化。`SampleResult.Committed` で 2 種の失敗を区別し、全操作を (runID, epoch) 単位で冪等化。**phase × collector 種別 × required の 17 行結果表**と `TestPhaseMatrix` を追加。03 / 05 は `Reset()` / `Snapshot()` を、06 は「baseline を 02 の reset hook で取る」を撤回し、03 / 04 / 05 / 06 とも `runctl.BaselineCollector` を実装する(`Collect` は I/O 禁止・conformance test `TestBaselineCollect_UsesFrozenSamplesOnly`) |
| H2 | 予算値が 02 と 04 で二重定義(04 の「per-target 1s / 全体 3s」が 02 の値と不整合) | **02** / **04** | 02 に**階層予算モデル**(run > phase > per-collector > per-target)を一元定義し、`ErrBudgetInversion` と `TestBudgetHierarchy` で不等式を CI 固定。04 は定数名(`PerTargetBudget` 1s / `PerCollectorBaselineBudget` 3.5s)を**引用のみ**にし、16 target が収まる根拠と収まらない場合の**決定的 drop 規則**を定義 |
| H3 | `POST /collect` を「FinishRun を内包する終端」と書いたが、現行 README では **buffered accesslog の非終端 flush** であり破壊的変更になる | **02** | `/collect` は**非終端のまま**維持。run を終了させるのは `POST /save` と**新設 `POST /finish`** のみ。`finishing` / `finished` への `/collect` は 409 + `Retry-After`。回帰テスト `TestCollectIsNonTerminal` |
| H4 | baseline を逐次採取する限り「freeze phase = 全セクション共通の**瞬間**の境界」は成立しない | **02** | 境界を**幅を持つ区間** `BoundaryWindow{Min, Max, Spread}` として定義。`BaselineConcurrency = 8` の並列採取で幅を縮め、実測スプレッドを記録し、上限超過時の partial / invalid 判定表を定義 |
| H5 | 06 の使用例 `WatchDBPool("mysql-db1_3306-isuconp", db.DB)` は 01 の自動 ID の **alias 部分でしかなく**、registry に存在しないため `ErrUnknownTarget` で pool が 1 つも計測されない | **06** / 01 | 06: 例を `RegisterDBTarget(id, driverName, dsn)` で明示 ID を作ってから同じ ID を渡す形に書き換え、`TargetIDForDSN` を使う代替も併記。01: 「自動導出 ID を手書きしてはならない」規則と lookup API(`TargetIDForDSN` / `Targets` / `Target`)を明文化 |
| H6 | 「run 途中の Watch/Unwatch は **run 状態自体を partial** にする」は 02 v6 に存在しない語彙(`RunState` と `Validity` を混同)であり、実現手段も無い | **06** | run の `Validity` は降格させず、**entry 単位の `Partial` + `Code` + `BaselineAt` / `FinalAt` + 06 固有 health キー**で表現する。**deferred activation**(run の watch set は `CaptureBaseline` 時点で確定)により遅れた baseline を作らない |
| H7 | `SHOW GRANTS` 単独では **role 経由の INSERT/UPDATE/DELETE/EXECUTE を検出できず**、危険な account を「安全」と誤判定する。起動時の別セッション検証も `SET ROLE` がセッション局所なので無効 | **09** | 同一 pinned connection 上の **`SET ROLE NONE` を主対策**とし `CURRENT_ROLE()` で無効化を検証。granted role は `SHOW GRANTS ... USING` で展開して allowlist 判定。`SET ROLE NONE` 不可時の fallback も規定 |
| H8 | peer の「単一 state + 直近 2 run 保持」では run A を abort して run B を開始した後の「run A への遅延 GET / Abort」を表現できない | **10** | `activeRunID` + `map[RunID]RunRecord` に分割し、**全 API 判定を RunRecord 単位**に変更。遷移表の行を 02 の 9 状態 + 「記録なし」に書き換え、**81 セルを table-driven** で固定 |
| H9 | polling / fetch / validation 失敗時に他 peer へ Ack と Abort のどちらを送るか未定義で、最大 TTL 分だけ系が停止し得た | **10** | 全 participant に**必ず 1 回だけ**実行する `sealRun(decision)` を定義し、正常完了 + 6 失敗点 × required/optional の **16 行決定表**で decision・hub の run 状態・次 run の即時開始可否・HTTP status を確定。fault injection テストを必須化 |
| H10 | 「app peer は SQL/HTTP/accesslog/counters を全て含む」と書きながら配布経路が別プロセスの agent binary しか無く、**別プロセスから in-process 状態は読めない**ため実現不能 | **10** | **(a) embedded `PeerHandler` / `ServePeer`**(アプリプロセス内・02 singleton Controller を共有・loopback 強制 + Bearer token・`ISUTOOLS_PEER` 既定 off)と **(b) standalone agent**(host/OS/DB セクションのみ)を分離し、**形態別セクション能力表**を hub の preflight 判定根拠として定義。「hub と agent は同一 binary」は撤回し `protocol_version` 完全一致 / `schema_version` hub ≥ peer / `library_version` は記録のみへ置換 |

### MEDIUM(12 件)

| # | 指摘(要約) | 対応計画 | 具体的な解決 |
|---|---|---|---|
| M1 | 自動 TargetID の `sha256(...)[:8]` が「8 hex 文字 = 32bit」とも読め、01 自身が否定した 32bit 公開 hash に逆戻りする。canonical tuple に **net(tcp/unix)が欠落** | **01** | tuple を `driver + net + canonical addr + database` と定義。hash は **sha256 先頭 16 バイト(128bit)を base32(RFC 4648・小文字・padding なし)で 26 文字**に固定。明示 ID にも長さ・文字種の制約を課し、**ID 比較は byte 単位の完全一致**と明記 |
| M2 | 境界あたりの実行文数の列挙が不正確(v5 自身が追加した `UTC_TIMESTAMP(6)` 4 回と、01 が Inspect ごとに実行する `SET time_zone` を数え落とし) | **04** | probe 文と定常 run 文を分けた**境界別の完全な列挙(表 A〜D)**を新設し、ABBA の実測条件をこの数へ更新。`SHOW GLOBAL STATUS LIKE 'Uptime'` は `performance_schema.global_status` へ変更(probe 失敗 target のみ SHOW 経路) |
| M3 | DB 側時計の逆行(NTP 等)で `final.UTCBefore < baseline.UTCAfter` になると 09 の鮮度判定区間が逆転し、**全サンプルが誤って stale** になる | **04** / **09** | 04: 保存済み DB-UTC 4 点の**順序検証**を定義し、異常時は run を partial にして **09 に鮮度判定を一切させない**契約に。09: `Stale bool` を撤回し 3 値 `Freshness`(fresh / stale / unknown)+ 閉じた理由 enum へ |
| M4 | MTU がデータモデルにだけ入り、実装ステップとテスト計画が link speed しかカバーしていない(speed と MTU は正常値の範囲も欠損の意味も異なる) | **05** | sysfs 属性リーダを `readLinkAttrs(sysFS fs.FS, ifname string) (linkAttrs, error)` として独立させ、speed と MTU の**受理範囲を別々に定義**。MTU の欠損 / 非数 / 範囲外 / Jumbo(9000)/ 境界(68・65536)と、**MTU が advisor 出力を一切変えない**退行防止テストを追加。あわせて `Collect` が I/O 禁止のため sysfs 読みを capture 時に確定 |
| M5 | web の live provider(`Provider DBPools func() []dbpool.Entry` / 描画時の `/proc` 再読み)が 02 の「**固定値だけから** immutable snapshot を構築」に反する | **06** / **03** | 06: live provider を撤回し `Collect(base, final)` の戻り値を snapshot の `DBPools` に格納。「登録が reset 後 → 初回 snapshot は登録時点比」テストも deferred activation により撤回し代替 2 本を用意。03: web は immutable snapshot を描画し**描画時に `/proc` を読み直さない**ことを明記 |
| M6 | profile の save 側採取点(snapshot 永続化時)に 02 の post-finish 処理が丸ごと入る。混入量は `DrainBudget` + `SnapshotBuildBudget` + `EnrichBudget` = **最大 17s** で、中身は**計測器自身のロック競合**に系統的に偏る | **07** | 採取点を **`FinishRun` 返却直後(close)** へ移動(option A)。`_reset` / `_save` を **`_open` / `_close`** に改名して v5 artifact と混ざらないようにし、freeze 点(`GenerationWindow.Max`)基準の `HeadLossNs` / `TailExcessNs` / `ApproxErrorNs` を artifact メタと Runs 詳細に**確定文言**で表示 |
| M7 | profile 採取の実行規約(同期/非同期)・冪等キー・abort/preempt 時の pair が未定義。`listFiles()` は `.html` のみ列挙するのに「files 一覧に載る」と記述 | **07** | 呼び出し側 goroutine で**同期実行**し採取瞬間を実測記録(`WriteTo` は ctx を取れないため予算は種別間のゲートとして実装)。冪等キーを `(RunID, Epoch, Point)` と定義し `/finish` → `/save` の 2 段呼び出しでも close 採取は 1 回。abort / preempt 時は close を採らず open 側を `orphan` として retention 最優先削除。発見経路は manifest と Runs 詳細のみと訂正 |
| M8 | 08 が 02 で撤回済みの API を参照(`runctl.StateInvalid` / `StatePartial` / `ErrResetInProgress` / `DrainPrevious` / `Controller.Reset`)し、テスト期待値も旧契約由来 | **08** | `StartResult.Validity`(`ValidityValid` / `ValidityPartial` / `ValidityInvalid`)へ統一。`ErrResetInProgress` → `runctl.ErrRunActive`、`DrainPrevious` → 「background の prev handle `Drain`(`DrainBudget` 10s)」、`Controller.Reset` → `Controller.StartRun`。同時 initialize の期待値を `TestConcurrentInitialize_Serialized_LastRunWins`(先行 run は **aborted + `ValidityInvalid`**)へ差し替え |
| M9 | driver エラー文字列の「sample が埋まっていたら置換」は**完全一致しか検出できず**、切り詰め・エスケープ・部分文字列としてのリテラル片が残る | **09** | **raw driver エラーを一切保存しない**。allowlist された分類 enum と driver エラーコード(errno / SQLSTATE)だけを持つ `PlanError` 構造体に写像し、**自由文字列フィールドを型として持たない**。漏洩検査テストを追加 |
| M10 | wire DTO に State / Error 構造が無く optional participant の失敗を hub に伝えられない。識別子の長さ・文字種と保持数の上限も無く、abort/start の高速反復下でメモリが有界にならない | **10** | 全 DTO をフィールド単位で定義し **strict decode(未知フィールド拒否)+ golden / fieldset / 02 ミラーの互換テスト**を必須化。`run_id` / `nonce` / `role` / `agent_id` の上限と文字種、`RetainedRuns = 2` / `NonceHistoryMax = 64` / `MaxPeers = 8` の hard cap と `TestPeerMemoryBounded`(≈17MiB の算出式)を定義 |
| M11 | per-peer budget を peer に伝える wire 項目が無く、「受信後に hub が縮小する」規定も無かった | **10** | `GET /peer/runs/{id}/snapshot?max_bytes=N` で hub が予算を渡す方式を採用(再直列化ループ)。hub 自身の snapshot が `HubSelfReserve` を超えた場合の**優先規則**と再配分(required 超過 → invalid、optional → partial)を定義 |
| M12 | ディレクトリを列挙しても「その `.conf` が実際に include されたか」は判定できず、`conf.d/foo.conf.bak` を有効設定として数える。UDS 推奨パス `/tmp/webapp.sock` は world-writable で pre-creation DoS を許し、RHEL 系 `nginx.service` の `PrivateTmp=true` とも衝突する | **11** | root conf を起点に `include`(glob 含む)を解決する `loadNginxConfTree(fsys, root, prefix)` を新設し `advisor.Options.NginxRoot *NginxRootConf` を追加。root conf が無い場合 `nginx-listen-backlog` は **`StatusSkip` + 明示 detail**。未解決 include や打ち切りがある場合も skip。UDS 推奨を `/run/<app>/`(0750)+ `app.sock`(0660)へ変更し stale socket の安全な除去手順を明記。既存 `Options.NginxConf []byte` は後方互換のため残す |

### 一貫性(MINOR・4 件)

| # | 指摘 | 対応計画 | 具体的な解決 |
|---|---|---|---|
| N1 | ヘッダ版数が **v3 のまま**(01 / 08 / 10)。05 / 11 は版数表記そのものが無く、06 も v3 のまま | 01 / 05 / 06 / 08 / 10 / 11 | 各ファイルのヘッダを **v6** に統一。以後、版間の差分は各ファイルの §v6 での変更点 と §v5 から撤回する主張 に集約し、本文に版数注記を散らさない(03 の方針を全ファイルへ適用) |
| N2 | 依存関係節の見出しと内容が **v4 のまま**で、`Inspect(ctx, id, fn)` 時代の 01 と handle を持たない 02 契約を前提にしていた | **本書** | §依存関係(v6)へ改題し、01 の purpose 別 credential / 02 の lifecycle + 階層予算 / 10 の embedded peer vs standalone agent を**契約名・API 署名つき**で書き換え。02 が定義済みの `BaselineHandle.Sample()` を 4 計画が引用する形に統一 |
| N3 | targets ファイルの permission 契約が **固定 0600** で、0400(より厳しい)を拒否してしまう | **10** | **`mode & 077 == 0`**(group / other にいかなる権限も無いこと)へ変更し 0600 と 0400 の双方を受理。regular file・所有者一致・64KiB 上限・symlink 拒否・DSN 非出力テストは維持 |
| N4 | 見積もりが再算定されていない(02 / 10 が変わり、01 / 03 / 04 / 05 / 06 / 07 / 08 / 09 / 11 も増えた) | **本書**(各計画の §見積もり を出典) | 全 11 計画の §見積もり の実数から再構築し、raw と +30% buffer の両方を表示(§リリース対応)。合計 raw **38.5 日 → 59.0 日**、buffer 後 **77 日**。二重計上の禁止規則(05 の MTU / 08 の `SerializeInitialize`)を明記 |
