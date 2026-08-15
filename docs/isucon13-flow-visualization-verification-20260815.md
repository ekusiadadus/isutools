# ISUCON13 Journey Visualization実機検証（2026-08-15）

`ekusiadadus@ssh.almightty.org`のWSL distribution `isucon13`で、Journey Funnel、
User Flow Graph、Transition Heatmapを公式benchmarkへ接続した記録です。既存binaryを退避し、
新しいsource/build/configを別directoryへ配置してから切り替えました。

## 対象と最終状態

| 項目 | 値 |
|---|---|
| ISUCON13 app revision | `5461a09381e3188bc4e2de40ea34d1f97aed3bcc` + 既存計測配線 |
| Go / host | Go `1.24.0` / linux-amd64 |
| artifact取得binary SHA-256 | `efe0f93c333f213d6f12857e42574cb9c3ae49113d1806fce6a20f08167be36c` |
| 最終稼働binary SHA-256 | `1508f64aae24768b2d1283844a4db2086b3e2be09558c7dda7b995546a68c907` |
| funnel config SHA-256 | `3636a421dcce5a4c2411205238841190c8adc729981bcc04617e459f1cbcd9da` |
| 最終source | `/home/isucon/isutools-flow-viz-final3-src-20260815-1600` |
| backup | `/home/isucon/isutools-flow-viz-backup-20260815-1350` |
| 管理port | `127.0.0.1:19196` |

最終確認時は`isupipe-go`、nginx、MySQL、PowerDNSがすべて`active`です。
`flow-viz` healthは`ok: funnel-configured`で、session dropとtiming missingは0でした。

## 公式benchmark実測

修正後runは`pass=true`、score `450,394`です。単独runのscoreは性能効果の証明には使いません。
このrunが証明するのは、公式workloadを壊さず、同じrun境界の実測journeyを保存・表示できることです。

| funnel | entered | completed | conversion | expired |
|---|---:|---:|---:|---:|
| `livestream_reservation` | 1,082 | 487 | 45.01% | 0 |
| `reaction_post` | 438 | 438 | 100.00% | 0 |

予約ファネルは`icon 1,082 → tags 658 → reserve 487 sessions`でした。reserve stepは6,001 requests、
5,514 retries、4xx 4,629、p95 10.486 msです。「予約が遅い」だけでなく、最終stepの大量retryと
4xxを同時に見られるため、失敗応答の条件と予約競合を先に調べる根拠になります。

reactionファネルは`read 438 → post 438 sessions`でconversion 100%でした。一方でread 22,871、
post 20,836 requestsなので、conversionとは別にpoll/retry負荷が大きいことが分かります。

User Flow Graphは105個の入力edgeを16 node / 48 edgeへbounded表示し、上限外2,747 transitionを
`hidden_count`で明示しました。上位には次が含まれます。

- `GET /api/user/:username/icon → 同route`: 250,517
- reaction GET → reaction POST: 24,147
- livecomment POST → reaction GET: 24,126

cycleとself-transitionは削除せず表示します。上限外を消えたことにしないため、graphは
`truncated=true`です。これはデータ欠損ではなく可視表示のbounded truncationです。

## 実測が見つけた集計不整合と修正

最初のrun（`pass=true`、score `400,664`）では、reaction funnelが
`entered=412 / completed=411 / expired=167`になりました。完了後のretryが時間窓を越えた時、
完了済みsessionをexpiredにも加算していたためです。性能差ではなく集計意味の不整合として扱い、
失敗する回帰testを追加して「expiredは時間窓内に完了しなかったsessionだけ」へ修正しました。

修正後runでは`entered=438 / completed=438 / expired=0`となり、completedとexpiredの重複が
解消しました。さらに、後続requestがない未完了sessionもgeneration内の最新観測時刻で期限判定する
回帰testを加え、最終binaryでGo 1.24 app test、再起動、healthを確認しました。最初のartifactも
削除せず、検出から修正までの証拠として保存しています。

## 保存artifact

| artifact | SHA-256 |
|---|---|
| `20260815-131202.386044726-000001_gen3_5461a09-dirty_score450394.json` | `e031d7dcf0c1474d76ae220584c93ec04c8984216252861a25349f70a63439d8` |
| 同名`.html` | `71a2fa1d1a6bf021252032b81c7ebb24c96fd972552d8aeb6aba16dbcbba2776` |
| `official-benchmark-20260815-131158-run-6398df0319f16729.json` | `a0a8b98eeccfd35e0d852f0dc5893e70e90bcfe78c9f6caababbcc190c288eb8` |

保存HTMLに`Journey Funnel`、`aria-label="user flow graph"`、`Transition Heatmap`があることを
実ファイルで確認しました。JSONにはraw `SESSIONID=`もHMAC keyの環境変数名も含まれません。
ブラウザのローカル`file://`制限により新しいscreenshotは作らず、自己完結HTMLとhashを一次証拠にします。
