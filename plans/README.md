# 計測ギャップ解消 実装計画(2026-08-04)

ISUCON 優勝記事・作問記事 5 本を isutools v1.1.0 の実装と突き合わせた調査で
特定した計測ギャップ 6 件の実装計画。各計画は独立した文書として `plans/`
配下に置き、実装時は 1 計画 = 1 ブランチ = 1 PR とする。

調査根拠の記事:

| 記事 | 主な計測手法 |
|---|---|
| ISUCON12 優勝 (NaruseJun) | pprotein(pprof+alp+slp)、Netdata、帯域ボトルネック |
| ISUCON13 優勝 (NaruseJun) | slp の Rows examined/sent 比、/initialize 自動計測、Sankey |
| ISUCON14 感想戦 (kawaemon) | flamegraph、TIME_WAIT 監視、ロック保持時間計測 |
| ISUCON10 予選作問 (progfay) | EXPLAIN(Using filesort)、降順/空間インデックス |
| isucandar 解説 (catatsuy) | ベンチマーカー設計論(isutools 側ギャップなし) |

## 計画一覧と優先順位

| # | 計画 | 効果 | 実装コスト | リリース目標 |
|---|---|---|---|---|
| [01](./01-sql-row-stats.md) | SQL 行効率(rows examined/sent) | 最大: インデックス判断の主指標 | 中 | v1.2.0 |
| [02](./02-runtime-contention.md) | mutex/block プロファイル + DB プール待ち | 大: ロック/プール詰まりの直接証拠 | 小 | v1.2.0 |
| [03](./03-procstats-network.md) | TIME_WAIT / NIC 帯域のランタイム観測 | 中: 接続枯渇・帯域飽和の検出 | 小 | v1.2.x |
| [04](./04-auto-reset-initialize.md) | /initialize 自動計測開始 | 中: 運用摩擦の削減(pprotein 方式) | 小 | v1.2.x |
| [05](./05-query-plan-capture.md) | 代表クエリの EXPLAIN 自動化 | 中: filesort/フルスキャンの根因提示 | 中 | v1.3.0 |
| [06](./06-multi-host.md) | 複数台構成の横断計測 | 大: app/DB 分離後の可視性 | 大 | v1.4.0 |

01 と 05 は重複領域がある(performance_schema の `SUM_NO_INDEX_USED` /
`SUM_SORT_MERGE_PASSES` が EXPLAIN の所見を部分的に代替する)。01 を先に
出荷し、05 は 01 で不足した「どのインデックスが使われたか」の粒度を補う。

## 全計画に共通する制約

1. **fail-open**: 計測の失敗はアプリを止めない。入力が得られないチェックは
   `StatusSkip` / セクション非表示に劣化する(advisor・collector 共通の既存方針)。
2. **オーバーヘッド既定ゼロ**: ランタイムコストが発生しうる機能
   (mutex/block プロファイル、EXPLAIN、exemplar 保持)は既定 off の
   opt-in とし、環境変数で明示的に有効化する。
3. **ABBA ゲート**: 各リリース tag の前に DESIGN.md §7 の on/off ベンチ比較を
   実施する。v1.1.0 は明示免除で出荷したが、常態化させない。
   計測系追加ごとに `examples/abba.sh` の再実行を release checklist に含める。
4. **TDD**: テスト先行(RED→GREEN)。集計カバレッジ 80% 以上を維持
   (CI ゲート)。Linux 依存の /proc 読みは `fs.FS` 注入 + `fstest.MapFS` で
   ユニットテストする(advisor/procstats の既存様式)。
5. **JSON 互換性**: Snapshot への追加は additive(omitempty)とし、既存
   キーの意味を変えない。schema_version は現行 3。トップレベル構造が
   変わる 06 のみ 4 へ bump する。
6. **ドキュメント**: 各 PR に README 環境変数表・docs/INTEGRATION.md・
   docs/IMPLEMENTATION_STATUS.md の更新を含める。
7. **配線パターンの踏襲**: 起動時検査は `advisor.Collect`、区間依存データは
   snapshot 時の `advisor.WithX` 差し替え(`WithQUICTelemetry` /
   `WithCacheTelemetry` と同型)、収集器は reset-to-snapshot デルタ
   (procstats と同型)、web は `Provider` の nil-skip フィールド追加で行う。

## リリース順序と依存関係

```
v1.2.0: 01 (sqlrows) + 02 (contention)   … 独立、並行実装可
v1.2.x: 03 (network) + 04 (auto-reset)   … 独立、並行実装可
v1.3.0: 05 (explain)                     … 01 の advisor 統合を再利用
v1.4.0: 06 (multi-host)                  … 03 の agent 側価値が前提
```

06 以外は互いに独立しており、順序の入れ替えは可能。06 のみ、単体ホストで
取れる情報(特に 03)が揃ってから着手する方が agent の価値が高い。
