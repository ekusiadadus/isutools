package sqlstats

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
)

// recordingDriver is a fake driver that records the DSNs it is opened with,
// the statements it executes, and the peak number of simultaneous
// connections. No real database is involved anywhere in this file.
type recordingDriver struct {
	mu       sync.Mutex
	dsns     []string
	stmts    []string
	rows     []*recordingRows
	openNow  int
	openPeak int
}

var regDriver = &recordingDriver{}

const regDriverName = "isutoolsregfake"

func init() { sql.Register(regDriverName, regDriver) }

func (d *recordingDriver) Open(dsn string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dsns = append(d.dsns, dsn)
	d.openNow++
	if d.openNow > d.openPeak {
		d.openPeak = d.openNow
	}
	return &recordingConn{driver: d}, nil
}

func (d *recordingDriver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dsns, d.stmts, d.rows = nil, nil, nil
	d.openNow, d.openPeak = 0, 0
}

func (d *recordingDriver) record(query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, query)
}

func (d *recordingDriver) newRows() *recordingRows {
	rows := &recordingRows{}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows = append(d.rows, rows)
	return rows
}

func (d *recordingDriver) closeConn() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openNow--
}

func (d *recordingDriver) snapshot() (dsns, stmts []string, peak int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dsns = append(dsns, d.dsns...)
	stmts = append(stmts, d.stmts...)
	return dsns, stmts, d.openPeak
}

type recordingConn struct{ driver *recordingDriver }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recordingConn: Prepare is not used")
}
func (c *recordingConn) Close() error { c.driver.closeConn(); return nil }
func (c *recordingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("recordingConn: transactions are not used")
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.driver.record(query)
	return driver.RowsAffected(0), nil
}

func (c *recordingConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(query)
	return c.driver.newRows(), nil
}

// recordingRows yields three rows so a test can consume one and leave the
// result set open.
type recordingRows struct {
	mu     sync.Mutex
	pos    int
	closed bool
}

func (r *recordingRows) Columns() []string { return []string{"c"} }

func (r *recordingRows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *recordingRows) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *recordingRows) Next(dest []driver.Value) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pos >= 3 {
		return io.EOF
	}
	r.pos++
	dest[0] = int64(r.pos)
	return nil
}

func newTestRegistry(t *testing.T) *registry {
	t.Helper()
	r := newRegistry()
	t.Cleanup(r.closeInspectors)
	return r
}

func mustRegister(t *testing.T, r *registry, id, dsn string) {
	t.Helper()
	if err := r.registerTarget(id, regDriverName, dsn); err != nil {
		t.Fatalf("registerTarget(%q): %v", id, err)
	}
}

func TestTargetIDIsDerivedFromCanonicalTuple(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		a, b   string
		same   bool
	}{
		{name: "identical dsn", driver: "mysql", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db1:3306)/isuconp", same: true},
		{name: "credentials excluded", driver: "mysql", a: "app:secret@tcp(db1:3306)/isuconp", b: "explain:other@tcp(db1:3306)/isuconp", same: true},
		{name: "params excluded", driver: "mysql", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db1:3306)/isuconp?parseTime=true", same: true},
		{name: "default port completed", driver: "mysql", a: "u:p@tcp(db1)/isuconp", b: "u:p@tcp(db1:3306)/isuconp", same: true},
		{name: "host case folded", driver: "mysql", a: "u:p@tcp(DB1:3306)/isuconp", b: "u:p@tcp(db1:3306)/isuconp", same: true},
		{name: "ipv6 canonicalised", driver: "mysql", a: "u:p@tcp([2001:DB8:0:0:0:0:0:1]:3306)/isuconp", b: "u:p@tcp([2001:db8::1]:3306)/isuconp", same: true},
		{name: "socket path cleaned", driver: "mysql", a: "u:p@unix(/tmp/../tmp/mysql.sock)/isuconp", b: "u:p@unix(/tmp/mysql.sock)/isuconp", same: true},
		{name: "net differs", driver: "mysql", a: "u:p@tcp(127.0.0.1:3306)/isuconp", b: "u:p@unix(/tmp/mysql.sock)/isuconp", same: false},
		{name: "database differs", driver: "mysql", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db1:3306)/isutrain", same: false},
		{name: "host differs", driver: "mysql", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db2:3306)/isuconp", same: false},
		{name: "port differs", driver: "mysql", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db1:13306)/isuconp", same: false},
		{name: "driver differs", driver: "", a: "u:p@tcp(db1:3306)/isuconp", b: "u:p@tcp(db1:3306)/isuconp", same: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driverA, driverB := tc.driver, tc.driver
			if tc.driver == "" {
				driverA, driverB = "mysql", "mariadb"
			}
			pa, ok := parseDSN(driverA, tc.a)
			if !ok {
				t.Fatalf("parseDSN(%q) failed", tc.a)
			}
			pb, ok := parseDSN(driverB, tc.b)
			if !ok {
				t.Fatalf("parseDSN(%q) failed", tc.b)
			}
			idA, idB := autoTargetID(driverA, pa), autoTargetID(driverB, pb)
			if (idA == idB) != tc.same {
				t.Fatalf("ids %q vs %q: same=%v, want %v", idA, idB, idA == idB, tc.same)
			}
		})
	}
}

func TestAutoTargetIDFormat(t *testing.T) {
	parsed, ok := parseDSN("mysql", "isuconp:isuconp@tcp(db1:3306)/isuconp")
	if !ok {
		t.Fatal("parseDSN failed")
	}
	id := autoTargetID("mysql", parsed)
	const wantAlias = "mysql-db1_3306-isuconp"
	if !strings.HasPrefix(id, wantAlias+"-") {
		t.Fatalf("id = %q, want alias prefix %q", id, wantAlias)
	}
	hash := strings.TrimPrefix(id, wantAlias+"-")
	if len(hash) != 26 {
		t.Fatalf("hash = %q (%d chars), want 26", hash, len(hash))
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			t.Fatalf("hash %q has byte %q outside the lowercase base32 alphabet", hash, c)
		}
	}
	if err := validateTargetID(id); err != nil {
		t.Fatalf("auto id must satisfy the explicit id rules: %v", err)
	}
	if again := autoTargetID("mysql", parsed); again != id {
		t.Fatalf("derivation is not deterministic: %q != %q", again, id)
	}
}

func TestAliasIsSluggedAndTruncated(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "mysql-db1:3306-isuconp", want: "mysql-db1_3306-isuconp"},
		{name: "collapses runs", in: "mysql-/var//run/-db", want: "mysql-_var_run_-db"},
		{name: "lowercases", in: "MySQL-DB1:3306-ISUCONP", want: "mysql-db1_3306-isuconp"},
		{name: "truncates to 32", in: "postgresqlx-averyveryverylonghost:5432-database", want: "postgresqlx-averyveryverylonghos"},
		{name: "trims trailing separators", in: "mysql-db1:3306-", want: "mysql-db1_3306"},
		{name: "never empty", in: "///", want: "db"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := slug(tc.in); got != tc.want {
				t.Fatalf("slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateTargetID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{name: "short", id: "db1", ok: true},
		{name: "uppercase", id: "DB1", ok: true},
		{name: "punctuation", id: "shard-1_main.db", ok: true},
		{name: "64 bytes", id: strings.Repeat("a", 64), ok: true},
		{name: "empty", id: "", ok: false},
		{name: "65 bytes", id: strings.Repeat("a", 65), ok: false},
		{name: "space", id: "a b", ok: false},
		{name: "non ascii", id: "データベース", ok: false},
		{name: "slash", id: "db/1", ok: false},
		{name: "colon", id: "db:1", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTargetID(tc.id)
			if tc.ok && err != nil {
				t.Fatalf("validateTargetID(%q) = %v, want nil", tc.id, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("validateTargetID(%q) = nil, want an error", tc.id)
				}
				if !errors.Is(err, ErrInvalidTargetID) {
					t.Fatalf("error = %v, want ErrInvalidTargetID", err)
				}
			}
		})
	}
}

func TestTargetIDsAreComparedByteForByte(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "u:p@tcp(db1:3306)/isuconp")
	mustRegister(t, r, "DB1", "u:p@tcp(db2:3306)/isuconp")

	if _, ok := r.target("db1"); !ok {
		t.Fatal("db1 must exist")
	}
	if _, ok := r.target("DB1"); !ok {
		t.Fatal("DB1 must be a separate target")
	}
	if _, ok := r.target("Db1"); ok {
		t.Fatal("Db1 must not resolve: ids are compared byte for byte")
	}
	if _, ok := r.target(" db1"); ok {
		t.Fatal("ids must not be trimmed")
	}
}

func TestRegisterDBTargetErrors(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "u:p@tcp(db1:3306)/isuconp")

	t.Run("same tuple is idempotent", func(t *testing.T) {
		if err := r.registerTarget("db1", regDriverName, "other:cred@tcp(db1:3306)/isuconp"); err != nil {
			t.Fatalf("re-registering the same database must be a no-op: %v", err)
		}
	})
	t.Run("different tuple on the same id", func(t *testing.T) {
		err := r.registerTarget("db1", regDriverName, "u:p@tcp(db9:3306)/isuconp")
		if !errors.Is(err, ErrDuplicateTarget) {
			t.Fatalf("err = %v, want ErrDuplicateTarget", err)
		}
	})
	t.Run("invalid id", func(t *testing.T) {
		if err := r.registerTarget("db 2", regDriverName, "u:p@tcp(db2:3306)/isuconp"); !errors.Is(err, ErrInvalidTargetID) {
			t.Fatalf("err = %v, want ErrInvalidTargetID", err)
		}
	})
	t.Run("unknown driver", func(t *testing.T) {
		if err := r.registerTarget("db2", "no-such-driver", "u:p@tcp(db2:3306)/isuconp"); !errors.Is(err, ErrUnknownDriver) {
			t.Fatalf("err = %v, want ErrUnknownDriver", err)
		}
	})
	t.Run("proxied driver name is accepted", func(t *testing.T) {
		if err := Register(regDriverName); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := r.registerTarget("db3", regDriverName+DriverSuffix, "u:p@tcp(db3:3306)/isuconp"); err != nil {
			t.Fatalf("registerTarget with proxied name: %v", err)
		}
		info, ok := r.target("db3")
		if !ok || info.Driver != regDriverName {
			t.Fatalf("info = %#v, want the base driver name", info)
		}
	})
}

func TestRegisterDBTargetAcceptsUnparsedDSNAndHidesIt(t *testing.T) {
	r := newTestRegistry(t)
	const dsn = "host=127.0.0.1 port=5432 user=isucon password=isucon dbname=isuconp"
	if err := r.registerTarget("pg", regDriverName, dsn); err != nil {
		t.Fatalf("explicit registration must accept an unparsed DSN: %v", err)
	}
	info, ok := r.target("pg")
	if !ok {
		t.Fatal("target pg is missing")
	}
	if info.Display != "(unparsed dsn)" {
		t.Fatalf("Display = %q, want the placeholder", info.Display)
	}
	if info.Schema != "" {
		t.Fatalf("Schema = %q, want empty for an unparsed DSN", info.Schema)
	}
	if _, known := r.features("pg"); known {
		t.Fatal("features must be unknown for an unparsed DSN")
	}
	if _, ok := r.targetIDForDSN(regDriverName, dsn); ok {
		t.Fatal("an unparsed DSN must not resolve to an id")
	}
	err := r.inspect(context.Background(), "pg", PurposeStats, func(context.Context, Querier) error { return nil })
	if !errors.Is(err, ErrUnparsedDSN) {
		t.Fatalf("err = %v, want ErrUnparsedDSN (hygiene cannot be applied)", err)
	}
	if notes := r.notesSnapshot(); len(notes) == 0 || !strings.Contains(notes[0], "unparsed DSN") {
		t.Fatalf("notes = %v, want an unparsed DSN note", notes)
	}
}

func TestExplicitRegistrationMustPrecedeObservation(t *testing.T) {
	r := newTestRegistry(t)
	const dsn = "u:p@tcp(db1:3306)/isuconp"
	r.observe(regDriverName, dsn)

	autoID, ok := r.targetIDForDSN(regDriverName, dsn)
	if !ok {
		t.Fatal("observation must register the target")
	}
	err := r.registerTarget("db1", regDriverName, dsn)
	if !errors.Is(err, ErrDuplicateTarget) {
		t.Fatalf("err = %v, want ErrDuplicateTarget", err)
	}
	if !strings.Contains(err.Error(), autoID) {
		t.Fatalf("error %v should name the auto-registered id %q", err, autoID)
	}
}

func TestObservationIsDeduplicatedAndBounded(t *testing.T) {
	r := newTestRegistry(t)
	const dsn = "u:p@tcp(db1:3306)/isuconp"

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The same endpoint reached with different credentials is one target.
			if i%2 == 0 {
				r.observe(regDriverName, dsn)
				return
			}
			r.observe(regDriverName, "other:cred@tcp(DB1)/isuconp")
		}(i)
	}
	wg.Wait()

	if got := r.targets(); len(got) != 1 {
		t.Fatalf("targets = %#v, want exactly one", got)
	}
	r.observe(regDriverName, "not a dsn")
	if got := r.targets(); len(got) != 1 {
		t.Fatalf("an unparsed DSN must not create a target: %#v", got)
	}
	notes := r.notesSnapshot()
	if len(notes) != 1 || !strings.Contains(notes[0], "unparsed DSN observed") {
		t.Fatalf("notes = %v, want one unparsed-DSN note", notes)
	}
	r.observe(regDriverName, "not a dsn either")
	if got := r.notesSnapshot(); len(got) != 1 {
		t.Fatalf("notes = %v, want the note to be recorded once", got)
	}
}

func TestTwoExplicitTargetsMaySharePurposeButLookupBecomesAmbiguous(t *testing.T) {
	r := newTestRegistry(t)
	const dsn = "u:p@tcp(db1:3306)/isuconp"
	mustRegister(t, r, "reads", dsn)
	mustRegister(t, r, "writes", dsn)

	if _, ok := r.targetIDForDSN(regDriverName, dsn); ok {
		t.Fatal("reverse lookup must fail when one endpoint has two names")
	}
	if got := r.targets(); len(got) != 2 || got[0].ID != "reads" || got[1].ID != "writes" {
		t.Fatalf("targets = %#v, want reads and writes sorted by id", got)
	}
}

func TestRegistryEnforcesMaxTargets(t *testing.T) {
	r := newTestRegistry(t)
	for i := 0; i < MaxTargets; i++ {
		mustRegister(t, r, "db"+string(rune('a'+i)), "u:p@tcp(db"+string(rune('a'+i))+":3306)/isuconp")
	}
	err := r.registerTarget("overflow", regDriverName, "u:p@tcp(overflow:3306)/isuconp")
	if !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("err = %v, want ErrTooManyTargets", err)
	}
	r.observe(regDriverName, "u:p@tcp(observed:3306)/isuconp")
	if got := r.targets(); len(got) != MaxTargets {
		t.Fatalf("len(targets) = %d, want %d", len(got), MaxTargets)
	}
	notes := r.notesSnapshot()
	if len(notes) != 2 {
		t.Fatalf("notes = %v, want one note per dropped target", notes)
	}
}

func TestDisplayNeverContainsCredentials(t *testing.T) {
	secrets := []struct {
		name   string
		dsn    string
		user   string
		pass   string
		want   string
		schema string
	}{
		{
			name: "mysql tcp", dsn: "appuser:s3cr3t@tcp(db1:3306)/isuconp?interpolateParams=true",
			user: "appuser", pass: "s3cr3t", want: "tcp(db1:3306)/isuconp", schema: "isuconp",
		},
		{
			name: "password contains at sign", dsn: "root:p@ssw0rd@tcp(127.0.0.1)/isuconp",
			user: "root", pass: "p@ssw0rd", want: "tcp(127.0.0.1:3306)/isuconp", schema: "isuconp",
		},
		{
			name: "unix socket", dsn: "adminuser:hunter2@unix(/var/run/mysqld/mysqld.sock)/isuconp",
			user: "adminuser", pass: "hunter2", want: "unix(/var/run/mysqld/mysqld.sock)/isuconp", schema: "isuconp",
		},
		{
			name: "url form", dsn: "postgres://pguser:pgsecret@db2:5432/isuconp?sslmode=disable",
			user: "pguser", pass: "pgsecret", want: "postgres://db2:5432/isuconp", schema: "isuconp",
		},
		{
			name: "url form with an at sign in the password", dsn: "postgres://pguser:pg@secret@db2:5432/isuconp",
			user: "pguser", pass: "pg@secret", want: "postgres://db2:5432/isuconp", schema: "isuconp",
		},
		{
			name: "no host", dsn: "solouser:solopass@/isuconp",
			user: "solouser", pass: "solopass", want: "tcp(127.0.0.1:3306)/isuconp", schema: "isuconp",
		},
		{
			name: "credentials without an at sign are not a dsn", dsn: "leakuser:leakpass/isuconp",
			user: "leakuser", pass: "leakpass", want: "(unparsed dsn)", schema: "",
		},
	}
	for _, tc := range secrets {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRegistry(t)
			mustRegister(t, r, "db1", tc.dsn)
			info, ok := r.target("db1")
			if !ok {
				t.Fatal("target missing")
			}
			if info.Display != tc.want {
				t.Fatalf("Display = %q, want %q", info.Display, tc.want)
			}
			if info.Schema != tc.schema {
				t.Fatalf("Schema = %q, want %q", info.Schema, tc.schema)
			}
			if strings.Contains(info.Display, tc.user) || strings.Contains(info.Display, tc.pass) {
				t.Fatalf("Display %q leaks a credential", info.Display)
			}
			if strings.ContainsAny(info.Display, "?&@") {
				t.Fatalf("Display %q must not carry dsn parameters or userinfo", info.Display)
			}
		})
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name     string
		driver   string
		dsn      string
		ok       bool
		form     dsnForm
		network  string
		addr     string
		database string
	}{
		{name: "full mysql", driver: "mysql", dsn: "u:p@tcp(db1:3306)/isuconp?parseTime=true", ok: true, form: formMySQL, network: "tcp", addr: "db1:3306", database: "isuconp"},
		{name: "no credentials", driver: "mysql", dsn: "tcp(db1:3306)/isuconp", ok: true, form: formMySQL, network: "tcp", addr: "db1:3306", database: "isuconp"},
		{name: "net without address", driver: "mysql", dsn: "u:p@tcp/isuconp", ok: true, form: formMySQL, network: "tcp", addr: "127.0.0.1:3306", database: "isuconp"},
		{name: "bare database", driver: "mysql", dsn: "/isuconp", ok: true, form: formMySQL, network: "tcp", addr: "127.0.0.1:3306", database: "isuconp"},
		{name: "no database", driver: "mysql", dsn: "u:p@tcp(db1:3306)/", ok: true, form: formMySQL, network: "tcp", addr: "db1:3306", database: ""},
		{name: "unix default socket", driver: "mysql", dsn: "u:p@unix/isuconp", ok: true, form: formMySQL, network: "unix", addr: "/tmp/mysql.sock", database: "isuconp"},
		{name: "url form", driver: "pgx", dsn: "postgres://u:p@db2/isuconp?sslmode=disable", ok: true, form: formURL, network: "tcp", addr: "db2:5432", database: "isuconp"},
		{name: "url form with port", driver: "pgx", dsn: "postgresql://db2:6432/isuconp", ok: true, form: formURL, network: "tcp", addr: "db2:6432", database: "isuconp"},
		{name: "url form unknown scheme has no default port", driver: "sqlserver", dsn: "sqlserver://host/isuconp", ok: true, form: formURL, network: "tcp", addr: "host", database: "isuconp"},
		{name: "empty", driver: "mysql", dsn: "", ok: false},
		{name: "no slash", driver: "mysql", dsn: "u:p@tcp(db1:3306)", ok: false},
		{name: "keyword value form", driver: "pgx", dsn: "host=db2 port=5432 dbname=isuconp", ok: false},
		{name: "credentials without at sign", driver: "mysql", dsn: "u:p/isuconp", ok: false},
		{name: "unterminated address", driver: "mysql", dsn: "u:p@tcp(db1:3306/isuconp", ok: false},
		{name: "url with an at sign in the password", driver: "pgx", dsn: "postgres://u:p@ss@db2/isuconp", ok: true, form: formURL, network: "tcp", addr: "db2:5432", database: "isuconp"},
		{name: "url with an invalid port", driver: "pgx", dsn: "postgres://db2:notaport/isuconp", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDSN(tc.driver, tc.dsn)
			if ok != tc.ok {
				t.Fatalf("parseDSN(%q) ok = %v, want %v (got %#v)", tc.dsn, ok, tc.ok, got)
			}
			if !tc.ok {
				return
			}
			if got.form != tc.form || got.network != tc.network || got.addr != tc.addr || got.database != tc.database {
				t.Fatalf("parsed = {form:%v net:%q addr:%q db:%q}, want {form:%v net:%q addr:%q db:%q}",
					got.form, got.network, got.addr, got.database, tc.form, tc.network, tc.addr, tc.database)
			}
		})
	}
}

func TestInspectorDSNAppliesConnectionHygiene(t *testing.T) {
	const hygiene = "interpolateParams=false&loc=UTC&multiStatements=false&parseTime=true&readTimeout=2s&timeout=1s&writeTimeout=2s"
	tests := []struct {
		name string
		dsn  string
		want string
		note bool
	}{
		{
			name: "drops database and overrides parameters",
			dsn:  "app:secret@tcp(db1:3306)/isuconp?multiStatements=true&interpolateParams=true&parseTime=false",
			want: "app:secret@tcp(db1:3306)/?" + hygiene,
		},
		{
			name: "keeps connectivity parameters",
			dsn:  "app:secret@tcp(db1:3306)/isuconp?charset=utf8mb4&tls=skip-verify",
			want: "app:secret@tcp(db1:3306)/?charset=utf8mb4&interpolateParams=false&loc=UTC&multiStatements=false&parseTime=true&readTimeout=2s&timeout=1s&tls=skip-verify&writeTimeout=2s",
		},
		{
			name: "unix socket",
			dsn:  "app@unix(/tmp/mysql.sock)/isuconp",
			want: "app@unix(/tmp/mysql.sock)/?" + hygiene,
		},
		{
			name: "no host and no credentials",
			dsn:  "/isuconp",
			want: "/?" + hygiene,
		},
		{
			name: "url form is left alone with a note",
			dsn:  "postgres://u:p@db2:5432/isuconp",
			want: "postgres://u:p@db2:5432/isuconp",
			note: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := parseDSN("mysql", tc.dsn)
			if !ok {
				t.Fatalf("parseDSN(%q) failed", tc.dsn)
			}
			got, note, err := parsed.inspectorDSN(tc.dsn)
			if err != nil {
				t.Fatalf("inspectorDSN: %v", err)
			}
			if got != tc.want {
				t.Fatalf("inspectorDSN =\n%q\nwant\n%q", got, tc.want)
			}
			if (note != "") != tc.note {
				t.Fatalf("note = %q, want note=%v", note, tc.note)
			}
			if tc.note {
				return
			}
			// The rebuilt DSN must survive a round trip with no database.
			back, ok := parseDSN("mysql", got)
			if !ok {
				t.Fatal("the rebuilt DSN must be parseable")
			}
			if back.database != "" {
				t.Fatalf("database = %q, want empty", back.database)
			}
			if back.features != (DSNFeatures{ParseTime: true}) {
				t.Fatalf("features = %#v, want parseTime only", back.features)
			}
		})
	}
}

func TestInspectorDSNRejectsUnparsedDSN(t *testing.T) {
	var unparsed parsedDSN
	if _, _, err := unparsed.inspectorDSN("whatever"); !errors.Is(err, ErrUnparsedDSN) {
		t.Fatalf("err = %v, want ErrUnparsedDSN", err)
	}
}

func TestFeaturesHasThreeStates(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "on", "u:p@tcp(db1:3306)/isuconp?interpolateParams=true&multiStatements=1")
	mustRegister(t, r, "off", "u:p@tcp(db2:3306)/isuconp")
	mustRegister(t, r, "unknown", "host=db3 dbname=isuconp")

	on, known := r.features("on")
	if !known || !on.InterpolateParams || !on.MultiStatements || on.ParseTime {
		t.Fatalf("features = %#v, known = %v", on, known)
	}
	off, known := r.features("off")
	if !known || off != (DSNFeatures{}) {
		t.Fatalf("features = %#v, known = %v", off, known)
	}
	if _, known := r.features("unknown"); known {
		t.Fatal("an unparsed DSN must report known = false")
	}
	if _, known := r.features("missing"); known {
		t.Fatal("an unregistered id must report known = false")
	}

	// A purpose credential must not change what Features reports.
	if err := r.registerInspector("off", PurposeExplain, regDriverName, "explain:pw@tcp(db2:3306)/isuconp?interpolateParams=true"); err != nil {
		t.Fatalf("registerInspector: %v", err)
	}
	after, known := r.features("off")
	if !known || after != (DSNFeatures{}) {
		t.Fatalf("features = %#v after registering an explain credential, want unchanged", after)
	}
}

func TestPurposeRegistrationKeepsIdentityStable(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")
	before, _ := r.target("db1")

	if err := r.registerInspector("db1", PurposeStats, regDriverName, "stats:statspw@tcp(db1:3306)/mysql"); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if err := r.registerInspector("db1", PurposeExplain, regDriverName, "explain:explainpw@tcp(db1:3306)/"); err != nil {
		t.Fatalf("explain: %v", err)
	}
	after, _ := r.target("db1")
	if after.ID != before.ID || after.Display != before.Display || after.Schema != before.Schema || after.Driver != before.Driver {
		t.Fatalf("identity changed: %#v -> %#v", before, after)
	}
	want := []Purpose{PurposeApp, PurposeStats, PurposeExplain}
	if len(after.Purposes) != len(want) {
		t.Fatalf("purposes = %v, want %v", after.Purposes, want)
	}
	for i, p := range want {
		if after.Purposes[i] != p {
			t.Fatalf("purposes = %v, want %v", after.Purposes, want)
		}
	}
	if strings.Contains(after.Display, "statspw") || strings.Contains(after.Display, "mysql") {
		t.Fatalf("Display %q must not reflect purpose credentials", after.Display)
	}
}

func TestRegisterDBInspectorErrors(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")

	tests := []struct {
		name    string
		id      string
		purpose Purpose
		driver  string
		dsn     string
		want    error
	}{
		{name: "unknown target", id: "nope", purpose: PurposeStats, driver: regDriverName, dsn: "u:p@tcp(db1:3306)/", want: ErrUnknownTarget},
		{name: "app purpose", id: "db1", purpose: PurposeApp, driver: regDriverName, dsn: "u:p@tcp(db1:3306)/", want: ErrInvalidPurpose},
		{name: "unknown purpose", id: "db1", purpose: Purpose("audit"), driver: regDriverName, dsn: "u:p@tcp(db1:3306)/", want: ErrInvalidPurpose},
		{name: "unknown driver", id: "db1", purpose: PurposeStats, driver: "no-such-driver", dsn: "u:p@tcp(db1:3306)/", want: ErrUnknownDriver},
		{name: "unparsed dsn", id: "db1", purpose: PurposeStats, driver: regDriverName, dsn: "host=db1 dbname=isuconp", want: ErrUnparsedDSN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.registerInspector(tc.id, tc.purpose, tc.driver, tc.dsn); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	if err := r.registerInspector("db1", PurposeStats, regDriverName, "stats:pw@tcp(db1:3306)/"); err != nil {
		t.Fatalf("first stats registration: %v", err)
	}
	if err := r.registerInspector("db1", PurposeStats, regDriverName, "stats:pw@tcp(db1:3306)/"); !errors.Is(err, ErrDuplicatePurpose) {
		t.Fatalf("err = %v, want ErrDuplicatePurpose", err)
	}
}

func TestInspectRejectsUnknownTargetAndPurpose(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")
	noop := func(context.Context, Querier) error { return nil }

	if err := r.inspect(context.Background(), "nope", PurposeStats, noop); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v, want ErrUnknownTarget", err)
	}
	if err := r.inspect(context.Background(), "db1", PurposeApp, noop); !errors.Is(err, ErrInvalidPurpose) {
		t.Fatalf("err = %v, want ErrInvalidPurpose", err)
	}
	if err := r.inspect(context.Background(), "db1", Purpose("audit"), noop); !errors.Is(err, ErrInvalidPurpose) {
		t.Fatalf("err = %v, want ErrInvalidPurpose", err)
	}
	if err := r.inspect(context.Background(), "db1", PurposeStats, nil); err == nil {
		t.Fatal("a nil callback must be rejected")
	}
}

func TestInspectExplainDoesNotFallBackToTheAppCredential(t *testing.T) {
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "appuser:apppw@tcp(db1:3306)/isuconp")

	called := false
	err := r.inspect(context.Background(), "db1", PurposeExplain, func(context.Context, Querier) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrPurposeNotRegistered) {
		t.Fatalf("err = %v, want ErrPurposeNotRegistered", err)
	}
	if called {
		t.Fatal("the callback must not run without an explain credential")
	}
	if dsns, _, _ := regDriver.snapshot(); len(dsns) != 0 {
		t.Fatalf("no connection may be opened: %v", dsns)
	}

	if err := r.registerInspector("db1", PurposeExplain, regDriverName, "explainuser:explainpw@tcp(db1:3306)/isuconp"); err != nil {
		t.Fatalf("registerInspector: %v", err)
	}
	if err := r.inspect(context.Background(), "db1", PurposeExplain, func(context.Context, Querier) error { return nil }); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	dsns, _, _ := regDriver.snapshot()
	if len(dsns) != 1 {
		t.Fatalf("dsns = %v, want exactly one connection", dsns)
	}
	if !strings.HasPrefix(dsns[0], "explainuser:explainpw@") {
		t.Fatalf("explain used %q, want the explain credential", dsns[0])
	}
}

func TestInspectStatsFallsBackToHygienicAppCredential(t *testing.T) {
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "appuser:apppw@tcp(db1:3306)/isuconp?multiStatements=true&interpolateParams=true")

	var seen string
	err := r.inspect(context.Background(), "db1", PurposeStats, func(ctx context.Context, q Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT 1")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if cols, err := rows.Columns(); err != nil || len(cols) != 1 {
			t.Errorf("columns = %v, %v", cols, err)
		}
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return err
			}
		}
		seen = "ran"
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if seen != "ran" {
		t.Fatal("callback did not run")
	}

	dsns, stmts, _ := regDriver.snapshot()
	if len(dsns) != 1 {
		t.Fatalf("dsns = %v, want one", dsns)
	}
	dsn := dsns[0]
	if !strings.HasPrefix(dsn, "appuser:apppw@") {
		t.Fatalf("stats must fall back to the app credential, got %q", dsn)
	}
	parsed, ok := parseDSN(regDriverName, dsn)
	if !ok {
		t.Fatalf("rebuilt dsn %q is not parseable", dsn)
	}
	if parsed.database != "" {
		t.Fatalf("database = %q, want empty so collector queries are not attributed to the app schema", parsed.database)
	}
	for key, want := range inspectorParams {
		if got := parsed.params[key]; got != want {
			t.Errorf("param %s = %q, want %q", key, got, want)
		}
	}
	if len(stmts) == 0 || stmts[0] != sessionInitStatement {
		t.Fatalf("statements = %v, want the session init statement first", stmts)
	}
}

func TestInspectExecIsRestrictedToSessionSettings(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{name: "set time zone", query: `SET time_zone = '+00:00'`, allowed: true},
		{name: "lowercase set", query: "set names utf8mb4", allowed: true},
		{name: "leading whitespace", query: "\n\t SET ROLE NONE", allowed: true},
		{name: "trailing semicolon", query: "SET ROLE NONE;", allowed: true},
		{name: "select", query: "SELECT 1", allowed: false},
		{name: "update", query: "UPDATE performance_schema.threads SET INSTRUMENTED='NO'", allowed: false},
		{name: "use", query: "USE isuconp", allowed: false},
		{name: "batch", query: "SET ROLE NONE; DROP TABLE users", allowed: false},
		{name: "settings prefix is not set", query: "SETTINGS foo", allowed: false},
		{name: "empty", query: "   ", allowed: false},
	}
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := r.inspect(context.Background(), "db1", PurposeStats, func(ctx context.Context, q Querier) error {
				_, err := q.ExecContext(ctx, tc.query)
				return err
			})
			switch {
			case tc.allowed && err != nil:
				t.Fatalf("ExecContext(%q) = %v, want nil", tc.query, err)
			case !tc.allowed && !errors.Is(err, ErrExecNotAllowed):
				t.Fatalf("ExecContext(%q) = %v, want ErrExecNotAllowed", tc.query, err)
			}
		})
	}
}

func TestInspectForceClosesLeakedRowsAndDisablesTheQuerier(t *testing.T) {
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")

	var escaped Querier
	var leaked Rows
	err := r.inspect(context.Background(), "db1", PurposeStats, func(ctx context.Context, q Querier) error {
		escaped = q
		rows, err := q.QueryContext(ctx, "SELECT c FROM t")
		if err != nil {
			return err
		}
		leaked = rows
		if !rows.Next() { // consume one row of three, then leak the result set
			t.Error("expected at least one row")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	regDriver.mu.Lock()
	driverRows := append([]*recordingRows(nil), regDriver.rows...)
	regDriver.mu.Unlock()
	if len(driverRows) != 1 {
		t.Fatalf("driver produced %d result sets, want 1", len(driverRows))
	}
	if !driverRows[0].isClosed() {
		t.Fatal("Inspect must force-close a result set the callback left open")
	}
	if err := leaked.Close(); err != nil {
		t.Fatalf("closing an already-closed wrapper must be a no-op: %v", err)
	}

	ctx := context.Background()
	if _, err := escaped.QueryContext(ctx, "SELECT 1"); !errors.Is(err, errQuerierDone) {
		t.Fatalf("QueryContext after return = %v, want errQuerierDone", err)
	}
	if _, err := escaped.ExecContext(ctx, "SET ROLE NONE"); !errors.Is(err, errQuerierDone) {
		t.Fatalf("ExecContext after return = %v, want errQuerierDone", err)
	}
	if err := escaped.QueryRowContext(ctx, "SELECT 1").Scan(new(int64)); !errors.Is(err, errQuerierDone) {
		t.Fatalf("QueryRowContext after return = %v, want errQuerierDone", err)
	}
	if err := escaped.QueryRowContext(ctx, "SELECT 1").Err(); !errors.Is(err, errQuerierDone) {
		t.Fatalf("Row.Err after return = %v, want errQuerierDone", err)
	}
}

func TestInspectQueryRowUsesThePinnedConnection(t *testing.T) {
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")

	var got int64
	err := r.inspect(context.Background(), "db1", PurposeStats, func(ctx context.Context, q Querier) error {
		return q.QueryRowContext(ctx, "SELECT c").Scan(&got)
	})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got != 1 {
		t.Fatalf("scanned %d, want 1", got)
	}
	_, stmts, _ := regDriver.snapshot()
	if len(stmts) != 2 || stmts[1] != "SELECT c" {
		t.Fatalf("statements = %v, want the session init plus the query", stmts)
	}
}

func TestInspectKeepsOneConnectionPerTargetAndPurpose(t *testing.T) {
	regDriver.reset()
	r := newTestRegistry(t)
	mustRegister(t, r, "db1", "app:apppw@tcp(db1:3306)/isuconp")

	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.inspect(context.Background(), "db1", PurposeStats, func(ctx context.Context, q Querier) error {
				rows, err := q.QueryContext(ctx, "SELECT 1")
				if err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
				return rows.Close()
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
	}
	dsns, _, peak := regDriver.snapshot()
	if peak > 1 {
		t.Fatalf("peak simultaneous connections = %d, want at most 1", peak)
	}
	if len(dsns) != 1 {
		t.Fatalf("opened %d connections, want the handle to be reused", len(dsns))
	}
}

func TestObservedDSNBecomesATargetThroughTheProxyDriver(t *testing.T) {
	if err := Register(regDriverName); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(Default.Reset)

	const dsn = "obsuser:obspw@tcp(observed.example:3306)/isuobserved"
	db, err := sql.Open(regDriverName+DriverSuffix, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	id, ok := TargetIDForDSN(regDriverName, dsn)
	if !ok {
		t.Fatal("opening a connection must register its target")
	}
	if viaProxyName, ok := TargetIDForDSN(regDriverName+DriverSuffix, dsn); !ok || viaProxyName != id {
		t.Fatalf("lookup by proxied driver name = %q, %v; want %q", viaProxyName, ok, id)
	}
	info, ok := Target(id)
	if !ok {
		t.Fatalf("Target(%q) is missing", id)
	}
	if info.Display != "tcp(observed.example:3306)/isuobserved" || info.Schema != "isuobserved" {
		t.Fatalf("info = %#v", info)
	}
	if strings.Contains(info.Display, "obspw") {
		t.Fatalf("Display %q leaks the password", info.Display)
	}
	if features, known := Features(id); !known || features != (DSNFeatures{}) {
		t.Fatalf("features = %#v, known = %v", features, known)
	}
	found := false
	for _, target := range Targets() {
		if target.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("Targets() does not list %q", id)
	}
	if err := RegisterDBTarget("", regDriverName, dsn); !errors.Is(err, ErrInvalidTargetID) {
		t.Fatalf("RegisterDBTarget = %v, want ErrInvalidTargetID", err)
	}
	if err := RegisterDBInspector("nope", PurposeStats, regDriverName, dsn); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("RegisterDBInspector = %v, want ErrUnknownTarget", err)
	}
	if err := Inspect(context.Background(), "nope", PurposeStats, func(context.Context, Querier) error { return nil }); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("Inspect = %v, want ErrUnknownTarget", err)
	}
	_ = Notes()
}
