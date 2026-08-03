# isutools

ISUCON 向けオールインワン計測モジュール。SQL計測とloopback管理UIを
`go get` + 1行で組み込み、ベンチ結果を自己完結HTML/JSONとして回収する。

- 設計書: [DESIGN.md](./DESIGN.md)
- License: MIT
- Runtime: Go 1.24+
- Status: v0.2 candidate。M1に加えてcollector health、SQL/HTTPの原子的generation、
  HTTP、nginx LTSV、区間procstats、DB schema/snapshot履歴をローカル実装済み。
  private-isuへのv0.2再統合とABBA性能判定は未実施

## 使い方(組み込みは1行)

```go
db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // 既存行を書き換えるだけ

// HTTPも集計する場合は既存Handlerを1回包む
http.ListenAndServe(":8080", isutools.HTTP(handler))
```

対象driverはこの呼び出しより前にblank import等で `database/sql` へ登録しておく。
登録成功時は管理serverが `127.0.0.1:19191` に一度だけ起動する。

- `GET http://127.0.0.1:19191/` — ライブレポート(合計時間降順ソート済み)
- `GET http://127.0.0.1:19191/snapshot.html` — 自己完結HTMLをダウンロード
- `GET http://127.0.0.1:19191/json` — 機械可読出力(`prev`付き)
- `POST http://127.0.0.1:19191/reset` — ベンチ前の集計reset
- `POST http://127.0.0.1:19191/collect` — buffered nginx logを期限付きでflush待ち・回収
- `POST http://127.0.0.1:19191/save?score=<score>` — HTML/JSONをatomic保存
- `GET http://127.0.0.1:19191/files/<name>` — 保存済みsnapshotを取得
- `ISUTOOLS=off` で全機能無効(素のdriverを使い、query pathの追加処理はゼロ)
- `ISUTOOLS_ADDR=off` で管理serverだけ無効(SQL集計は継続)

既定の管理serverはアプリのrouter・nginxを経由せず、loopback以外へbindしない。
`ISUTOOLS_ADDR=0.0.0.0:19191` のような非loopback bindは
`ISUTOOLS_TOKEN` が必須で、全endpointを `Authorization: Bearer <token>` で保護する。
tokenなしの非loopback指定はfail-closedで管理serverを起動しない。
同一ポートに載せる場合は `isutools.Handler()` を任意routerへmountできるが、
アクセス制御は呼び出し側の責任になる。

`SQLDriverName` は計測登録に失敗しても元driver名へfail-openする。アプリ起動は守る一方、
欠損はsnapshot schema v3の `meta.partial` / `meta.health` に残す。

## M2 collector設定

nginxは [examples/nginx-isutools.conf](./examples/nginx-isutools.conf) のLTSV formatを使い、
アプリプロセスから読めるログパスを設定する。

```sh
ISUTOOLS_NGINX_LOG=/var/log/nginx/isutools.log
```

Linuxではprocstatsを自動で有効化し、`POST /reset` からsnapshot取得までの
CPU差分(1 core=100%)と終了時RSSを表示する。PID namespaceを分けたcontainerでは
host processを見るため `pid: "host"` 等が別途必要になる。

SQLとHTTPはそれぞれ、reset開始前のin-flight計測を旧generationへ完了させてから凍結する。
管理endpoint自体は同時resetを直列化する。ただしcollector間のswapは現在逐次実行なので、
release gateでは従来どおり「reset応答後にベンチを開始」を必須とする。
ベンチ後は `POST /collect` の成功を確認してからsnapshot/saveを取得する。

## ロードマップ

| Version | 対象 | 状態 |
|---|---|---|
| v0.1.0 / M1 | database/sql、Snapshot HTML/JSON、buildinfo、host情報 | 実装・private-isu基本統合済み |
| v0.2 / M2 | HTTP/1.1・2、nginx LTSV、ベンチ区間procstats、health/security | local candidate実装済み、remote/ABBA未完 |
| v0.3 / M3 | Apache、gqlgen operation adapter | 未実装 |
| v1.0 / M4 | WebSocket/SSE接続、HTTP/3互換性、全体ABBA gate | 未実装 |

pgx native API、分散trace、外部storage、genericなWebSocket frame計測はv1対象外。
詳細な契約・受け入れ条件・未決事項は [DESIGN.md](./DESIGN.md) を参照。
