package dbpool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// The whole package is tested without a database. Pools are either real
// *sql.DB values opened on a connector that never connects, or scripted
// samplers injected through watchStats, which is what lets the delta
// arithmetic be driven precisely.

const (
	fakeDriverName = "isutoolsdbpoolfake"
	appDSN         = "isucon:isucon@tcp(127.0.0.1:3306)/isuconp?parseTime=true"
	appDisplay     = "tcp(127.0.0.1:3306)/isuconp"
	secondDSN      = "isucon:isucon@tcp(127.0.0.1:3307)/isuconp?parseTime=true"
)

var errNoConnection = errors.New("dbpool test: this driver never connects")

func init() { sql.Register(fakeDriverName, fakeDriver{}) }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errNoConnection }

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return nil, errNoConnection }
func (fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

// newFakeDB returns a usable *sql.DB whose Stats can be read without any
// connection ever being established.
func newFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeConnector{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// registerTarget makes id resolvable through the process-wide registry.
// RegisterDBTarget is idempotent for the same database, so tests may share ids.
func registerTarget(t *testing.T, id, dsn string) {
	t.Helper()
	if err := sqlstats.RegisterDBTarget(id, fakeDriverName, dsn); err != nil {
		t.Fatalf("RegisterDBTarget(%q) = %v, want nil", id, err)
	}
}

// scriptedStats is a sampler that returns prepared DBStats values in order,
// repeating the last one, and counts how often it was read. The count is what
// proves that Collect performs no sampling of its own.
type scriptedStats struct {
	mu         sync.Mutex
	values     []sql.DBStats
	calls      int
	panicAfter int // panic from this call index onwards; 0 disables
}

func newScript(values ...sql.DBStats) *scriptedStats {
	return &scriptedStats{values: values, panicAfter: -1}
}

func (s *scriptedStats) stats() sql.DBStats {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.mu.Unlock()
	if s.panicAfter >= 0 && index >= s.panicAfter {
		panic("dbpool test: sampler exploded")
	}
	if len(s.values) == 0 {
		return sql.DBStats{}
	}
	if index >= len(s.values) {
		index = len(s.values) - 1
	}
	return s.values[index]
}

func (s *scriptedStats) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeClock gives the tests control over the boundary timestamps, so a
// farewell sample can be shown to be stamped strictly before the closing
// boundary rather than merely "around the same time".
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newTestCollector returns a collector on a controllable clock.
func newTestCollector() (*Collector, *fakeClock) {
	clock := newClock()
	c := New()
	c.now = clock.Now
	return c, clock
}

// captureFunc is the shape both boundary methods share.
type captureFunc func(context.Context, string, runctl.Epoch) (runctl.SampleResult, error)

// mustCapture takes a boundary sample and fails the test if it does not
// commit.
func mustCapture(t *testing.T, capture captureFunc, runID string, epoch runctl.Epoch) runctl.SampleResult {
	t.Helper()
	res, err := capture(context.Background(), runID, epoch)
	if err != nil {
		t.Fatalf("capture(%q, %d) = %v, want nil", runID, epoch, err)
	}
	if !res.Committed {
		t.Fatalf("capture(%q, %d) committed = false, want true", runID, epoch)
	}
	return res
}

// mustCollect derives the report and fails the test if it cannot.
func mustCollect(t *testing.T, c *Collector, base, final runctl.SampleResult) []Entry {
	t.Helper()
	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect = %v, want nil", err)
	}
	entries, ok := value.([]Entry)
	if !ok {
		t.Fatalf("Collect returned %T, want []dbpool.Entry", value)
	}
	return entries
}

// collectEntries runs a full boundary pair and returns the report.
func collectEntries(t *testing.T, c *Collector, runID string, epoch runctl.Epoch) []Entry {
	t.Helper()
	base := mustCapture(t, c.CaptureBaseline, runID, epoch)
	final := mustCapture(t, c.CaptureFinal, runID, epoch)
	return mustCollect(t, c, base, final)
}

// entryByID looks one entry up by TargetID.
func entryByID(t *testing.T, entries []Entry, targetID string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.TargetID == targetID {
			return e
		}
	}
	t.Fatalf("no entry for %q in %+v", targetID, entries)
	return Entry{}
}

// targetIDs lists the reported TargetIDs in report order.
func targetIDs(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.TargetID)
	}
	return out
}
