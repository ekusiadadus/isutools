# isutools

ISUCON 向けオールインワン計測モジュール。`go get` + 3行でアプリに組み込み、
SQL / HTTP / GraphQL / アクセスログ / プロセスリソースをブラウザでソート済み表示する。

- 設計書: [DESIGN.md](./DESIGN.md)
- Status: 設計フェーズ(実装はこれから。TDD で進める)

## ゴール

```go
isutools.RegisterSQL("mysql")
db, _ = sqlx.Open("mysql"+os.Getenv("ISUTOOLS_SQL_POSTFIX"), dsn)
r.Mount("/debug/isutools", isutools.Handler())
```

→ `http://localhost:8080/debug/isutools` で合計時間降順のレポートが見える。

## 対応対象(Phase 1)

MySQL / MariaDB / PostgreSQL / nginx / Apache / GraphQL /
HTTP 1.1 / HTTP/2 / HTTP/3 (QUIC) / git hash (+dirty) / プロセス CPU・メモリ
