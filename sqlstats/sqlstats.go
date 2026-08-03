// Package sqlstats wraps database/sql drivers with a measuring proxy and
// aggregates every query into an in-memory table. Works with any driver
// (MySQL/MariaDB via go-sql-driver, PostgreSQL via pgx stdlib or lib/pq).
package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"time"

	proxy "github.com/shogo82148/go-sql-proxy"

)

// DriverSuffix is appended to the original driver name on registration:
// Register("mysql") makes "mysql:isutools" available.
const DriverSuffix = ":isutools"

// Default is the generation-scoped store all proxied drivers report into.
var Default = NewStore(10_000)

var (
	connMu    sync.Mutex
	firstName string
	firstDSN  string
)

// FirstConn returns the base driver name and DSN of the first connection
// opened through a wrapped driver. dbinspect uses it to open its own raw
// connection for schema inspection without any extra integration code.
func FirstConn() (driverName, dsn string, ok bool) {
	connMu.Lock()
	defer connMu.Unlock()
	return firstName, firstDSN, firstName != ""
}

// dsnCapturingDriver records the first DSN opened, then delegates to the
// measuring proxy driver.
type dsnCapturingDriver struct {
	driver.Driver
	base string
}

func (d dsnCapturingDriver) Open(dsn string) (driver.Conn, error) {
	connMu.Lock()
	if firstName == "" {
		firstName, firstDSN = d.base, dsn
	}
	connMu.Unlock()
	return d.Driver.Open(dsn)
}

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
	sql.Register(wrapped, dsnCapturingDriver{
		Driver: proxy.NewProxyContext(drv, hooks()),
		base:   name,
	})
	return nil
}

func hooks() *proxy.HooksContext {
	pre := func(_ context.Context, _ *proxy.Stmt, _ []driver.NamedValue) (interface{}, error) {
		return hookMeasurement{started: time.Now(), measurement: Default.begin()}, nil
	}
	observe := func(ctx interface{}, stmt *proxy.Stmt, err error) {
		// A panic in measurement must never break the application's query.
		defer func() { _ = recover() }()
		measurement, ok := ctx.(hookMeasurement)
		if !ok {
			return
		}
		// Ensure a normalization or aggregation failure cannot pin reset forever.
		defer Default.discard(measurement.measurement)
		if err == driver.ErrSkip {
			return
		}
		Default.finish(measurement.measurement, normalize(stmt.QueryString), time.Since(measurement.started))
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

type hookMeasurement struct {
	started     time.Time
	measurement *measurement
}
