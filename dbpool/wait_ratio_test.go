package dbpool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"runtime"
	"testing"
	"time"
)

type waitingDriver struct{}

func (waitingDriver) Open(string) (driver.Conn, error) { return waitingConn{}, nil }

type waitingConnector struct{}

func (waitingConnector) Connect(context.Context) (driver.Conn, error) { return waitingConn{}, nil }
func (waitingConnector) Driver() driver.Driver                        { return waitingDriver{} }

type waitingConn struct{}

func (waitingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (waitingConn) Close() error                        { return nil }
func (waitingConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

// A real database/sql wait that starts before the baseline and ends after it
// contributes duration without count to this run. There is no single cohort
// whose average can be computed by dividing the two boundary deltas.
func TestWaitRatioUnavailableWhenWaitCrossesBaseline(t *testing.T) {
	db := sql.OpenDB(waitingConnector{})
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	held, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	c := New()
	if err := c.watchStats("app", "app", db.Stats); err != nil {
		t.Fatal(err)
	}

	waiter := make(chan error, 1)
	go func() {
		conn, err := db.Conn(ctx)
		if err == nil {
			err = conn.Close()
		}
		waiter <- err
	}()
	for db.Stats().WaitCount == 0 {
		if err := ctx.Err(); err != nil {
			t.Fatalf("waiter did not enter database/sql queue: %v", err)
		}
		runtime.Gosched()
	}
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("waiter did not complete: %v", ctx.Err())
	}
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	entry := entryByID(t, mustCollect(t, c, base, final), "app")
	if entry.WaitCount != 0 || entry.WaitDuration <= 0 {
		t.Fatalf("boundary deltas = count %d, duration %s; want 0 and positive", entry.WaitCount, entry.WaitDuration)
	}
	if got, ok := entry.WaitRatio(); ok || got != 0 {
		t.Fatalf("WaitRatio = (%s, %t), want unavailable", got, ok)
	}
}

func TestWaitRatioOnlyForCompletePositiveDeltas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
		ok    bool
	}{
		{"complete", Entry{WaitCount: 2, WaitDuration: 10 * time.Millisecond}, true},
		{"no_count", Entry{WaitDuration: time.Second}, false},
		{"no_duration", Entry{WaitCount: 1}, false},
		{"partial", Entry{WaitCount: 1, WaitDuration: time.Second, Partial: true}, false},
		{"code", Entry{WaitCount: 1, WaitDuration: time.Second, Code: CodeCounterRewind}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.entry.WaitRatio()
			if ok != tc.ok || (ok && got != 5*time.Millisecond) || (!ok && got != 0) {
				t.Fatalf("WaitRatio = (%s, %t), want availability %t", got, ok, tc.ok)
			}
		})
	}
}
