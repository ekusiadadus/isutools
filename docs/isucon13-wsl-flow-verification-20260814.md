# ISUCON13 WSL2 Flow Labels実機検証（2026-08-14）

`ekusiadadus@ssh.almightty.org`のWSL distribution `isucon13`へ、User Flowと
Scenario Stories用の安全な疑似session・scenario labelを導入した記録です。既存の
ISUCON13 Go初期実装とisutools配線をbackupしてから、同一binaryでON/OFFを切り替え、
smoke test、ABBA micro benchmark、公式benchmarkの順に確認しました。

## 固定した対象

| 項目 | 値 |
|---|---|
| wsl-isucon revision | `876bd43a6f6f1048b8d341b2ab62d7afff01efd2` |
| local isutools base | `e8ada1188f5028c0350a9cacaeaeec4ff44e1b2a` + 本変更（未commit） |
| remote source | `/home/isucon/isutools-flow-src-20260814-191340` |
| backup | `/home/isucon/isutools-flow-backup-20260814-191340` |
| final binary SHA-256 | `d1098d081107e242fbb6a39cbe807bce97c49ff4c134e505c776af749c03bc2c` |
| 管理port | `127.0.0.1:19196` |

GoアプリはEcho v4 adapterを使用します。`SESSIONID` cookieはHMAC-SHA256による
非可逆な疑似IDに変換し、明示scenario `isucon13_official`を付けました。nginxは
upstream responseで返った内部labelだけをLTSVへ記録し、clientからの同名headerは消去し、
response headerも外部へ公開しません。HMAC keyは`/etc/isutools/flow-label.env`へ
`root:root 0600`、親directoryを`0750`として保存しています。

## ON/OFF smoke test

同一の3 requestを、同じsynthetic `SESSIONID`で送って比較しました。

| 状態 | access log | User Flow | Scenario Stories | health |
|---|---:|---:|---:|---|
| ON | 3 | 2 | 1 | `ok: enabled` |
| OFF | 3 | 0 | 0 | `disabled: flow-labels-off` |

OFFは一時drop-in `zz-flow-off.conf`だけで行い、検証後に削除してONへ戻しました。
clientが送った偽のsession/scenario headerは採用されず、保存artifactとclient responseには
raw cookie、偽label、HMAC keyが含まれないことも確認しました。

## 公式benchmark

最終ON状態で`./bench run --dns-port 1053 --enable-ssl`を実行し、終了code 0、
`pass=true`、score `11,928`でした。これは動作・収集経路の実証であり、性能保証ではありません。

| 項目 | 結果 |
|---|---:|
| access log lines | 11,701 |
| User Flow | 上位20 |
| Scenario Stories | 上位20 |
| top transition | `GET /api/livestream/:livestream_id/reaction` → `POST /api/livestream/:livestream_id/reaction`（647） |
| top story | `isucon13_official` / `POST /api/icon`（236 sessions、236 requests） |

上位件数・session数・step数は意図的にboundedです。このrunでは候補が上限を超えたため、
access log healthはtruncationを明示する`degraded`になりました。表示済み上位20件は有効ですが、
全候補を保持したという意味ではありません。

成果物はWSL ext4へ保存しています。

| artifact | SHA-256 |
|---|---|
| `official-benchmark-20260814-194019-run-4a02ba00dacefa4e.json` | `059554579daad46d3e4b5d9e67695506f2429997ad32d8616d18b5a86ac80ef4` |
| `20260814-194019.322952180-000001_gen3_876bd43-dirty_score11928.json` | `9a808eb02666aaa05f95854ddc3b5fba128220ca2bfb023de7357731eab183fa` |
| 同名`.html` | `73854ed3d01b9e054232b4f81095beb1f618fa4c7755d9c45cee2dc56862b77b` |

## UI証跡

上記の自己完結HTMLを制御PCへ取得し、Chrome headlessで1600px幅にレンダリングしました。
READMEの画像はlive UIを作り直したmockではなく、この保存済みrunの該当sectionを切り出したものです。

| 画像 | SHA-256 |
|---|---|
| `docs/images/isutools-isucon13-scenario-stories.png` | `f507000bbff12cea51925efe03f764180e19f7c2e4abf594bb6642d60f6e538f` |
| `docs/images/isutools-isucon13-user-flow.png` | `08b6c6264c7e20eb3773ddf86a16e442ca5be5f26914c2e15cc8dbf9163f0fd9` |

## ABBA性能gate

`/api/tag`へ各500 request・concurrency 10、3 blockの
`off → on → on → off`を、上記と同じbinary fingerprintで実行しました。

| 指標 | 推定差 | 95% CI | gate | 判定 |
|---|---:|---:|---:|---|
| score overhead | 1.4710% | -2.3489%〜5.2908% | 上限2% | fail |
| p95 regression | 2.4812% | -14.2025%〜19.1649% | 上限2% | fail |
| error-rate delta | 0.0000 | 0.0000〜0.0000 | 上限0.001 | pass |

短時間sampleの分散が大きく、2%以下を統計的に確認できなかったためoverallは`FAIL`です。
機能導入は完了していますが、厳格な性能受入条件を満たしたとは扱いません。raw dataは
`/home/isucon/isutools-data/flow-label-abba-final-20260814.tsv`と同名`.meta`にあります。

## PR前の現行binary再検証

HTTP middlewareのoptional interface互換性と1xx response処理をreviewで修正した後、現行sourceを
`/home/isucon/isutools-flow-src-20260814-213041`へ再配置し、同一binaryで公式benchmarkと
10 block ABBAを再実行しました。READMEのUI画像は上記11,928 run由来のままですが、画像と
保存HTMLのhash対応は変わりません。

| 項目 | 結果 |
|---|---|
| current binary SHA-256 | `21972dd96019f2492a679e46e6608b390c1d12584fe96f96b88d6e0af9be9416` |
| official benchmark | `pass=true`、score `11,983` |
| access log / flow / story | 12,198行 / 上位20 / 上位20 |
| score overhead | -0.2549%（95% CI -2.5226%〜2.0128%、gate上限2%、fail） |
| p95 regression | 0.5571%（95% CI -2.5673%〜3.6816%、gate上限2%、fail） |
| error-rate delta | 0.0000（95% CI 0.0000〜0.0000、gate上限0.001、pass） |

公式benchmark artifactは
`/home/isucon/isutools-data/official-benchmark-20260814-213413-run-34426160b7cbd146.json`
（SHA-256 `d496244d455d30a7949f1352313e1319ca7dc09a2a4c290a7261b446234a9ddc`）、
snapshot JSONは`20260814-213413.676169571-000001_gen4_876bd43-dirty_score11983.json`
（SHA-256 `2884ddb462987437f7d677086e1456443b5fa875d1792a6cd8d1b506d22c40e6`）です。
ABBA raw dataは`/home/isucon/isutools-data/flow-label-abba-pr-20260814.tsv`と同名`.meta`に
保存しました。scoreのCI上限は2%を0.0128 point上回り、p95も上限を超えたため、sample数を
増やした後も厳格な性能gateは`FAIL`のままです。既定の`auto`かつ未設定時と明示`off`では
middleware wrapperを通らない設計ですが、ON時の2%以下を実証したという主張はしません。

## 最終状態とrollback

最終確認時点で`nginx`と`isupipe-go`は`active`、`nginx -t`は成功し、実行中processは
`/home/isucon/webapp/go/isupipe`を参照しています。flow labelsはONです。PR前監査ではprocessに
keyが残る一方で再起動用`flow-label.env`が欠落していたため、同じkeyを出力せずに
`root:root 0600`で復元しました。復元後の再起動・readinessとkey読込まで再確認しています。

rollback時はbackupの`app/main.go`、`go.mod`、`go.sum`、`isupipe`、
`system/nginx/isutools.conf`、`system/systemd/isutools.conf`を元の場所へ戻し、
`flow-labels.conf`と一時overrideを外して`daemon-reload`後にnginx・isupipe-goを再起動します。
backupと計測artifactは自動削除していません。
