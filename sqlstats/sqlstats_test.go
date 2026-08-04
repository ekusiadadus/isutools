package sqlstats

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"
	"testing"
)

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (*fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{}, nil }
func (*fakeConn) Close() error                          { return nil }
func (*fakeConn) Begin() (driver.Tx, error)             { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

type fakeStmt struct{}

func (*fakeStmt) Close() error  { return nil }
func (*fakeStmt) NumInput() int { return -1 }
func (*fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (*fakeStmt) Query([]driver.Value) (driver.Rows, error) { return &fakeRows{}, nil }

type fakeRows struct{ done bool }

func (*fakeRows) Columns() []string { return []string{"c"} }
func (*fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

func init() {
	sql.Register("isutoolsfake", fakeDriver{})
	sql.Register("isutoolsconcurrentfake", fakeDriver{})
}

func TestRegisterAndObserveQueries(t *testing.T) {
	t.Cleanup(Default.Reset)
	if err := Register("isutoolsfake"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	db, err := sql.Open("isutoolsfake"+DriverSuffix, "dsn")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query("SELECT   1\nFROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec("UPDATE t SET c = ?"); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	entries := Default.Snapshot()
	byKey := map[string]int64{}
	for _, e := range entries {
		byKey[e.Key] = e.Count
	}
	if byKey["SELECT ? FROM t"] != 1 {
		t.Errorf("query not observed (normalized): %v", entries)
	}
	if byKey["UPDATE t SET c = ?"] != 1 {
		t.Errorf("exec not observed: %v", entries)
	}
}

func TestFirstConnCapturesDSN(t *testing.T) {
	t.Cleanup(Default.Reset)
	connMu.Lock()
	firstName, firstDSN = "", ""
	connMu.Unlock()
	if err := Register("isutoolsfake"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	db, err := sql.Open("isutoolsfake"+DriverSuffix, "user:pass@tcp(db:3306)/isuconp")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	name, dsn, ok := FirstConn()
	if !ok {
		t.Fatal("FirstConn: no connection captured")
	}
	if name != "isutoolsfake" {
		t.Errorf("driver = %q, want base name isutoolsfake", name)
	}
	if dsn != "user:pass@tcp(db:3306)/isuconp" {
		t.Errorf("dsn = %q", dsn)
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	if err := Register("isutoolsfake"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := Register("isutoolsfake"); err != nil {
		t.Fatalf("second must not panic or fail: %v", err)
	}
}

func TestRegisterUnknownDriverFails(t *testing.T) {
	if err := Register("no-such-driver"); err == nil {
		t.Fatal("want error for unknown driver, got nil")
	}
}

func TestRegisterIsSafeUnderConcurrency(t *testing.T) {
	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Register("isutoolsconcurrentfake")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
