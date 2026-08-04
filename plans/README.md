# 計測ギャップ解消 実装計画 v5(2026-08-04 第4回レビュー反映版)

初版(5fbe54c)→ v2(基盤+機能へ再構成)→ v3(run 境界契約)→
v4(0b68d6c、2 契約分離)と改訂し、v4 も第4回レビューで CRITICAL 4 件
(run 終了境界・部分失敗回復・自動 EXPLAIN 安全性)により差し戻された。
本 v5 はレビューの承認条件
「**02 に Start → Finish → Abort → Ack/Expire の完全な run lifecycle を
定義し、それを 10 の wire protocol へ一対一で写す**」に従う改訂である。

v5 の中核変更: 02 の collector 契約に **Freeze / CaptureFinal
(全 collector 共通の終了境界)**と **handle を返す BeginBoundary**を
定義し、run lifecycle を完全な状態機械(Abort・Ack/Expire 含む)にした。
10 は Start/Finish/Abort/Ack を wire に一対一で写し、**遷移表・
AbortRun による部分開始回復・freeze 受付 + polling の deadline 分離**を
定義。09 は**最小権限 inspector ユーザー必須**(EXPLAIN SELECT でも
stored function 経由の副作用があり得るため)。

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
| [01](./01-db-target-registry.md) | DB target registry | **安定 TargetID**(DSN 構造から決定的導出 + 明示登録)+ 接続1本の inspector + allowlist 表示 | shard 非対応・接続順命名の不安定・DSN(credential)の露出。04/06/09/10 の前提 |
| [02](./02-reset-coordinator.md) | Run lifecycle coordinator | **完全な run lifecycle(Start/Finish/Abort/Ack/Expire)**・generation/baseline の 2 契約(handle 付き境界 + 全 collector 共通の終了 freeze)・409+nonce・singleton Controller・不変 StartResult | 自己デッドロック・冒頭欠落・終了境界の混入・部分失敗からの回復。08/10 の前提 |
| [03](./03-hoststats.md) | hoststats | memory/disk/PSI/cgroup/ホスト同一性(namespace 含む) | 「OS 資源が見える」の過大表現の解消。10 の前提 |

機能:

| # | 計画 | 旧番号 | 要点 |
|---|---|---|---|
| [04](./04-sql-row-stats.md) | SQL 行効率 | 旧01 | **全 digest baseline** 方式へ修正。NULL digest overflow・counter 後退・sent=0 を正しく扱う |
| [05](./05-network-stats.md) | ネットワーク観測 | 旧03 | v1 は**表示のみ**(advisor 閾値なし)。単位・namespace・新規 NIC を修正 |
| [06](./06-db-pool-stats.md) | DB プール統計 | 旧02の一部 | v1 は**表示のみ**。advisor 閾値は private-isu 実測後 |
| [07](./07-runtime-profiles.md) | runtime プロファイル | 旧02の一部 | rate 意味論の修正。**累積プロファイル**として正しく扱い、reset/save の 2 点 + diff_base 手順を提供 |
| [08](./08-auto-reset.md) | 計測開始の自動化 | 旧04 | HTTP 自己呼び出し廃止。**同期 API(ResetNow)が正、middleware は best-effort に格下げ** |
| [09](./09-query-plan-capture.md) | EXPLAIN 自動化 | 旧05 | **raw exemplar 廃止**。MySQL 8 の QUERY_SAMPLE_TEXT 経路に限定。実行は collect/save 時のみ |
| [10](./10-multi-host.md) | 複数台横断計測 | 旧06 | **ADR からやり直し**。agent protocol / hub / distributed reset の 3 段階、2〜3 週間規模 |
| [11](./11-nginx-transport.md) | nginx transport / ランタイム | 新規 | ISUCON 本 9-8/9-9 由来: UNIX domain socket 機会・`listen backlog=`・PGO の静的検査 + MTU 表示(05 に委譲)。依存なし・即実装可 |

## 依存関係(v4: 依存種別を明示)

**02 は全計画の実装順依存**である点に注意(v3 の図は 06/08/10 のみを
02 依存としており、02 自身の「全 collector 移行」要求と矛盾していた)。

| 依存元 | 依存先 | 種別 |
|---|---|---|
| 04 sqlrows / 09 explain / 10 multi-host / 06 dbpool | 01 registry | **API 依存**(TargetID / Inspect / Features) |
| 03 / 04 / 05 / 06(BaselineCollector)、08 / 10(StartRun・nonce) | 02 coordinator | **実装順依存**(collector 契約への移行が前提) |
| 07 profiles | 02 | **artifact 参照**(run_id をファイル名に使用)+ 設定箇所(singleton runtime 初期化) |
| 09 explain | 04 | API 依存(digest delta と DB 側 UTC 境界時刻) |
| 10 multi-host | 03(identity/hoststats)・05(観測項目)・01・02 | 実装順依存 |
| 11 nginx-transport | 05 | **実装委譲**(MTU 列は 05 の実装に含める。静的検査 2 件と go-pgo は独立) |
| 03/05 | (相互)| FS 注入設計(procfs/sysfs 分離)の共有 |

## リリース対応(見積もり v4 改訂: 全計画を計上 + buffer 数値化)

```
v1.2.0: 01(2日) + 02(5日) + 04(2.5日)            = 実装 9.5 日 → +30% ≈ 12.5 日
v1.2.x: 03(2日) + 05(1.75日※) + 06(1日) + 11(1日) = 実装 5.75 日 → +30% ≈ 7.5 日
v1.3.0: 07(2日) + 08(1.5日) + 09(3日)             = 実装 6.5 日 → +30% ≈ 8.5 日
v1.4.0: 10                                        = 17 日 → +30% ≈ 22 日
※ 05 は 11 から委譲された MTU 列 +0.25 日を含む
(v5: 02 の Finish/Abort 契約 +1 日、09 の最小権限検証 +0.5 日、
 10 の Abort/Ack + 遷移表 +2 日を反映)
```

buffer(+30%)は統合・機能単位 ABBA・ドキュメント・レビュー対応分。

## 全計画共通の契約

1. **fail-open**: 計測失敗はアプリを止めない。skip 理由は health に残す。
2. **feature flag 必須**: 各機能は専用の環境変数で単独 on/off できる。
   既定値は各計画に明記(ランタイムコストのあるものは既定 off)。
   v3 時点の一覧: `ISUTOOLS_SQLROWS`(04・既定on)、
   `ISUTOOLS_HOSTSTATS`(03・既定on)、`ISUTOOLS_NETSTATS`(05・既定on)、
   `ISUTOOLS_DBPOOL`(06・登録時のみ)、`ISUTOOLS_MUTEX_FRACTION` /
   `ISUTOOLS_BLOCK_RATE_NS` / `ISUTOOLS_HEAP_PROFILE`(07・**既定off**)、
   `ISUTOOLS_RESET_ON_INITIALIZE`(08・既定off)、
   `ISUTOOLS_EXPLAIN`(09・**既定off**)。
   **例外(v4 で明文化、v5 で表現修正)**: 設定ファイル・buildinfo の
   読み取りだけで完結する静的 advisor check(11 の 3 check、既存の
   nginx/OS check 類)は、**ベンチ実行中の追加観測を行わない**
   (設定 I/O と解析は境界時のみ)ため専用 flag を要求しない。
   ランタイム観測・追加クエリ・追加 I/O を伴う機能のみ flag 必須とする。
3. **機能単位 ABBA**: `examples/abba.sh` を拡張し、
   (a) 全機能 off vs 全機能 on、(b) baseline vs 単一機能 on、の両モードを
   サポートする。リリース tag 前に対象機能の (b) を必ず実施する。
   全体 on/off だけでは追加機能単体の影響を分離できないため。
4. **schema version 契約**(本版で定義):
   - additive(新しい省略可能キーの追加): bump しない。
     `meta.capabilities` 配列(additive で導入)に機能名を追加して宣言する
   - 既存キーの意味変更: bump する
   - キーの削除・型変更(破壊): bump + 移行注記
   - peer 互換判定は revision ではなく
     `schema_version` + `protocol_version` + `capabilities` で行う(→ 10)
5. **TDD** + 集計カバレッジ 80%(CI ゲート)。/proc・/sys は FS 注入 +
   `fstest.MapFS`。procfs と sysfs は**別の FS として注入**する
   (現行 procstats の注入 root は /proc であり /sys を読めないため)。
6. **advisor 閾値は実測に基づく**: 新しい warn 閾値は private-isu での
   フィールド検証を経てから既定有効にする。それまでは表示のみ、
   または provisional と明記する。
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
| [HIGH] ResetResult の可変性 / request context の background 波及 | 02: 不変 StartResult + PreviousRunResult 分離。background は WithoutCancel + 内部 timeout |
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
| [LOW] README の ResetResult 表記 / 静的 check の「コストゼロ」/ 07 の heap・手動削除残存 / 04 の risk 表先祖返り / 見積もり未再算定 | 各該当箇所を修正(本 v5 に反映済み) |
