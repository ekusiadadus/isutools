package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// leakPassword is deliberately distinctive: every assertion in this file is
// "this exact string appears nowhere", so a partial or encoded echo would show
// up as a different failure rather than passing silently.
const leakPassword = "Tr0ub4dor-and-3-horse-battery"

const leakDSN = "isuconp:" + leakPassword + "@tcp(db1:3306)/isuconp?parseTime=true"

const leakDriverName = "isutoolsleakyfake"

// leakyDriver echoes the DSN it was handed into every error it raises, the way
// real drivers do when a dial, a handshake or a DSN parse fails.
type leakyDriver struct {
	mu      sync.Mutex
	mode    string
	lastErr string
}

var leakDriver = &leakyDriver{}

func init() { sql.Register(leakDriverName, leakDriver) }

func (d *leakyDriver) setMode(mode string) {
	d.mu.Lock()
	d.mode, d.lastErr = mode, ""
	d.mu.Unlock()
}

// fail records the error it produces so a test can prove the raw driver text
// really did carry the password, and that the redaction is what removed it.
func (d *leakyDriver) fail(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	d.mu.Lock()
	d.lastErr = err.Error()
	d.mu.Unlock()
	return err
}

func (d *leakyDriver) raw() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastErr
}

func (d *leakyDriver) modeIs(mode string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mode == mode
}

func (d *leakyDriver) OpenConnector(dsn string) (driver.Connector, error) {
	if d.modeIs("open") {
		return nil, d.fail("leaky: cannot parse dsn %q", dsn)
	}
	return leakyConnector{driver: d, dsn: dsn}, nil
}

func (d *leakyDriver) Open(dsn string) (driver.Conn, error) {
	if d.modeIs("connect") {
		return nil, d.fail("leaky: dial tcp for dsn %q failed", dsn)
	}
	return &leakyConn{driver: d, dsn: dsn}, nil
}

type leakyConnector struct {
	driver *leakyDriver
	dsn    string
}

func (c leakyConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c leakyConnector) Driver() driver.Driver                        { return c.driver }

type leakyConn struct {
	driver *leakyDriver
	dsn    string
}

func (c *leakyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("leakyConn: Prepare is not used")
}
func (c *leakyConn) Close() error { return nil }
func (c *leakyConn) Begin() (driver.Tx, error) {
	return nil, errors.New("leakyConn: transactions are not used")
}

func (c *leakyConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if c.driver.modeIs("session") {
		return nil, c.driver.fail("leaky: session init rejected on %q", c.dsn)
	}
	return driver.RowsAffected(0), nil
}

func (c *leakyConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case c.driver.modeIs("query"):
		return nil, c.driver.fail("leaky: query refused for dsn %q", c.dsn)
	case c.driver.modeIs("safe"):
		return nil, errors.New("leaky: Table 'isuconp.missing' doesn't exist")
	case c.driver.modeIs("rows"):
		return &leakyRows{driver: c.driver, dsn: c.dsn}, nil
	default:
		return &leakyRows{driver: c.driver, dsn: c.dsn}, nil
	}
}

type leakyRows struct {
	driver *leakyDriver
	dsn    string
}

func (r *leakyRows) Columns() []string { return []string{"c"} }
func (r *leakyRows) Close() error      { return nil }

func (r *leakyRows) Next([]driver.Value) error {
	if r.driver.modeIs("rows") {
		return r.driver.fail("leaky: connection lost mid-result for %q", r.dsn)
	}
	return io.EOF
}

func newRedactTestRegistry(t *testing.T) *registry {
	t.Helper()
	r := newRegistry()
	t.Cleanup(r.closeInspectors)
	t.Cleanup(func() { leakDriver.setMode("") })
	return r
}

// registryExposure gathers every string the registry hands out, so one
// assertion can cover "the returned error, the health note, or anything else
// the registry exposes".
func registryExposure(t *testing.T, r *registry, id string, err error) string {
	t.Helper()
	var b strings.Builder
	if err != nil {
		b.WriteString(err.Error())
		b.WriteString("\n")
	}
	for _, note := range r.notesSnapshot() {
		b.WriteString(note)
		b.WriteString("\n")
	}
	for _, target := range r.targets() {
		fmt.Fprintf(&b, "%+v\n", target)
	}
	if info, ok := r.target(id); ok {
		fmt.Fprintf(&b, "%+v\n", info)
	}
	features, known := r.features(id)
	fmt.Fprintf(&b, "%+v %v\n", features, known)
	if derived, ok := r.targetIDForDSN(leakDriverName, leakDSN); ok {
		b.WriteString(derived)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDriverErrorsNeverExposeTheDSN drives every path on which a driver error
// can reach the outside world and asserts the password appears on none of
// them. Drivers echo their DSN freely, so the registry has to assume they do.
func TestDriverErrorsNeverExposeTheDSN(t *testing.T) {
	modes := []struct {
		name string
		mode string
	}{
		{name: "sql.Open", mode: "open"},
		{name: "connect", mode: "connect"},
		{name: "session init", mode: "session"},
		{name: "query", mode: "query"},
		{name: "rows", mode: "rows"},
	}
	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			r := newRedactTestRegistry(t)
			if err := r.registerTarget("app", leakDriverName, leakDSN); err != nil {
				t.Fatalf("registerTarget: %v", err)
			}
			leakDriver.setMode(tc.mode)

			var callbackErr error
			err := r.inspect(context.Background(), "app", PurposeStats, func(ctx context.Context, q Querier) error {
				rows, qerr := q.QueryContext(ctx, "SELECT 1")
				if qerr != nil {
					callbackErr = qerr
					return qerr
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
				}
				callbackErr = rows.Err()
				return rows.Err()
			})
			if err == nil {
				t.Fatal("the driver failure must be reported, not swallowed")
			}
			if !errors.Is(err, ErrDriverFailed) {
				t.Fatalf("error = %v, want it to wrap ErrDriverFailed", err)
			}
			// Without this the test could pass against a driver that never
			// echoed the DSN, proving nothing about the redaction.
			if !strings.Contains(leakDriver.raw(), leakPassword) {
				t.Fatalf("the fake driver did not echo the password (%q); the test would prove nothing", leakDriver.raw())
			}

			exposed := registryExposure(t, r, "app", err)
			if callbackErr != nil {
				exposed += callbackErr.Error()
			}
			if strings.Contains(exposed, leakPassword) {
				t.Fatalf("the DSN password reached the outside world:\n%s", exposed)
			}
			if strings.Contains(exposed, leakDSN) {
				t.Fatalf("the DSN reached the outside world:\n%s", exposed)
			}
			// The report is still useful: it names the target and the
			// allowlist-rebuilt endpoint.
			if !strings.Contains(err.Error(), `"app"`) || !strings.Contains(err.Error(), "tcp(db1:3306)/isuconp") {
				t.Fatalf("error = %v, want the target id and display in it", err)
			}
		})
	}
}

// TestDriverErrorKeepsMessagesThatCarryNoCredential checks the other side of
// the trade: a driver message that mentions no secret is still reported, so
// redaction does not cost every diagnosis.
func TestDriverErrorKeepsMessagesThatCarryNoCredential(t *testing.T) {
	r := newRedactTestRegistry(t)
	if err := r.registerTarget("app", leakDriverName, leakDSN); err != nil {
		t.Fatalf("registerTarget: %v", err)
	}
	leakDriver.setMode("safe")

	err := r.inspect(context.Background(), "app", PurposeStats, func(ctx context.Context, q Querier) error {
		_, qerr := q.QueryContext(ctx, "SELECT 1")
		return qerr
	})
	if err == nil {
		t.Fatal("the driver failure must be reported")
	}
	if !strings.Contains(err.Error(), "Table 'isuconp.missing' doesn't exist") {
		t.Fatalf("error = %v, want the credential-free driver message kept", err)
	}
	if strings.Contains(registryExposure(t, r, "app", err), leakPassword) {
		t.Fatal("the password must not appear even on the message-kept path")
	}
}

// TestDriverErrorPassesControlFlowSentinelsThrough guards the redaction
// against breaking callers: sql.ErrNoRows is how a lookup reports "absent",
// and it has to survive errors.Is.
func TestDriverErrorPassesControlFlowSentinelsThrough(t *testing.T) {
	r := newRedactTestRegistry(t)
	if err := r.registerTarget("app", leakDriverName, leakDSN); err != nil {
		t.Fatalf("registerTarget: %v", err)
	}
	leakDriver.setMode("")

	var scanErr error
	if err := r.inspect(context.Background(), "app", PurposeStats, func(ctx context.Context, q Querier) error {
		scanErr = q.QueryRowContext(ctx, "SELECT 1").Scan(new(int64))
		return nil
	}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		t.Fatalf("Scan error = %v, want sql.ErrNoRows to survive redaction", scanErr)
	}
}

// TestDriverErrorRedactsEncodedPasswordEchoes covers a URL-form DSN, where the
// password reaches the driver percent-encoded and the decoded form alone would
// not match.
func TestDriverErrorRedactsEncodedPasswordEchoes(t *testing.T) {
	const password = "p@ss w0rd/with+specials"
	const urlDSN = "leaky://isuconp:p%40ss%20w0rd%2Fwith%2Bspecials@db1:3306/isuconp"

	r := newRedactTestRegistry(t)
	if err := r.registerTarget("app", leakDriverName, urlDSN); err != nil {
		t.Fatalf("registerTarget: %v", err)
	}
	leakDriver.setMode("connect")

	err := r.inspect(context.Background(), "app", PurposeStats, func(context.Context, Querier) error {
		return nil
	})
	if err == nil {
		t.Fatal("the driver failure must be reported")
	}
	exposed := registryExposure(t, r, "app", err)
	if strings.Contains(exposed, password) || strings.Contains(exposed, urlDSN) {
		t.Fatalf("the URL-form DSN password reached the outside world:\n%s", exposed)
	}
	if strings.Contains(exposed, "p%40ss%20w0rd%2Fwith%2Bspecials") {
		t.Fatalf("the encoded password reached the outside world:\n%s", exposed)
	}
}

// TestDriverErrorDetailIsBounded keeps a chatty driver from inflating a note
// or a snapshot with an unbounded message.
func TestDriverErrorDetailIsBounded(t *testing.T) {
	d := newDriverErrors("app", PurposeStats, "tcp(db1:3306)/isuconp", credential{}, "")
	err := d.wrap("inspect query", errors.New(strings.Repeat("verbose ", 500)))
	if len(err.Error()) > maxDriverErrDetail+200 {
		t.Fatalf("wrapped error is %d bytes, want it bounded", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "...") {
		t.Fatalf("error = %q, want a truncation marker", err.Error())
	}
	if d.wrap("inspect query", nil) != nil {
		t.Fatal("a nil driver error must stay nil")
	}

	// Truncation must not split a rune: the result is marshalled into JSON.
	multibyte := d.wrap("inspect query", errors.New(strings.Repeat("接続に失敗しました ", 100)))
	if !utf8.ValidString(multibyte.Error()) {
		t.Fatalf("truncated error is not valid UTF-8: %q", multibyte.Error())
	}
}

// TestCloseDBInspectorsIsSafeToCallAnyTime covers the exported shutdown entry
// point: the registry must stay usable and reopen on demand afterwards.
func TestCloseDBInspectorsIsSafeToCallAnyTime(t *testing.T) {
	CloseDBInspectors()
	CloseDBInspectors()

	r := newRedactTestRegistry(t)
	if err := r.registerTarget("app", leakDriverName, leakDSN); err != nil {
		t.Fatalf("registerTarget: %v", err)
	}
	inspectOnce := func() error {
		return r.inspect(context.Background(), "app", PurposeStats, func(ctx context.Context, q Querier) error {
			rows, err := q.QueryContext(ctx, "SELECT 1")
			if err != nil {
				return err
			}
			return rows.Close()
		})
	}
	if err := inspectOnce(); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	r.closeInspectors()
	if err := inspectOnce(); err != nil {
		t.Fatalf("inspect after closing the pooled handles: %v", err)
	}
}
