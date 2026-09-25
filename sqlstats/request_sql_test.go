package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/ekusiadadus/isutools/internal/requestsql"
	proxy "github.com/shogo82148/go-sql-proxy"
)

func TestContextQueriesIncrementRequestCounter(t *testing.T) {
	t.Cleanup(Default.Reset)
	if err := Register("isutoolsfake"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("isutoolsfake"+DriverSuffix, "request-sql")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, counter := requestsql.WithCounter(context.Background())
	if _, err := db.ExecContext(ctx, "UPDATE t SET c = 1"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("request SQL count = %d, want 2", got)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET c = 2"); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("late query changed request count to %d", got)
	}
}

func TestHookCountsFailuresButNotErrSkip(t *testing.T) {
	t.Cleanup(Default.Reset)
	ctx, counter := requestsql.WithCounter(context.Background())
	h := hooks()
	stmt := &proxy.Stmt{QueryString: "SELECT 1"}
	pre, err := h.PreExec(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostExec(ctx, pre, stmt, nil, nil, driver.ErrSkip); err != nil {
		t.Fatal(err)
	}
	pre, err = h.PreExec(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostExec(ctx, pre, stmt, nil, nil, errors.New("database failed")); err != nil {
		t.Fatal(err)
	}
	pre, err = h.PreQuery(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostQuery(ctx, pre, stmt, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("request SQL count = %d, want 2", got)
	}
	if entries := Default.Snapshot(); len(entries) != 1 || entries[0].Count != 2 || entries[0].ErrorCount != 1 {
		t.Fatalf("SQL aggregate = %#v", entries)
	}
}
