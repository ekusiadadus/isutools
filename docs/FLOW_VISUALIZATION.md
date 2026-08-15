# Flow Visualization

User FlowとScenario Storiesのboundedな実測データを、改善判断へ使える4つの表示へ変換します。

| 表示 | 意味 | 注意点 |
|---|---|---|
| Journey Funnel | 明示stepへ到達した疑似session、conversion、drop-off、retry、p95、4xx/5xx | URLや認証状態からgoalを推測しない |
| User Flow Graph | 上位node/edgeの有向遷移。循環をそのまま表示 | node数超過は`(other)`へ集約 |
| Transition Heatmap | 同じgraphのfrom/to行列 | request latencyではなく遷移count |
| Run Diff | funnelのpercentage-point差とtransition count差 | workload shapeの差であり因果効果ではない |

## 最小設定

```bash
export ISUTOOLS_FLOW_LABELS=on
export ISUTOOLS_SESSION_COOKIE=SESSIONID
export ISUTOOLS_SESSION_HMAC_KEY='32-byte以上のgitへ入れない乱数'
export ISUTOOLS_FLOW_SOURCE=middleware
export ISUTOOLS_FLOW_VIZ=on
export ISUTOOLS_FUNNEL_CONFIG=/etc/isutools/funnels.yaml
```

configはアプリ実行userが読めるregular fileにします。HMAC key用のroot-only directoryを共用して
権限を緩めず、必要なら[ISUCON13例](../examples/isucon13-wsl/README.md)のように別directoryへ分離します。
環境変数とconfigはprocess起動時に1回だけ読みます。変更後はアプリprocessを再起動します。

[ISUCON13設定例](../examples/isucon13-wsl/funnels.isutools.yaml)を必要なscenarioとrouteへ合わせて変更します。
ファイル形式は[JSON Schema](./schemas/flow-funnels-v1.schema.json)でも確認できます。JSONはYAMLのsubsetとして同じloaderで扱います。

## 集計契約

- `mode: ordered`だけを受理し、定義した順序で最初に到達したsessionを数えます。
- step間に別routeがあってもよく、後段だけを先に通ってもconversionにはなりません。
- 到達済みstepの再訪はretryです。`requests`と`sessions`を分けるためpoll/retryをconversionと誤認しません。
- `within`は最初のstepからの時間窓で、最大24時間です。次のstepが来ないsessionも、同じgenerationで観測した最新時刻が窓を越えた時点で`expired`として残ります。完了済みsessionとは重複しません。
- middleware sourceはstatus、duration、request開始時刻を渡します。proxy sourceはrequest duration/statusを持ちますがwall-clock timestampを持たないため、時間窓はpartialになります。
- conversionは疑似session単位、p95/4xx/5xxは受理したstep request単位です。

## 上限と失敗状態

- config: 64 KiB、構造nesting 32、構造記号4,096、16 funnels、各2〜16 steps
- session states: 10,000 / generation
- graph: 既定16 nodes / 48 edges、hard cap 32 / 128
- scenario/ID: 64 bytes、route template: 512 bytes
- YAML alias/anchor、unknown field、duplicate funnel/step/route、query/fragmentを含むrouteを拒否
- sessionやtimingの欠損、無効graph edge、上限超過は`partial`とCollector Healthへ表示

HTMLはinline SVG/CSSだけで完結し、CDNや外部JavaScriptを要求しません。保存snapshotにも集計済みデータを含むため、後から同じgraph・heatmap・diffを再表示できます。

## ISUCON13での実測ストーリー

公式benchmarkで予約ファネル45.01%、reaction投稿100.00%を計測し、graph/heatmapを保存しました。
最初のrunで完了済みsessionをexpiredにも数える不整合が見えたため、回帰testを追加して修正し、
再runで重複が解消したことまで確認しています。score差は性能改善とは解釈しません。

値、artifact hash、調査に使える読み方は
[ISUCON13 Journey Visualization実機検証](./isucon13-flow-visualization-verification-20260815.md)にあります。
