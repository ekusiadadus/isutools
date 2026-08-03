// Package sqlstats wraps database/sql drivers with a measuring proxy and
// aggregates every query into an in-memory table. Works with any driver
// (MySQL/MariaDB via go-sql-driver, PostgreSQL via pgx stdlib or lib/pq).
package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	proxy "github.com/shogo82148/go-sql-proxy"

	"github.com/ekusiadadus/isutools/internal/agg"
)

// DriverSuffix is appended to the original driver name on registration:
// Register("mysql") makes "mysql:isutools" available.
const DriverSuffix = ":isutools"

// Default is the table all proxied drivers report into.
var Default = agg.NewTable(agg.DefaultMaxKeys)

// Register wraps each named, already-registered driver and registers the
// measuring variant under name+DriverSuffix. Calling it again for the same
// name is a no-op.
func Register(names ...string) error {
	for _, name := range names {
		if err := register(name); err != nil {
			return err
		}
	}
	return nil
}

func register(name string) error {
	wrapped := name + DriverSuffix
	for _, n := range sql.Drivers() {
		if n == wrapped {
			return nil
		}
	}
	db, err := sql.Open(name, "")
	if err != nil {
		return fmt.Errorf("isutools: resolve driver %q: %w", name, err)
	}
	drv := db.Driver()
	if cerr := db.Close(); cerr != nil {
		return fmt.Errorf("isutools: close probe for %q: %w", name, cerr)
	}
	sql.Register(wrapped, proxy.NewProxyContext(drv, hooks()))
	return nil
}

func hooks() *proxy.HooksContext {
	pre := func(_ context.Context, _ *proxy.Stmt, _ []driver.NamedValue) (interface{}, error) {
		return time.Now(), nil
	}
	observe := func(ctx interface{}, stmt *proxy.Stmt, err error) {
		// A panic in measurement must never break the application's query.
		defer func() { _ = recover() }()
		if err == driver.ErrSkip {
			return
		}
		start, ok := ctx.(time.Time)
		if !ok {
			return
		}
		Default.Observe(normalize(stmt.QueryString), time.Since(start))
	}
	return &proxy.HooksContext{
		PreExec: pre,
		PostExec: func(_ context.Context, ctx interface{}, stmt *proxy.Stmt, _ []driver.NamedValue, _ driver.Result, err error) error {
			observe(ctx, stmt, err)
			return nil
		},
		PreQuery: pre,
		PostQuery: func(_ context.Context, ctx interface{}, stmt *proxy.Stmt, _ []driver.NamedValue, _ driver.Rows, err error) error {
			observe(ctx, stmt, err)
			return nil
		},
	}
}
