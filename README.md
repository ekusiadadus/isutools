# isutools

ISUCON 向けオールインワン計測モジュール。SQL計測とloopback管理UIを
`go get` + 1行で組み込み、ベンチ結果を自己完結HTML/JSONとして回収する。

- 設計書: [DESIGN.md](./DESIGN.md)
- License: MIT
- Runtime: Go 1.24+
- Status: M1 core v0.1.0実装済み(SQL計測 + snapshot UI + buildinfo + host情報)。
  collector health、原子的generation reset、private-isu ABBA検証は未完

## 使い方(組み込みは1行)

```go
db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // 既存行を書き換えるだけ
```

対象driverはこの呼び出しより前にblank import等で `database/sql` へ登録しておく。
登録成功時は管理serverが `127.0.0.1:19191` に一度だけ起動する。

- `GET http://127.0.0.1:19191/` — ライブレポート(合計時間降順ソート済み)
- `GET http://127.0.0.1:19191/snapshot.html` — 自己完結HTMLをダウンロード
- `GET http://127.0.0.1:19191/json` — 機械可読出力(`prev`付き)
- `POST http://127.0.0.1:19191/reset` — ベンチ前の集計reset
- `ISUTOOLS=off` で全機能無効(素のdriverを使い、query pathの追加処理はゼロ)
- `ISUTOOLS_ADDR=off` で管理serverだけ無効(SQL集計は継続)

既定の管理serverはアプリのrouter・nginxを経由せず、loopback以外へbindしない。
認証機能が入るまで、`ISUTOOLS_ADDR` を非loopback addressへ変更する運用はrelease対象外。
同一ポートに載せる場合は `isutools.Handler()` を任意routerへmountできるが、
アクセス制御は呼び出し側の責任になる。

`SQLDriverName` は計測登録に失敗しても元driver名へfail-openする。アプリ起動は守る一方、
計測欠損を表示するhealth/partial契約はM1の残作業である。

## ロードマップ

| Version | 対象 | 状態 |
|---|---|---|
| v0.1.0 / M1 | database/sql、Snapshot HTML/JSON、buildinfo、host情報 | core実装済み、review gate未完 |
| v0.2 / M2 | HTTP/1.1・2、nginx、ベンチ区間procstats | 未実装 |
| v0.3 / M3 | Apache、gqlgen operation adapter | 未実装 |
| v1.0 / M4 | WebSocket/SSE接続、HTTP/3互換性、全体ABBA gate | 未実装 |

pgx native API、分散trace、外部storage、genericなWebSocket frame計測はv1対象外。
詳細な契約・受け入れ条件・未決事項は [DESIGN.md](./DESIGN.md) を参照。
