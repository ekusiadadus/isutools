package sqlrows

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

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// This file drives the collector through the real registry, over a driver
// registered with database/sql that answers with canned rows. It is what fixes
// the contract the whole package rests on: the connection sqlrows samples
// through carries no default database, so MySQL cannot file its statements
// under the application's schema.

const registryDriverName = "isutoolssqlrowsfake"

type registryDriver struct {
	mu    sync.Mutex
	dsns  []string
	stmts []string
	args  [][]driver.NamedValue
	// on records the DSN each statement was issued through, so a test can ask
	// what a particular connection was made to run.
	on []string
}

var cannedDriver = &registryDriver{}

func init() { sql.Register(registryDriverName, cannedDriver) }

func (d *registryDriver) Open(dsn string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dsns = append(d.dsns, dsn)
	return &registryConn{driver: d, dsn: dsn}, nil
}

func (d *registryDriver) record(dsn, query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, query)
	d.args = append(d.args, args)
	d.on = append(d.on, dsn)
}

// Every test in this binary shares one driver, and database/sql pools the
// connections behind it, so assertions select by endpoint rather than by time:
// a test states which target it is talking about and gets exactly that
// target's connections and statements, however often the suite has run.

// dsnsFor returns every DSN opened whose text contains sub.
func (d *registryDriver) dsnsFor(sub string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, dsn := range d.dsns {
		if strings.Contains(dsn, sub) {
			out = append(out, dsn)
		}
	}
	return out
}

// recordsOn returns every statement issued on connections whose DSN contains
// sub, in the order they were issued.
func (d *registryDriver) recordsOn(sub string) []registryRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []registryRecord
	for i, dsn := range d.on {
		if strings.Contains(dsn, sub) {
			out = append(out, registryRecord{query: d.stmts[i], args: d.args[i]})
		}
	}
	return out
}

// statementsOn is recordsOn without the arguments.
func (d *registryDriver) statementsOn(sub string) []string {
	records := d.recordsOn(sub)
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.query)
	}
	return out
}

// registryRecord is one statement as the driver saw it.
type registryRecord struct {
	query string
	args  []driver.NamedValue
}

type registryConn struct {
	driver *registryDriver
	dsn    string
}

func (c *registryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("registryConn: prepared statements are not used")
}
func (c *registryConn) Close() error { return nil }
func (c *registryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("registryConn: transactions are not used")
}

func (c *registryConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.driver.record(c.dsn, query, args)
	return driver.RowsAffected(0), nil
}

func (c *registryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(c.dsn, query, args)
	return cannedRowsFor(query, args, c.dsn), nil
}

// defaultSchemaOfDSN is the server's view of the connection's default
// database: whatever the DSN put after the last "/". It is what makes this
// driver faithful to the bug being guarded against — a DSN the registry could
// not rebuild still carries the application's schema, and the connection opened
// from it really does have one.
func defaultSchemaOfDSN(dsn string) driver.Value {
	tail := dsn
	if slash := strings.LastIndex(tail, "/"); slash >= 0 {
		tail = tail[slash+1:]
	}
	if q := strings.Index(tail, "?"); q >= 0 {
		tail = tail[:q]
	}
	if tail == "" {
		return nil
	}
	return []byte(tail)
}

// cannedRowsFor answers each statement of a boundary with driver-native
// values: []byte for text and numbers, time.Time for timestamps, which is what
// a MySQL driver hands back.
func cannedRowsFor(query string, args []driver.NamedValue, dsn string) driver.Rows {
	switch {
	case query == probePerformanceSchema:
		return &cannedRows{vals: [][]driver.Value{{int64(1)}}}
	case query == probeDefaultSchema:
		return &cannedRows{vals: [][]driver.Value{{defaultSchemaOfDSN(dsn)}}}
	case query == probeDigestConsumer:
		return &cannedRows{vals: [][]driver.Value{{[]byte("YES")}}}
	case query == probeColumns:
		rows := make([][]driver.Value, 0, len(requiredColumns)+1)
		for _, name := range append(append([]string(nil), requiredColumns...), optionalQuerySampleColumn) {
			rows = append(rows, []driver.Value{[]byte(name)})
		}
		return &cannedRows{vals: rows}
	case query == probeUptime:
		return &cannedRows{vals: [][]driver.Value{{[]byte("1000")}}}
	case query == metaPFS:
		return &cannedRows{vals: [][]driver.Value{{[]byte("uuid-registry"), []byte("1000"), baseTime}}}
	case query == digestRows:
		// The schema the collector bound is echoed back, so the row can only
		// be attributed when the binding really happened.
		schema, _ := args[0].Value.(string)
		return &cannedRows{vals: [][]driver.Value{
			{[]byte(schema), []byte("aaa"), int64(3), int64(30), int64(300), int64(3), int64(0), int64(0), int64(0), int64(1), int64(0)},
			{nil, nil, int64(2), int64(20), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)},
		}}
	case query == clockAfter:
		return &cannedRows{vals: [][]driver.Value{{baseTime.Add(time.Millisecond)}}}
	case strings.HasPrefix(query, digestTextPrefix):
		return &cannedRows{vals: [][]driver.Value{{[]byte("aaa"), []byte("SELECT * FROM `posts` WHERE `id` = ?")}}}
	default:
		return &cannedRows{}
	}
}

type cannedRows struct {
	vals [][]driver.Value
	pos  int
}

func (r *cannedRows) Columns() []string {
	if len(r.vals) == 0 {
		return []string{"c"}
	}
	cols := make([]string, len(r.vals[0]))
	for i := range cols {
		cols[i] = "c"
	}
	return cols
}

func (r *cannedRows) Close() error { return nil }

func (r *cannedRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.pos])
	r.pos++
	return nil
}

// TestInspectorConnectionHasNoDefaultDatabase runs a full boundary through the
// registry and checks both halves of the self-contamination fix: the DSN the
// inspector opens carries no database, and the schema travels as a bound
// argument instead.
func TestInspectorConnectionHasNoDefaultDatabase(t *testing.T) {
	const targetID = "sqlrows-registry-db"
	// The endpoint is this test's alone, so its assertions cannot be satisfied
	// — or broken — by another test's target.
	const targetEndpoint = "127.0.0.1:3306"
	err := sqlstats.RegisterDBTarget(targetID, registryDriverName,
		"isu:secret@tcp("+targetEndpoint+")/isuconp?parseTime=true")
	if err != nil {
		t.Fatalf("RegisterDBTarget: %v", err)
	}
	info, ok := sqlstats.Target(targetID)
	if !ok {
		t.Fatalf("target %q was not registered", targetID)
	}

	c := New()
	// Other tests in this binary register targets of their own; this one
	// measures exactly the target it registered, and asserts only over that
	// target's endpoint.
	c.targets = func() []sqlstats.TargetInfo { return []sqlstats.TargetInfo{info} }

	res, err := c.CaptureBaseline(context.Background(), "run-registry", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if !res.Committed {
		t.Fatal("the boundary did not commit")
	}
	target := sampleOfResult(t, res).Targets[targetID]
	if target == nil || !target.Captured {
		t.Fatalf("target = %+v, want a captured reading", target)
	}
	if row, ok := target.Digests["aaa"]; !ok || row.CountStar != 3 || row.RowsExamined != 300 {
		t.Fatalf("digest aaa = %+v (present=%v), want the canned counters", row, ok)
	}
	if !target.HasOverflow || target.Overflow.CountStar != 2 {
		t.Fatalf("overflow = %+v (present=%v), want the both-NULL row", target.Overflow, target.HasOverflow)
	}

	dsns := cannedDriver.dsnsFor(targetEndpoint)
	if len(dsns) == 0 {
		t.Fatal("no connection was opened")
	}
	for _, dsn := range dsns {
		if !strings.Contains(dsn, "/?") {
			t.Fatalf("inspector DSN %q still carries a default database", dsn)
		}
		for _, want := range []string{"interpolateParams=false", "parseTime=true", "loc=UTC", "multiStatements=false"} {
			if !strings.Contains(dsn, want) {
				t.Fatalf("inspector DSN %q is missing %q", dsn, want)
			}
		}
	}

	records := cannedDriver.recordsOn(targetEndpoint)
	if len(records) == 0 || records[0].query != "SET time_zone = '+00:00'" {
		t.Fatalf("first statement = %+v, want the session initialisation", records)
	}
	for i, record := range records {
		assertNoDefaultDatabaseCall(t, record.query)
		if record.query != digestRows {
			continue
		}
		if len(record.args) != 1 {
			t.Fatalf("digest read %d took %d arguments, want the bound schema only", i, len(record.args))
		}
		if got, _ := record.args[0].Value.(string); got != "isuconp" {
			t.Fatalf("bound schema = %v, want isuconp", record.args[0].Value)
		}
	}
}

// TestURLFormTargetIsSkippedNotContaminated is the negative half of the
// self-contamination contract.
//
// The registry can only strip the default database from a DSN it is able to
// rebuild; the URL form is handed to the driver unchanged and keeps the
// application's schema. Every statement of this collector would then be
// recorded as a digest of the schema it is measuring — the closing boundary's
// own reads landing inside the interval they report. The target is therefore
// skipped with a code of its own, on the first question asked, and never
// connected to again.
func TestURLFormTargetIsSkippedNotContaminated(t *testing.T) {
	const targetID = "sqlrows-registry-url-db"
	const targetEndpoint = "10.0.0.9:3306"
	const urlDSN = "mysql://isu:secret@" + targetEndpoint + "/isuconp"
	if err := sqlstats.RegisterDBTarget(targetID, registryDriverName, urlDSN); err != nil {
		t.Fatalf("RegisterDBTarget: %v", err)
	}
	info, ok := sqlstats.Target(targetID)
	if !ok {
		t.Fatalf("target %q was not registered", targetID)
	}

	c := New()
	c.targets = func() []sqlstats.TargetInfo { return []sqlstats.TargetInfo{info} }

	ctx := context.Background()
	base, err := c.CaptureBaseline(ctx, "run-url", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	// A target that must not be measured is not a failed target: the boundary
	// still commits, and the rest of the run keeps its other targets.
	if !base.Committed {
		t.Fatal("a misconfigured target made the whole boundary fail")
	}
	target := sampleOfResult(t, base).Targets[targetID]
	if target == nil || target.Captured {
		t.Fatalf("target = %+v, want no reading from a connection that contaminates", target)
	}
	if target.Code != CodeInspectorDefaultDB {
		t.Fatalf("code = %q (%s), want %q", target.Code, target.Err, CodeInspectorDefaultDB)
	}
	if !strings.Contains(target.Err, "isuconp") {
		t.Fatalf("reason = %q, want it to name the schema that would have been contaminated", target.Err)
	}

	// Nothing that reads the measured schema was issued on that connection,
	// and the boundary gave up at the hygiene check rather than carrying on:
	// the check is the last thing that connection was ever asked.
	issued := cannedDriver.statementsOn(targetEndpoint)
	for _, stmt := range issued {
		switch {
		case stmt == digestRows, stmt == metaPFS, stmt == metaSHOW, stmt == uptimeSHOW,
			strings.HasPrefix(stmt, digestTextPrefix):
			t.Fatalf("a measurement statement ran on a connection with a default database: %q", stmt)
		}
	}
	if len(issued) == 0 || issued[len(issued)-1] != probeDefaultSchema {
		t.Fatalf("statements on the URL-form connection = %v, want them to stop at the hygiene check", issued)
	}

	// The closing boundary must not connect to it at all: re-asking would keep
	// paying the contamination the verdict exists to report.
	final, err := c.CaptureFinal(ctx, "run-url", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	if after := cannedDriver.statementsOn(targetEndpoint); len(after) != len(issued) {
		t.Fatalf("the closing boundary issued %v on a connection already known to contaminate", after[len(issued):])
	}

	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	section, ok := value.(*Section)
	if !ok {
		t.Fatalf("Collect() = %T, want *Section", value)
	}
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q: a skipped target degrades the run", section.Validity, runctl.ValidityPartial)
	}
	var reported *TargetSection
	for i := range section.Targets {
		if section.Targets[i].TargetID == targetID {
			reported = &section.Targets[i]
		}
	}
	if reported == nil || reported.Usable || reported.Code != CodeInspectorDefaultDB {
		t.Fatalf("section target = %+v, want it unusable with %q", reported, CodeInspectorDefaultDB)
	}
	noted := false
	for _, note := range section.Health {
		if note.Key == HealthSkip && strings.Contains(note.Message, targetID) {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("health = %+v, want the skipped target named under %q", section.Health, HealthSkip)
	}
}

// TestRegisteredFormsNeverCallDATABASE covers the rule stated at the top of
// sql.go over both DSN forms the registry accepts. DATABASE() would attribute
// this collector's own statements to the application's schema — and SCHEMA() is
// the same function under another name, so neither may appear.
func TestRegisteredFormsNeverCallDATABASE(t *testing.T) {
	targets := []struct{ id, endpoint, dsn string }{
		{
			id:       "sqlrows-forms-mysql-db",
			endpoint: "10.0.0.7:3306",
			dsn:      "isu:secret@tcp(10.0.0.7:3306)/isuconp?parseTime=true",
		},
		{
			id:       "sqlrows-forms-url-db",
			endpoint: "10.0.0.8:3306",
			dsn:      "mysql://isu:secret@10.0.0.8:3306/isuconp",
		},
	}
	infos := make([]sqlstats.TargetInfo, 0, len(targets))
	for _, target := range targets {
		if err := sqlstats.RegisterDBTarget(target.id, registryDriverName, target.dsn); err != nil {
			t.Fatalf("RegisterDBTarget(%s): %v", target.id, err)
		}
		info, ok := sqlstats.Target(target.id)
		if !ok {
			t.Fatalf("target %q was not registered", target.id)
		}
		infos = append(infos, info)
	}

	c := New()
	c.targets = func() []sqlstats.TargetInfo { return infos }
	if _, err := c.CaptureBaseline(context.Background(), "run-forms", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	for _, target := range targets {
		issued := cannedDriver.statementsOn(target.endpoint)
		if len(issued) == 0 {
			t.Fatalf("no statements reached %s, so its form proves nothing", target.id)
		}
		for _, stmt := range issued {
			assertNoDefaultDatabaseCall(t, stmt)
		}
	}

	// The generated statements, including the ones a boundary only issues for
	// some servers or some digest counts, are checked as a set as well: a
	// statement that is never reached by this fixture must still obey the rule.
	for _, stmt := range allStatements() {
		assertNoDefaultDatabaseCall(t, stmt)
	}
}

// allStatements is every statement text this package can send.
func allStatements() []string {
	return []string{
		probePerformanceSchema,
		probeDefaultSchema,
		probeDigestConsumer,
		probeColumns,
		probeUptime,
		metaPFS,
		metaSHOW,
		uptimeSHOW,
		digestRows,
		clockAfter,
		digestTextQuery(1),
		digestTextQuery(DigestTextFetchLimit),
	}
}

func assertNoDefaultDatabaseCall(t *testing.T, stmt string) {
	t.Helper()
	upper := strings.ToUpper(stmt)
	for _, banned := range []string{"DATABASE()", "SCHEMA()"} {
		if strings.Contains(upper, banned) {
			t.Fatalf("statement resolves the default database with %s: %q", banned, stmt)
		}
	}
}
