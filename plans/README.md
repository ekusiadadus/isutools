# 計測ギャップ解消 実装計画 v3(2026-08-04 第2回レビュー反映版)

初版(5fbe54c)は第1回レビューで再構成を指示され、v2(a3d26d9)で
基盤 3 計画 + 機能 7 計画へ分割した。v2 は第2回レビューで
「多くは解消、ただし **02→08→10 の run 境界契約に CRITICAL 3 件**が残る」
として差し戻された。本 v3 は指示された修正順序
(02 → 08 → 10 → 01 → 04/07/09 補強)に従って改訂したものである。

v3 の中核変更: **二段階境界(BeginBoundary/Drain)**、並行 reset の
**409 拒否 + nonce 冪等化**、process-wide **singleton Controller**、
分散プロトコルへの **FinishRun 終了バリア**と immutable run 取得、
**安定 TargetID** の第一 API 化。

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
| [02](./02-reset-coordinator.md) | Reset coordinator | **二段階境界(BeginBoundary/Drain)**・409+nonce・singleton Controller・run_id/ResetResult | 自己デッドロック・並行 reset 汚染・世代境界の証拠欠如。08/10 の前提 |
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

## 依存関係(「06 以外は独立」の撤回)

```
01 registry ──→ 04 sqlrows ──→ 09 explain
      │                              │
      └──────────→ 10 multi-host ←───┘
02 coordinator ─→ 08 auto-reset ─→ 10
      └──────────→ 06 dbpool(区間デルタの baseline 契約)
03 hoststats ───→ 10(peer identity / DB ホスト観測)
05 network ─────→ 10(agent の観測項目)
07 profiles: 01/02 と独立(artifact 保存のみ 02 の run_id を使用)
```

- 01/04/09/10 は DSN 保持の一元化(registry)を共有する
- 02/08/10 は reset 契約を共有する
- 03/05/10 は FS 注入(procfs と sysfs の分離)と Provider/health 表示を共有する

## リリース対応(見積もり v3 改訂: 個別計画の単純合計 + buffer)

```
v1.2.0: 01(2日) + 02(3日) + 04(2.5日)  = 実装 7.5 日
v1.2.x: 03(2日) + 05(1.5日) + 06(1日)  = 実装 4.5 日
v1.3.0: 07(2日) + 08(1.5日) + 09(2.5日) = 実装 6 日
v1.4.0: 10                              = 15 日(ADR 2 日を含む)
```

上記は実装のみの単純合計。各リリースには **統合・機能単位 ABBA・
ドキュメント・レビュー対応の buffer として +30%** を見込む
(v1.2.0 ≈ 10 日、v1.2.x ≈ 6 日、v1.3.0 ≈ 8 日)。

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
