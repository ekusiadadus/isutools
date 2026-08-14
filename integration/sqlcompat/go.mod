module github.com/ekusiadadus/isutools/integration/sqlcompat

go 1.24.0

require (
	github.com/ekusiadadus/isutools v0.0.0
	github.com/lib/pq v1.12.3
	github.com/mattn/go-sqlite3 v1.14.49
)

require github.com/shogo82148/go-sql-proxy v0.7.3 // indirect

replace github.com/ekusiadadus/isutools => ../..
