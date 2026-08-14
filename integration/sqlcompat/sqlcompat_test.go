package sqlcompat

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/ekusiadadus/isutools/sqlstats"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

func TestSQLiteDatabaseSQLInstrumentation(t *testing.T) {
	testInstrumentedDatabase(t, "sqlite3", ":memory:", []string{
		`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO items (name) VALUES ('fixture')`,
	}, `SELECT count(*) FROM items`)
}

func TestPostgreSQLDatabaseSQLInstrumentation(t *testing.T) {
	dsn := os.Getenv("ISUTOOLS_INTEGRATION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ISUTOOLS_INTEGRATION_POSTGRES_DSN is not set")
	}
	testInstrumentedDatabase(t, "postgres", dsn, []string{
		`CREATE TEMP TABLE items (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL)`,
		`INSERT INTO items (name) VALUES ('fixture')`,
	}, `SELECT count(*) FROM items`)
}

func testInstrumentedDatabase(t *testing.T, driverName, dsn string, statements []string, query string) {
	t.Helper()
	sqlstats.Default.Reset()
	if err := sqlstats.Register(driverName); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName+sqlstats.DriverSuffix, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}

	var observations int64
	for _, entry := range sqlstats.Default.Snapshot() {
		observations += entry.Count
	}
	if observations < int64(len(statements)+1) {
		t.Fatalf("recorded queries = %d, want at least %d", observations, len(statements)+1)
	}
}
