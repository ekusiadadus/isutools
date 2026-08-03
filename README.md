# isutools

ISUCON 向けオールインワン計測モジュール。`go get` + 2行でアプリに組み込み、
SQL / HTTP / GraphQL / アクセスログ / プロセスリソースをブラウザでソート済み表示する。

- 設計書: [DESIGN.md](./DESIGN.md)
- Status: M1 実装済み(SQL 計測 + snapshot UI + buildinfo + host 情報)

## 使い方(組み込みは1行)

```go
db, _ = sqlx.Open(isutools.SQLDriverName("mysql"), dsn) // 既存の sqlx.Open を書き換えるだけ
```

これだけで管理サーバが `127.0.0.1:19191` に起動する(`ISUTOOLS_ADDR` で変更・`off` で無効):

- `GET :19191/` — ライブレポート(合計時間降順ソート済み)
- `GET :19191/snapshot.html` — 自己完結 HTML をダウンロード(手元の PC でダブルクリックで閲覧)
- `GET :19191/json` — 機械可読出力(前回世代との比較用に `prev` 付き)
- `POST :19191/reset` — 集計リセット(ベンチ前に叩く。世代番号が進む)
- `ISUTOOLS=off` で全機能無効(素のドライバ名を返すため、オーバーヘッドゼロ)

管理サーバはアプリのルーター・nginx を経由しないため外部に露出しない。
同一ポートに載せたい場合は `isutools.Handler()` を任意のルーターに Mount してもよい。

レポートには git revision(+dirty)・ホスト情報(CPU モデル / コア数 / メモリ GB / OS)が常に表示される。

## 対応対象(Phase 1)

MySQL / MariaDB / PostgreSQL / nginx / Apache / GraphQL /
HTTP 1.1 / HTTP/2 / HTTP/3 (QUIC) / git hash (+dirty) / プロセス CPU・メモリ
