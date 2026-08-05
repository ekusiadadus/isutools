package queryplan

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// The tests in this file go through the real registry and through
// database/sql, so the scan conversions, the connection hygiene and the
// purpose rules are the real ones. The database is a driver that answers from
// a script; no network is involved.

const planDriverName = "isutoolsqueryplanfake"

type planDriver struct {
	mu   sync.Mutex
	dsns []string
	args map[string][]driver.NamedValue
}

var planFakeDriver = &planDriver{args: map[string][]driver.NamedValue{}}

func init() { sql.Register(planDriverName, planFakeDriver) }

func (d *planDriver) Open(dsn string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dsns = append(d.dsns, dsn)
	return &planConn{driver: d}, nil
}

func (d *planDriver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dsns = nil
	d.args = map[string][]driver.NamedValue{}
}

func (d *planDriver) openedDSNs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dsns...)
}

func (d *planDriver) argsFor(prefix string) []driver.NamedValue {
	d.mu.Lock()
	defer d.mu.Unlock()
	for query, args := range d.args {
		if strings.HasPrefix(query, prefix) {
			return args
		}
	}
	return nil
}

func (d *planDriver) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.args[query] = args
}

type planConn struct{ driver *planDriver }

func (c *planConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("planConn: statements are issued directly")
}
func (c *planConn) Close() error { return nil }
func (c *planConn) Begin() (driver.Tx, error) {
	return nil, errors.New("planConn: transactions are not used")
}

func (c *planConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.driver.record(query, args)
	return driver.RowsAffected(0), nil
}

func (c *planConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(query, args)
	return scriptFor(query), nil
}

// planRows is a scripted result set.
type planRows struct {
	columns []string
	rows    [][]driver.Value
	pos     int
}

func (r *planRows) Columns() []string { return r.columns }
func (r *planRows) Close() error      { return nil }
func (r *planRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

// scriptFor answers the session sequence. Values are returned in the driver
// types a MySQL driver really produces: text columns as []byte, integers as
// int64, timestamps as time.Time (parseTime is on for inspector DSNs).
func scriptFor(query string) driver.Rows {
	switch {
	case query == stmtCurrentRole:
		return &planRows{columns: []string{"CURRENT_ROLE()"}, rows: [][]driver.Value{{[]byte("NONE")}}}
	case strings.HasPrefix(query, stmtShowGrants):
		rows := make([][]driver.Value, 0, len(leastPrivilegeGrants))
		for _, grant := range leastPrivilegeGrants {
			rows = append(rows, []driver.Value{[]byte(grant)})
		}
		return &planRows{columns: []string{"Grants for isutools_explain@%"}, rows: rows}
	case query == stmtInstrumented:
		return &planRows{columns: []string{"INSTRUMENTED"}, rows: [][]driver.Value{{[]byte("NO")}}}
	case query == stmtSampleColumn:
		return &planRows{columns: []string{"COLUMN_NAME"}, rows: [][]driver.Value{{[]byte("QUERY_SAMPLE_TEXT")}}}
	case query == stmtMaxSQLTextLength:
		return &planRows{columns: []string{"@@performance_schema_max_sql_text_length"}, rows: [][]driver.Value{{int64(1024)}}}
	case strings.HasPrefix(query, samplePrefix):
		return &planRows{
			columns: []string{"DIGEST", "QUERY_SAMPLE_TEXT", "QUERY_SAMPLE_SEEN"},
			rows: [][]driver.Value{{
				[]byte("d1"),
				[]byte("SELECT id FROM posts WHERE user_id = 42"),
				baseTime.Add(15 * time.Second),
			}},
		}
	case strings.HasPrefix(query, explainPrefix):
		return &planRows{
			columns: explainColumns,
			rows: [][]driver.Value{
				// An impossible WHERE: every column NULL but the last.
				{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []byte("Impossible WHERE")},
				{int64(1), []byte("SIMPLE"), []byte("posts"), nil, []byte("ALL"),
					nil, nil, nil, nil, int64(12345), 100.0, []byte("Using filesort")},
			},
		}
	default:
		// USE, UPDATE performance_schema.threads and anything else that
		// returns no result set.
		return &planRows{}
	}
}

// TestExplainNeverFallsBackToAnotherCredential is the no-fallback contract,
// exercised through the real registry: the target has an application
// credential and a stats credential, and no explain credential. The plan is
// skipped, and the driver is never opened at all — which is the observable
// form of "no other credential was used".
func TestExplainNeverFallsBackToAnotherCredential(t *testing.T) {
	planFakeDriver.reset()
	t.Cleanup(sqlstats.CloseDBInspectors)

	const id = "qp-no-explain-cred"
	mustRegisterTarget(t, id, "app_user:apppw@tcp(127.0.0.1:3307)/isuconp")
	mustRegisterInspector(t, id, sqlstats.PurposeStats, "stats_user:statspw@tcp(127.0.0.1:3307)/isuconp")

	rows := interval(usableTarget(id, selectStat("d1", "SELECT ?", 100)))
	section, err := Capture(context.Background(), Input{Rows: rows})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, id)
	if target.Code != CodePurposeUnregistered {
		t.Fatalf("code = %q, want %q", target.Code, CodePurposeUnregistered)
	}
	if len(target.Plans) != 0 {
		t.Fatalf("plans = %+v, want none", target.Plans)
	}
	if _, ok := noteFor(section, CodePurposeUnregistered); !ok {
		t.Fatalf("health = %+v, want the missing-credential note", section.Health)
	}
	for _, dsn := range planFakeDriver.openedDSNs() {
		t.Fatalf("a connection was opened with %q; the explain credential is the only one this package may use", dsn)
	}
}

// TestCaptureThroughTheRealRegistry exercises the whole path with database/sql
// doing the conversions: NULL columns, an integer row count arriving as int64,
// and a timestamp arriving as time.Time.
func TestCaptureThroughTheRealRegistry(t *testing.T) {
	planFakeDriver.reset()
	t.Cleanup(sqlstats.CloseDBInspectors)

	const id = "qp-explain-cred"
	mustRegisterTarget(t, id, "app_user:apppw@tcp(127.0.0.1:3308)/isuconp")
	mustRegisterInspector(t, id, sqlstats.PurposeExplain, "explain_user:explainpw@tcp(127.0.0.1:3308)/isuconp")

	rows := interval(usableTarget(id, selectStat("d1", "SELECT ? FROM posts WHERE user_id = ?", 100)))
	section, err := Capture(context.Background(), Input{Rows: rows})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, id, 0)
	if plan.Err != nil {
		t.Fatalf("plan error = %+v, want none", *plan.Err)
	}
	if plan.Freshness != FreshnessFresh {
		t.Fatalf("freshness = %q, want fresh", plan.Freshness)
	}
	if !plan.SampleSeen.Equal(baseTime.Add(15 * time.Second)) {
		t.Fatalf("sample_seen = %v, want the driver's own timestamp", plan.SampleSeen)
	}
	if len(plan.Rows) != 2 {
		t.Fatalf("rows = %+v, want both EXPLAIN rows", plan.Rows)
	}
	null := plan.Rows[0]
	if null.SelectType != nil || null.Table != nil || null.Type != nil || null.Rows != nil {
		t.Fatalf("the impossible-WHERE row must scan as absent columns, got %+v", null)
	}
	if null.Extra == nil || *null.Extra != "Impossible WHERE" {
		t.Fatalf("extra = %v, want the one column that was not NULL", null.Extra)
	}
	scan := plan.Rows[1]
	if scan.Rows == nil || *scan.Rows != 12345 {
		t.Fatalf("row estimate = %v, want the int64 converted", scan.Rows)
	}
	if scan.Type == nil || *scan.Type != "ALL" {
		t.Fatalf("type = %v, want ALL", scan.Type)
	}

	// The registry's hygiene rules reached the driver: no default database,
	// no interpolation, no statement batches.
	dsns := planFakeDriver.openedDSNs()
	if len(dsns) == 0 {
		t.Fatal("no connection was opened")
	}
	for _, dsn := range dsns {
		if !strings.Contains(dsn, "explain_user") {
			t.Fatalf("connection opened with %q, want the explain credential", dsn)
		}
		for _, want := range []string{"interpolateParams=false", "multiStatements=false", "parseTime=true"} {
			if !strings.Contains(dsn, want) {
				t.Fatalf("inspector dsn %q is missing %q", dsn, want)
			}
		}
	}
	// Both halves of the digest table's key arrived as bound parameters.
	args := planFakeDriver.argsFor(samplePrefix)
	if len(args) != 2 || args[0].Value != "isuconp" || args[1].Value != "d1" {
		t.Fatalf("sample read args = %+v, want the schema and the digest bound", args)
	}
}

func mustRegisterTarget(t *testing.T, id, dsn string) {
	t.Helper()
	if err := sqlstats.RegisterDBTarget(id, planDriverName, dsn); err != nil {
		t.Fatalf("register target %s: %v", id, err)
	}
}

// inspectorsRegistered remembers which purpose credentials this process has
// already attached.
var (
	inspectorsMu         sync.Mutex
	inspectorsRegistered = map[string]struct{}{}
)

// mustRegisterInspector attaches a purpose credential once per process.
//
// The registry is process-wide and has no unregister: RegisterDBTarget is
// idempotent for the same database, but RegisterDBInspector deliberately
// rejects a second credential for a purpose it already holds, because that is
// how it stops one credential being swapped for another. Re-registering on the
// second iteration of -count would therefore fail on the guard rather than on
// anything this test is about, so the registration is skipped when it is
// already in place — the credential under test is identical either way.
func mustRegisterInspector(t *testing.T, id string, purpose sqlstats.Purpose, dsn string) {
	t.Helper()
	inspectorsMu.Lock()
	defer inspectorsMu.Unlock()
	key := id + "/" + string(purpose)
	if _, done := inspectorsRegistered[key]; done {
		return
	}
	if err := sqlstats.RegisterDBInspector(id, purpose, planDriverName, dsn); err != nil {
		t.Fatalf("register %s credential for %s: %v", purpose, id, err)
	}
	inspectorsRegistered[key] = struct{}{}
}
